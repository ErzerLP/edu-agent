//go:build !windows

package command

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/creack/pty"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/agentcontext"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/agentcontroller"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/agentloop"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/agentui"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/api"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/config"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/id"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/modelclient"
)

// 使用独立测试数据库启动真实服务；模型只负责发起工具，不伪造业务结果。
func agentLearningService(t *testing.T) (*App, *api.Client) {
	t.Helper()
	db, binary := os.Getenv("TEST_DATABASE_URL"), os.Getenv("EDU_AGENT_INTEGRATION_SERVER")
	if db == "" || binary == "" {
		t.Skip("需要独立 TEST_DATABASE_URL 与 EDU_AGENT_INTEGRATION_SERVER")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	listener.Close()
	endpoint := "http://" + addr
	env := []string{"DATABASE_URL=" + db, "LISTEN_ADDR=" + addr, "PUBLIC_BASE_URL=" + endpoint, "MIGRATE_ON_START=true", "MODEL_REQUIRED=false", "NOCTURNE_ENABLED=false", "ADMIN_UI_ENABLED=false", "SHUTDOWN_TIMEOUT=2s", "PAIRING_RATE_LIMIT_PER_MINUTE=1000", "AUTH_FAILURE_LIMIT_PER_MINUTE=1000", "DEVICE_RATE_LIMIT_PER_MINUTE=10000"}
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	t.Cleanup(cancel)
	server := exec.CommandContext(ctx, binary, "serve")
	server.Env = env
	var logs bytes.Buffer
	server.Stderr = &logs
	if err = server.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { server.Process.Signal(os.Interrupt); _ = server.Wait() })
	ready := false
	for i := 0; i < 100; i++ {
		r, e := http.Get(endpoint + "/healthz")
		if e == nil {
			r.Body.Close()
			ready = true
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("测试服务启动超时")
		case <-time.After(50 * time.Millisecond):
		}
	}
	if !ready {
		cancel()
		_ = server.Wait()
		t.Fatalf("测试服务未启动：%s", logs.String())
	}
	pair := exec.CommandContext(ctx, binary, "pairing-code", "create")
	pair.Env = append(env, "MIGRATE_ON_START=false")
	code, err := pair.Output()
	if err != nil {
		t.Fatalf("生成测试配对码失败：%v", err)
	}
	issued, err := api.NewClient(endpoint, "", 5*time.Second, nil).Pair(ctx, strings.TrimSpace(string(code)), "Issue11 验收")
	if err != nil {
		t.Fatal(err)
	}
	cfg, creds := pairedStores(endpoint, issued.Token)
	app, _, _ := newTestApp(cfg, creds, nil)
	app.NewUUID = id.NewUUID
	app.InputIsTTY = func() bool { return true }
	app.OutputIsTTY = func() bool { return true }
	return app, api.NewClient(endpoint, issued.Token, 5*time.Second, nil)
}

type agentLearningModel struct {
	kind, view string
	requests   []modelclient.Request
}

func (m *agentLearningModel) Complete(_ context.Context, r modelclient.Request) (modelclient.Response, error) {
	m.requests = append(m.requests, r)
	if len(m.requests) == 1 {
		name := "open_learning_workflow"
		params := map[string]string{"workflow": m.kind}
		if m.view != "" {
			name = "learning_context"
			params = map[string]string{"view": m.view}
			if m.view == "search" {
				params["query"] = "channel"
			}
		}
		found := false
		for _, tool := range r.Tools {
			if tool.Function.Name == name {
				found = true
			}
		}
		if !found {
			return modelclient.Response{}, fmt.Errorf("正式工具未注册")
		}
		args, _ := json.Marshal(params)
		return modelclient.Response{Message: modelclient.Message{Role: "assistant", ToolCalls: []modelclient.ToolCall{{ID: "formal-1", Type: "function", Function: modelclient.ToolFunction{Name: name, Arguments: string(args)}}}}}, nil
	}
	return modelclient.Response{Message: modelclient.Message{Role: "assistant", Content: "正式流程已返回"}}, nil
}

func newAgentLearningLoop(t *testing.T, c *api.Client, b agentcontext.Binding, kind string) (*agentloop.Session, *agentLearningModel) {
	t.Helper()
	m := &agentLearningModel{kind: kind}
	s, err := agentloop.New(m, agentcontext.New(c, b), agentloop.Options{ContextWindow: 32768, LearningBinding: b, NewUUID: id.NewUUID})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s, m
}

func TestAgentLearningRealServiceWorkflows(t *testing.T) {
	app, client := agentLearningService(t)
	space, err := client.MutateLearningSpace(t.Context(), "", api.LearningSpaceCommand{OperationID: mustAgentUUID(t), Name: "Go-" + mustAgentUUID(t), Status: "active"})
	if err != nil {
		t.Fatal(err)
	}
	c := client.WithLearningSpace(space.ID)
	goal, err := app.createGoal(t.Context(), c, "掌握 Go 并发")
	if err != nil {
		t.Fatal(err)
	}
	b := agentcontext.Binding{SpaceID: space.ID, GoalID: goal.GoalID}
	for _, test := range []struct{ kind, script string }{
		{"goal", "e\nname\n并发实践\ns\nq\n"},
		{"planning", "n\n\ne\npurpose\n应用到并发服务\n.\nc\n1\ny\ny\n"},
	} {
		t.Run(test.kind, func(t *testing.T) {
			s, m := newAgentLearningLoop(t, c, b, test.kind)
			r, err := s.Send(t.Context(), "打开正式流程")
			if err != nil || r.Workflow == nil {
				t.Fatalf("未进入批准面板：%v", err)
			}
			before, err := c.Goal(t.Context(), goal.GoalID)
			if err != nil {
				t.Fatal(err)
			}
			if len(m.requests) != 1 {
				t.Fatal("模型自行越过了用户流程")
			}
			var out bytes.Buffer
			outcome := app.runAgentWorkflow(t.Context(), *r.Workflow, strings.NewReader(test.script), &out)
			if outcome.Status != "returned" {
				t.Fatalf("正式页面失败：%+v\n%s", outcome, out.String())
			}
			after, err := c.Goal(t.Context(), goal.GoalID)
			if err != nil || after.Revision != before.Revision+1 {
				t.Fatalf("未通过正式版本校验保存：%v %+v\n%s", err, after, out.String())
			}
			if test.kind == "planning" {
				plans, e := c.PlanningList(t.Context(), goal.GoalID)
				if e != nil || len(plans) != 1 || plans[0].State != "applied" {
					t.Fatalf("未正式采用规划：%v %+v", e, plans)
				}
			}
			if _, err = s.ResolveLearningWorkflow(t.Context(), r.Workflow.CallID, outcome); err != nil {
				t.Fatal(err)
			}
			messages := m.requests[len(m.requests)-1].Messages
			if !strings.Contains(messages[len(messages)-1].Content, goal.GoalID) {
				t.Fatal("正式结果没有回到工具调用")
			}
		})
	}
	if _, err := client.WithLearningSpace(api.DefaultLearningSpaceID).Goal(t.Context(), goal.GoalID); err == nil {
		t.Fatal("服务端接受了跨区目标")
	}
	stale := goalRequest(goal, mustAgentUUID(t))
	if _, err := c.ReviseGoal(t.Context(), stale); err == nil {
		t.Fatal("服务端接受了旧版本目标覆盖")
	}
	collection, err := c.ChangeKnowledgeCollection(t.Context(), api.KnowledgeCollectionCommand{ID: mustAgentUUID(t), Action: "create", Name: "Go 教材", Source: "本地 Markdown"})
	if err != nil {
		t.Fatal(err)
	}
	t.Run("import_pty", func(t *testing.T) { testAgentImportPTY(t, app, c, b, collection.ID) })
	t.Run("scoped_reads_and_explicit_selection", func(t *testing.T) {
		second, e := app.createGoal(t.Context(), c, "Go 内存管理")
		if e != nil {
			t.Fatal(e)
		}
		english, e := client.MutateLearningSpace(t.Context(), "", api.LearningSpaceCommand{OperationID: mustAgentUUID(t), Name: "英语-" + mustAgentUUID(t), Status: "active"})
		if e != nil {
			t.Fatal(e)
		}
		englishGoal, e := app.createGoal(t.Context(), client.WithLearningSpace(english.ID), "英语听力")
		if e != nil {
			t.Fatal(e)
		}
		for _, selection := range []agentcontext.Binding{b, {SpaceID: english.ID, GoalID: englishGoal.GoalID}} {
			s, m := newAgentLearningLoop(t, client, selection, "")
			m.view = "progress"
			if _, e = s.Send(t.Context(), "我的进度"); e != nil {
				t.Fatal(e)
			}
			messages := m.requests[len(m.requests)-1].Messages
			body := messages[len(messages)-1].Content
			if !strings.Contains(body, selection.GoalID) || strings.Contains(body, second.GoalID) || strings.Contains(body, `"error"`) {
				t.Fatalf("注册工具返回错误范围：%s", body)
			}
			cp, e := s.ExportCheckpoint()
			if e != nil || cp.LearningBinding != selection {
				t.Fatalf("结果 checkpoint 丢失身份：%v", e)
			}
		}
		page, e := c.Goals(t.Context(), "", "", "", 20)
		if e != nil || len(page.Items) != 2 {
			t.Fatalf("目标选择前置：%v", e)
		}
		s, _ := newAgentLearningLoop(t, c, agentcontext.Binding{SpaceID: space.ID}, "goal")
		r, e := s.Send(t.Context(), "查看一个目标")
		if e != nil || r.Workflow == nil {
			t.Fatalf("未进入显式选择：%v", e)
		}
		var out bytes.Buffer
		outcome := app.runAgentWorkflow(t.Context(), *r.Workflow, strings.NewReader("2\nq\n"), &out)
		selected, ok := outcome.Data.(api.GoalRevision)
		if outcome.Status != "returned" || !ok || selected.GoalID != page.Items[1].GoalID {
			t.Fatalf("误用默认目标：%+v\n%s", outcome, out.String())
		}
		if !strings.Contains(out.String(), page.Items[0].GoalManagement().Details.Name) || !strings.Contains(out.String(), page.Items[1].GoalManagement().Details.Name) {
			t.Fatal("没有展示目标歧义选择")
		}
		head, e := c.WithCollection(collection.ID).KnowledgeHead(t.Context())
		if e != nil {
			t.Fatal(e)
		}
		scope, e := c.FreezeKnowledgeScope(t.Context(), api.KnowledgeScopeSnapshot{ID: mustAgentUUID(t), Entries: []api.KnowledgeScopeEntry{{CollectionID: collection.ID, RevisionID: head.RevisionID}}})
		if e != nil {
			t.Fatal(e)
		}
		latest, e := c.Goal(t.Context(), goal.GoalID)
		if e != nil {
			t.Fatal(e)
		}
		req := goalRequest(latest, mustAgentUUID(t))
		details := latest.GoalManagement().Details
		details.ScopeSnapshotID = scope.ID
		req.Details = &details
		if _, e = c.ReviseGoal(t.Context(), req); e != nil {
			t.Fatal(e)
		}
		search, model := newAgentLearningLoop(t, c, b, "")
		model.view = "search"
		if _, e = search.Send(t.Context(), "检索 channel"); e != nil {
			t.Fatal(e)
		}
		messages := model.requests[len(model.requests)-1].Messages
		body := messages[len(messages)-1].Content
		if !strings.Contains(body, scope.ID) || strings.Contains(body, `"error"`) {
			t.Fatalf("没有检索绑定冻结资料：%s", body)
		}
		preset := config.DefaultAgentConfig("ollama")
		app.Config.(*memoryConfigStore).value.Agent = &preset
		app.NewModel = func(config.AgentConfig, string) (agentloop.Model, error) { return fixedAgentModel{}, nil }
		work := t.TempDir()
		ui := &agentNavigationFixture{t: t, root: work, bindings: []agentcontext.Binding{b, {SpaceID: english.ID, GoalID: englishGoal.GoalID}}}
		app.AgentUI = ui
		app.AgentSessionRoot = func() (string, error) {
			t.Error("no-save 切区打开了持久存储")
			return "", fmt.Errorf("不应读取存储")
		}
		if exit := app.Run(t.Context(), []string{"agent", "--space", b.SpaceID, "--goal", b.GoalID, "--workspace", work, "--no-save"}); exit != 0 || ui.calls != 2 {
			t.Fatalf("切区未完整执行：exit=%d calls=%d", exit, ui.calls)
		}
	})
	t.Run("archived_context_cannot_publish", func(t *testing.T) {
		if _, err := c.MutateLearningSpace(t.Context(), space.ID, api.LearningSpaceCommand{OperationID: mustAgentUUID(t), ExpectedVersion: space.Version, Name: space.Name, Status: "archived"}); err != nil {
			t.Fatal(err)
		}
		outcome := app.runAgentWorkflow(t.Context(), agentloop.LearningWorkflow{Kind: "import", Binding: b}, strings.NewReader(""), io.Discard)
		if outcome.Status != "unavailable" || outcome.Code != "learning_space_archived" {
			t.Fatalf("归档后仍可进入发布：%+v", outcome)
		}
	})
}

type agentNavigationFixture struct {
	t        *testing.T
	root     string
	bindings []agentcontext.Binding
	calls    int
}

func (u *agentNavigationFixture) Run(_ context.Context, conversation agentui.Conversation, _ string) error {
	u.t.Helper()
	c := conversation.(*agentcontroller.Controller)
	if u.calls >= len(u.bindings) || c.LearningBinding() != u.bindings[u.calls] || c.Status().Persistent || c.LearningWorkspaceRoot() != u.root {
		u.t.Fatal("切区丢失绑定、no-save 或原工作区")
	}
	u.calls++
	if u.calls < len(u.bindings) {
		return &agentui.NavigateLearningContext{Binding: u.bindings[u.calls]}
	}
	return nil
}

func mustAgentUUID(t *testing.T) string {
	t.Helper()
	v, e := id.NewUUID()
	if e != nil {
		t.Fatal(e)
	}
	return v
}

func testAgentImportPTY(t *testing.T, app *App, c *api.Client, b agentcontext.Binding, collection string) {
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
	file := filepath.Join(t.TempDir(), "go.md")
	if err = os.WriteFile(file, []byte("# Go 并发\n使用 channel 通信。\n"), 0600); err != nil {
		t.Fatal(err)
	}
	s, m := newAgentLearningLoop(t, c, b, "import")
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- (agentui.Runner{In: terminal, Out: terminal, Session: s, ModelName: "流程 fixture", Workflow: app.runAgentWorkflow}).Run(ctx)
	}()
	chunks := make(chan string, 64)
	go func() {
		buf := make([]byte, 4096)
		for {
			n, e := primary.Read(buf)
			if n > 0 {
				select {
				case chunks <- string(buf[:n]):
				case <-ctx.Done():
					return
				}
			}
			if e != nil {
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
			case e := <-done:
				t.Fatalf("提前退出：%v\n%s", e, output)
			case <-ctx.Done():
				t.Fatalf("未显示 %q\n%s", text, output)
			}
		}
	}
	wait("Enter 发送")
	io.WriteString(primary, "导入资料\r")
	wait("Enter 打开")
	io.WriteString(primary, "\r")
	wait("Go 教材")
	io.WriteString(primary, "\r")
	wait("来源路径")
	io.WriteString(primary, file+"\r")
	wait("Enter 服务端检查")
	io.WriteString(primary, "\r")
	wait("Ctrl+S 确认")
	if _, err := c.WithCollection(collection).KnowledgeHead(ctx); err == nil {
		t.Fatal("用户确认前已经发布")
	}
	io.WriteString(primary, "\x13")
	wait("已提交版本")
	io.WriteString(primary, "\x1b")
	wait("正式流程已返回")
	io.WriteString(primary, "\x03")
	select {
	case e := <-done:
		if e != nil {
			t.Fatal(e)
		}
	case <-ctx.Done():
		t.Fatal("Agent 未退出")
	}
	if _, err := c.WithCollection(collection).KnowledgeHead(t.Context()); err != nil {
		t.Fatal(err)
	}
	messages := m.requests[len(m.requests)-1].Messages
	if !strings.Contains(messages[len(messages)-1].Content, `"published"`) {
		t.Fatal("发布结果没有经过正式工具返回")
	}
}
