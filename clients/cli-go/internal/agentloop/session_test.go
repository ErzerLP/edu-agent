package agentloop

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/api"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/modelclient"
)

type fakeModel struct {
	responses []modelclient.Response
	requests  []modelclient.Request
	err       error
}

func (m *fakeModel) Complete(_ context.Context, request modelclient.Request) (modelclient.Response, error) {
	m.requests = append(m.requests, request)
	if len(m.responses) == 0 {
		if m.err != nil {
			return modelclient.Response{}, m.err
		}
		return modelclient.Response{}, errors.New("fake model has no response")
	}
	response := m.responses[0]
	m.responses = m.responses[1:]
	return response, nil
}

type fakeServer struct {
	retrieveCalls  int
	createCalls    int
	created        api.MemoryCandidateRequest
	createErrors   []error
	createRequests []api.MemoryCandidateRequest
}

func (s *fakeServer) RetrieveKnowledge(context.Context, api.KnowledgeRetrievalRequest) (api.KnowledgeRetrievalResult, error) {
	s.retrieveCalls++
	return api.KnowledgeRetrievalResult{Hits: []api.RetrievalHit{{CanonicalSlice: "图的顶点由边连接。"}}}, nil
}
func (*fakeServer) CurrentSession(context.Context) (api.SessionView, error) {
	return api.SessionView{}, nil
}
func (*fakeServer) Routes(context.Context, string, int, bool) (api.RoutesPage, error) {
	return api.RoutesPage{}, nil
}
func (*fakeServer) Reviews(context.Context, string, int, *time.Time) (api.ReviewsPage, error) {
	return api.ReviewsPage{}, nil
}
func (*fakeServer) MemoryCandidates(context.Context, string, int) (api.MemoryCandidatePage, error) {
	return api.MemoryCandidatePage{}, nil
}
func (s *fakeServer) CreateMemoryCandidate(_ context.Context, request api.MemoryCandidateRequest) (api.MemoryOperationResponse, error) {
	s.createCalls++
	s.created = request
	s.createRequests = append(s.createRequests, request)
	if len(s.createErrors) > 0 {
		err := s.createErrors[0]
		s.createErrors = s.createErrors[1:]
		if err != nil {
			return api.MemoryOperationResponse{}, err
		}
	}
	return api.MemoryOperationResponse{Candidate: &api.MemoryCandidateView{Candidate: api.MemoryCandidate{ID: "candidate-1", Status: "pending_review"}}}, nil
}

func TestSessionExecutesKnowledgeToolBeforeAnswer(t *testing.T) {
	model := &fakeModel{responses: []modelclient.Response{
		{Message: toolMessage("call-1", "search_knowledge", `{"query":"图论"}`)},
		{Message: modelclient.Message{Role: "assistant", Content: "我们先从顶点和边开始。"}},
	}}
	server := &fakeServer{}
	session := newTestSession(t, model, server)
	result, err := session.Send(t.Context(), "教我图论")
	if err != nil {
		t.Fatal(err)
	}
	if result.Text == "" || server.retrieveCalls != 1 || len(model.requests) != 2 {
		t.Fatalf("result=%+v calls=%d requests=%d", result, server.retrieveCalls, len(model.requests))
	}
	messages := model.requests[1].Messages
	if messages[len(messages)-1].Role != "tool" || !strings.Contains(messages[len(messages)-1].Content, "图的顶点") {
		t.Fatalf("tool message=%+v", messages[len(messages)-1])
	}
}

func TestPreferenceWriteWaitsForExplicitConfirmation(t *testing.T) {
	arguments := `{"content":"回答时先给结论再解释","reason":"用户明确要求长期保持回答风格","category":"interaction_preference","sensitivity":"non_sensitive","stability":"stable"}`
	model := &fakeModel{responses: []modelclient.Response{
		{Message: toolMessage("call-pref", "remember_preference", arguments)},
		{Message: modelclient.Message{Role: "assistant", Content: "偏好候选已提交，等待服务端策略处理。"}},
	}}
	server := &fakeServer{}
	session := newTestSession(t, model, server)
	result, err := session.Send(t.Context(), "以后请先给结论，并长期记住")
	if err != nil || result.Pending == nil || server.createCalls != 0 {
		t.Fatalf("result=%+v calls=%d err=%v", result, server.createCalls, err)
	}
	result, err = session.ResolvePreference(t.Context(), true)
	if err != nil || result.Text == "" || server.createCalls != 1 {
		t.Fatalf("result=%+v calls=%d err=%v", result, server.createCalls, err)
	}
	if server.created.Category != "interaction_preference" || server.created.PayloadSchemaVersion != 1 {
		t.Fatalf("created=%+v", server.created)
	}
}

func TestPreferenceDeclineProducesToolResultWithoutWrite(t *testing.T) {
	model := &fakeModel{responses: []modelclient.Response{
		{Message: toolMessage("call-pref", "remember_preference", `{"content":"晚上学习","reason":"用户提到时间","category":"time_constraint","sensitivity":"non_sensitive","stability":"transient"}`)},
		{Message: modelclient.Message{Role: "assistant", Content: "好的，这次不保存。"}},
	}}
	server := &fakeServer{}
	session := newTestSession(t, model, server)
	result, err := session.Send(t.Context(), "别保存，只是说说")
	if err != nil || result.Pending == nil {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	result, err = session.ResolvePreference(t.Context(), false)
	if err != nil || server.createCalls != 0 || !strings.Contains(result.Text, "不保存") {
		t.Fatalf("result=%+v calls=%d err=%v", result, server.createCalls, err)
	}
}

func TestPreferenceResolutionRemainsUnambiguousWhenFollowupModelFails(t *testing.T) {
	arguments := `{"content":"回答时先给结论","reason":"用户明确要求长期保持回答风格","category":"interaction_preference","sensitivity":"non_sensitive","stability":"stable"}`
	model := &fakeModel{
		responses: []modelclient.Response{{Message: toolMessage("call-pref", "remember_preference", arguments)}},
		err:       errors.New("provider unavailable"),
	}
	server := &fakeServer{}
	session := newTestSession(t, model, server)
	result, err := session.Send(t.Context(), "请长期记住")
	if err != nil || result.Pending == nil {
		t.Fatalf("send result=%+v err=%v", result, err)
	}
	result, err = session.ResolvePreference(t.Context(), true)
	if err != nil || result.Pending != nil || server.createCalls != 1 || !strings.Contains(result.Text, "已提交") {
		t.Fatalf("resolve result=%+v calls=%d err=%v", result, server.createCalls, err)
	}
	if _, err = session.Send(t.Context(), "继续学习"); err == nil || strings.Contains(err.Error(), "待确认") {
		t.Fatalf("session remained stuck in confirmation: %v", err)
	}
}

func TestPreferenceRetryReusesOperationIDAfterAmbiguousFailure(t *testing.T) {
	arguments := `{"content":"回答时先给结论","reason":"用户明确要求长期保持回答风格","category":"interaction_preference","sensitivity":"non_sensitive","stability":"stable"}`
	model := &fakeModel{responses: []modelclient.Response{
		{Message: toolMessage("call-pref", "remember_preference", arguments)},
		{Message: modelclient.Message{Role: "assistant", Content: "偏好候选已提交。"}},
	}}
	server := &fakeServer{createErrors: []error{errors.New("response lost")}}
	uuidCalls := 0
	session, err := New(model, server, Options{
		ContextWindow: 32768, MaxToolRounds: 8,
		Now: func() time.Time { return time.Date(2026, 8, 29, 0, 0, 0, 0, time.UTC) },
		NewUUID: func() (string, error) {
			uuidCalls++
			if uuidCalls == 1 {
				return "60000000-0000-4000-8000-000000000001", nil
			}
			return "60000000-0000-4000-8000-000000000002", nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := session.Send(t.Context(), "请长期记住")
	if err != nil || result.Pending == nil {
		t.Fatalf("send result=%+v err=%v", result, err)
	}
	if _, err := session.ResolvePreference(t.Context(), true); !errors.Is(err, ErrPreferenceOutcomeUnknown) {
		t.Fatalf("first ambiguous write err=%v", err)
	}
	if _, err := session.ResolvePreference(t.Context(), false); !errors.Is(err, ErrPreferenceOutcomeUnknown) {
		t.Fatalf("ambiguous write allowed decline: %v", err)
	}
	result, err = session.ResolvePreference(t.Context(), true)
	if err != nil || result.Pending != nil || server.createCalls != 2 || uuidCalls != 1 {
		t.Fatalf("retry result=%+v calls=%d uuidCalls=%d err=%v", result, server.createCalls, uuidCalls, err)
	}
	if len(server.createRequests) != 2 || server.createRequests[0].OperationID == "" || server.createRequests[0].OperationID != server.createRequests[1].OperationID {
		t.Fatalf("operation IDs changed across retry: %+v", server.createRequests)
	}

	model.responses = append(model.responses,
		modelclient.Response{Message: toolMessage("call-pref-2", "remember_preference", arguments)},
		modelclient.Response{Message: modelclient.Message{Role: "assistant", Content: "第二个偏好候选已提交。"}},
	)
	result, err = session.Send(t.Context(), "再记住一个偏好")
	if err != nil || result.Pending == nil {
		t.Fatalf("second send result=%+v err=%v", result, err)
	}
	if _, err := session.ResolvePreference(t.Context(), true); err != nil {
		t.Fatal(err)
	}
	if uuidCalls != 2 || len(server.createRequests) != 3 || server.createRequests[2].OperationID == server.createRequests[0].OperationID {
		t.Fatalf("subsequent preference operation IDs = %+v uuidCalls=%d", server.createRequests, uuidCalls)
	}
}

func TestSessionRejectsExcessiveToolCallsAndOversizedCurrentGroup(t *testing.T) {
	calls := make([]modelclient.ToolCall, maxToolCallsPerResponse+1)
	for index := range calls {
		calls[index] = modelclient.ToolCall{ID: "call", Type: "function", Function: modelclient.ToolFunction{Name: "get_learning_progress", Arguments: `{}`}}
	}
	model := &fakeModel{responses: []modelclient.Response{{Message: modelclient.Message{Role: "assistant", ToolCalls: calls}}}}
	server := &fakeServer{}
	session := newTestSession(t, model, server)
	if _, err := session.Send(t.Context(), "读取进度"); err == nil || !strings.Contains(err.Error(), "单轮") {
		t.Fatalf("excessive calls err=%v", err)
	}

	session, err := New(&fakeModel{}, server, Options{
		ContextWindow: 4096, MaxToolRounds: 1, Now: time.Now,
		NewUUID: func() (string, error) { return "60000000-0000-4000-8000-000000000001", nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	session.messages = append(session.messages, modelclient.Message{Role: "user", Content: strings.Repeat("x", 12<<10)})
	if _, err := session.contextMessages(); err == nil || !strings.Contains(err.Error(), "上下文上限") {
		t.Fatalf("oversized context err=%v", err)
	}
}

func TestSessionRejectsMultibyteInputBeyondContextBudget(t *testing.T) {
	t.Parallel()
	model := &fakeModel{responses: []modelclient.Response{{Message: modelclient.Message{Role: "assistant", Content: "不应调用"}}}}
	session := newTestSession(t, model, &fakeServer{})
	_, err := session.Send(t.Context(), strings.Repeat("学", maxUserInputBytes/3+2))
	if err == nil || !strings.Contains(err.Error(), "输入内容过长") || len(model.requests) != 0 {
		t.Fatalf("err=%v requests=%d", err, len(model.requests))
	}
}

func newTestSession(t *testing.T, model Model, server Server) *Session {
	t.Helper()
	session, err := New(model, server, Options{
		ContextWindow: 32768, MaxToolRounds: 8,
		Now:     func() time.Time { return time.Date(2026, 8, 29, 0, 0, 0, 0, time.UTC) },
		NewUUID: func() (string, error) { return "60000000-0000-4000-8000-000000000001", nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	return session
}

func toolMessage(id, name, arguments string) modelclient.Message {
	return modelclient.Message{Role: "assistant", ToolCalls: []modelclient.ToolCall{{
		ID: id, Type: "function", Function: modelclient.ToolFunction{Name: name, Arguments: arguments},
	}}}
}
