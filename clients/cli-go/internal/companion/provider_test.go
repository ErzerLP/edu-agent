package companion

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/localexec"
	wire "github.com/edu-agent/edu-agent/packages/agentcore/companion"
)

func native(t *testing.T) (*Provider, wire.Grant) {
	t.Helper()
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("需要原生 Linux/macOS")
	}
	p, err := NewProvider(wire.Device{ID: "native", Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := p.Close(); err != nil {
			t.Error(err)
		}
	})
	return p, wire.Grant{Conversation: "conversation", Files: true, Shell: true}
}

func operation(t *testing.T, p *Provider, g wire.Grant, tool string, args any) wire.Receipt {
	t.Helper()
	raw, _ := json.Marshal(args)
	r := p.Execute(t.Context(), g, wire.Operation{ID: wire.Secret(), Run: "run", Tool: tool, Arguments: raw})
	if r.Error != "" {
		t.Fatalf("操作失败：%s %s", r.Error, r.Value)
	}
	return r
}

func TestRealFilesRequireFrozenApprovalAndPreserveWorkspace(t *testing.T) {
	p, g := native(t)
	r := operation(t, p, g, "prepare_write", map[string]any{"path": "note.txt", "mode": "create", "content": "学习内容"})
	var preview struct{ Plan, Preview string }
	if json.Unmarshal(r.Value, &preview) != nil || preview.Plan == "" || !strings.Contains(preview.Preview, "学习内容") {
		t.Fatalf("未产生真实预览：%s", r.Value)
	}
	if _, err := os.Stat(filepath.Join(p.device.Workspace, "note.txt")); !os.IsNotExist(err) {
		t.Fatal("确认前发布了文件")
	}
	operation(t, p, g, "commit", map[string]string{"plan": preview.Plan})
	data, err := os.ReadFile(filepath.Join(p.device.Workspace, "note.txt"))
	if err != nil || string(data) != "学习内容" {
		t.Fatal("确认后未发布真实文件", err)
	}
	r = operation(t, p, g, "read", map[string]string{"path": "note.txt"})
	if !strings.Contains(string(r.Value), "学习内容") {
		t.Fatal("读取结果并非来自工作区")
	}
	hash := fmt.Sprintf("sha256:%x", sha256.Sum256(data))
	r = operation(t, p, g, "prepare_write", map[string]any{"path": "note.txt", "mode": "replace", "content": "冻结的新正文", "expected_hash": hash})
	if json.Unmarshal(r.Value, &preview) != nil || preview.Plan == "" {
		t.Fatal("替换未冻结")
	}
	if err = os.WriteFile(filepath.Join(p.device.Workspace, "note.txt"), []byte("外部新版本"), 0600); err != nil {
		t.Fatal(err)
	}
	operation(t, p, g, "commit", map[string]string{"plan": preview.Plan})
	data, err = os.ReadFile(filepath.Join(p.device.Workspace, "note.txt"))
	if err != nil || string(data) != "外部新版本" {
		t.Fatal("确认覆盖了过期文件版本")
	}
	outside := t.TempDir()
	if err = os.WriteFile(filepath.Join(outside, "secret"), []byte("不可读取"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink(outside, filepath.Join(p.device.Workspace, "escape")); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"../secret", "escape/secret"} {
		r = operation(t, p, g, "read", map[string]string{"path": path})
		if strings.Contains(string(r.Value), "不可读取") {
			t.Fatal("突破工作区")
		}
	}
	operation(t, p, g, "shell", map[string]any{"command": "printf native > shell-result", "cwd": outside, "wait_ms": 3000})
	if data, err := os.ReadFile(filepath.Join(outside, "shell-result")); err != nil || string(data) != "native" {
		t.Fatal("给原生 Shell 增加了文件工作区围栏", err)
	}
	other := g
	other.Conversation = "other"
	r = p.Execute(t.Context(), other, wire.Operation{ID: "cross", Tool: "read", Arguments: json.RawMessage(`{"path":"note.txt"}`)})
	if r.Error == "" {
		t.Fatal("跨对话访问 provider")
	}
}

func TestNativePartialInputKeepsActualReceipt(t *testing.T) {
	p, g := native(t)
	r := operation(t, p, g, "shell", map[string]any{"command": "sleep 5", "stdin": true, "wait_ms": 0})
	var task localexec.Snapshot
	json.Unmarshal(r.Value, &task)
	raw, _ := json.Marshal(map[string]string{"action": "input", "task_id": task.TaskID, "content": strings.Repeat("x", 48<<10)})
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	op := wire.Operation{ID: "partial-input", Run: "run", Tool: "task", Arguments: raw}
	r = p.Execute(ctx, g, op)
	var input localexec.InputResult
	_ = json.Unmarshal(r.Value, &input)
	if input.Outcome == "written" {
		ctx2, cancel2 := context.WithTimeout(t.Context(), 100*time.Millisecond)
		defer cancel2()
		op.ID = "partial-input-second"
		r = p.Execute(ctx2, g, op)
	}
	if json.Unmarshal(r.Value, &input) != nil || input.Written <= 0 || input.Written >= 48<<10 || input.Outcome != "partial" || r.Error == "" {
		t.Fatalf("部分输入回执丢失：%+v %s", input, r.Error)
	}
	if again := p.Execute(t.Context(), g, op); string(again.Value) != string(r.Value) {
		t.Fatal("部分输入被重试")
	}
}

func TestNativeShellInputReceiptDedupAndCleanup(t *testing.T) {
	p, g := native(t)
	r := operation(t, p, g, "shell", map[string]any{"command": "cat", "stdin": true, "wait_ms": 0})
	var task localexec.Snapshot
	json.Unmarshal(r.Value, &task)
	if task.TaskID == "" || !task.Controllable {
		t.Fatalf("没有真实任务：%s", r.Value)
	}
	raw, _ := json.Marshal(map[string]string{"action": "input", "task_id": task.TaskID, "content": "仅写入一次\n"})
	op := wire.Operation{ID: "input-once", Run: "run", Tool: "task", Arguments: raw}
	first := p.Execute(t.Context(), g, op)
	second := p.Execute(t.Context(), g, op)
	if first.Error != "" || string(first.Value) != string(second.Value) {
		t.Fatal("重复输入丢失回执")
	}
	operation(t, p, g, "task", map[string]string{"action": "close_input", "task_id": task.TaskID})
	operation(t, p, g, "task", map[string]any{"action": "wait", "task_id": task.TaskID, "wait_ms": 3000})
	page, err := p.tasks.Read(g.Conversation, task.TaskID, "stdout", 0, 4096)
	if err != nil || string(page.Data) != "仅写入一次\n" {
		t.Fatalf("stdin 重放或缺失：%q %v", page.Data, err)
	}
	if page.Availability != "memory" || page.Saved != 0 {
		t.Fatal("自动保存原生输出")
	}
	long := operation(t, p, g, "shell", map[string]any{"command": "sleep 30", "wait_ms": 0})
	json.Unmarshal(long.Value, &task)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err = p.tasks.Close(ctx); err != nil {
		t.Fatal(err)
	}
	status, err := p.tasks.Status(g.Conversation, task.TaskID)
	if err != nil || status.Controllable || status.State == localexec.StateRunning {
		t.Fatalf("退出没有收尾：%+v %v", status, err)
	}
	if _, err = p.tasks.Interrupt(t.Context(), g.Conversation, task.TaskID); err == nil {
		t.Fatal("向旧任务继续控制")
	}
}

func TestNativePTYAndOutputGap(t *testing.T) {
	p, g := native(t)
	r := operation(t, p, g, "shell", map[string]any{"command": "read value; printf '收到:%s\\n' \"$value\"", "pty": true, "rows": 24, "cols": 80, "wait_ms": 0})
	var task localexec.Snapshot
	json.Unmarshal(r.Value, &task)
	if !task.PTY {
		t.Fatalf("未分配 PTY：%s", r.Value)
	}
	operation(t, p, g, "task", map[string]any{"action": "resize", "task_id": task.TaskID, "rows": 40, "cols": 100})
	operation(t, p, g, "task", map[string]string{"action": "input", "task_id": task.TaskID, "content": "交互\n"})
	operation(t, p, g, "task", map[string]any{"action": "wait", "task_id": task.TaskID, "wait_ms": 3000})
	page, err := p.tasks.Read(g.Conversation, task.TaskID, "stdout", 0, 4096)
	if err != nil || !strings.Contains(string(page.Data), "收到:交互") {
		t.Fatalf("PTY 交互未完成：%q %v", page.Data, err)
	}
	_ = p.tasks.Close(t.Context())
	p.tasks = localexec.New(localexec.Options{OutputBytesPerTask: 16, OutputBytesTotal: 16})
	r = operation(t, p, g, "shell", map[string]any{"command": "printf 123456789012345678901234567890", "wait_ms": 3000})
	json.Unmarshal(r.Value, &task)
	page, err = p.tasks.Read(g.Conversation, task.TaskID, "stdout", 16, 16)
	if err != nil || !page.Truncated || page.NextOffset != 30 || page.Saved != 0 {
		t.Fatalf("输出缺口被伪装：%+v %v", page, err)
	}
}
