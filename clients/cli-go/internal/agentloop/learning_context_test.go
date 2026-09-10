package agentloop

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/agentcontext"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/api"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/modelclient"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/workspace"
)

func TestLearningWorkflowPausesRejectsModelApprovalAndDoesNotReplay(t *testing.T) {
	m := &fakeModel{responses: []modelclient.Response{{Message: toolMessage("workflow-1", "open_learning_workflow", `{"workflow":"planning"}`)}, {Message: modelclient.Message{Role: "assistant", Content: "已返回正式流程结果"}}}}
	s := newTestSession(t, m, &fakeServer{})
	defer s.Close()
	r, err := s.Send(t.Context(), "完善目标")
	if err != nil || r.Workflow == nil || len(m.requests) != 1 {
		t.Fatalf("没有等待正式流程：%+v %v", r, err)
	}
	if _, err := s.ExportCheckpoint(); err == nil {
		t.Fatal("未完成流程不能写成已完成 checkpoint")
	}
	if _, err := s.ResolveLearningWorkflow(t.Context(), "另一请求", WorkflowOutcome{Status: "published"}); err == nil {
		t.Fatal("接受了错误回调身份")
	}
	r, err = s.ResolveLearningWorkflow(t.Context(), r.Workflow.CallID, WorkflowOutcome{Status: "cancelled"})
	if err != nil || r.Text == "" || !strings.Contains(m.requests[1].Messages[len(m.requests[1].Messages)-1].Content, "cancelled") {
		t.Fatalf("未反馈取消：%+v %v", r, err)
	}
	cp, err := s.ExportCheckpoint()
	if err != nil {
		t.Fatal(err)
	}
	restored := newTestSession(t, &fakeModel{}, &fakeServer{})
	defer restored.Close()
	if err := restored.RestoreCheckpoint(cp); err != nil || restored.pendingWorkflow != nil {
		t.Fatalf("恢复重放了正式流程：%v", err)
	}
	bad := newTestSession(t, &fakeModel{responses: []modelclient.Response{{Message: toolMessage("bad", "open_learning_workflow", `{"workflow":"import","path":"/tmp/fake","confirmed":true}`)}, {Message: modelclient.Message{Role: "assistant", Content: "参数被拒绝"}}}}, &fakeServer{})
	defer bad.Close()
	result, err := bad.Send(t.Context(), "导入")
	if err != nil || result.Workflow != nil || result.Text != "参数被拒绝" {
		t.Fatalf("接受模型路径或批准：%+v %v", result, err)
	}
}

func TestLearningCheckpointCannotMoveBetweenBindings(t *testing.T) {
	s := newTestSession(t, &fakeModel{}, &fakeServer{})
	defer s.Close()
	s.options.LearningBinding = agentcontext.Binding{SpaceID: "10000000-0000-4000-8000-000000000001"}
	cp, err := s.ExportCheckpoint()
	if err != nil {
		t.Fatal(err)
	}
	other := newTestSession(t, &fakeModel{}, &fakeServer{})
	defer other.Close()
	if err := other.RestoreCheckpoint(cp); err == nil {
		t.Fatal("跨区 checkpoint 被安装")
	}
	cp.LearningBinding = agentcontext.Binding{}
	cp.SchemaVersion = 1
	raw, err := json.Marshal(cp)
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := DecodeSessionCheckpoint(raw)
	if err != nil || legacy.LearningBinding.SpaceID != api.DefaultLearningSpaceID {
		t.Fatalf("旧历史未固定映射默认区：%+v %v", legacy.LearningBinding, err)
	}
}

func TestBoundLearningToolsFitMinimumWindow(t *testing.T) {
	w, err := workspace.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	m := &fakeModel{responses: []modelclient.Response{{Message: modelclient.Message{Role: "assistant", Content: "区内交流"}}}}
	c := agentcontext.New(api.NewClient("http://127.0.0.1:1", "token", time.Second, nil), agentcontext.Binding{SpaceID: "10000000-0000-4000-8000-000000000001", GoalID: "20000000-0000-4000-8000-000000000001", SessionID: "30000000-0000-4000-8000-000000000001"})
	s, err := New(m, c, Options{ContextWindow: 4096, Workspace: w, WorkspaceStatus: w.Status(), LearningBinding: c.Binding, NewUUID: func() (string, error) { return "40000000-0000-4000-8000-000000000001", nil }})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err = s.Send(t.Context(), "继续学习"); err != nil {
		t.Fatal(err)
	}
	found := map[string]bool{}
	for _, tool := range m.requests[0].Tools {
		found[tool.Function.Name] = true
	}
	if !found["learning_context"] || !found["open_learning_workflow"] || !found["read"] {
		t.Fatalf("注册不完整：%v", found)
	}
	if !strings.Contains(m.requests[0].Messages[0].Content, c.AgentBinding()) {
		t.Fatal("模型未收到显式绑定")
	}
}
