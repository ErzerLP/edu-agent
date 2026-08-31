package agentloop

import (
	"context"
	"encoding/json"
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

type usageCalibratingModel struct {
	requests []modelclient.Request
}

func (m *usageCalibratingModel) Complete(_ context.Context, request modelclient.Request) (modelclient.Response, error) {
	m.requests = append(m.requests, request)
	estimated := NewTokenEstimator().EstimateRequest(request)
	return modelclient.Response{
		Message: modelclient.Message{Role: "assistant", Content: "calibrated"},
		Usage:   &modelclient.Usage{PromptTokens: estimated * 2, CompletionTokens: 1, TotalTokens: estimated*2 + 1},
	}, nil
}

type fakeServer struct {
	retrieveCalls    int
	retrieveRequest  api.KnowledgeRetrievalRequest
	currentCalls     int
	currentResult    api.SessionView
	currentErr       error
	routeCalls       int
	routeResult      api.RoutesPage
	reviewCalls      int
	reviewCursor     string
	reviewLimit      int
	reviewDue        *time.Time
	reviewResult     api.ReviewsPage
	exportCalls      int
	exportCursor     string
	exportLimit      int
	exportResult     api.MemoryExportPage
	exportErr        error
	candidateCalls   int
	candidateResult  api.MemoryCandidateView
	candidateErr     error
	createCalls      int
	created          api.MemoryCandidateRequest
	createErrors     []error
	createRequests   []api.MemoryCandidateRequest
	createResult     *api.MemoryOperationResponse
	decisionCalls    int
	decisionID       string
	decided          api.MemoryCandidateDecisionRequest
	decisionErrors   []error
	decisionRequests []api.MemoryCandidateDecisionRequest
}

func (s *fakeServer) RetrieveKnowledge(_ context.Context, request api.KnowledgeRetrievalRequest) (api.KnowledgeRetrievalResult, error) {
	s.retrieveCalls++
	s.retrieveRequest = request
	return api.KnowledgeRetrievalResult{Hits: []api.RetrievalHit{{CanonicalSlice: "图的顶点由边连接。"}}}, nil
}
func (s *fakeServer) CurrentSession(context.Context) (api.SessionView, error) {
	s.currentCalls++
	return s.currentResult, s.currentErr
}
func (s *fakeServer) Routes(context.Context, string, int, bool) (api.RoutesPage, error) {
	s.routeCalls++
	return s.routeResult, nil
}
func (s *fakeServer) Reviews(_ context.Context, cursor string, limit int, due *time.Time) (api.ReviewsPage, error) {
	s.reviewCalls++
	s.reviewCursor = cursor
	s.reviewLimit = limit
	s.reviewDue = due
	return s.reviewResult, nil
}
func (*fakeServer) MemoryCandidates(context.Context, string, int) (api.MemoryCandidatePage, error) {
	return api.MemoryCandidatePage{}, nil
}
func (s *fakeServer) ExportMemory(_ context.Context, cursor string, limit int) (api.MemoryExportPage, error) {
	s.exportCalls++
	s.exportCursor = cursor
	s.exportLimit = limit
	if s.exportResult.Items == nil {
		s.exportResult.Items = []api.MemoryExportItem{}
	}
	if s.exportResult.ReasonCodes == nil {
		s.exportResult.ReasonCodes = []string{}
	}
	return s.exportResult, s.exportErr
}
func (s *fakeServer) MemoryCandidate(context.Context, string) (api.MemoryCandidateView, error) {
	s.candidateCalls++
	if s.candidateResult.Candidate.Category == "" {
		s.candidateResult.Candidate.Category = "interaction_preference"
		s.candidateResult.Candidate.Sensitivity = "non_sensitive"
		s.candidateResult.Candidate.Stability = "stable"
		s.candidateResult.Candidate.ValidUntil = time.Date(2036, 1, 1, 0, 0, 0, 0, time.UTC)
	}
	return s.candidateResult, s.candidateErr
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
	if s.createResult != nil {
		return *s.createResult, nil
	}
	return api.MemoryOperationResponse{Candidate: &api.MemoryCandidateView{Candidate: api.MemoryCandidate{ID: "candidate-1", Status: "pending_review", Revision: 1}}}, nil
}
func (s *fakeServer) DecideMemoryCandidate(_ context.Context, candidateID string, request api.MemoryCandidateDecisionRequest) (api.MemoryOperationResponse, error) {
	s.decisionCalls++
	s.decisionID = candidateID
	s.decided = request
	s.decisionRequests = append(s.decisionRequests, request)
	if len(s.decisionErrors) > 0 {
		err := s.decisionErrors[0]
		s.decisionErrors = s.decisionErrors[1:]
		if err != nil {
			return api.MemoryOperationResponse{}, err
		}
	}
	return api.MemoryOperationResponse{Candidate: &api.MemoryCandidateView{Candidate: api.MemoryCandidate{ID: candidateID, Status: "admitted", Revision: request.ExpectedRevision + 1}}}, nil
}

func TestSessionLearningStatusUsesAuthoritativeCurrentSession(t *testing.T) {
	t.Run("active", func(t *testing.T) {
		server := &fakeServer{currentResult: api.SessionView{Session: api.TutoringSession{State: "RouteActive"}}}
		session := newTestSession(t, &fakeModel{}, server)
		status, err := session.LearningStatus(t.Context())
		if err != nil || !status.Active || status.View.Session.State != "RouteActive" || server.currentCalls != 1 {
			t.Fatalf("status=%+v err=%v calls=%d", status, err, server.currentCalls)
		}
	})

	t.Run("no current session", func(t *testing.T) {
		server := &fakeServer{currentErr: &api.APIError{Code: "current_session_not_found", Status: 404, RequestID: "request-1"}}
		session := newTestSession(t, &fakeModel{}, server)
		status, err := session.LearningStatus(t.Context())
		if err != nil || status.Active || server.currentCalls != 1 {
			t.Fatalf("status=%+v err=%v calls=%d", status, err, server.currentCalls)
		}
	})

	t.Run("unavailable", func(t *testing.T) {
		server := &fakeServer{currentErr: errors.New("server unavailable")}
		session := newTestSession(t, &fakeModel{}, server)
		if _, err := session.LearningStatus(t.Context()); err == nil || server.currentCalls != 1 {
			t.Fatalf("err=%v calls=%d", err, server.currentCalls)
		}
	})
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
	if len(model.requests[0].Tools) != len(Tools()) || model.requests[0].MaxTokens <= 0 {
		t.Fatalf("production request omitted tools or output reserve: %+v", model.requests[0])
	}
	if server.retrieveRequest.QueryContextSchemaVersion != api.QueryContextSchemaVersion {
		t.Fatalf("query context version=%q", server.retrieveRequest.QueryContextSchemaVersion)
	}
	messages := model.requests[1].Messages
	if messages[len(messages)-1].Role != "tool" || !strings.Contains(messages[len(messages)-1].Content, "图的顶点") {
		t.Fatalf("tool message=%+v", messages[len(messages)-1])
	}
}

func TestSessionUsesProviderPromptUsageForBoundedCalibration(t *testing.T) {
	model := &usageCalibratingModel{}
	session := newTestSession(t, model, &fakeServer{})
	before := session.estimator.Calibration()
	if _, err := session.Send(t.Context(), "校准当前请求预算"); err != nil {
		t.Fatal(err)
	}
	if len(model.requests) != 1 || session.estimator.Calibration() <= before || session.estimator.Calibration() > 2.5 {
		t.Fatalf("provider usage was not applied safely: requests=%d before=%f after=%f", len(model.requests), before, session.estimator.Calibration())
	}
}

func TestSessionPublishesSafeThinkingAndToolLifecycle(t *testing.T) {
	model := &fakeModel{responses: []modelclient.Response{
		{Message: toolMessage("call-1", "search_knowledge", `{"query":"图论"}`)},
		{Message: modelclient.Message{Role: "assistant", Content: "我们先从顶点和边开始。"}},
	}}
	session := newTestSession(t, model, &fakeServer{})
	var activities []Activity
	ctx := WithActivityReporter(t.Context(), func(activity Activity) {
		activities = append(activities, activity)
	})
	if _, err := session.Send(ctx, "教我图论"); err != nil {
		t.Fatal(err)
	}
	if len(activities) != 6 {
		t.Fatalf("activities=%+v", activities)
	}
	wantKinds := []ActivityKind{ActivityThinking, ActivityThinking, ActivityTool, ActivityTool, ActivityThinking, ActivityThinking}
	wantStatuses := []EventStatus{EventRunning, EventSucceeded, EventRunning, EventSucceeded, EventRunning, EventSucceeded}
	for index := range activities {
		if activities[index].Kind != wantKinds[index] || activities[index].Event.Status != wantStatuses[index] {
			t.Fatalf("activity[%d]=%+v", index, activities[index])
		}
	}
	if activities[2].Event.ID != "call-1" || activities[3].Event.ID != "call-1" ||
		activities[2].Event.Tool != "search_knowledge" || strings.Contains(activities[2].Event.Summary, "图论") {
		t.Fatalf("tool lifecycle leaked arguments or lost identity: %+v", activities[2:4])
	}
	if activities[0].Event.ID == activities[4].Event.ID || activities[0].Event.Summary != "正在分析问题" ||
		activities[4].Event.Summary != "正在结合工具结果继续分析" {
		t.Fatalf("thinking lifecycle=%+v", activities)
	}
}

func TestAllReadToolsExecuteContract(t *testing.T) {
	route := api.RouteRevision{
		RouteRevisionID: "route-revision", GoalRevisionID: "goal-revision", Revision: 2,
		Steps: []api.RouteStep{
			{Ordinal: 1, NodeRevisionID: "node-1", TeachingIntent: "理解", CompletionCondition: "能解释"},
			{Ordinal: 2, NodeRevisionID: "node-2", TeachingIntent: "应用", CompletionCondition: "能练习"},
		},
	}
	tests := []struct {
		name       string
		tool       string
		arguments  string
		server     *fakeServer
		wantOutput string
		verify     func(*testing.T, *fakeServer)
	}{
		{
			name: "knowledge", tool: "search_knowledge", arguments: `{"query":"图论"}`,
			server: &fakeServer{}, wantOutput: "图的顶点",
			verify: func(t *testing.T, server *fakeServer) {
				if server.retrieveCalls != 1 || server.retrieveRequest.QueryContextSchemaVersion != api.QueryContextSchemaVersion {
					t.Fatalf("retrieve calls=%d request=%+v", server.retrieveCalls, server.retrieveRequest)
				}
			},
		},
		{
			name: "progress", tool: "get_learning_progress", arguments: `{}`,
			server: &fakeServer{currentResult: api.SessionView{Session: api.TutoringSession{State: "Diagnostic"}}}, wantOutput: `"active":true`,
			verify: func(t *testing.T, server *fakeServer) {
				if server.currentCalls != 1 {
					t.Fatalf("current calls=%d", server.currentCalls)
				}
			},
		},
		{
			name: "route", tool: "get_learning_route", arguments: `{"offset":1}`,
			server: &fakeServer{currentResult: api.SessionView{Session: api.TutoringSession{State: "RouteActive"}, WorkItem: &api.SessionWorkItem{RouteRevision: &route}}}, wantOutput: `"node_revision_id":"node-2"`,
			verify: func(t *testing.T, server *fakeServer) {
				if server.currentCalls != 1 || server.routeCalls != 0 {
					t.Fatalf("current calls=%d route calls=%d", server.currentCalls, server.routeCalls)
				}
			},
		},
		{
			name: "reviews", tool: "get_due_reviews", arguments: `{"cursor":"review-next"}`,
			server: &fakeServer{reviewResult: api.ReviewsPage{Items: []api.ReviewSchedule{{NodeRevisionID: "node-1", Step: 2}}, NextCursor: "review-more"}}, wantOutput: `"next_cursor":"review-more"`,
			verify: func(t *testing.T, server *fakeServer) {
				if server.reviewCalls != 1 || server.reviewCursor != "review-next" || server.reviewLimit != 20 || server.reviewDue == nil {
					t.Fatalf("review call=%d cursor=%q limit=%d due=%v", server.reviewCalls, server.reviewCursor, server.reviewLimit, server.reviewDue)
				}
			},
		},
		{
			name: "preferences", tool: "list_long_term_preferences", arguments: `{"cursor":"memory-next"}`,
			server: &fakeServer{
				exportResult:    api.MemoryExportPage{Items: []api.MemoryExportItem{{Record: api.MemoryRecord{LogicalMemoryID: "memory-1", CandidateID: "candidate-1", Revision: 2}, ContentStatus: "available", Content: "回答时先给结论"}}, NextCursor: "memory-more"},
				candidateResult: api.MemoryCandidateView{Candidate: api.MemoryCandidate{Category: "interaction_preference", Stability: "stable", ValidUntil: time.Date(2036, 1, 1, 0, 0, 0, 0, time.UTC)}},
			}, wantOutput: "回答时先给结论",
			verify: func(t *testing.T, server *fakeServer) {
				if server.exportCalls != 1 || server.exportCursor != "memory-next" || server.exportLimit != 20 || server.candidateCalls != 1 {
					t.Fatalf("export calls=%d cursor=%q limit=%d candidate calls=%d", server.exportCalls, server.exportCursor, server.exportLimit, server.candidateCalls)
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			model := &fakeModel{responses: []modelclient.Response{
				{Message: toolMessage("call-"+test.name, test.tool, test.arguments)},
				{Message: modelclient.Message{Role: "assistant", Content: "工具测试完成"}},
			}}
			session := newTestSession(t, model, test.server)
			result, err := session.Send(t.Context(), "执行工具测试")
			if err != nil || result.Text != "工具测试完成" || len(model.requests) != 2 {
				t.Fatalf("result=%+v requests=%d err=%v", result, len(model.requests), err)
			}
			messages := model.requests[1].Messages
			content := messages[len(messages)-1].Content
			if !strings.Contains(content, test.wantOutput) || strings.Contains(content, `"error"`) {
				t.Fatalf("tool content=%s", content)
			}
			test.verify(t, test.server)
		})
	}
}

func TestPreferenceListingFailsClosedWhenMetadataIsExpiredOrUnavailable(t *testing.T) {
	tests := []struct {
		name   string
		server *fakeServer
	}{
		{
			name: "expired",
			server: &fakeServer{
				exportResult:    api.MemoryExportPage{Items: []api.MemoryExportItem{{Record: api.MemoryRecord{LogicalMemoryID: "memory-1", CandidateID: "candidate-1", Revision: 1}, ContentStatus: "available", Content: "不应暴露的过期偏好"}}, ReasonCodes: []string{}},
				candidateResult: api.MemoryCandidateView{Candidate: api.MemoryCandidate{Category: "interaction_preference", Sensitivity: "non_sensitive", Stability: "stable", ValidUntil: time.Date(2026, 8, 28, 0, 0, 0, 0, time.UTC)}},
			},
		},
		{
			name: "metadata unavailable",
			server: &fakeServer{
				exportResult: api.MemoryExportPage{Items: []api.MemoryExportItem{{Record: api.MemoryRecord{LogicalMemoryID: "memory-1", CandidateID: "candidate-1", Revision: 1}, ContentStatus: "available", Content: "不应在缺少分类时暴露"}}, ReasonCodes: []string{}},
				candidateErr: &api.TransportError{Category: "connection_failed"},
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			model := &fakeModel{responses: []modelclient.Response{
				{Message: toolMessage("call-preferences", "list_long_term_preferences", `{}`)},
				{Message: modelclient.Message{Role: "assistant", Content: "没有可用的长期偏好。"}},
			}}
			session := newTestSession(t, model, test.server)
			if _, err := session.Send(t.Context(), "读取偏好"); err != nil {
				t.Fatal(err)
			}
			messages := model.requests[1].Messages
			content := messages[len(messages)-1].Content
			if strings.Contains(content, "不应") || !strings.Contains(content, `"items":[]`) {
				t.Fatalf("tool content=%s", content)
			}
			if test.name == "metadata unavailable" && (!strings.Contains(content, "candidate_metadata_unavailable") || !strings.Contains(content, `"degraded":true`)) {
				t.Fatalf("degraded metadata result=%s", content)
			}
		})
	}
}

func TestReadToolsRejectNonObjectAndTrailingArguments(t *testing.T) {
	tests := []struct {
		name      string
		tool      string
		arguments string
	}{
		{name: "null", tool: "get_learning_progress", arguments: `null`},
		{name: "trailing", tool: "search_knowledge", arguments: `{"query":"图论"} {}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			model := &fakeModel{responses: []modelclient.Response{
				{Message: toolMessage("call-invalid", test.tool, test.arguments)},
				{Message: modelclient.Message{Role: "assistant", Content: "参数无效，未调用服务端。"}},
			}}
			server := &fakeServer{}
			session := newTestSession(t, model, server)
			result, err := session.Send(t.Context(), "执行错误参数测试")
			if err != nil || len(result.Events) != 1 || result.Events[0].Status != EventInvalid {
				t.Fatalf("result=%+v err=%v", result, err)
			}
			if server.retrieveCalls != 0 || server.currentCalls != 0 || len(model.requests) != 2 {
				t.Fatalf("server calls retrieve=%d current=%d requests=%d", server.retrieveCalls, server.currentCalls, len(model.requests))
			}
			messages := model.requests[1].Messages
			if content := messages[len(messages)-1].Content; !strings.Contains(content, `"error":"invalid_arguments"`) {
				t.Fatalf("tool content=%s", content)
			}
		})
	}
}

func TestLearningProgressTreatsMissingSessionAsEmptyState(t *testing.T) {
	model := &fakeModel{responses: []modelclient.Response{
		{Message: toolMessage("call-progress", "get_learning_progress", `{}`)},
		{Message: modelclient.Message{Role: "assistant", Content: "你还没有进行中的学习会话。"}},
	}}
	server := &fakeServer{currentErr: &api.APIError{Code: "not_found", Status: 404, RequestID: "request-1"}}
	session := newTestSession(t, model, server)
	if _, err := session.Send(t.Context(), "看看进度"); err != nil {
		t.Fatal(err)
	}
	messages := model.requests[1].Messages
	if content := messages[len(messages)-1].Content; !strings.Contains(content, "no_current_session") || strings.Contains(content, `"error"`) {
		t.Fatalf("tool content=%s", content)
	}
}

func TestPreferenceConfirmationAdmitsPendingPersonalContext(t *testing.T) {
	arguments := `{"content":"我正在准备研究生入学考试","reason":"用户明确要求长期记住个人背景","category":"personal_context","sensitivity":"non_sensitive","stability":"stable"}`
	model := &fakeModel{responses: []modelclient.Response{
		{Message: toolMessage("call-pref", "remember_preference", arguments)},
		{Message: modelclient.Message{Role: "assistant", Content: "长期偏好已保存。"}},
	}}
	server := &fakeServer{}
	session := newTestSession(t, model, server)
	result, err := session.Send(t.Context(), "请长期记住我正在准备研究生入学考试")
	if err != nil || result.Pending == nil || server.createCalls != 0 || server.decisionCalls != 0 {
		t.Fatalf("result=%+v create=%d decision=%d err=%v", result, server.createCalls, server.decisionCalls, err)
	}
	result, err = session.ResolvePreference(t.Context(), true)
	if err != nil || result.Text == "" || server.createCalls != 1 || server.decisionCalls != 1 {
		t.Fatalf("result=%+v create=%d decision=%d err=%v", result, server.createCalls, server.decisionCalls, err)
	}
	if server.created.Category != "personal_context" || server.created.PayloadSchemaVersion != 1 ||
		server.decisionID != "candidate-1" || server.decided.ExpectedRevision != 1 || server.decided.Decision != "admit" ||
		server.decided.PayloadSchemaVersion != 1 || server.created.OperationID == server.decided.OperationID {
		t.Fatalf("created=%+v decisionID=%q decided=%+v", server.created, server.decisionID, server.decided)
	}
	messages := model.requests[1].Messages
	if content := messages[len(messages)-1].Content; !strings.Contains(content, `"saved":true`) || !strings.Contains(content, `"status":"admitted"`) {
		t.Fatalf("tool result=%s", content)
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
	if err != nil || result.Pending != nil || server.createCalls != 1 || server.decisionCalls != 1 || !strings.Contains(result.Text, "已保存") {
		t.Fatalf("resolve result=%+v create=%d decision=%d err=%v", result, server.createCalls, server.decisionCalls, err)
	}
	if _, err = session.Send(t.Context(), "继续学习"); err == nil || strings.Contains(err.Error(), "待确认") {
		t.Fatalf("session remained stuck in confirmation: %v", err)
	}
}

func TestPreferenceDeterministicRejectionAllowsDecline(t *testing.T) {
	arguments := `{"content":"回答时先给结论","reason":"用户明确要求长期保持回答风格","category":"interaction_preference","sensitivity":"non_sensitive","stability":"stable"}`
	model := &fakeModel{responses: []modelclient.Response{
		{Message: toolMessage("call-pref", "remember_preference", arguments)},
		{Message: modelclient.Message{Role: "assistant", Content: "服务端拒绝后没有保存该偏好。"}},
	}}
	server := &fakeServer{createErrors: []error{&api.APIError{Code: "forbidden", Status: 403}}}
	session := newTestSession(t, model, server)
	result, err := session.Send(t.Context(), "请长期记住")
	if err != nil || result.Pending == nil || len(result.Events) != 1 || result.Events[0].Status != EventConfirmationRequired {
		t.Fatalf("send result=%+v err=%v", result, err)
	}
	if _, err := session.ResolvePreference(t.Context(), true); err == nil || errors.Is(err, ErrPreferenceOutcomeUnknown) || !strings.Contains(err.Error(), "forbidden") {
		t.Fatalf("deterministic rejection err=%v", err)
	}
	result, err = session.ResolvePreference(t.Context(), false)
	if err != nil || result.Pending != nil || !strings.Contains(result.Text, "没有保存") || server.createCalls != 1 {
		t.Fatalf("decline result=%+v calls=%d err=%v", result, server.createCalls, err)
	}
}

func TestPreferenceRetryReusesOperationIDAfterAmbiguousFailure(t *testing.T) {
	arguments := `{"content":"回答时先给结论","reason":"用户明确要求长期保持回答风格","category":"interaction_preference","sensitivity":"non_sensitive","stability":"stable"}`
	model := &fakeModel{responses: []modelclient.Response{
		{Message: toolMessage("call-pref", "remember_preference", arguments)},
		{Message: modelclient.Message{Role: "assistant", Content: "长期偏好已保存。"}},
	}}
	server := &fakeServer{createErrors: []error{errors.New("response lost")}}
	uuidCalls := 0
	session, err := New(model, server, Options{
		ContextWindow: 32768, MaxToolRounds: 8,
		Now: func() time.Time { return time.Date(2026, 8, 29, 0, 0, 0, 0, time.UTC) },
		NewUUID: func() (string, error) {
			ids := []string{
				"60000000-0000-4000-8000-000000000001",
				"60000000-0000-4000-8000-000000000002",
				"60000000-0000-4000-8000-000000000003",
				"60000000-0000-4000-8000-000000000004",
			}
			if uuidCalls >= len(ids) {
				return "", errors.New("unexpected UUID request")
			}
			value := ids[uuidCalls]
			uuidCalls++
			return value, nil
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
	createID, decisionID := session.pendingOperationID, session.pendingDecisionOperationID
	session.hotRawTokenLimit = 1
	session.trimRawHistory()
	turn := session.turns[session.currentTurnID]
	if createID == "" || decisionID == "" || createID == decisionID ||
		session.pendingOperationID != createID || session.pendingDecisionOperationID != decisionID ||
		turn == nil || !turn.Protected || !turn.OutcomeUnknown || !messagesContainText(session.messages, "请长期记住") {
		t.Fatalf("outcome-unknown preference was trimmed or IDs changed: create=%q decision=%q turn=%+v messages=%+v", createID, decisionID, turn, session.messages)
	}
	if _, err := session.ResolvePreference(t.Context(), false); !errors.Is(err, ErrPreferenceOutcomeUnknown) {
		t.Fatalf("ambiguous write allowed decline: %v", err)
	}
	result, err = session.ResolvePreference(t.Context(), true)
	if err != nil || result.Pending != nil || server.createCalls != 2 || server.decisionCalls != 1 || uuidCalls != 2 {
		t.Fatalf("retry result=%+v create=%d decision=%d uuidCalls=%d err=%v", result, server.createCalls, server.decisionCalls, uuidCalls, err)
	}
	if len(server.createRequests) != 2 || server.createRequests[0].OperationID == "" || server.createRequests[0].OperationID != server.createRequests[1].OperationID {
		t.Fatalf("operation IDs changed across retry: %+v", server.createRequests)
	}

	model.responses = append(model.responses,
		modelclient.Response{Message: toolMessage("call-pref-2", "remember_preference", arguments)},
		modelclient.Response{Message: modelclient.Message{Role: "assistant", Content: "第二个长期偏好已保存。"}},
	)
	result, err = session.Send(t.Context(), "再记住一个偏好")
	if err != nil || result.Pending == nil {
		t.Fatalf("second send result=%+v err=%v", result, err)
	}
	if _, err := session.ResolvePreference(t.Context(), true); err != nil {
		t.Fatal(err)
	}
	if uuidCalls != 4 || len(server.createRequests) != 3 || len(server.decisionRequests) != 2 ||
		server.createRequests[2].OperationID == server.createRequests[0].OperationID ||
		server.decisionRequests[1].OperationID == server.decisionRequests[0].OperationID {
		t.Fatalf("subsequent preference create=%+v decisions=%+v uuidCalls=%d", server.createRequests, server.decisionRequests, uuidCalls)
	}
}

func TestPreferenceDecisionRetryReusesOperationIDAfterAmbiguousFailure(t *testing.T) {
	arguments := `{"content":"回答时先给结论","reason":"用户明确要求长期保持回答风格","category":"interaction_preference","sensitivity":"non_sensitive","stability":"stable"}`
	model := &fakeModel{responses: []modelclient.Response{
		{Message: toolMessage("call-pref", "remember_preference", arguments)},
		{Message: modelclient.Message{Role: "assistant", Content: "长期偏好已保存。"}},
	}}
	server := &fakeServer{decisionErrors: []error{errors.New("decision response lost")}}
	session := newTestSession(t, model, server)
	result, err := session.Send(t.Context(), "请长期记住")
	if err != nil || result.Pending == nil {
		t.Fatalf("send result=%+v err=%v", result, err)
	}
	if _, err := session.ResolvePreference(t.Context(), true); !errors.Is(err, ErrPreferenceOutcomeUnknown) {
		t.Fatalf("first decision err=%v", err)
	}
	if _, err := session.ResolvePreference(t.Context(), false); !errors.Is(err, ErrPreferenceOutcomeUnknown) {
		t.Fatalf("ambiguous decision allowed decline: %v", err)
	}
	result, err = session.ResolvePreference(t.Context(), true)
	if err != nil || result.Pending != nil || server.createCalls != 2 || server.decisionCalls != 2 {
		t.Fatalf("retry result=%+v create=%d decision=%d err=%v", result, server.createCalls, server.decisionCalls, err)
	}
	if len(server.createRequests) != 2 || server.createRequests[0].OperationID != server.createRequests[1].OperationID ||
		len(server.decisionRequests) != 2 || server.decisionRequests[0].OperationID != server.decisionRequests[1].OperationID {
		t.Fatalf("operation IDs changed create=%+v decision=%+v", server.createRequests, server.decisionRequests)
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
	if _, err := session.contextMessages(); err == nil {
		t.Fatal("oversized context was accepted")
	} else {
		var contextErr *ContextError
		if !errors.As(err, &contextErr) || contextErr.Code != ContextTurnTooLarge {
			t.Fatalf("oversized context err=%v", err)
		}
	}
}

func TestTokenEstimatorChargesToolsAndBoundsUsageCalibration(t *testing.T) {
	estimator := NewTokenEstimator()
	messages := []modelclient.Message{{Role: "system", Content: "rules"}, {Role: "user", Content: "hello 学习"}}
	withoutTools := estimator.EstimateRequest(modelclient.Request{Messages: messages})
	withTools := estimator.EstimateRequest(modelclient.Request{Messages: messages, Tools: Tools()})
	if withTools <= withoutTools {
		t.Fatalf("tool schemas were not charged: without=%d with=%d", withoutTools, withTools)
	}
	before := estimator.Calibration()
	estimator.ObserveActual(0, modelclient.Usage{PromptTokens: 100})
	estimator.ObserveActual(100, modelclient.Usage{})
	estimator.ObserveActual(100, modelclient.Usage{PromptTokens: 10000})
	if estimator.Calibration() != before {
		t.Fatalf("invalid usage polluted calibration: before=%f after=%f", before, estimator.Calibration())
	}
	estimator.ObserveActual(100, modelclient.Usage{PromptTokens: 200})
	if got := estimator.Calibration(); got <= 1 || got > 2.5 {
		t.Fatalf("valid EWMA calibration out of bounds: %f", got)
	}
}

func TestContextPlannerProjectsHistoryAndKeepsToolGroupsAtomic(t *testing.T) {
	estimator := NewTokenEstimator()
	call := toolMessage("old-call", "search_knowledge", `{"query":"old"}`)
	messages := []modelclient.Message{
		{Role: "system", Content: "rules"},
		{Role: "user", Content: "old question"},
		call,
		{Role: "tool", ToolCallID: "old-call", Content: strings.Repeat("x", 12<<10)},
		{Role: "assistant", Content: "old answer"},
		{Role: "user", Content: "current question"},
	}
	history := map[string]string{"old-call": `{"degraded":false,"summary":"history"}`}
	plan, err := (ContextPlanner{ContextWindow: 4096, Mode: ContextCompactionAuto, Estimator: estimator}).Plan(messages, nil, history)
	if err != nil {
		t.Fatal(err)
	}
	foundCall, foundResult := false, false
	for _, message := range plan.Request.Messages {
		if len(message.ToolCalls) > 0 && message.ToolCalls[0].ID == "old-call" {
			foundCall = true
		}
		if message.ToolCallID == "old-call" {
			foundResult = true
			if message.Content != history["old-call"] || !json.Valid([]byte(message.Content)) {
				t.Fatalf("history projection is not valid compact JSON: %q", message.Content)
			}
		}
	}
	if foundCall != foundResult || !foundCall {
		t.Fatalf("tool group was split or unexpectedly dropped: call=%t result=%t messages=%+v", foundCall, foundResult, plan.Request.Messages)
	}

	delete(history, "old-call")
	_, err = (ContextPlanner{ContextWindow: 4096, Mode: ContextCompactionAuto, Estimator: estimator}).Plan(messages, nil, history)
	assertContextCode(t, err, ContextRecentTurnsTooLarge)
}

func TestContextPlannerFailsClosedWhenMandatoryRecentTurnsDoNotFit(t *testing.T) {
	t.Parallel()
	estimator := NewTokenEstimator()
	for _, test := range []struct {
		name     string
		messages []modelclient.Message
	}{
		{
			name: "only one of two recent turns fits",
			messages: []modelclient.Message{
				{Role: "user", Content: "turn one"}, {Role: "assistant", Content: strings.Repeat("a", 4500)},
				{Role: "user", Content: "turn two"}, {Role: "assistant", Content: strings.Repeat("b", 4500)},
				{Role: "user", Content: "current"},
			},
		},
		{
			name: "no recent turn fits",
			messages: []modelclient.Message{
				{Role: "user", Content: "turn one"}, {Role: "assistant", Content: strings.Repeat("a", 9000)},
				{Role: "user", Content: "current"},
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := (ContextPlanner{ContextWindow: 4096, Mode: ContextCompactionAuto, Estimator: estimator}).Plan(test.messages, nil, nil)
			assertContextCode(t, err, ContextRecentTurnsTooLarge)
		})
	}
}

func TestContextPlannerOffAndTypedBudgetFailures(t *testing.T) {
	estimator := NewTokenEstimator()
	planner := ContextPlanner{ContextWindow: 4096, Mode: ContextCompactionOff, Estimator: estimator}
	_, err := planner.Plan([]modelclient.Message{
		{Role: "system", Content: "rules"},
		{Role: "user", Content: strings.Repeat("x", 9<<10)},
		{Role: "assistant", Content: "old"},
		{Role: "user", Content: "current"},
	}, nil, nil)
	assertContextCode(t, err, ContextBudgetInvalid)

	_, err = (ContextPlanner{ContextWindow: 4096, Mode: ContextCompactionAuto, Estimator: estimator}).Plan(
		[]modelclient.Message{{Role: "system", Content: "rules"}, {Role: "user", Content: "current"}},
		[]modelclient.Tool{{Type: "function", Function: modelclient.ToolDefinition{Name: "huge", Description: strings.Repeat("schema", 4000), Parameters: json.RawMessage(`{"type":"object"}`)}}}, nil)
	assertContextCode(t, err, ContextBudgetInvalid)

	_, err = (ContextPlanner{ContextWindow: 4096, Mode: ContextCompactionAuto, Estimator: estimator}).Plan(
		[]modelclient.Message{{Role: "system", Content: "rules"}, {Role: "user", Content: strings.Repeat("x", 9<<10)}}, nil, nil)
	assertContextCode(t, err, ContextTurnTooLarge)
}

func TestToolProjectionProducesValidHistoryAndEnforcesTurnBudget(t *testing.T) {
	projection := projectToolResult("search_knowledge", map[string]any{
		"knowledge_revision_id": "revision-1",
		"hits":                  []map[string]any{{"path": "notes/graph.md", "node_revision_id": "node-1", "text": strings.Repeat("知识", 5000)}},
	})
	if !json.Valid([]byte(projection.Live)) || !json.Valid([]byte(projection.History)) || !json.Valid([]byte(projection.Recall)) {
		t.Fatalf("projection contains invalid JSON: %+v", projection)
	}

	session := newTestSession(t, &fakeModel{}, &fakeServer{})
	session.appendToolResult("search_knowledge", "call-1", map[string]any{"hits": []any{strings.Repeat("a", 4000)}})
	session.appendToolResult("remember_preference", "call-2", map[string]any{"submitted": true, "saved": true, "status": "admitted", "candidate_id": "candidate-1", "content": strings.Repeat("b", 4000)})
	last := session.messages[len(session.messages)-1].Content
	if !json.Valid([]byte(last)) || !strings.Contains(last, `"truncated":true`) || !strings.Contains(last, `"degraded":true`) || !strings.Contains(last, `"saved":true`) || !strings.Contains(last, `"status":"admitted"`) {
		t.Fatalf("cumulative tool budget lost valid degraded preference outcome: %s", last)
	}
}

func messagesContainText(messages []modelclient.Message, text string) bool {
	for _, message := range messages {
		if strings.Contains(message.Content, text) {
			return true
		}
	}
	return false
}

func assertContextCode(t *testing.T, err error, code string) {
	t.Helper()
	var contextErr *ContextError
	if !errors.As(err, &contextErr) || contextErr.Code != code {
		t.Fatalf("error=%v, want context code %s", err, code)
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
	return session
}

func toolMessage(id, name, arguments string) modelclient.Message {
	return modelclient.Message{Role: "assistant", ToolCalls: []modelclient.ToolCall{{
		ID: id, Type: "function", Function: modelclient.ToolFunction{Name: name, Arguments: arguments},
	}}}
}
