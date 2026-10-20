package agentloop

import (
	"fmt"
	"strings"
	"testing"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/modelclient"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/workspace"
)

func stepCalls(names ...string) modelclient.Message {
	m := modelclient.Message{Role: "assistant"}
	for i, name := range names {
		m.ToolCalls = append(m.ToolCalls, modelclient.ToolCall{ID: fmt.Sprintf("step-%d", i), Type: "function", Function: modelclient.ToolFunction{Name: name, Arguments: `{}`}})
	}
	return m
}

// A multi-call response must stop dispatching at three, even when the summary fails.
func TestToolStepsMultipleCallsAndSummaryFailure(t *testing.T) {
	for _, failSummary := range []bool{false, true} {
		t.Run(fmt.Sprint(failSummary), func(t *testing.T) {
			m := &fakeModel{responses: []modelclient.Response{{Message: stepCalls("search_knowledge", "search_knowledge", "search_knowledge", "search_knowledge", "search_knowledge")}}}
			for i := range m.responses[0].Message.ToolCalls {
				m.responses[0].Message.ToolCalls[i].Function.Arguments = `{"query":"图论"}`
			}
			if !failSummary {
				m.responses = append(m.responses, modelclient.Response{Message: modelclient.Message{Role: "assistant", Content: "已查询三次。"}})
			}
			server := &fakeServer{}
			s := newTestSession(t, m, server)
			s.options.MaxToolSteps = 3
			r, err := s.Send(t.Context(), "分步查询")
			if err != nil || server.retrieveCalls != 3 || !strings.Contains(r.Text, "继续") {
				t.Fatalf("must pause after three: result=%+v err=%v executed=%d", r, err, server.retrieveCalls)
			}
			if len(m.requests) != 2 || len(m.requests[1].Tools) != 0 {
				t.Fatalf("must request exactly one summary without tools: %+v", m.requests)
			}
			for _, message := range m.requests[1].Messages {
				for _, call := range message.ToolCalls {
					if call.ID == "step-3" || call.ID == "step-4" {
						t.Fatal("summary contains an unexecuted call")
					}
				}
			}
			m.responses = append(m.responses, modelclient.Response{Message: modelclient.Message{Role: "assistant", Content: "五次查询完成"}})
			r, err = s.Send(t.Context(), "继续")
			if err != nil || server.retrieveCalls != 5 || r.Text != "五次查询完成" {
				t.Fatalf("resume repeated completed calls: result=%+v err=%v executed=%d", r, err, server.retrieveCalls)
			}
		})
	}
}

func TestToolStepsExecutedFailuresCount(t *testing.T) {
	m := &fakeModel{responses: []modelclient.Response{
		{Message: stepCalls(workspace.ToolRead, workspace.ToolRead, workspace.ToolRead, workspace.ToolRead)},
		{Message: modelclient.Message{Role: "assistant", Content: "读取失败，需检查路径。"}},
	}}
	executor := &fakeWorkspaceExecutor{status: workspace.Status{Available: true, Label: "project"}, results: map[string]workspace.Result{workspace.ToolRead: {Summary: "读取失败", Value: map[string]any{"error": "read_failed"}}}}
	s := newWorkspaceTestSession(t, m, executor)
	s.options.MaxToolSteps = 3
	r, err := s.Send(t.Context(), "读取文件")
	if err != nil || len(executor.calls) != 3 || !strings.Contains(r.Text, "继续") {
		t.Fatalf("failed executions must count: result=%+v err=%v calls=%v", r, err, executor.calls)
	}
	for _, event := range r.Events {
		if event.Status != EventFailed {
			t.Fatalf("expected failed execution: %+v", event)
		}
	}
}

func TestToolStepsSummaryExcludesUnexecutedRepeatedCallID(t *testing.T) {
	m := &fakeModel{responses: []modelclient.Response{
		{Message: toolMessage("step-3", "get_learning_progress", `{}`)},
		{Message: stepCalls("get_learning_progress", "get_learning_progress", "get_learning_progress", "get_learning_progress")},
		{Message: modelclient.Message{Role: "assistant", Content: "本轮已读取三次。"}},
	}}
	s := newTestSession(t, m, &fakeServer{})
	s.options.MaxToolSteps = 3
	if _, err := s.Send(t.Context(), "分步读取"); err != nil {
		t.Fatal(err)
	}
	if len(m.requests) != 3 {
		t.Fatalf("requests=%d", len(m.requests))
	}
	batch := 0
	for _, message := range m.requests[2].Messages {
		if len(message.ToolCalls) == 0 {
			continue
		}
		batch++
		if batch == 2 && len(message.ToolCalls) != 2 {
			t.Fatalf("summary included unexecuted calls: %+v", message.ToolCalls)
		}
	}
	if batch != 2 {
		t.Fatalf("lost completed history: batches=%d", batch)
	}
}

func TestToolStepsApprovalWaitAndNoRepeatedMutation(t *testing.T) {
	for _, resolution := range []FileMutationResolution{FileMutationApprove, FileMutationDecline} {
		t.Run(string(resolution), func(t *testing.T) {
			prepared := &workspace.PreparedMutation{Presentation: workspace.MutationPresentation{Tool: workspace.ToolWrite, Operation: "write_create", Path: "notes.md"}}
			executor := &fakeWorkspaceExecutor{
				status:        workspace.Status{Available: true, Label: "project"},
				prepared:      map[string]*workspace.PreparedMutation{workspace.ToolWrite: prepared},
				commitResults: []workspace.Result{{Publication: workspace.PublicationCompleted, Summary: "已创建文件", Value: map[string]any{"publication_outcome": "completed"}}},
			}
			m := &fakeModel{responses: []modelclient.Response{
				{Message: stepCalls(workspace.ToolRead, workspace.ToolWrite, workspace.ToolRead, workspace.ToolRead)},
				{Message: modelclient.Message{Role: "assistant", Content: "文件已创建。"}},
				{Message: modelclient.Message{Role: "assistant", Content: "全部完成"}},
			}}
			s := newWorkspaceTestSession(t, m, executor)
			s.options.MaxToolSteps = 3
			r, err := s.Send(t.Context(), "创建笔记")
			if err != nil || r.PendingFileMutation == nil || len(executor.commitCalls) != 0 || len(m.requests) != 1 {
				t.Fatalf("approval must wait before execution: %+v %v", r, err)
			}
			wantReads, wantWrites := 2, 1
			if resolution == FileMutationDecline {
				wantReads, wantWrites = 3, 0
			}
			r, err = s.ResolveFileMutation(t.Context(), r.PendingFileMutation.CallID, resolution)
			if err != nil || len(executor.calls) != wantReads || len(executor.commitCalls) != wantWrites || !strings.Contains(r.Text, "继续") {
				t.Fatalf("approval reset or consumed the allowance: %+v %v reads=%v writes=%v", r, err, executor.calls, executor.commitCalls)
			}
			r, err = s.Send(t.Context(), "继续")
			if err != nil || r.Text != "全部完成" || len(executor.calls) != 3 || len(executor.commitCalls) != wantWrites {
				t.Fatalf("resume repeated side effects: %+v %v reads=%v writes=%v", r, err, executor.calls, executor.commitCalls)
			}
		})
	}
}

func TestToolStepsOrdinaryWaitDoesNotReset(t *testing.T) {
	calls := stepCalls("get_learning_progress", "ask_user_question", "get_learning_progress", "get_learning_progress")
	calls.ToolCalls[1].Function.Arguments = `{"question_id":"next","header":"方向","question":"选择下一步","mode":"single","options":[{"id":"a","label":"A","description":"选A"},{"id":"b","label":"B","description":"选B"}]}`
	m := &fakeModel{responses: []modelclient.Response{{Message: calls}, {Message: modelclient.Message{Role: "assistant", Content: "已读取并确认方向。"}}}}
	s := newTestSession(t, m, &fakeServer{})
	s.options.MaxToolSteps = 3
	r, err := s.Send(t.Context(), "分步处理")
	if err != nil || r.PendingQuestion == nil {
		t.Fatalf("missing question: %+v %v", r, err)
	}
	r, err = s.ResolveQuestion(t.Context(), QuestionAnswer{QuestionID: "next", Status: QuestionAnswered, OptionIDs: []string{"a"}})
	if err != nil || len(r.Events) != 3 || !strings.Contains(r.Text, "继续") {
		t.Fatalf("ordinary wait reset count: %+v %v", r, err)
	}
}
