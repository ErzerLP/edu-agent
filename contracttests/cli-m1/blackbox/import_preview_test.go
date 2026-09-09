package blackbox

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBlackBoxImportPreviewConfirmedBatchAndOperation(t *testing.T) {
	h := newHarnessWithOptions(t, harnessOptions{withoutModel: true})
	h.primaryHome = h.newCLIHome("import-user")
	h.pair(h.primaryHome, h.serverURL, "导入验收")
	run := func(args ...string) commandResult {
		t.Helper()
		r := h.runCLI(h.primaryHome, "", args...)
		requireExit(t, r, 0, "导入工作流")
		if strings.Contains(string(r.stdout), "\x1b[") {
			t.Fatal("非 TTY 输出控制序列")
		}
		return r
	}
	var space struct {
		ID string `json:"id"`
	}
	created := run("space", "create", "--name", "Go 后端")
	if err := json.Unmarshal(created.stdout, &space); err != nil {
		t.Fatal(err)
	}
	collection := randomUUID(t)
	run("--space", space.ID, "knowledge", "library", "create", "--id", collection, "--name", "导入资料", "--source", "测试本地来源")
	root := t.TempDir()
	for name, body := range map[string]string{"a.md": "# Go\noriginal channel material\n", "b.txt": "# literal text\n```\n原文", "bad.md": string([]byte{255}), "skip.md": "excluded"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	scan := run("knowledge", "import", "scan", "--exclude", "skip.md", root)
	var report struct {
		Items []struct {
			Status   string          `json:"status"`
			Document json.RawMessage `json:"document"`
		} `json:"items"`
	}
	if err := json.Unmarshal(scan.stdout, &report); err != nil {
		t.Fatal(err)
	}
	docs := []json.RawMessage{}
	for _, i := range report.Items {
		if i.Status == "ready" {
			docs = append(docs, i.Document)
		}
	}
	if len(docs) != 2 || len(report.Items) != 4 {
		t.Fatalf("逐项扫描错误：%s", scan.stdout)
	}
	op := randomUUID(t)
	request := map[string]any{"operation_id": op, "expected_parent_revision_id": nil, "source": "go-cli-import-v1", "documents": docs}
	write := func(name string, value any) string {
		t.Helper()
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		p := filepath.Join(t.TempDir(), name)
		if err = os.WriteFile(p, raw, 0600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	requestFile := write("preview.json", request)
	preview := run("--space", space.ID, "knowledge", "import", "preview", "--collection", collection, "--request", requestFile)
	var p struct {
		Status  string `json:"status"`
		Receipt string `json:"receipt"`
		Summary struct {
			Added int `json:"added"`
		} `json:"summary"`
	}
	if err := json.Unmarshal(preview.stdout, &p); err != nil || p.Status != "ready" || p.Summary.Added != 2 {
		t.Fatalf("预览错误：%s %v", preview.stdout, err)
	}
	if count := h.scalarInt("预览不发布", `SELECT count(*) FROM knowledge_revisions WHERE collection_id=$1`, collection); count != 0 {
		t.Fatal("预览发布了版本")
	}
	confirmFile := write("confirm.json", map[string]any{"request": request, "receipt": p.Receipt})
	args := []string{"--space", space.ID, "knowledge", "import", "confirm", "--collection", collection, "--request", confirmFile}
	result := run(args...)
	var committed struct {
		Summary struct {
			Operation  string `json:"operation_id"`
			Added      int    `json:"added"`
			Space      string `json:"space_id"`
			Collection string `json:"collection_id"`
		} `json:"summary"`
		Revision struct {
			ID string `json:"revision_id"`
		} `json:"revision"`
	}
	if err := json.Unmarshal(result.stdout, &committed); err != nil || committed.Summary.Operation != op || committed.Summary.Added != 2 || committed.Summary.Space != space.ID || committed.Summary.Collection != collection {
		t.Fatalf("真实回执：%s %v", result.stdout, err)
	}
	h.serverProcess.stop(t)
	h.serverProcess = startProcess(t, "edu-agentd-import-restart", serverBin, []string{"serve"}, h.serverEnv, openProcessLog(t, "edu-agentd-import-restart"))
	waitHTTPStatus(t, h.serverURL+"/livez", http.StatusOK, nil)
	for _, replayed := range []commandResult{run(args...), run("--space", space.ID, "knowledge", "import", "operation", "--collection", collection, "--id", op)} {
		var r struct {
			Summary  json.RawMessage `json:"summary"`
			Replayed bool            `json:"replayed"`
		}
		if json.Unmarshal(replayed.stdout, &r) != nil || !r.Replayed {
			t.Fatalf("原操作核对失败：%s", replayed.stdout)
		}
	}
	if count := h.scalarInt("原子导入版本数", `SELECT count(*) FROM knowledge_revisions WHERE collection_id=$1`, collection); count != 1 {
		t.Fatalf("重试创建了 %d 个版本", count)
	}
	view := run("--space", space.ID, "knowledge", "library", "preview", "--collection", collection, "--id", committed.Revision.ID)
	if !strings.Contains(string(view.stdout), "import-source-v1") || !strings.Contains(string(view.stdout), "原文") || strings.Contains(string(view.stdout), root) {
		t.Fatalf("资料页来源或正文错误：%s", view.stdout)
	}
	if n := h.scalarInt("导入不创建目标", `SELECT count(*) FROM learning_goal_revisions`); n != 0 {
		t.Fatal("导入创建了目标")
	}
}
