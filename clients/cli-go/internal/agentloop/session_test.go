package agentloop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/api"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/modelclient"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/workspace"
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

type cancelAfterMutationModel struct {
	requests        int
	followupStarted chan struct{}
}

func (m *cancelAfterMutationModel) Complete(ctx context.Context, _ modelclient.Request) (modelclient.Response, error) {
	m.requests++
	if m.requests == 1 {
		return modelclient.Response{Message: toolMessage("write-call", workspace.ToolWrite, `{"path":"notes.md","mode":"create","content":"hello"}`)}, nil
	}
	close(m.followupStarted)
	<-ctx.Done()
	return modelclient.Response{}, ctx.Err()
}

type fakeWorkspaceExecutor struct {
	status         workspace.Status
	results        map[string]workspace.Result
	prepareResults map[string]workspace.Result
	prepared       map[string]*workspace.PreparedMutation
	commitResults  []workspace.Result
	calls          []string
	prepareCalls   []string
	commitCalls    []string
	started        chan string
	block          bool
	commitStarted  chan string
	commitBlock    bool
}

func (w *fakeWorkspaceExecutor) Definitions() []modelclient.Tool { return workspace.Definitions() }
func (w *fakeWorkspaceExecutor) Execute(ctx context.Context, toolName, _ string) workspace.Result {
	w.calls = append(w.calls, toolName)
	if w.started != nil {
		select {
		case w.started <- toolName:
		case <-ctx.Done():
		}
	}
	if w.block {
		<-ctx.Done()
		return workspace.Result{Value: map[string]any{"code": workspace.CodeCancelled}, Summary: "工作区工具已取消"}
	}
	if result, ok := w.results[toolName]; ok {
		return result
	}
	return workspace.Result{Value: map[string]any{"code": workspace.CodeInvalidArguments}, Summary: "未知工作区工具"}
}
func (w *fakeWorkspaceExecutor) PrepareMutation(ctx context.Context, toolName, _ string) (*workspace.PreparedMutation, workspace.Result) {
	w.prepareCalls = append(w.prepareCalls, toolName)
	if w.block {
		<-ctx.Done()
		return nil, workspace.Result{Value: map[string]any{"code": workspace.CodeCancelled}, Summary: "工作区工具已取消", Publication: workspace.PublicationUnchanged}
	}
	if prepared := w.prepared[toolName]; prepared != nil {
		return prepared, workspace.Result{}
	}
	if result, ok := w.prepareResults[toolName]; ok {
		return nil, result
	}
	return nil, workspace.Result{Value: map[string]any{"error": workspace.CodeInvalidArguments, "code": workspace.CodeInvalidArguments}, Summary: "文件修改参数无效", Publication: workspace.PublicationUnchanged}
}
func (w *fakeWorkspaceExecutor) CommitMutation(ctx context.Context, prepared *workspace.PreparedMutation) workspace.Result {
	toolName := ""
	if prepared != nil {
		toolName = prepared.Presentation.Tool
		w.commitCalls = append(w.commitCalls, toolName)
	}
	if w.commitStarted != nil {
		select {
		case w.commitStarted <- toolName:
		case <-ctx.Done():
		}
	}
	if w.commitBlock {
		<-ctx.Done()
		code, summary := workspace.CodeCancelled, "工作区工具已取消"
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			code, summary = workspace.CodeTimeout, "工作区工具已超时"
		}
		return workspace.Result{
			Value: map[string]any{
				"error":               code,
				"code":                code,
				"complete":            false,
				"publication_outcome": string(workspace.PublicationUnchanged),
			},
			Summary: summary, Publication: workspace.PublicationUnchanged,
		}
	}
	if len(w.commitResults) == 0 {
		return workspace.Result{Value: map[string]any{"error": workspace.CodeInternalError}, Summary: "文件修改失败", Publication: workspace.PublicationUnchanged}
	}
	result := w.commitResults[0]
	w.commitResults = w.commitResults[1:]
	return result
}
func (w *fakeWorkspaceExecutor) Status() workspace.Status { return w.status }
func (*fakeWorkspaceExecutor) Close() error               { return nil }

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
	status := "admitted"
	if request.Decision == "reject" {
		status = "rejected"
	}
	return api.MemoryOperationResponse{Candidate: &api.MemoryCandidateView{Candidate: api.MemoryCandidate{ID: candidateID, Status: status, Revision: request.ExpectedRevision + 1}}}, nil
}

func TestSessionSupportsUnlimitedToolLoopAndOptionalUserLimit(t *testing.T) {
	t.Parallel()
	responses := make([]modelclient.Response, 0, 62)
	for index := 0; index < 61; index++ {
		responses = append(responses, modelclient.Response{Message: modelclient.Message{Role: "assistant", ToolCalls: []modelclient.ToolCall{{
			ID: fmt.Sprintf("call-%d", index), Type: "function", Function: modelclient.ToolFunction{Name: "get_learning_progress", Arguments: `{}`},
		}}}})
	}
	responses = append(responses, modelclient.Response{Message: modelclient.Message{Role: "assistant", Content: "完成长工具循环"}})
	model := &fakeModel{responses: responses}
	uuidCalls := 0
	session, err := New(model, &fakeServer{}, Options{
		ContextWindow: 1_000_000, MaxToolRounds: 0, Now: time.Now,
		NewUUID: func() (string, error) {
			uuidCalls++
			return fmt.Sprintf("60000000-0000-4000-8000-%012d", uuidCalls), nil
		},
	})
	if err != nil {
		t.Fatalf("unlimited tool loop rejected: %v", err)
	}
	result, err := session.Send(t.Context(), "完成超过旧上限的工具循环")
	if err != nil {
		t.Fatalf("unlimited tool loop failed: %v", err)
	}
	if result.Text != "完成长工具循环" || len(model.requests) != 62 {
		t.Fatalf("result=%+v requests=%d", result, len(model.requests))
	}

	limitedModel := &fakeModel{responses: []modelclient.Response{
		{Message: modelclient.Message{Role: "assistant", ToolCalls: []modelclient.ToolCall{{ID: "limited-call", Type: "function", Function: modelclient.ToolFunction{Name: "get_learning_progress", Arguments: `{}`}}}}},
		{Message: modelclient.Message{Role: "assistant", Content: "不应到达"}},
	}}
	limited, err := New(limitedModel, &fakeServer{}, Options{
		ContextWindow: 32768, MaxToolRounds: 1, Now: time.Now,
		NewUUID: func() (string, error) { return "60000000-0000-4000-8000-000000000999", nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := limited.Send(t.Context(), "测试自选保护值"); err == nil || !strings.Contains(err.Error(), "保护值") {
		t.Fatalf("optional tool-round guard err=%v", err)
	}
	if _, err := New(&fakeModel{}, &fakeServer{}, Options{
		ContextWindow: 4096, MaxToolRounds: -1, Now: time.Now,
		NewUUID: func() (string, error) { return "60000000-0000-4000-8000-000000000998", nil },
	}); err == nil {
		t.Fatal("negative MaxToolRounds accepted")
	}
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

func TestSessionExecutesWorkspaceToolWithLocalAuthorityBeforeAnswer(t *testing.T) {
	const contentHash = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	executor := &fakeWorkspaceExecutor{
		status: workspace.Status{Available: true, Label: "project"},
		results: map[string]workspace.Result{
			workspace.ToolRead: {
				Value:     map[string]any{"path": "notes.md", "content": "workspace text\n", "complete": true, "content_hash": contentHash},
				Summary:   "已读取 notes.md",
				Reference: &workspace.Reference{Path: "notes.md", ContentHash: contentHash, Kind: "file"},
			},
		},
	}
	model := &fakeModel{responses: []modelclient.Response{
		{Message: toolMessage("workspace-call", workspace.ToolRead, `{"path":"notes.md"}`)},
		{Message: modelclient.Message{Role: "assistant", Content: "已结合工作区文件回答。"}},
	}}
	session := newWorkspaceTestSession(t, model, executor)
	result, err := session.Send(t.Context(), "读取工作区笔记")
	if err != nil {
		t.Fatal(err)
	}
	if result.Text == "" || len(model.requests) != 2 || !reflect.DeepEqual(executor.calls, []string{workspace.ToolRead}) {
		t.Fatalf("result=%+v requests=%d calls=%+v", result, len(model.requests), executor.calls)
	}
	toolNames := make(map[string]bool, len(model.requests[0].Tools))
	for _, definition := range model.requests[0].Tools {
		toolNames[definition.Function.Name] = true
	}
	for _, expected := range []string{workspace.ToolList, workspace.ToolRead, workspace.ToolSearch, workspace.ToolWrite, workspace.ToolEdit} {
		if !toolNames[expected] {
			t.Fatalf("workspace tool %q missing from request: %+v", expected, toolNames)
		}
	}
	if toolNames["shell"] {
		t.Fatalf("forbidden workspace tool shell was exposed")
	}
	messages := model.requests[1].Messages
	last := messages[len(messages)-1]
	if last.Role != "tool" || last.ToolCallID != "workspace-call" || !strings.Contains(last.Content, "workspace text") {
		t.Fatalf("workspace tool message=%+v", last)
	}
	if session.toolReferences["workspace-call"] != nil {
		t.Fatalf("workspace result was recorded as server reference: %+v", session.toolReferences["workspace-call"])
	}
	reference := session.workspaceReferences["workspace-call"]
	if reference == nil || reference.Path != "notes.md" || reference.ContentHash != contentHash {
		t.Fatalf("workspace reference=%+v", reference)
	}
	session.contextRuntime.mu.Lock()
	defer session.contextRuntime.mu.Unlock()
	foundWorkspaceSource := false
	for _, sourceID := range session.contextRuntime.ledger.SourceOrder {
		source := session.contextRuntime.ledger.Sources[sourceID]
		if source.ModelMessage.ToolCallID != "workspace-call" {
			continue
		}
		foundWorkspaceSource = source.Authority == AuthorityWorkspaceSnapshot && source.Freshness == FreshnessWorkspaceObserved && source.ServerReference == nil && source.WorkspaceReference != nil
	}
	if !foundWorkspaceSource {
		t.Fatal("workspace tool result did not retain local workspace authority")
	}
}

func TestFileMutationConfirmFreezesPendingOperationAcrossYOLOSwitch(t *testing.T) {
	prepared := &workspace.PreparedMutation{Presentation: workspace.MutationPresentation{
		Tool: workspace.ToolWrite, Operation: "write_create", Path: "notes.md", PreviewKind: "content", Preview: "hello\n",
	}}
	executor := &fakeWorkspaceExecutor{
		status:   workspace.Status{Available: true, Label: "project"},
		prepared: map[string]*workspace.PreparedMutation{workspace.ToolWrite: prepared},
		commitResults: []workspace.Result{{
			Value:   map[string]any{"path": "notes.md", "content_hash": "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "complete": true, "publication_outcome": "completed"},
			Summary: "已创建 notes.md", Reference: &workspace.Reference{Path: "notes.md", ContentHash: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Kind: "file"},
			Publication: workspace.PublicationCompleted,
		}},
	}
	model := &fakeModel{responses: []modelclient.Response{
		{Message: toolMessage("write-call", workspace.ToolWrite, `{"path":"notes.md","mode":"create","content":"hello\n"}`)},
		{Message: modelclient.Message{Role: "assistant", Content: "文件已创建。"}},
	}}
	session := newWorkspaceTestSession(t, model, executor)
	if mode := session.FileAuthorizationMode(); mode != FileAuthorizationConfirm {
		t.Fatalf("default mode=%q", mode)
	}
	pendingResult, err := session.Send(t.Context(), "创建笔记")
	if err != nil {
		t.Fatal(err)
	}
	pending := pendingResult.PendingFileMutation
	if pending == nil || pending.CallID != "write-call" || pending.Path != "notes.md" || len(executor.commitCalls) != 0 || len(model.requests) != 1 {
		t.Fatalf("pending=%+v commits=%+v requests=%d", pending, executor.commitCalls, len(model.requests))
	}
	if err := session.SetFileAuthorizationMode(FileAuthorizationYOLO); err != nil {
		t.Fatal(err)
	}
	if len(executor.commitCalls) != 0 {
		t.Fatalf("mode switch auto-approved pending mutation: %+v", executor.commitCalls)
	}
	resolved, err := session.ResolveFileMutation(t.Context(), pending.CallID, FileMutationApprove)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Text != "文件已创建。" || !reflect.DeepEqual(executor.commitCalls, []string{workspace.ToolWrite}) || len(model.requests) != 2 {
		t.Fatalf("resolved=%+v commits=%+v requests=%d", resolved, executor.commitCalls, len(model.requests))
	}
}

func TestFileMutationDeclineIsIsolatedFromOtherResolvers(t *testing.T) {
	prepared := &workspace.PreparedMutation{Presentation: workspace.MutationPresentation{
		Tool: workspace.ToolEdit, Operation: "edit", Path: "notes.md", PreviewKind: "diff", Preview: "-old\n+new\n",
	}}
	executor := &fakeWorkspaceExecutor{
		status:   workspace.Status{Available: true, Label: "project"},
		prepared: map[string]*workspace.PreparedMutation{workspace.ToolEdit: prepared},
	}
	model := &fakeModel{responses: []modelclient.Response{
		{Message: toolMessage("edit-call", workspace.ToolEdit, `{"path":"notes.md","expected_hash":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","edits":[{"old_text":"old","new_text":"new"}]}`)},
		{Message: modelclient.Message{Role: "assistant", Content: "已保留原文件。"}},
	}}
	session := newWorkspaceTestSession(t, model, executor)
	pendingResult, err := session.Send(t.Context(), "编辑笔记")
	if err != nil {
		t.Fatal(err)
	}
	pending := pendingResult.PendingFileMutation
	if pending == nil {
		t.Fatal("file mutation did not enter its dedicated pending state")
	}
	if _, err := session.ResolveQuestion(t.Context(), QuestionAnswer{QuestionID: "q", Status: QuestionCancelled}); err == nil {
		t.Fatal("question resolver authorized or consumed a file mutation")
	}
	if _, err := session.ResolvePreference(t.Context(), PreferenceDecline); err == nil {
		t.Fatal("preference resolver authorized or consumed a file mutation")
	}
	if len(executor.commitCalls) != 0 {
		t.Fatalf("unrelated resolver committed mutation: %+v", executor.commitCalls)
	}
	resolved, err := session.ResolveFileMutation(t.Context(), pending.CallID, FileMutationDecline)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Text != "已保留原文件。" || len(executor.commitCalls) != 0 || len(model.requests) != 2 {
		t.Fatalf("resolved=%+v commits=%+v requests=%d", resolved, executor.commitCalls, len(model.requests))
	}
	messages := model.requests[1].Messages
	last := messages[len(messages)-1]
	if last.Role != "tool" || last.ToolCallID != "edit-call" || !strings.Contains(last.Content, workspace.CodeAuthorizationDenied) {
		t.Fatalf("decline tool result=%+v", last)
	}
}

func TestFileMutationYOLOCommitsFutureOperationWithoutPending(t *testing.T) {
	prepared := &workspace.PreparedMutation{Presentation: workspace.MutationPresentation{
		Tool: workspace.ToolWrite, Operation: "write_replace", Path: "notes.md", PreviewKind: "diff", Preview: "-old\n+new\n",
	}}
	executor := &fakeWorkspaceExecutor{
		status:   workspace.Status{Available: true, Label: "project"},
		prepared: map[string]*workspace.PreparedMutation{workspace.ToolWrite: prepared},
		commitResults: []workspace.Result{{
			Value:   map[string]any{"path": "notes.md", "complete": true, "publication_outcome": "completed"},
			Summary: "已替换 notes.md", Publication: workspace.PublicationCompleted,
		}},
	}
	model := &fakeModel{responses: []modelclient.Response{
		{Message: toolMessage("write-call", workspace.ToolWrite, `{"path":"notes.md","mode":"replace","content":"new","expected_hash":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`)},
		{Message: modelclient.Message{Role: "assistant", Content: "已更新。"}},
	}}
	session := newWorkspaceTestSession(t, model, executor)
	if err := session.SetFileAuthorizationMode(FileAuthorizationYOLO); err != nil {
		t.Fatal(err)
	}
	result, err := session.Send(t.Context(), "直接更新")
	if err != nil {
		t.Fatal(err)
	}
	if result.PendingFileMutation != nil || result.Text != "已更新。" || !reflect.DeepEqual(executor.commitCalls, []string{workspace.ToolWrite}) || len(model.requests) != 2 {
		t.Fatalf("result=%+v commits=%+v requests=%d", result, executor.commitCalls, len(model.requests))
	}
}

func TestFileMutationUnknownOutcomeStopsSiblingsAndFollowUpModel(t *testing.T) {
	prepared := &workspace.PreparedMutation{Presentation: workspace.MutationPresentation{
		Tool: workspace.ToolWrite, Operation: "write_create", Path: "notes.md", PreviewKind: "content", Preview: "hello",
	}}
	executor := &fakeWorkspaceExecutor{
		status:   workspace.Status{Available: true, Label: "project"},
		prepared: map[string]*workspace.PreparedMutation{workspace.ToolWrite: prepared},
		commitResults: []workspace.Result{{
			Value:   map[string]any{"path": "notes.md", "code": workspace.CodeOutcomeUnknown, "publication_outcome": "unknown", "complete": false},
			Summary: "文件发布结果无法确认", Publication: workspace.PublicationUnknown,
		}},
	}
	model := &fakeModel{responses: []modelclient.Response{
		{Message: modelclient.Message{Role: "assistant", ToolCalls: []modelclient.ToolCall{
			{ID: "write-call", Type: "function", Function: modelclient.ToolFunction{Name: workspace.ToolWrite, Arguments: `{"path":"notes.md","mode":"create","content":"hello"}`}},
			{ID: "read-call", Type: "function", Function: modelclient.ToolFunction{Name: workspace.ToolRead, Arguments: `{"path":"notes.md"}`}},
		}}},
		{Message: modelclient.Message{Role: "assistant", Content: "不应继续调用模型"}},
	}}
	session := newWorkspaceTestSession(t, model, executor)
	if err := session.SetFileAuthorizationMode(FileAuthorizationYOLO); err != nil {
		t.Fatal(err)
	}
	result, err := session.Send(t.Context(), "创建后读取")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Text, "无法确认") || len(model.requests) != 1 || len(executor.calls) != 0 {
		t.Fatalf("result=%+v requests=%d readCalls=%+v", result, len(model.requests), executor.calls)
	}
	foundTool := false
	for _, message := range session.messages {
		if message.Role == "tool" && message.ToolCallID == "write-call" && strings.Contains(message.Content, workspace.CodeOutcomeUnknown) {
			foundTool = true
		}
		if message.Role == "tool" && message.ToolCallID == "read-call" {
			t.Fatal("sibling tool was retained after unknown publication")
		}
	}
	if !foundTool {
		t.Fatal("unknown publication tool result was not preserved")
	}
}

func TestFileMutationModelFailurePreservesCompletedEffect(t *testing.T) {
	prepared := &workspace.PreparedMutation{Presentation: workspace.MutationPresentation{
		Tool: workspace.ToolWrite, Operation: "write_create", Path: "notes.md", PreviewKind: "content", Preview: "hello",
	}}
	executor := &fakeWorkspaceExecutor{
		status:   workspace.Status{Available: true, Label: "project"},
		prepared: map[string]*workspace.PreparedMutation{workspace.ToolWrite: prepared},
		commitResults: []workspace.Result{{
			Value:   map[string]any{"path": "notes.md", "content_hash": "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "complete": true, "publication_outcome": "completed"},
			Summary: "已创建 notes.md", Reference: &workspace.Reference{Path: "notes.md", ContentHash: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Kind: "file"},
			Publication: workspace.PublicationCompleted,
		}},
	}
	model := &fakeModel{
		responses: []modelclient.Response{{Message: toolMessage("write-call", workspace.ToolWrite, `{"path":"notes.md","mode":"create","content":"hello"}`)}},
		err:       errors.New("provider failed after publication"),
	}
	session := newWorkspaceTestSession(t, model, executor)
	if err := session.SetFileAuthorizationMode(FileAuthorizationYOLO); err != nil {
		t.Fatal(err)
	}
	result, err := session.Send(t.Context(), "创建文件")
	if err != nil {
		t.Fatalf("completed file effect should return a local fallback, got %v", err)
	}
	if !strings.Contains(result.Text, "文件修改已完成") || len(model.requests) != 2 {
		t.Fatalf("result=%+v requests=%d", result, len(model.requests))
	}
	foundTool, foundFallback := false, false
	for _, message := range session.messages {
		if message.Role == "tool" && message.ToolCallID == "write-call" {
			foundTool = true
		}
		if message.Role == "assistant" && strings.Contains(message.Content, "文件修改已完成") {
			foundFallback = true
		}
	}
	if !foundTool || !foundFallback || session.workspaceReferences["write-call"] == nil {
		t.Fatalf("effect history tool=%t fallback=%t reference=%+v", foundTool, foundFallback, session.workspaceReferences["write-call"])
	}
}

func TestFileMutationCancellationAfterPublicationPreservesEffect(t *testing.T) {
	prepared := &workspace.PreparedMutation{Presentation: workspace.MutationPresentation{
		Tool: workspace.ToolWrite, Operation: "write_create", Path: "notes.md", PreviewKind: "content", Preview: "hello",
	}}
	executor := &fakeWorkspaceExecutor{
		status:   workspace.Status{Available: true, Label: "project"},
		prepared: map[string]*workspace.PreparedMutation{workspace.ToolWrite: prepared},
		commitResults: []workspace.Result{{
			Value:   map[string]any{"path": "notes.md", "content_hash": "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "complete": true, "publication_outcome": "completed"},
			Summary: "已创建 notes.md", Reference: &workspace.Reference{Path: "notes.md", ContentHash: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Kind: "file"},
			Publication: workspace.PublicationCompleted,
		}},
	}
	model := &cancelAfterMutationModel{followupStarted: make(chan struct{})}
	session := newWorkspaceTestSession(t, model, executor)
	if err := session.SetFileAuthorizationMode(FileAuthorizationYOLO); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	type outcome struct {
		result Result
		err    error
	}
	finished := make(chan outcome, 1)
	go func() {
		result, err := session.Send(ctx, "创建文件")
		finished <- outcome{result: result, err: err}
	}()
	select {
	case <-model.followupStarted:
	case <-time.After(time.Second):
		t.Fatal("follow-up model request did not start")
	}
	cancel()
	select {
	case completed := <-finished:
		if completed.err != nil || !strings.Contains(completed.result.Text, "文件修改已完成") {
			t.Fatalf("result=%+v err=%v", completed.result, completed.err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled post-publication turn did not finish")
	}
	if model.requests != 2 || session.workspaceReferences["write-call"] == nil {
		t.Fatalf("requests=%d reference=%+v", model.requests, session.workspaceReferences["write-call"])
	}
}

func TestWorkspaceToolCancellationSkipsSiblingsAndFollowUpModel(t *testing.T) {
	executor := &fakeWorkspaceExecutor{
		status:  workspace.Status{Available: true, Label: "project"},
		started: make(chan string, 1),
		block:   true,
	}
	model := &fakeModel{responses: []modelclient.Response{
		{Message: modelclient.Message{Role: "assistant", ToolCalls: []modelclient.ToolCall{
			{ID: "read-call", Type: "function", Function: modelclient.ToolFunction{Name: workspace.ToolRead, Arguments: `{"path":"notes.md"}`}},
			{ID: "list-call", Type: "function", Function: modelclient.ToolFunction{Name: workspace.ToolList, Arguments: `{}`}},
		}}},
		{Message: modelclient.Message{Role: "assistant", Content: "不应继续调用模型"}},
	}}
	session := newWorkspaceTestSession(t, model, executor)
	var activities []Activity
	activityCtx := WithActivityReporter(t.Context(), func(activity Activity) {
		activities = append(activities, activity)
	})
	ctx, cancel := context.WithCancel(activityCtx)
	result := make(chan error, 1)
	go func() {
		_, err := session.Send(ctx, "读取后列目录")
		result <- err
	}()
	select {
	case tool := <-executor.started:
		if tool != workspace.ToolRead {
			t.Fatalf("first tool=%q", tool)
		}
	case <-time.After(time.Second):
		t.Fatal("workspace read tool did not start")
	}
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("send err=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("workspace cancellation did not finish")
	}
	if !reflect.DeepEqual(executor.calls, []string{workspace.ToolRead}) || len(model.requests) != 1 {
		t.Fatalf("calls=%+v requests=%d", executor.calls, len(model.requests))
	}
	if len(session.workspaceReferences) != 0 {
		t.Fatalf("cancelled workspace result polluted history: %+v", session.workspaceReferences)
	}
	var runningWithPath, stoppedWithPath bool
	for _, activity := range activities {
		if activity.Kind != ActivityTool || activity.Event.ID != "read-call" {
			continue
		}
		if activity.Event.Status == EventRunning && activity.File != nil && activity.File.Path == "notes.md" {
			runningWithPath = true
		}
		if activity.Event.Status == EventFailed && activity.Phase == ActivityStopped && activity.StableCode == workspace.CodeCancelled && activity.File != nil && activity.File.Path == "notes.md" {
			stoppedWithPath = true
		}
	}
	if !runningWithPath || !stoppedWithPath {
		t.Fatalf("cancelled activity missing path or terminal state: %+v", activities)
	}
}

func TestFileMutationCommitCancellationAndTimeoutPublishTerminalActivity(t *testing.T) {
	for _, test := range []struct {
		name     string
		cancel   bool
		wantCode string
		wantErr  error
	}{
		{name: "cancelled", cancel: true, wantCode: workspace.CodeCancelled, wantErr: context.Canceled},
		{name: "timeout", wantCode: workspace.CodeTimeout, wantErr: context.DeadlineExceeded},
	} {
		t.Run(test.name, func(t *testing.T) {
			prepared := &workspace.PreparedMutation{Presentation: workspace.MutationPresentation{
				Tool: workspace.ToolWrite, Operation: "write_create", Path: "notes.md", PreviewKind: "content", Preview: "hello\n",
			}}
			executor := &fakeWorkspaceExecutor{
				status:        workspace.Status{Available: true, Label: "project"},
				prepared:      map[string]*workspace.PreparedMutation{workspace.ToolWrite: prepared},
				commitStarted: make(chan string, 1),
				commitBlock:   true,
			}
			model := &fakeModel{responses: []modelclient.Response{{
				Message: toolMessage("write-call", workspace.ToolWrite, `{"path":"notes.md","mode":"create","content":"hello"}`),
			}}}
			session := newWorkspaceTestSession(t, model, executor)
			if err := session.SetFileAuthorizationMode(FileAuthorizationYOLO); err != nil {
				t.Fatal(err)
			}
			if !test.cancel {
				session.options.ToolTimeout = 20 * time.Millisecond
			}
			var activities []Activity
			activityCtx := WithActivityReporter(t.Context(), func(activity Activity) {
				activities = append(activities, activity)
			})
			ctx := activityCtx
			var cancel context.CancelFunc
			if test.cancel {
				ctx, cancel = context.WithCancel(activityCtx)
				t.Cleanup(cancel)
			}
			finished := make(chan error, 1)
			go func() {
				_, err := session.Send(ctx, "创建文件")
				finished <- err
			}()
			select {
			case tool := <-executor.commitStarted:
				if tool != workspace.ToolWrite {
					t.Fatalf("commit tool=%q", tool)
				}
			case <-time.After(time.Second):
				t.Fatal("workspace mutation commit did not start")
			}
			if cancel != nil {
				cancel()
			}
			select {
			case err := <-finished:
				if !errors.Is(err, test.wantErr) {
					t.Fatalf("send err=%v want=%v", err, test.wantErr)
				}
			case <-time.After(time.Second):
				t.Fatal("workspace mutation stop did not finish")
			}
			if len(model.requests) != 1 || len(session.workspaceReferences) != 0 {
				t.Fatalf("requests=%d references=%+v", len(model.requests), session.workspaceReferences)
			}
			var runningWithPath, stoppedWithPath bool
			for _, activity := range activities {
				if activity.Kind != ActivityTool || activity.Event.ID != "write-call" {
					continue
				}
				if activity.Event.Status == EventRunning && activity.File != nil && activity.File.Path == "notes.md" {
					runningWithPath = true
				}
				if activity.Event.Status == EventFailed && activity.Phase == ActivityStopped && activity.StableCode == test.wantCode &&
					activity.File != nil && activity.File.Path == "notes.md" && activity.File.Operation == "write_create" {
					stoppedWithPath = true
				}
			}
			if !runningWithPath || !stoppedWithPath {
				t.Fatalf("mutation activity missing path or terminal state: %+v", activities)
			}
		})
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

func TestSessionPublishesActualContextTokensAndCumulativeCacheHitRate(t *testing.T) {
	openAIHit, deepSeekHit := 9000, 3000
	model := &fakeModel{responses: []modelclient.Response{
		{
			Message: modelclient.Message{Role: "assistant", Content: "first"},
			Usage: &modelclient.Usage{
				PromptTokens: 12000, CompletionTokens: 10, TotalTokens: 12010,
				PromptTokensDetails: &modelclient.PromptTokensDetails{CachedTokens: &openAIHit},
			},
		},
		{
			Message: modelclient.Message{Role: "assistant", Content: "second"},
			Usage: &modelclient.Usage{
				PromptTokens: 8000, CompletionTokens: 10, TotalTokens: 8010,
				PromptCacheHitTokens: &deepSeekHit,
			},
		},
		{
			Message: modelclient.Message{Role: "assistant", Content: "third"},
			Usage:   &modelclient.Usage{PromptTokens: 5000, CompletionTokens: 10, TotalTokens: 5010},
		},
	}}
	session := newTestSession(t, model, &fakeServer{})
	for _, input := range []string{"first question", "second question", "third question"} {
		if _, err := session.Send(t.Context(), input); err != nil {
			t.Fatal(err)
		}
	}
	status := session.ContextStatus()
	if status.Estimated || status.CurrentTokens != 5000 || status.ContextWindow != 32768 || status.WindowPercent != 16 ||
		status.CachePromptTokens != 20000 || status.CacheReadTokens != 12000 || !status.CacheHitRateAvailable || status.CacheHitRate != 60 {
		t.Fatalf("context status=%+v", status)
	}
	found := false
drain:
	for {
		select {
		case event := <-session.ContextUpdates():
			if event.Kind == ContextEventStatus && event.Status.CurrentTokens == 5000 && event.Status.CacheHitRate == 60 {
				found = true
			}
		default:
			break drain
		}
	}
	if !found {
		t.Fatal("actual token and cumulative cache status event was not published")
	}
}

func TestSessionIncludesCacheCreationMissInCumulativeRate(t *testing.T) {
	cacheCreation, cacheHit := 8000, 9000
	model := &fakeModel{responses: []modelclient.Response{
		{
			Message: modelclient.Message{Role: "assistant", Content: "cold"},
			Usage: &modelclient.Usage{
				PromptTokens: 12000, CompletionTokens: 10, TotalTokens: 12010,
				PromptTokensDetails: &modelclient.PromptTokensDetails{CacheCreationTokens: &cacheCreation},
			},
		},
		{
			Message: modelclient.Message{Role: "assistant", Content: "warm"},
			Usage: &modelclient.Usage{
				PromptTokens: 12000, CompletionTokens: 10, TotalTokens: 12010,
				PromptTokensDetails: &modelclient.PromptTokensDetails{CachedTokens: &cacheHit},
			},
		},
	}}
	session := newTestSession(t, model, &fakeServer{})
	for _, input := range []string{"cold question", "warm question"} {
		if _, err := session.Send(t.Context(), input); err != nil {
			t.Fatal(err)
		}
	}
	status := session.ContextStatus()
	if status.CachePromptTokens != 24000 || status.CacheReadTokens != 9000 || !status.CacheHitRateAvailable || status.CacheHitRate != 37.5 {
		t.Fatalf("cache creation miss was not included in cumulative rate: %+v", status)
	}
}

func TestSessionPublishesExplicitZeroCacheHitRate(t *testing.T) {
	zeroCacheHit := 0
	model := &fakeModel{responses: []modelclient.Response{{
		Message: modelclient.Message{Role: "assistant", Content: "uncached"},
		Usage: &modelclient.Usage{
			PromptTokens: 4000, CompletionTokens: 10, TotalTokens: 4010,
			PromptTokensDetails: &modelclient.PromptTokensDetails{CachedTokens: &zeroCacheHit},
		},
	}}}
	session := newTestSession(t, model, &fakeServer{})
	if _, err := session.Send(t.Context(), "uncached question"); err != nil {
		t.Fatal(err)
	}
	status := session.ContextStatus()
	if status.Estimated || status.CurrentTokens != 4000 || status.CachePromptTokens != 4000 || status.CacheReadTokens != 0 ||
		!status.CacheHitRateAvailable || status.CacheHitRate != 0 {
		t.Fatalf("context status=%+v", status)
	}
	found := false
drain:
	for {
		select {
		case event := <-session.ContextUpdates():
			if event.Kind == ContextEventStatus && event.Status.CachePromptTokens == 4000 &&
				event.Status.CacheHitRateAvailable && event.Status.CacheHitRate == 0 {
				found = true
			}
		default:
			break drain
		}
	}
	if !found {
		t.Fatal("explicit zero cache-hit status event was not published")
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
	if len(activities) < 9 {
		t.Fatalf("activities=%+v", activities)
	}
	phases := make(map[ActivityPhase]bool)
	toolRunning, toolDone := false, false
	firstThinkingID, secondThinkingID := "", ""
	for _, activity := range activities {
		if activity.StartedAt.IsZero() || activity.UpdatedAt.IsZero() || activity.UpdatedAt.Before(activity.StartedAt) {
			t.Fatalf("activity timestamps=%+v", activity)
		}
		phases[activity.Phase] = true
		if activity.Event.ID == "call-1" {
			if activity.Event.Tool != "search_knowledge" || strings.Contains(activity.Event.Summary, "图论") {
				t.Fatalf("tool lifecycle leaked arguments or lost identity: %+v", activity)
			}
			toolRunning = toolRunning || activity.Event.Status == EventRunning
			toolDone = toolDone || activity.Event.Status == EventSucceeded
		}
		if activity.Kind == ActivityThinking && activity.Event.Summary == "正在分析问题" {
			firstThinkingID = activity.Event.ID
		}
		if activity.Kind == ActivityThinking && activity.Event.Summary == "正在结合工具结果继续分析" && strings.HasPrefix(activity.Event.ID, "thinking-") {
			secondThinkingID = activity.Event.ID
		}
	}
	for _, phase := range []ActivityPhase{ActivityPreparingContext, ActivityWaitingModel, ActivityValidatingResponse, ActivityAssemblingTools, ActivityExecutingTool, ActivityContinuingAfterTool} {
		if !phases[phase] {
			t.Fatalf("missing phase %s: %+v", phase, activities)
		}
	}
	if !toolRunning || !toolDone || firstThinkingID == "" || secondThinkingID == "" || firstThinkingID == secondThinkingID {
		t.Fatalf("lifecycle incomplete: running=%t done=%t first=%q second=%q activities=%+v", toolRunning, toolDone, firstThinkingID, secondThinkingID, activities)
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
	result, err = session.ResolvePreference(t.Context(), PreferenceSave)
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
	result, err = session.ResolvePreference(t.Context(), PreferenceDecline)
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
	result, err = session.ResolvePreference(t.Context(), PreferenceSave)
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
	if _, err := session.ResolvePreference(t.Context(), PreferenceSave); err == nil || errors.Is(err, ErrPreferenceOutcomeUnknown) || !strings.Contains(err.Error(), "forbidden") {
		t.Fatalf("deterministic rejection err=%v", err)
	}
	result, err = session.ResolvePreference(t.Context(), PreferenceDecline)
	if err != nil || result.Pending != nil || !strings.Contains(result.Text, "没有保存") || server.createCalls != 1 {
		t.Fatalf("decline result=%+v calls=%d err=%v", result, server.createCalls, err)
	}
}

func TestPreferencePendingCandidateAdmissionFailureIsCompensatedBeforeLocalChoice(t *testing.T) {
	arguments := `{"content":"回答时先给结论","reason":"用户明确要求长期保持回答风格","category":"interaction_preference","sensitivity":"non_sensitive","stability":"stable"}`
	model := &fakeModel{responses: []modelclient.Response{
		{Message: toolMessage("call-pref", "remember_preference", arguments)},
		{Message: modelclient.Message{Role: "assistant", Content: "本次只在当前会话使用。"}},
	}}
	server := &fakeServer{decisionErrors: []error{&api.APIError{Code: "admission_forbidden", Status: 403}, nil}}
	session := newTestSession(t, model, server)
	result, err := session.Send(t.Context(), "请长期记住")
	if err != nil || result.Pending == nil {
		t.Fatalf("send result=%+v err=%v", result, err)
	}
	if _, err := session.ResolvePreference(t.Context(), PreferenceSave); err == nil || errors.Is(err, ErrPreferenceOutcomeUnknown) || !strings.Contains(err.Error(), "admission_forbidden") {
		t.Fatalf("compensated deterministic error=%v", err)
	}
	if server.createCalls != 1 || server.decisionCalls != 2 || len(server.decisionRequests) != 2 {
		t.Fatalf("writes create=%d decisions=%d requests=%+v", server.createCalls, server.decisionCalls, server.decisionRequests)
	}
	admit, reject := server.decisionRequests[0], server.decisionRequests[1]
	if admit.Decision != "admit" || reject.Decision != "reject" || reject.ExpectedRevision != admit.ExpectedRevision ||
		admit.OperationID == reject.OperationID || server.createRequests[0].OperationID == reject.OperationID {
		t.Fatalf("admit=%+v reject=%+v create=%+v", admit, reject, server.createRequests[0])
	}
	if session.pendingCandidateID != "" || session.pendingRejectOperationID != "" || session.pendingOperationID != "" || session.pendingDecisionOperationID != "" {
		t.Fatalf("compensated write state not cleared: candidate=%q create=%q admit=%q reject=%q", session.pendingCandidateID, session.pendingOperationID, session.pendingDecisionOperationID, session.pendingRejectOperationID)
	}
	result, err = session.ResolvePreference(t.Context(), PreferenceSessionOnly)
	if err != nil || !strings.Contains(result.Text, "当前会话") {
		t.Fatalf("local choice was not restored: result=%+v err=%v", result, err)
	}
}

func TestPreferenceCompensationUnknownKeepsRetryOnlyStateAndReusesRejectOperationID(t *testing.T) {
	arguments := `{"content":"回答时先给结论","reason":"用户明确要求长期保持回答风格","category":"interaction_preference","sensitivity":"non_sensitive","stability":"stable"}`
	model := &fakeModel{responses: []modelclient.Response{
		{Message: toolMessage("call-pref", "remember_preference", arguments)},
		{Message: modelclient.Message{Role: "assistant", Content: "本次只在当前会话使用。"}},
	}}
	server := &fakeServer{decisionErrors: []error{
		&api.APIError{Code: "admission_forbidden", Status: 403},
		errors.New("reject response lost"),
		nil,
	}}
	session := newTestSession(t, model, server)
	result, err := session.Send(t.Context(), "请长期记住")
	if err != nil || result.Pending == nil {
		t.Fatalf("send result=%+v err=%v", result, err)
	}
	if _, err := session.ResolvePreference(t.Context(), PreferenceSave); !errors.Is(err, ErrPreferenceOutcomeUnknown) {
		t.Fatalf("unknown compensation err=%v", err)
	}
	createID, admitID, rejectID := session.pendingOperationID, session.pendingDecisionOperationID, session.pendingRejectOperationID
	if session.pendingCandidateID != "candidate-1" || session.pendingCandidateRevision != 1 || createID == "" || admitID == "" || rejectID == "" {
		t.Fatalf("retry state candidate=%q revision=%d create=%q admit=%q reject=%q", session.pendingCandidateID, session.pendingCandidateRevision, createID, admitID, rejectID)
	}
	if _, err := session.ResolvePreference(t.Context(), PreferenceDecline); !errors.Is(err, ErrPreferenceOutcomeUnknown) {
		t.Fatalf("unknown compensation allowed decline: %v", err)
	}
	if _, err := session.ResolvePreference(t.Context(), PreferenceRetry); err == nil || errors.Is(err, ErrPreferenceOutcomeUnknown) || !strings.Contains(err.Error(), "admission_forbidden") {
		t.Fatalf("compensation retry did not return original deterministic failure: %v", err)
	}
	if server.createCalls != 1 || server.decisionCalls != 3 || len(server.decisionRequests) != 3 {
		t.Fatalf("retry duplicated stages: create=%d decisions=%d requests=%+v", server.createCalls, server.decisionCalls, server.decisionRequests)
	}
	if server.decisionRequests[0].OperationID != admitID || server.decisionRequests[1].OperationID != rejectID || server.decisionRequests[2].OperationID != rejectID ||
		server.decisionRequests[1].Decision != "reject" || server.decisionRequests[2].Decision != "reject" {
		t.Fatalf("operation IDs changed across compensation retry: %+v", server.decisionRequests)
	}
	if session.pendingCandidateID != "" || session.pendingRejectOperationID != "" || session.pendingOperationID != "" || session.pendingDecisionOperationID != "" {
		t.Fatalf("resolved compensation state not cleared: candidate=%q create=%q admit=%q reject=%q", session.pendingCandidateID, session.pendingOperationID, session.pendingDecisionOperationID, session.pendingRejectOperationID)
	}
	result, err = session.ResolvePreference(t.Context(), PreferenceSessionOnly)
	if err != nil || !strings.Contains(result.Text, "当前会话") {
		t.Fatalf("local choices did not recover after verified rejection: result=%+v err=%v", result, err)
	}
	if turn := session.turns[session.currentTurnID]; turn == nil || turn.Protected || turn.OutcomeUnknown || !turn.Completed {
		t.Fatalf("reconciled preference turn remained protected or unknown: %+v", turn)
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
				"60000000-0000-4000-8000-000000000005",
				"60000000-0000-4000-8000-000000000006",
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
	if _, err := session.ResolvePreference(t.Context(), PreferenceSave); !errors.Is(err, ErrPreferenceOutcomeUnknown) {
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
	if _, err := session.ResolvePreference(t.Context(), PreferenceDecline); !errors.Is(err, ErrPreferenceOutcomeUnknown) {
		t.Fatalf("ambiguous write allowed decline: %v", err)
	}
	result, err = session.ResolvePreference(t.Context(), PreferenceRetry)
	if err != nil || result.Pending != nil || server.createCalls != 2 || server.decisionCalls != 1 || uuidCalls != 3 {
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
	if _, err := session.ResolvePreference(t.Context(), PreferenceSave); err != nil {
		t.Fatal(err)
	}
	if uuidCalls != 6 || len(server.createRequests) != 3 || len(server.decisionRequests) != 2 ||
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
	if _, err := session.ResolvePreference(t.Context(), PreferenceSave); !errors.Is(err, ErrPreferenceOutcomeUnknown) {
		t.Fatalf("first decision err=%v", err)
	}
	if _, err := session.ResolvePreference(t.Context(), PreferenceDecline); !errors.Is(err, ErrPreferenceOutcomeUnknown) {
		t.Fatalf("ambiguous decision allowed decline: %v", err)
	}
	result, err = session.ResolvePreference(t.Context(), PreferenceRetry)
	if err != nil || result.Pending != nil || server.createCalls != 2 || server.decisionCalls != 2 {
		t.Fatalf("retry result=%+v create=%d decision=%d err=%v", result, server.createCalls, server.decisionCalls, err)
	}
	if len(server.createRequests) != 2 || server.createRequests[0].OperationID != server.createRequests[1].OperationID ||
		len(server.decisionRequests) != 2 || server.decisionRequests[0].OperationID != server.decisionRequests[1].OperationID {
		t.Fatalf("operation IDs changed create=%+v decision=%+v", server.createRequests, server.decisionRequests)
	}
}

func TestSessionAcceptsLargeToolCallGroupAndRejectsOversizedCurrentGroup(t *testing.T) {
	calls := make([]modelclient.ToolCall, 64)
	for index := range calls {
		calls[index] = modelclient.ToolCall{ID: fmt.Sprintf("call-%d", index), Type: "function", Function: modelclient.ToolFunction{Name: "get_learning_progress", Arguments: `{}`}}
	}
	model := &fakeModel{responses: []modelclient.Response{
		{Message: modelclient.Message{Role: "assistant", ToolCalls: calls}},
		{Message: modelclient.Message{Role: "assistant", Content: "已处理全部工具结果"}},
	}}
	server := &fakeServer{}
	uuidCalls := 0
	session, err := New(model, server, Options{
		ContextWindow: 1_000_000, MaxToolRounds: 0, Now: time.Now,
		NewUUID: func() (string, error) {
			uuidCalls++
			return fmt.Sprintf("61000000-0000-4000-8000-%012d", uuidCalls), nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := session.Send(t.Context(), "读取大量工具结果")
	if err != nil {
		t.Fatalf("large tool-call group failed: %v", err)
	}
	if result.Text != "已处理全部工具结果" || server.currentCalls != len(calls) {
		t.Fatalf("result=%+v toolCalls=%d", result, server.currentCalls)
	}
	if session.currentToolResultShares != len(calls) {
		t.Fatalf("tool result budget shares=%d want=%d", session.currentToolResultShares, len(calls))
	}

	session, err = New(&fakeModel{}, server, Options{
		ContextWindow: 4096, MaxToolRounds: 0, Now: time.Now,
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

func TestWorkspaceProjectionSharesMinimumContextBudgetAcrossFourCalls(t *testing.T) {
	const hash = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	executor := &fakeWorkspaceExecutor{
		status: workspace.Status{Available: true, Label: "project"},
		results: map[string]workspace.Result{
			workspace.ToolList: {
				Value:   map[string]any{"path": ".", "entries": []any{map[string]any{"path": "alpha.txt", "type": "file"}, map[string]any{"path": "beta.txt", "type": "file"}}, "returned": 2, "complete": false, "next_offset": 2, "truncation_reason": "entry_limit"},
				Summary: "已列出工作区", Reference: &workspace.Reference{Path: ".", ContentHash: hash, Kind: "directory_listing"},
			},
			workspace.ToolRead: {
				Value:   map[string]any{"path": "notes.md", "content": strings.Repeat("工作区内容", 1200), "content_hash": hash, "complete": false, "next_offset": 20, "next_byte_offset": 0, "truncation_reason": "result_bytes"},
				Summary: "已读取 notes.md", Reference: &workspace.Reference{Path: "notes.md", ContentHash: hash, Kind: "file"},
			},
			workspace.ToolSearch: {
				Value:   map[string]any{"path": ".", "matches": []any{map[string]any{"path": "notes.md", "line": 3, "column": 2, "preview": strings.Repeat("匹配", 500)}}, "returned": 1, "scanned_files": 30, "scanned_bytes": 9000, "complete": false, "truncation_reason": "result_bytes"},
				Summary: "已搜索工作区", Reference: &workspace.Reference{Path: ".", ContentHash: hash, Kind: "search_result"},
			},
		},
	}
	calls := []modelclient.ToolCall{
		{ID: "list-call", Type: "function", Function: modelclient.ToolFunction{Name: workspace.ToolList, Arguments: `{}`}},
		{ID: "read-one", Type: "function", Function: modelclient.ToolFunction{Name: workspace.ToolRead, Arguments: `{"path":"notes.md"}`}},
		{ID: "search-call", Type: "function", Function: modelclient.ToolFunction{Name: workspace.ToolSearch, Arguments: `{"query":"工作区"}`}},
		{ID: "read-two", Type: "function", Function: modelclient.ToolFunction{Name: workspace.ToolRead, Arguments: `{"path":"notes.md","offset":20}`}},
	}
	model := &fakeModel{responses: []modelclient.Response{
		{Message: modelclient.Message{Role: "assistant", ToolCalls: calls}},
		{Message: modelclient.Message{Role: "assistant", Content: "已结合四个结果。"}},
	}}
	uuidCalls := 0
	session, err := New(model, &fakeServer{}, Options{
		ContextWindow: 4096, MaxToolRounds: 8, Workspace: executor,
		Now: func() time.Time { return time.Date(2026, 8, 29, 0, 0, 0, 0, time.UTC) },
		NewUUID: func() (string, error) {
			uuidCalls++
			return fmt.Sprintf("62000000-0000-4000-8000-%012d", uuidCalls), nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := session.Send(t.Context(), "读取并搜索工作区"); err != nil {
		t.Fatal(err)
	}
	if len(model.requests) != 2 {
		t.Fatalf("requests=%d", len(model.requests))
	}
	if model.requests[1].MaxTokens != 512 {
		t.Fatalf("minimum-context workspace output reserve=%d", model.requests[1].MaxTokens)
	}
	contents := map[string]string{}
	for _, message := range model.requests[1].Messages {
		if message.Role == "tool" {
			if !json.Valid([]byte(message.Content)) {
				t.Fatalf("invalid projection for %s: %s", message.ToolCallID, message.Content)
			}
			contents[message.ToolCallID] = message.Content
		}
	}
	for callID, required := range map[string][]string{
		"list-call":   {`"entries"`, `"next_offset":2`, `"path":"."`},
		"read-one":    {`"content"`, `"content_hash"`, `"next_offset":20`},
		"search-call": {`"matches"`, `"scanned_files":30`, `"path":"."`},
		"read-two":    {`"content"`, `"next_byte_offset":0`, `"truncation_reason"`},
	} {
		content := contents[callID]
		for _, fragment := range required {
			if !strings.Contains(content, fragment) {
				t.Fatalf("%s projection lost %s: %s", callID, fragment, content)
			}
		}
	}
	if session.currentToolResultTokens > session.currentToolResultBudget {
		t.Fatalf("tool tokens=%d budget=%d", session.currentToolResultTokens, session.currentToolResultBudget)
	}
}

func TestWorkspaceProjectionRetainsMutationPreviewAndFailureRecovery(t *testing.T) {
	session := newWorkspaceTestSession(t, &fakeModel{}, &fakeWorkspaceExecutor{status: workspace.Status{Available: true, Label: "project"}})
	if _, err := session.startTurn(); err != nil {
		t.Fatal(err)
	}
	const hash = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	results := []struct {
		tool   string
		callID string
		result workspace.Result
		want   []string
	}{
		{workspace.ToolWrite, "write", workspace.Result{Value: map[string]any{"path": "notes.md", "operation": "write_replace", "content_hash": hash, "complete": true, "publication_outcome": "completed", "preview": strings.Repeat("+新增内容\n", 400), "preview_kind": "diff", "first_changed_line": 7}, Summary: "已替换 notes.md", Publication: workspace.PublicationCompleted}, []string{`"preview"`, `"operation":"write_replace"`, `"first_changed_line":7`, `"publication_outcome":"completed"`}},
		{workspace.ToolEdit, "edit", workspace.Result{Value: map[string]any{"error": workspace.CodeReplacementNotUnique, "code": workspace.CodeReplacementNotUnique, "complete": false, "path": "notes.md", "expected_hash": hash, "message": "文件编辑目标文本不唯一", "suggestion": "扩大 old_text 上下文使其只匹配一次"}, Summary: "文件编辑目标文本不唯一", Publication: workspace.PublicationUnchanged}, []string{workspace.CodeReplacementNotUnique, `"message"`, `"suggestion"`, `"expected_hash"`}},
	}
	for _, test := range results {
		if err := session.appendWorkspaceToolResult(test.tool, test.callID, test.result); err != nil {
			t.Fatal(err)
		}
		content := session.messages[len(session.messages)-1].Content
		for _, fragment := range test.want {
			if !strings.Contains(content, fragment) {
				t.Fatalf("%s projection lost %s: %s", test.tool, fragment, content)
			}
		}
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
			return fmt.Sprintf("60000000-0000-4000-8000-%012d", uuidCalls), nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return session
}

func newWorkspaceTestSession(t *testing.T, model Model, executor workspace.Executor) *Session {
	t.Helper()
	uuidCalls := 0
	session, err := New(model, &fakeServer{}, Options{
		ContextWindow: 32768, MaxToolRounds: 8, Workspace: executor,
		Now: func() time.Time { return time.Date(2026, 8, 29, 0, 0, 0, 0, time.UTC) },
		NewUUID: func() (string, error) {
			uuidCalls++
			return fmt.Sprintf("61000000-0000-4000-8000-%012d", uuidCalls), nil
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
