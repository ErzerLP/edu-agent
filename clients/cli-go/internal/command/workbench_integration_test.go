//go:build !windows

package command

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/creack/pty"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/api"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/workbench"
)

// 使用真实服务、扫描器与工作台叶子验证前置模块组合；不设置教学或客户端模型。
func TestWorkbenchRealServiceTwoSpacesAndThreeGoals(t *testing.T) {
	app, client := agentLearningService(t)
	service := workbenchService{app: *app}
	load := func(req workbench.Request) workbench.Page {
		t.Helper()
		page, err := service.Load(t.Context(), req)
		if err != nil {
			t.Fatalf("工作台 %s/%s 失败：%v", req.Page, req.Action, err)
		}
		return page
	}
	var original api.SessionView
	var originalSpace string
	var revisions []string
	for _, direction := range []struct {
		name, markdown string
		goals          []string
	}{
		{"Go", "# Go 并发\n使用 channel 通信。\n", []string{"Go 并发", "Go 内存"}},
		{"英语", "# 英语听力\n练习 listening。\n", []string{"英语听力"}},
	} {
		space, err := client.MutateLearningSpace(t.Context(), "", api.LearningSpaceCommand{OperationID: mustAgentUUID(t), Name: direction.name, Status: "active"})
		if err != nil {
			t.Fatal(err)
		}
		c := client.WithLearningSpace(space.ID)
		collection, err := c.ChangeKnowledgeCollection(t.Context(), api.KnowledgeCollectionCommand{ID: mustAgentUUID(t), Action: "create", Name: "教材", Source: "Issue12 本地资料"})
		if err != nil {
			t.Fatal(err)
		}
		t.Run(direction.name+"工作台导入", func(t *testing.T) {
			workbenchImportRealPTY(t, app, c, space.ID, collection.ID, direction.markdown)
		})
		head, err := c.WithCollection(collection.ID).KnowledgeHead(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		revisions = append(revisions, head.RevisionID)
		materials := load(workbench.Request{Space: space.ID, Page: "collection", Resource: collection.ID})
		frozen := load(workbench.Request{Space: space.ID, Page: "collection", Resource: collection.ID, Action: "freeze", Basis: materials.Basis, Entity: mustAgentUUID(t)})
		if frozen.Scope == "" {
			t.Fatal("真实资料页未返回冻结范围")
		}
		for _, name := range direction.goals {
			goalID := mustAgentUUID(t)
			saved := load(workbench.Request{Space: space.ID, Page: "new-goal", Scope: frozen.Scope, Action: "save", Operation: mustAgentUUID(t), Entity: goalID, Values: map[string]string{"name": name, "text": name + "\n分阶段学习", "priority": "normal"}})
			if saved.Redirect != "goal/"+goalID {
				t.Fatal("目标保存没有返回自身详情")
			}
			sessions, err := c.Sessions(t.Context(), goalID, "", "", 20)
			if err != nil || len(sessions.Items) != 0 {
				t.Fatalf("保存目标产生教学副作用：%v", err)
			}
			goal := load(workbench.Request{Space: space.ID, Page: "goal", Resource: goalID})
			found := false
			for _, entry := range goal.Entries {
				if strings.HasPrefix(entry.ID, "agent:") {
					found = entry.ID == "agent:"+goalID+"/"
				}
			}
			if !found || !strings.Contains(goal.Content, name) || !strings.Contains(goal.Content, frozen.Scope) {
				t.Fatal("真实目标详情丢失名称、范围或 Agent 绑定")
			}
			started := load(workbench.Request{Space: space.ID, Page: "goal", Resource: goalID, Version: goal.Version, Action: "new-session", Operation: mustAgentUUID(t), Entity: mustAgentUUID(t)})
			sessionID := strings.TrimPrefix(started.Redirect, "session/")
			view, err := c.Session(t.Context(), sessionID)
			if err != nil || view.WorkItem == nil || view.WorkItem.GoalRevision == nil || view.WorkItem.GoalRevision.GoalID != goalID {
				t.Fatalf("独立教学没有绑定原目标：%v", err)
			}
			if original.Session.SessionID == "" {
				original, originalSpace = view, space.ID
			}
		}
		goals, err := c.Goals(t.Context(), "", "", "", 20)
		if err != nil || len(goals.Items) != len(direction.goals) {
			t.Fatalf("跨区或同区目标被替换：%v，数量=%d", err, len(goals.Items))
		}
	}
	if revisions[0] == revisions[1] {
		t.Fatal("两区同名资料错误共享了版本身份")
	}
	// 新建 App 模拟客户端重新启动，仅通过服务端恢复已确认的原教学事实。
	restarted, _, _ := newTestApp(app.Config, app.Credentials, &fakeTerminal{})
	page, err := (workbenchService{app: *restarted}).Load(t.Context(), workbench.Request{Space: originalSpace, Page: "session", Resource: original.Session.SessionID})
	if err != nil || page.Session != original.Session.SessionID || page.Version != original.Session.AggregateVersion || page.Stage != original.Session.State {
		t.Fatalf("第二客户端没有恢复原教学上下文：%v", err)
	}
}

func workbenchImportRealPTY(t *testing.T, app *App, client *api.Client, space, collection, markdown string) {
	t.Helper()
	t.Setenv("TERM", "xterm-256color")
	primary, terminal, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer primary.Close()
	defer terminal.Close()
	if err = pty.Setsize(primary, &pty.Winsize{Rows: 40, Cols: 140}); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "README.md")
	if err = os.WriteFile(file, []byte(markdown), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		model := workbench.New(ctx, workbenchService{app: *app}, space)
		model.Open(ctx, "import/"+collection)
		_, err := tea.NewProgram(model, tea.WithContext(ctx), tea.WithInput(terminal), tea.WithOutput(terminal), tea.WithAltScreen()).Run()
		done <- err
	}()
	chunks := make(chan string, 64)
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := primary.Read(buf)
			if n > 0 {
				select {
				case chunks <- string(buf[:n]):
				case <-ctx.Done():
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()
	output := ""
	wait := func(text string) {
		t.Helper()
		for !strings.Contains(output, text) {
			select {
			case chunk := <-chunks:
				output += chunk
			case err := <-done:
				t.Fatalf("工作台提前退出：%v\n%s", err, output)
			case <-ctx.Done():
				t.Fatalf("没有显示 %q\n%s", text, output)
			}
		}
	}
	wait("来源路径")
	io.WriteString(primary, file+"\r")
	wait("Enter 服务端检查")
	io.WriteString(primary, "\r")
	wait("Ctrl+S 确认")
	var apiErr *api.APIError
	if _, err := client.WithCollection(collection).KnowledgeHead(ctx); !errors.As(err, &apiErr) || apiErr.Code != "not_found" {
		t.Fatalf("确认前未保持空集合：%v", err)
	}
	io.WriteString(primary, "\x13")
	wait("已提交版本")
	io.WriteString(primary, "\x1b")
	wait("区内 · 资料集合")
	if strings.Contains(output, "\x1b[?1049l") {
		t.Fatal("导入返回时退出了全屏")
	}
	io.WriteString(primary, "\x03")
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("工作台未退出")
	}
	head, err := client.WithCollection(collection).KnowledgeHead(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	value, err := client.WithCollection(collection).KnowledgeLibraryView(t.Context(), head.RevisionID, "export", false)
	if err != nil {
		t.Fatal(err)
	}
	var exported struct {
		Documents []struct{ Path, Markdown string } `json:"documents"`
	}
	err = decodeLibrary(value, &exported)
	if err != nil || len(exported.Documents) != 1 || exported.Documents[0].Path != "README.md" || !strings.Contains(exported.Documents[0].Markdown, strings.TrimSpace(strings.SplitN(markdown, "\n", 2)[1])) {
		t.Fatalf("导入正文与所选区不一致：%v，%+v", err, exported.Documents)
	}
}
