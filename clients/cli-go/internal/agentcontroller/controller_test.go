package agentcontroller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/agentloop"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/agentsession"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/api"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/fileeffects"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/filelock"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/keybackend"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/modelclient"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/workspace"
)

type controllerSecretBackend struct {
	mu    sync.Mutex
	value []byte
}

func (*controllerSecretBackend) Available(keybackend.Locator) error { return nil }
func (b *controllerSecretBackend) Load(keybackend.Locator, int) ([]byte, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.value) == 0 {
		return nil, keybackend.ErrNotFound
	}
	return append([]byte(nil), b.value...), nil
}
func (b *controllerSecretBackend) Store(_ keybackend.Locator, value []byte) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.value = append([]byte(nil), value...)
	return nil
}
func (b *controllerSecretBackend) Delete(keybackend.Locator) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.value = nil
	return nil
}

type controllerModel struct {
	mu       sync.Mutex
	requests []modelclient.Request
}

func (m *controllerModel) Complete(_ context.Context, request modelclient.Request) (modelclient.Response, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.requests = append(m.requests, request)
	if len(request.Messages) > 0 && request.Messages[0].Role == "system" && request.Messages[0].Content == "为这段学习对话生成简洁自然的中文会话标题。只输出单行JSON：{\"title\":\"...\"}。不得包含换行、工具信息、路径、错误或推理。" {
		return modelclient.Response{Message: modelclient.Message{Role: "assistant", Content: `{"title":"代数复习"}`}}, nil
	}
	return modelclient.Response{Message: modelclient.Message{Role: "assistant", Content: "已回答"}}, nil
}

type toolTitleModel struct {
	mu       sync.Mutex
	requests []modelclient.Request
}

func (m *toolTitleModel) Complete(_ context.Context, request modelclient.Request) (modelclient.Response, error) {
	m.mu.Lock()
	m.requests = append(m.requests, request)
	m.mu.Unlock()
	if len(request.Messages) > 0 && request.Messages[0].Role == "system" && strings.Contains(request.Messages[0].Content, "会话标题") {
		return modelclient.Response{Message: modelclient.Message{Role: "assistant", Content: `{"title":"代数复习"}`}}, nil
	}
	if len(request.Tools) == 1 {
		return modelclient.Response{Message: modelclient.Message{Role: "assistant"}}, nil
	}
	if len(request.Messages) > 0 && request.Messages[len(request.Messages)-1].Role == "tool" {
		return modelclient.Response{Message: modelclient.Message{Role: "assistant", Content: "工具结果后的回答"}}, nil
	}
	return modelclient.Response{Message: modelclient.Message{Role: "assistant", ToolCalls: []modelclient.ToolCall{{
		ID: "title-tool-call", Type: "function", Function: modelclient.ToolFunction{Name: "search_knowledge", Arguments: `{"query":"图论"}`},
	}}}}, nil
}

type generationTitleModel struct {
	titleStarted chan struct{}
	releaseTitle chan struct{}
}

func isTitleRequest(request modelclient.Request) bool {
	return len(request.Messages) > 0 && request.Messages[0].Role == "system" && strings.Contains(request.Messages[0].Content, "会话标题")
}

type titleSchedulerModel struct {
	mu            sync.Mutex
	titleCalls    int
	activeTitles  int
	maxConcurrent int
	started       chan int
	release       chan struct{}
}

func (m *titleSchedulerModel) Complete(ctx context.Context, request modelclient.Request) (modelclient.Response, error) {
	if !isTitleRequest(request) {
		return modelclient.Response{Message: modelclient.Message{Role: "assistant", Content: "主对话回答"}}, nil
	}
	m.mu.Lock()
	m.titleCalls++
	call := m.titleCalls
	m.activeTitles++
	if m.activeTitles > m.maxConcurrent {
		m.maxConcurrent = m.activeTitles
	}
	m.mu.Unlock()
	m.started <- call
	select {
	case <-ctx.Done():
		m.mu.Lock()
		m.activeTitles--
		m.mu.Unlock()
		return modelclient.Response{}, ctx.Err()
	case <-m.release:
		m.mu.Lock()
		m.activeTitles--
		m.mu.Unlock()
		return modelclient.Response{Message: modelclient.Message{Role: "assistant", Content: fmt.Sprintf(`{"title":"标题%d"}`, call)}}, nil
	}
}

type timeoutThenSuccessTitleModel struct {
	mu         sync.Mutex
	titleCalls int
	started    chan int
	canceled   chan struct{}
}

func (m *timeoutThenSuccessTitleModel) Complete(ctx context.Context, request modelclient.Request) (modelclient.Response, error) {
	if !isTitleRequest(request) {
		return modelclient.Response{Message: modelclient.Message{Role: "assistant", Content: "主对话回答"}}, nil
	}
	m.mu.Lock()
	m.titleCalls++
	call := m.titleCalls
	m.mu.Unlock()
	m.started <- call
	if call == 1 {
		<-ctx.Done()
		close(m.canceled)
		return modelclient.Response{}, ctx.Err()
	}
	return modelclient.Response{Message: modelclient.Message{Role: "assistant", Content: `{"title":"恢复成功"}`}}, nil
}

type failingTitleModel struct {
	titleContent string
	titleErr     error
}

func (m failingTitleModel) Complete(_ context.Context, request modelclient.Request) (modelclient.Response, error) {
	if isTitleRequest(request) {
		return modelclient.Response{Message: modelclient.Message{Role: "assistant", Content: m.titleContent}}, m.titleErr
	}
	return modelclient.Response{Message: modelclient.Message{Role: "assistant", Content: "主对话继续"}}, nil
}

func waitForTitleCalls(t *testing.T, started <-chan int, want int) {
	t.Helper()
	select {
	case got := <-started:
		if got != want {
			t.Fatalf("title call=%d, want %d", got, want)
		}
	case <-time.After(time.Second):
		t.Fatalf("title call %d did not start", want)
	}
}

func (m *generationTitleModel) Complete(_ context.Context, request modelclient.Request) (modelclient.Response, error) {
	if len(request.Messages) > 0 && request.Messages[0].Role == "system" && strings.Contains(request.Messages[0].Content, "会话标题") {
		select {
		case m.titleStarted <- struct{}{}:
		default:
		}
		<-m.releaseTitle
		return modelclient.Response{Message: modelclient.Message{Role: "assistant", Content: `{"title":"旧会话迟到标题"}`}}, nil
	}
	return modelclient.Response{Message: modelclient.Message{Role: "assistant", Content: "已回答"}}, nil
}

type switchLockServer struct {
	controllerServer
	root    string
	enabled bool
	lock    *filelock.Lock
}

func (s *switchLockServer) ExportMemory(ctx context.Context, cursor string, limit int) (api.MemoryExportPage, error) {
	if s.enabled && s.lock == nil {
		lock, err := filelock.Acquire(ctx, filepath.Join(s.root, "profile.lock"), filelock.Exclusive, time.Second)
		if err != nil {
			return api.MemoryExportPage{}, err
		}
		s.lock = lock
	}
	return s.controllerServer.ExportMemory(ctx, cursor, limit)
}

func (s *switchLockServer) release() {
	if s.lock != nil {
		_ = s.lock.Close()
		s.lock = nil
	}
}

type controllerServer struct {
	generation       api.MemoryGenerationStamp
	exportErr        error
	retrieveResult   api.KnowledgeRetrievalResult
	retrieveErr      error
	candidate        api.MemoryCandidate
	createErr        error
	decisionErr      error
	createRequests   []api.MemoryCandidateRequest
	decisionRequests []api.MemoryCandidateDecisionRequest
}

func (s *controllerServer) RetrieveKnowledge(_ context.Context, _ api.KnowledgeRetrievalRequest) (api.KnowledgeRetrievalResult, error) {
	if s.retrieveErr != nil {
		return api.KnowledgeRetrievalResult{}, s.retrieveErr
	}
	return s.retrieveResult, nil
}
func (*controllerServer) CurrentSession(context.Context) (api.SessionView, error) {
	return api.SessionView{}, errors.New("not found")
}
func (*controllerServer) Reviews(context.Context, string, int, *time.Time) (api.ReviewsPage, error) {
	return api.ReviewsPage{}, nil
}
func (s *controllerServer) ExportMemory(context.Context, string, int) (api.MemoryExportPage, error) {
	if s.exportErr != nil {
		return api.MemoryExportPage{}, s.exportErr
	}
	return api.MemoryExportPage{ReadGeneration: s.generation}, nil
}
func (s *controllerServer) MemoryCandidate(_ context.Context, candidateID string) (api.MemoryCandidateView, error) {
	candidate := s.candidate
	if candidate.ID == "" {
		candidate = api.MemoryCandidate{ID: candidateID, Status: "pending_review", Revision: 1}
	}
	return api.MemoryCandidateView{Candidate: candidate}, nil
}
func (s *controllerServer) CreateMemoryCandidate(_ context.Context, request api.MemoryCandidateRequest) (api.MemoryOperationResponse, error) {
	s.createRequests = append(s.createRequests, request)
	if s.createErr != nil {
		return api.MemoryOperationResponse{}, s.createErr
	}
	candidate := s.candidate
	if candidate.ID == "" {
		candidate = api.MemoryCandidate{ID: "candidate-1", Status: "pending_review", Revision: 1}
		s.candidate = candidate
	}
	return api.MemoryOperationResponse{Candidate: &api.MemoryCandidateView{Candidate: candidate}}, nil
}
func (s *controllerServer) DecideMemoryCandidate(_ context.Context, candidateID string, request api.MemoryCandidateDecisionRequest) (api.MemoryOperationResponse, error) {
	s.decisionRequests = append(s.decisionRequests, request)
	if s.decisionErr != nil {
		return api.MemoryOperationResponse{}, s.decisionErr
	}
	status := "admitted"
	if request.Decision == "reject" {
		status = "rejected"
	}
	candidate := s.candidate
	candidate.ID = candidateID
	candidate.Status = status
	candidate.Revision = max(candidate.Revision, request.ExpectedRevision) + 1
	s.candidate = candidate
	return api.MemoryOperationResponse{Candidate: &api.MemoryCandidateView{Candidate: candidate}}, nil
}

type scriptedControllerModel struct {
	mu        sync.Mutex
	responses []modelclient.Response
}

func (m *scriptedControllerModel) Complete(_ context.Context, request modelclient.Request) (modelclient.Response, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.responses) == 0 {
		return modelclient.Response{}, errors.New("no scripted response")
	}
	response := m.responses[0]
	m.responses = m.responses[1:]
	return response, nil
}

type controllerWorkspaceExecutor struct {
	status         workspace.Status
	executeResults []workspace.Result
	prepared       map[string]*workspace.PreparedMutation
	commitResults  []workspace.Result
}

func (w *controllerWorkspaceExecutor) Definitions() []modelclient.Tool {
	return workspace.Definitions()
}
func (w *controllerWorkspaceExecutor) Execute(_ context.Context, _, _ string) workspace.Result {
	if len(w.executeResults) > 0 {
		result := w.executeResults[0]
		w.executeResults = w.executeResults[1:]
		return result
	}
	return workspace.Result{Value: map[string]any{"error": workspace.CodeInvalidArguments, "code": workspace.CodeInvalidArguments}, Publication: workspace.PublicationUnchanged}
}
func (w *controllerWorkspaceExecutor) PrepareMutation(_ context.Context, toolName, _ string) (*workspace.PreparedMutation, workspace.Result) {
	if prepared := w.prepared[toolName]; prepared != nil {
		return prepared, workspace.Result{}
	}
	return nil, workspace.Result{Value: map[string]any{"error": workspace.CodeInvalidArguments, "code": workspace.CodeInvalidArguments}, Publication: workspace.PublicationUnchanged}
}
func (w *controllerWorkspaceExecutor) CommitMutation(_ context.Context, _ *workspace.PreparedMutation) workspace.Result {
	if len(w.commitResults) == 0 {
		return workspace.Result{Value: map[string]any{"error": workspace.CodeInternalError, "code": workspace.CodeInternalError}, Publication: workspace.PublicationUnchanged}
	}
	result := w.commitResults[0]
	w.commitResults = w.commitResults[1:]
	return result
}
func (w *controllerWorkspaceExecutor) Status() workspace.Status { return w.status }
func (*controllerWorkspaceExecutor) Close() error               { return nil }

type lockingControllerModel struct {
	root string
	lock *filelock.Lock
}

func (m *lockingControllerModel) Complete(context.Context, modelclient.Request) (modelclient.Response, error) {
	lock, err := filelock.Acquire(context.Background(), filepath.Join(m.root, "profile.lock"), filelock.Exclusive, time.Second)
	if err != nil {
		return modelclient.Response{}, err
	}
	m.lock = lock
	return modelclient.Response{Message: modelclient.Message{Role: "assistant", Content: "锁冲突前已提交的回答"}}, nil
}

func controllerStore(t *testing.T, root string, secrets agentsession.SecretBackend) *agentsession.Store {
	return controllerStoreWithLimits(t, root, secrets, agentsession.Limits{})
}

func controllerStoreWithLimits(t *testing.T, root string, secrets agentsession.SecretBackend, limits agentsession.Limits) *agentsession.Store {
	t.Helper()
	profile, err := agentsession.ProfileFingerprint("https://server.example/api")
	if err != nil {
		t.Fatal(err)
	}
	store, err := agentsession.Open(t.Context(), agentsession.Options{Root: root, ProfileFingerprint: profile, Secrets: secrets, Limits: limits})
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func controllerDependencies(store *agentsession.Store, model agentloop.Model, server agentloop.Server, workspaceRoot string, provider Provider) Dependencies {
	sequence := 0
	return Dependencies{
		Store: store, Model: model, Server: server, Provider: provider, WorkspaceRoot: workspaceRoot,
		LoopOptions: agentloop.Options{
			ContextWindow: 8192, MaxToolRounds: 4, ContextCompaction: agentloop.ContextCompactionAuto,
			ReasoningEffort: modelclient.ReasoningEffortAuto, ModelTimeout: time.Second, ToolTimeout: time.Second,
			NewUUID: func() (string, error) {
				sequence++
				return "10000000-0000-4000-8000-" + []string{"000000000001", "000000000002", "000000000003", "000000000004"}[(sequence-1)%4], nil
			},
		},
	}
}

func TestControllerRemovesEmptyPersistentSessionOnNormalClose(t *testing.T) {
	root, workspaceRoot := t.TempDir(), t.TempDir()
	secrets := &controllerSecretBackend{}
	model := &controllerModel{}
	server := &controllerServer{generation: api.MemoryGenerationStamp{LearnerGeneration: 1, MemoryGeneration: 1}}
	provider := Provider{Name: "ollama", Endpoint: "http://127.0.0.1:11434/v1", Model: "local"}

	store := controllerStore(t, root, secrets)
	controller, err := Start(t.Context(), controllerDependencies(store, model, server, workspaceRoot, provider), false)
	if err != nil {
		t.Fatal(err)
	}
	controller.Close()

	store = controllerStore(t, root, secrets)
	defer store.Close()
	listed, err := store.List(t.Context())
	if err != nil || len(listed) != 0 {
		t.Fatalf("empty session remained listed: %+v err=%v", listed, err)
	}
}

func TestControllerShutdownRetriesFailedPublicationWithCommittedTranscript(t *testing.T) {
	root, workspaceRoot := t.TempDir(), t.TempDir()
	secrets := &controllerSecretBackend{}
	profile, err := agentsession.ProfileFingerprint("https://server.example/api")
	if err != nil {
		t.Fatal(err)
	}
	openStore := func() *agentsession.Store {
		store, openErr := agentsession.Open(t.Context(), agentsession.Options{
			Root: root, ProfileFingerprint: profile, Secrets: secrets, LockTimeout: 30 * time.Millisecond,
		})
		if openErr != nil {
			t.Fatal(openErr)
		}
		return store
	}
	server := &controllerServer{generation: api.MemoryGenerationStamp{LearnerGeneration: 1, MemoryGeneration: 1}}
	provider := Provider{Name: "ollama", Endpoint: "http://127.0.0.1:11434/v1", Model: "local"}
	seed, err := Start(t.Context(), controllerDependencies(openStore(), &controllerModel{}, server, workspaceRoot, provider), false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := seed.Send(t.Context(), "初始轮次"); err != nil {
		t.Fatal(err)
	}
	sessionID := seed.SessionID()
	if err := seed.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}

	lockingModel := &lockingControllerModel{root: root}
	resumed, err := Resume(t.Context(), controllerDependencies(openStore(), lockingModel, server, workspaceRoot, provider), ResumeOptions{SessionID: sessionID, CurrentWorkspace: workspaceRoot})
	if err != nil {
		t.Fatal(err)
	}
	result, sendErr := resumed.Send(t.Context(), "必须在关闭前重试保存")
	if lockingModel.lock == nil {
		t.Fatal("model did not hold the profile publication lock")
	}
	if closeErr := lockingModel.lock.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	if !errors.Is(sendErr, agentsession.ErrCheckpointSaveFailed) || result.Text != "锁冲突前已提交的回答" {
		t.Fatalf("result=%+v err=%v", result, sendErr)
	}
	if state, _ := resumed.SessionPersistenceStatus(); state != "failed" {
		t.Fatalf("persistence state=%q", state)
	}
	if err := resumed.Shutdown(t.Context()); err != nil {
		t.Fatalf("shutdown retry failed: %v", err)
	}

	store := openStore()
	handle, loaded, err := store.OpenSession(t.Context(), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	defer handle.Close()
	defer store.Close()
	if loaded.Interrupted != nil || loaded.Record.Lifecycle != "closed" {
		t.Fatalf("shutdown retry did not consume recovery marker: %+v", loaded)
	}
	transcript, err := agentsession.DecodeTranscript(loaded.Record.Transcript, agentsession.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	foundUser, foundAnswer := false, false
	for _, entry := range transcript.Entries {
		foundUser = foundUser || entry.Kind == agentsession.TranscriptKindUser && entry.Text == "必须在关闭前重试保存"
		foundAnswer = foundAnswer || entry.Kind == agentsession.TranscriptKindAssistant && entry.Text == "锁冲突前已提交的回答" && entry.ModelCommitted
	}
	if !foundUser || !foundAnswer {
		t.Fatalf("shutdown retry lost committed transcript: %+v", transcript.Entries)
	}
}

func TestControllerShutdownRetriesStableSaveBeforeClearingLoop(t *testing.T) {
	root, workspaceRoot := t.TempDir(), t.TempDir()
	secrets := &controllerSecretBackend{}
	model := &controllerModel{}
	server := &controllerServer{generation: api.MemoryGenerationStamp{LearnerGeneration: 1, MemoryGeneration: 1}}
	provider := Provider{Name: "ollama", Endpoint: "http://127.0.0.1:11434/v1", Model: "local"}
	store := controllerStore(t, root, secrets)
	controller, err := Start(t.Context(), controllerDependencies(store, model, server, workspaceRoot, provider), false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := controller.Send(t.Context(), "关闭前保存这轮"); err != nil {
		t.Fatal(err)
	}
	controller.mu.Lock()
	revisionBefore := controller.record.RecordRevision
	controller.saveFailed = checkpointPersistenceError(errors.New("previous publication failed"))
	controller.mu.Unlock()
	sessionID := controller.SessionID()
	if err := controller.Shutdown(t.Context()); err != nil {
		t.Fatalf("shutdown retry failed: %v", err)
	}

	store = controllerStore(t, root, secrets)
	handle, loaded, err := store.OpenSession(t.Context(), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	defer handle.Close()
	defer store.Close()
	checkpoint, err := agentloop.DecodeSessionCheckpoint(loaded.Record.Checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, message := range checkpoint.Messages {
		if message.Role == "user" && message.Content == "关闭前保存这轮" {
			found = true
		}
	}
	if !found || loaded.Record.RecordRevision <= revisionBefore || loaded.Record.Lifecycle != "closed" {
		t.Fatalf("shutdown did not publish latest stable checkpoint: record=%+v checkpoint=%+v", loaded.Record, checkpoint.Messages)
	}
}

func TestControllerShutdownReportsUnstableCheckpointWithoutClearingReceipt(t *testing.T) {
	root, workspaceRoot := t.TempDir(), t.TempDir()
	secrets := &controllerSecretBackend{}
	model := &controllerModel{}
	modelResponse := modelclient.Response{
		Message: modelclient.Message{
			Role: "assistant",
			ToolCalls: []modelclient.ToolCall{{
				ID: "call-pref", Type: "function",
				Function: modelclient.ToolFunction{Name: "remember_preference", Arguments: `{"content":"回答简洁","reason":"用户明确要求","category":"interaction_preference","sensitivity":"non_sensitive","stability":"stable"}`},
			}},
		},
	}
	modelWithPending := &scriptedControllerModel{responses: []modelclient.Response{modelResponse}}
	server := &controllerServer{generation: api.MemoryGenerationStamp{LearnerGeneration: 1, MemoryGeneration: 1}}
	provider := Provider{Name: "ollama", Endpoint: "http://127.0.0.1:11434/v1", Model: "local"}
	store := controllerStore(t, root, secrets)
	controller, err := Start(t.Context(), controllerDependencies(store, modelWithPending, server, workspaceRoot, provider), false)
	if err != nil {
		t.Fatal(err)
	}
	result, err := controller.Send(t.Context(), "请记住")
	if err != nil || result.Pending == nil {
		t.Fatalf("pending=%+v err=%v", result, err)
	}
	sessionID := controller.SessionID()
	if err := controller.Shutdown(t.Context()); !errors.Is(err, agentsession.ErrCheckpointSaveFailed) {
		t.Fatalf("shutdown error=%v, want checkpoint save failure", err)
	}
	if state, _ := controller.SessionPersistenceStatus(); state != "failed" {
		t.Fatalf("persistence state=%q", state)
	}
	store = controllerStore(t, root, secrets)
	handle, loaded, err := store.OpenSession(t.Context(), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	defer handle.Close()
	defer store.Close()
	if loaded.Interrupted == nil || loaded.Record.Lifecycle != "active" {
		t.Fatalf("failed shutdown falsely consumed recovery marker: %+v", loaded)
	}
	_ = model
}

func TestFinishOperationReturnsCheckpointExportFailureAndPreservesResult(t *testing.T) {
	root, workspaceRoot := t.TempDir(), t.TempDir()
	secrets := &controllerSecretBackend{}
	model := &scriptedControllerModel{responses: []modelclient.Response{{
		Message: modelclient.Message{
			Role: "assistant",
			ToolCalls: []modelclient.ToolCall{{
				ID: "call-pref", Type: "function",
				Function: modelclient.ToolFunction{Name: "remember_preference", Arguments: `{"content":"回答简洁","reason":"用户明确要求","category":"interaction_preference","sensitivity":"non_sensitive","stability":"stable"}`},
			}},
		},
	}}}
	server := &controllerServer{generation: api.MemoryGenerationStamp{LearnerGeneration: 1, MemoryGeneration: 1}}
	provider := Provider{Name: "ollama", Endpoint: "http://127.0.0.1:11434/v1", Model: "local"}
	store := controllerStore(t, root, secrets)
	controller, err := Start(t.Context(), controllerDependencies(store, model, server, workspaceRoot, provider), false)
	if err != nil {
		t.Fatal(err)
	}
	if result, err := controller.Send(t.Context(), "请记住"); err != nil || result.Pending == nil {
		t.Fatalf("pending result=%+v err=%v", result, err)
	}
	original := agentloop.Result{Text: "模型已经提交的正文"}
	returned, err := controller.finishOperation(t.Context(), original, nil)
	if !errors.Is(err, agentsession.ErrCheckpointSaveFailed) || returned.Text != original.Text {
		t.Fatalf("returned=%+v err=%v", returned, err)
	}
	controller.mu.Lock()
	transcript := append([]agentsession.TranscriptEntryV1(nil), controller.transcript.Entries...)
	controller.mu.Unlock()
	foundCommitted := false
	for _, entry := range transcript {
		if entry.Kind == agentsession.TranscriptKindAssistant && entry.Text == original.Text && entry.ModelCommitted {
			foundCommitted = true
		}
	}
	if !foundCommitted {
		t.Fatalf("committed result was not retained for shutdown retry: %+v", transcript)
	}
	if state, _ := controller.SessionPersistenceStatus(); state != "failed" {
		t.Fatalf("persistence state=%q", state)
	}
	controller.abort()
}

func TestControllerSavePublicationFullDegradesNewSessionAndPreservesResult(t *testing.T) {
	root, workspaceRoot := t.TempDir(), t.TempDir()
	secrets := &controllerSecretBackend{}
	profile, err := agentsession.ProfileFingerprint("https://server.example/api")
	if err != nil {
		t.Fatal(err)
	}
	store, err := agentsession.Open(t.Context(), agentsession.Options{
		Root: root, ProfileFingerprint: profile, Secrets: secrets,
		Limits: agentsession.Limits{SessionPlaintextBytes: 8 << 10, SessionCiphertextBytes: 64 << 10},
	})
	if err != nil {
		t.Fatal(err)
	}
	longAnswer := strings.Repeat(strings.Repeat("答", 200)+"\n", 20)
	model := &scriptedControllerModel{responses: []modelclient.Response{
		{Message: modelclient.Message{Role: "assistant", Content: longAnswer}},
		{Message: modelclient.Message{Role: "assistant", Content: "未保存模式仍可继续"}},
		{Message: modelclient.Message{Role: "assistant", Content: "未保存模式仍可继续"}},
		{Message: modelclient.Message{Role: "assistant", Content: "未保存模式仍可继续"}},
	}}
	server := &controllerServer{generation: api.MemoryGenerationStamp{LearnerGeneration: 1, MemoryGeneration: 1}}
	provider := Provider{Name: "ollama", Endpoint: "http://127.0.0.1:11434/v1", Model: "local"}
	controller, err := Start(t.Context(), controllerDependencies(store, model, server, workspaceRoot, provider), false)
	if err != nil {
		t.Fatal(err)
	}
	sessionID := controller.SessionID()
	result, err := controller.Send(t.Context(), "产生较长回答")
	if !errors.Is(err, agentsession.ErrStoreFull) || result.Text != longAnswer {
		t.Fatalf("publication result bytes=%d err=%v", len(result.Text), err)
	}
	status := controller.Status()
	state, detail := controller.SessionPersistenceStatus()
	if status.Persistent || !strings.Contains(status.DegradedReason, "session_store_full") || state != "unsaved" || !strings.Contains(detail, "session_store_full") {
		t.Fatalf("degraded status=%+v state=%q detail=%q", status, state, detail)
	}
	if result, err := controller.Send(t.Context(), "继续"); err != nil || result.Text != "未保存模式仍可继续" {
		t.Fatalf("continued unsaved result=%+v err=%v", result, err)
	}
	if err := controller.Shutdown(t.Context()); err != nil {
		t.Fatalf("unsaved shutdown failed: %v", err)
	}

	store, err = agentsession.Open(t.Context(), agentsession.Options{
		Root: root, ProfileFingerprint: profile, Secrets: secrets,
		Limits: agentsession.Limits{SessionPlaintextBytes: 8 << 10, SessionCiphertextBytes: 64 << 10},
	})
	if err != nil {
		t.Fatal(err)
	}
	handle, loaded, err := store.OpenSession(t.Context(), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	defer handle.Close()
	defer store.Close()
	if loaded.Interrupted == nil {
		t.Fatalf("publication failure consumed durable recovery marker: %+v", loaded)
	}
}

func TestControllerCreateStoreFullDegradesToVisibleUnsavedConversation(t *testing.T) {
	root, workspaceRoot := t.TempDir(), t.TempDir()
	secrets := &controllerSecretBackend{}
	profile, err := agentsession.ProfileFingerprint("https://server.example/api")
	if err != nil {
		t.Fatal(err)
	}
	store, err := agentsession.Open(t.Context(), agentsession.Options{Root: root, ProfileFingerprint: profile, Secrets: secrets, Limits: agentsession.Limits{Sessions: 1}})
	if err != nil {
		t.Fatal(err)
	}
	transcript, err := agentsession.EncodeTranscript(agentsession.TranscriptV1{SchemaVersion: 1, Entries: []agentsession.TranscriptEntryV1{}}, agentsession.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	handle, _, err := store.Create(t.Context(), agentsession.CreateInput{Title: "existing", Checkpoint: []byte(`{}`), Transcript: transcript})
	if err != nil {
		t.Fatal(err)
	}
	_ = handle.Close()
	model := &controllerModel{}
	server := &controllerServer{generation: api.MemoryGenerationStamp{LearnerGeneration: 1, MemoryGeneration: 1}}
	provider := Provider{Name: "ollama", Endpoint: "http://127.0.0.1:11434/v1", Model: "local"}
	controller, err := Start(t.Context(), controllerDependencies(store, model, server, workspaceRoot, provider), false)
	if err != nil {
		t.Fatalf("store-full new session should degrade, got %v", err)
	}
	status := controller.Status()
	if status.Persistent || !strings.Contains(status.DegradedReason, "session_store_full") {
		t.Fatalf("status=%+v", status)
	}
	if result, err := controller.Send(t.Context(), "继续无保存对话"); err != nil || result.Text == "" {
		t.Fatalf("unsaved conversation result=%+v err=%v", result, err)
	}
	if err := controller.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestAutomaticTitleSchedulingRequiresTurnAndTimeThresholdsAndSingleConcurrency(t *testing.T) {
	root, workspaceRoot := t.TempDir(), t.TempDir()
	secrets := &controllerSecretBackend{}
	limits := agentsession.DefaultLimits()
	limits.AutoTitleTurnInterval = 3
	limits.AutoTitleMinInterval = 10 * time.Minute
	limits.AutoTitleRequestTimeout = time.Second
	model := &titleSchedulerModel{started: make(chan int, 4), release: make(chan struct{}, 4)}
	now := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	dependencies := controllerDependencies(
		controllerStoreWithLimits(t, root, secrets, limits), model,
		&controllerServer{generation: api.MemoryGenerationStamp{LearnerGeneration: 1, MemoryGeneration: 1}}, workspaceRoot,
		Provider{Name: "ollama", Endpoint: "http://127.0.0.1:11434/v1", Model: "local"},
	)
	dependencies.Now = func() time.Time { return now }
	controller, err := Start(t.Context(), dependencies, false)
	if err != nil {
		t.Fatal(err)
	}
	defer controller.Close()

	if _, err := controller.Send(t.Context(), "首个 eligible turn"); err != nil {
		t.Fatal(err)
	}
	waitForTitleCalls(t, model.started, 1)
	for _, input := range []string{"第二轮", "第三轮", "第四轮"} {
		if _, err := controller.Send(t.Context(), input); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case call := <-model.started:
		t.Fatalf("concurrent title call started: %d", call)
	case <-time.After(20 * time.Millisecond):
	}
	model.release <- struct{}{}
	deadline := time.Now().Add(time.Second)
	for {
		controller.mu.Lock()
		pending := controller.titleCancel != nil
		controller.mu.Unlock()
		if !pending {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("first title job did not finish")
		}
		time.Sleep(time.Millisecond)
	}

	now = now.Add(9 * time.Minute)
	if _, err := controller.Send(t.Context(), "轮数已满足但时间未满足"); err != nil {
		t.Fatal(err)
	}
	select {
	case call := <-model.started:
		t.Fatalf("title call %d ignored minimum time interval", call)
	case <-time.After(20 * time.Millisecond):
	}
	now = now.Add(time.Minute)
	if _, err := controller.Send(t.Context(), "轮数和时间都满足"); err != nil {
		t.Fatal(err)
	}
	waitForTitleCalls(t, model.started, 2)
	model.release <- struct{}{}
	deadline = time.Now().Add(time.Second)
	for controller.SessionTitle() != "标题2" {
		if time.Now().After(deadline) {
			t.Fatalf("second eligible title was not saved: %q", controller.SessionTitle())
		}
		time.Sleep(time.Millisecond)
	}
	model.mu.Lock()
	calls, maxConcurrent := model.titleCalls, model.maxConcurrent
	model.mu.Unlock()
	if calls != 2 || maxConcurrent != 1 {
		t.Fatalf("title calls=%d max concurrent=%d", calls, maxConcurrent)
	}
}

func TestAutomaticTitleRequestTimeoutPublishesFailureAndCanRecoverAfterThresholds(t *testing.T) {
	root, workspaceRoot := t.TempDir(), t.TempDir()
	secrets := &controllerSecretBackend{}
	limits := agentsession.DefaultLimits()
	limits.AutoTitleTurnInterval = 2
	limits.AutoTitleMinInterval = time.Minute
	limits.AutoTitleRequestTimeout = 20 * time.Millisecond
	model := &timeoutThenSuccessTitleModel{started: make(chan int, 3), canceled: make(chan struct{})}
	now := time.Date(2030, 2, 3, 4, 5, 6, 0, time.UTC)
	dependencies := controllerDependencies(
		controllerStoreWithLimits(t, root, secrets, limits), model,
		&controllerServer{generation: api.MemoryGenerationStamp{LearnerGeneration: 1, MemoryGeneration: 1}}, workspaceRoot,
		Provider{Name: "ollama", Endpoint: "http://127.0.0.1:11434/v1", Model: "local"},
	)
	dependencies.Now = func() time.Time { return now }
	controller, err := Start(t.Context(), dependencies, false)
	if err != nil {
		t.Fatal(err)
	}
	defer controller.Close()
	if result, err := controller.Send(t.Context(), "超时后保留本地摘要"); err != nil || result.Text != "主对话回答" {
		t.Fatalf("main result=%+v err=%v", result, err)
	}
	waitForTitleCalls(t, model.started, 1)
	select {
	case <-model.canceled:
	case <-time.After(time.Second):
		t.Fatal("injected title request timeout did not cancel the model")
	}
	deadline := time.Now().Add(time.Second)
	for controller.Status().TitleFailureCode != sessionTitleFailedCode {
		if time.Now().After(deadline) {
			t.Fatal("timeout failure status was not published")
		}
		time.Sleep(time.Millisecond)
	}
	if title := controller.SessionTitle(); title != "超时后保留本地摘要" {
		t.Fatalf("timeout changed fallback title: %q", title)
	}
	assertSingleTitleFailure(t, controller)

	if _, err := controller.Send(t.Context(), "只有轮数之一"); err != nil {
		t.Fatal(err)
	}
	select {
	case call := <-model.started:
		t.Fatalf("timeout retried before thresholds: %d", call)
	case <-time.After(20 * time.Millisecond):
	}
	now = now.Add(time.Minute)
	if _, err := controller.Send(t.Context(), "满足轮数和时间"); err != nil {
		t.Fatal(err)
	}
	waitForTitleCalls(t, model.started, 2)
	deadline = time.Now().Add(time.Second)
	for controller.SessionTitle() != "恢复成功" {
		if time.Now().After(deadline) {
			t.Fatalf("title did not recover: %q", controller.SessionTitle())
		}
		time.Sleep(time.Millisecond)
	}
	if status := controller.Status(); status.TitleFailureCode != "" {
		t.Fatalf("successful title did not clear failure status: %+v", status)
	}
	for _, entry := range controller.SessionTranscript().Entries {
		if entry.Notice != nil && entry.Notice.Code == sessionTitleFailedCode {
			t.Fatalf("successful title retained failure transcript: %+v", entry)
		}
	}
}

func TestAutomaticTitleFailureMatrixRetainsFallbackAndBoundedStatus(t *testing.T) {
	tests := []struct {
		name    string
		content string
		err     error
	}{
		{name: "provider error", err: errors.New("provider unavailable")},
		{name: "content filter", err: errors.New("content filtered")},
		{name: "transport error", err: errors.New("transport reset")},
		{name: "malformed json", content: `{"title":`},
		{name: "trailing json", content: `{"title":"安全"}{}`},
		{name: "oversize response", content: strings.Repeat("x", agentsession.DefaultLimits().AutoTitleResponseBytes+1)},
		{name: "unsafe title", content: "{\"title\":\"双行\\n标题\"}"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root, workspaceRoot := t.TempDir(), t.TempDir()
			controller, err := Start(t.Context(), controllerDependencies(
				controllerStore(t, root, &controllerSecretBackend{}), failingTitleModel{titleContent: test.content, titleErr: test.err},
				&controllerServer{generation: api.MemoryGenerationStamp{LearnerGeneration: 1, MemoryGeneration: 1}}, workspaceRoot,
				Provider{Name: "ollama", Endpoint: "http://127.0.0.1:11434/v1", Model: "local"},
			), false)
			if err != nil {
				t.Fatal(err)
			}
			defer controller.Close()
			if result, err := controller.Send(t.Context(), "失败时的本地摘要"); err != nil || result.Text != "主对话继续" {
				t.Fatalf("main conversation result=%+v err=%v", result, err)
			}
			deadline := time.Now().Add(time.Second)
			for controller.Status().TitleFailureCode != sessionTitleFailedCode {
				if time.Now().After(deadline) {
					t.Fatal("title failure status missing")
				}
				time.Sleep(time.Millisecond)
			}
			if title := controller.SessionTitle(); title != "失败时的本地摘要" {
				t.Fatalf("failure changed fallback title: %q", title)
			}
			assertSingleTitleFailure(t, controller)
		})
	}
}

func TestPendingAutomaticTitleCannotOverrideManualRenameOrScheduleAgain(t *testing.T) {
	root, workspaceRoot := t.TempDir(), t.TempDir()
	secrets := &controllerSecretBackend{}
	limits := agentsession.DefaultLimits()
	limits.AutoTitleTurnInterval = 1
	limits.AutoTitleMinInterval = time.Nanosecond
	model := &generationTitleModel{titleStarted: make(chan struct{}, 2), releaseTitle: make(chan struct{})}
	controller, err := Start(t.Context(), controllerDependencies(
		controllerStoreWithLimits(t, root, secrets, limits), model,
		&controllerServer{generation: api.MemoryGenerationStamp{LearnerGeneration: 1, MemoryGeneration: 1}}, workspaceRoot,
		Provider{Name: "ollama", Endpoint: "http://127.0.0.1:11434/v1", Model: "local"},
	), false)
	if err != nil {
		t.Fatal(err)
	}
	defer controller.Close()
	if _, err := controller.Send(t.Context(), "触发竞态"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-model.titleStarted:
	case <-time.After(time.Second):
		t.Fatal("title request did not start")
	}
	controller.mu.Lock()
	revision := controller.record.RecordRevision
	controller.mu.Unlock()
	renamed, err := controller.RenameSession(t.Context(), controller.SessionID(), "永久人工标题", revision)
	if err != nil {
		t.Fatal(err)
	}
	if renamed.TitleSource != "manual" {
		t.Fatalf("renamed summary=%+v", renamed)
	}
	close(model.releaseTitle)
	time.Sleep(20 * time.Millisecond)
	if title := controller.SessionTitle(); title != "永久人工标题" {
		t.Fatalf("late title overwrote manual rename: %q", title)
	}
	if _, err := controller.Send(t.Context(), "人工标题后继续"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-model.titleStarted:
		t.Fatal("manual title allowed another automatic request")
	case <-time.After(20 * time.Millisecond):
	}
}

func assertSingleTitleFailure(t *testing.T, controller *Controller) {
	t.Helper()
	status := controller.Status()
	if status.TitleFailureCode != sessionTitleFailedCode {
		t.Fatalf("status=%+v", status)
	}
	notices := 0
	for _, notice := range status.Notices {
		if strings.HasPrefix(notice, "["+sessionTitleFailedCode+"]") {
			notices++
		}
	}
	transcriptNotices := 0
	for _, entry := range controller.SessionTranscript().Entries {
		if entry.Notice != nil && entry.Notice.Code == sessionTitleFailedCode {
			transcriptNotices++
		}
	}
	if notices != 1 || transcriptNotices != 1 {
		t.Fatalf("failure status notices=%d transcript notices=%d status=%+v", notices, transcriptNotices, status)
	}
}

func TestControllerGeneratesBoundedToolFreeAutomaticTitle(t *testing.T) {
	root, workspaceRoot := t.TempDir(), t.TempDir()
	secrets := &controllerSecretBackend{}
	model := &controllerModel{}
	server := &controllerServer{generation: api.MemoryGenerationStamp{LearnerGeneration: 1, MemoryGeneration: 1}}
	provider := Provider{Name: "ollama", Endpoint: "http://127.0.0.1:11434/v1", Model: "local"}

	store := controllerStore(t, root, secrets)
	controller, err := Start(t.Context(), controllerDependencies(store, model, server, workspaceRoot, provider), false)
	if err != nil {
		t.Fatal(err)
	}
	defer controller.Close()
	if _, err := controller.Send(t.Context(), "复习三角函数恒等式"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for {
		controller.mu.Lock()
		title := controller.record.Title
		titleRevision := controller.record.TitleRevision
		controller.mu.Unlock()
		if title == "代数复习" {
			if titleRevision < 3 {
				t.Fatalf("title revision did not advance for fallback and generated titles: %d", titleRevision)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("automatic title was not saved, current=%q", title)
		}
		time.Sleep(5 * time.Millisecond)
	}
	model.mu.Lock()
	defer model.mu.Unlock()
	found := false
	for _, request := range model.requests {
		if request.MaxTokens == 96 && request.ReasoningEffort == modelclient.ReasoningEffortNone {
			if request.Tools != nil || len(request.Messages) != 2 || !strings.Contains(request.Messages[1].Content, "复习三角函数恒等式") || !strings.Contains(request.Messages[1].Content, "已回答") {
				t.Fatalf("unsafe or incomplete title request: %+v", request)
			}
			found = true
		}
	}
	if !found {
		t.Fatalf("bounded tool-free title request missing: %+v", model.requests)
	}
}

func TestControllerSwitchSaveFailureAfterTargetPreflightPreservesCurrent(t *testing.T) {
	root, workspaceRoot := t.TempDir(), t.TempDir()
	secrets := &controllerSecretBackend{}
	model := &controllerModel{}
	server := &switchLockServer{controllerServer: controllerServer{generation: api.MemoryGenerationStamp{LearnerGeneration: 1, MemoryGeneration: 1}}, root: root}
	provider := Provider{Name: "ollama", Endpoint: "http://127.0.0.1:11434/v1", Model: "local"}
	current, err := Start(t.Context(), controllerDependencies(controllerStore(t, root, secrets), model, server, workspaceRoot, provider), false)
	if err != nil {
		t.Fatal(err)
	}
	defer current.Close()
	if _, err := current.Send(t.Context(), "切换失败后保留"); err != nil {
		t.Fatal(err)
	}
	target, err := Start(t.Context(), controllerDependencies(controllerStore(t, root, secrets), model, server, workspaceRoot, provider), false)
	if err != nil {
		t.Fatal(err)
	}
	targetID := target.SessionID()
	if err := target.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
	items, err := current.ListSessions(t.Context(), SessionListRequest{})
	if err != nil {
		t.Fatal(err)
	}
	var summary agentsession.Summary
	for _, item := range items {
		if item.Summary.SessionID == targetID {
			summary = item.Summary
		}
	}
	currentID, generation := current.SessionID(), current.Generation()
	server.enabled = true
	_, err = current.CommitSwitch(t.Context(), current.PlanSwitch(summary), SwitchConfirmation{})
	server.release()
	server.enabled = false
	if err == nil || current.SessionID() != currentID || current.Generation() != generation {
		t.Fatalf("err=%v current=%s generation=%d", err, current.SessionID(), current.Generation())
	}
	if gate := current.SwitchGate(); !gate.Allowed {
		t.Fatalf("current Session remained blocked after failed save: %+v", gate)
	}
	if _, err := current.Send(t.Context(), "仍可继续"); err != nil {
		t.Fatalf("current Session did not remain usable: %v", err)
	}
}

func TestControllerLateAutomaticTitleCannotCrossSwitchGeneration(t *testing.T) {
	root, workspaceRoot := t.TempDir(), t.TempDir()
	secrets := &controllerSecretBackend{}
	titleModel := &generationTitleModel{titleStarted: make(chan struct{}, 1), releaseTitle: make(chan struct{})}
	server := &controllerServer{generation: api.MemoryGenerationStamp{LearnerGeneration: 1, MemoryGeneration: 1}}
	provider := Provider{Name: "ollama", Endpoint: "http://127.0.0.1:11434/v1", Model: "local"}
	target, err := Start(t.Context(), controllerDependencies(controllerStore(t, root, secrets), &controllerModel{}, server, workspaceRoot, provider), false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := target.Send(t.Context(), "目标内容"); err != nil {
		t.Fatal(err)
	}
	targetID, targetTitle := target.SessionID(), target.SessionTitle()
	if err := target.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
	current, err := Start(t.Context(), controllerDependencies(controllerStore(t, root, secrets), titleModel, server, workspaceRoot, provider), false)
	if err != nil {
		t.Fatal(err)
	}
	defer current.Close()
	if _, err := current.Send(t.Context(), "触发旧标题"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-titleModel.titleStarted:
	case <-time.After(time.Second):
		t.Fatal("automatic title request did not start")
	}
	items, err := current.ListSessions(t.Context(), SessionListRequest{})
	if err != nil {
		t.Fatal(err)
	}
	var summary agentsession.Summary
	for _, item := range items {
		if item.Summary.SessionID == targetID {
			summary = item.Summary
		}
	}
	if _, err := current.CommitSwitch(t.Context(), current.PlanSwitch(summary), SwitchConfirmation{}); err != nil {
		t.Fatal(err)
	}
	close(titleModel.releaseTitle)
	time.Sleep(20 * time.Millisecond)
	if current.SessionID() != targetID || current.SessionTitle() != targetTitle {
		t.Fatalf("late title crossed generation: id=%s title=%q want=%q", current.SessionID(), current.SessionTitle(), targetTitle)
	}
}

func TestControllerSafeSwitchNewRenameDeleteAndSameTargetNoop(t *testing.T) {
	root, workspaceRoot := t.TempDir(), t.TempDir()
	secrets := &controllerSecretBackend{}
	model := &controllerModel{}
	server := &controllerServer{generation: api.MemoryGenerationStamp{LearnerGeneration: 1, MemoryGeneration: 1}}
	provider := Provider{Name: "ollama", Endpoint: "http://127.0.0.1:11434/v1", Model: "local"}
	current, err := Start(t.Context(), controllerDependencies(controllerStore(t, root, secrets), model, server, workspaceRoot, provider), false)
	if err != nil {
		t.Fatal(err)
	}
	defer current.Close()
	if _, err := current.Send(t.Context(), "当前会话内容"); err != nil {
		t.Fatal(err)
	}
	currentID := current.SessionID()

	target, err := Start(t.Context(), controllerDependencies(controllerStore(t, root, secrets), model, server, workspaceRoot, provider), false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := target.Send(t.Context(), "目标会话内容"); err != nil {
		t.Fatal(err)
	}
	targetID := target.SessionID()
	if err := target.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}

	items, err := current.ListSessions(t.Context(), SessionListRequest{})
	if err != nil {
		t.Fatal(err)
	}
	var targetSummary agentsession.Summary
	for _, item := range items {
		if item.Summary.SessionID == targetID {
			targetSummary = item.Summary
		}
	}
	if targetSummary.SessionID == "" {
		t.Fatalf("target missing from picker list: %+v", items)
	}
	plan := current.PlanSwitch(targetSummary)
	generation, err := current.CommitSwitch(t.Context(), plan, SwitchConfirmation{})
	if err != nil {
		t.Fatal(err)
	}
	if generation != 2 || current.Generation() != 2 || current.SessionID() != targetID || current.FileAuthorizationMode() != agentloop.FileAuthorizationConfirm {
		t.Fatalf("generation=%d current=%s mode=%s", generation, current.SessionID(), current.FileAuthorizationMode())
	}
	transcript := current.SessionTranscript()
	foundTarget, foundCurrent := false, false
	for _, entry := range transcript.Entries {
		foundTarget = foundTarget || entry.Kind == agentsession.TranscriptKindUser && entry.Text == "目标会话内容"
		foundCurrent = foundCurrent || entry.Kind == agentsession.TranscriptKindUser && entry.Text == "当前会话内容"
	}
	if !foundTarget || foundCurrent {
		t.Fatalf("switched transcript=%+v", transcript.Entries)
	}

	items, err = current.ListSessions(t.Context(), SessionListRequest{})
	if err != nil {
		t.Fatal(err)
	}
	var currentSummary agentsession.Summary
	for _, item := range items {
		if item.Current {
			currentSummary = item.Summary
		}
	}
	noOpGeneration, err := current.CommitSwitch(t.Context(), current.PlanSwitch(currentSummary), SwitchConfirmation{})
	if err != nil || noOpGeneration != generation {
		t.Fatalf("same target no-op generation=%d err=%v", noOpGeneration, err)
	}
	renamed, err := current.RenameSession(t.Context(), targetID, "手动标题", currentSummary.RecordRevision)
	if err != nil || renamed.Title != "手动标题" || renamed.TitleSource != "manual" {
		t.Fatalf("renamed=%+v err=%v", renamed, err)
	}
	reset, err := current.RenameSession(t.Context(), targetID, "", renamed.RecordRevision)
	if err != nil || reset.TitleSource != "auto" {
		t.Fatalf("reset=%+v err=%v", reset, err)
	}
	if err := current.DeleteSession(t.Context(), agentsession.DeleteTarget{StorageID: reset.StorageID}); !errors.Is(err, ErrCurrentSession) {
		t.Fatalf("current storage-only delete err=%v", err)
	}
	if err := current.DeleteSession(t.Context(), agentsession.DeleteTarget{SessionID: targetID, StorageID: reset.StorageID, ExpectedRecordRevision: reset.RecordRevision}); !errors.Is(err, ErrCurrentSession) {
		t.Fatalf("current delete err=%v", err)
	}

	newGeneration, err := current.NewSession(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if newGeneration != 3 || current.SessionID() == targetID || current.SessionID() == currentID || len(current.SessionTranscript().Entries) != 0 {
		t.Fatalf("new generation=%d id=%s transcript=%+v", newGeneration, current.SessionID(), current.SessionTranscript().Entries)
	}
	items, err = current.ListSessions(t.Context(), SessionListRequest{})
	if err != nil {
		t.Fatal(err)
	}
	var oldTarget agentsession.Summary
	for _, item := range items {
		if item.Summary.SessionID == targetID {
			oldTarget = item.Summary
		}
	}
	if oldTarget.SessionID == "" {
		t.Fatal("old target missing before delete")
	}
	if err := current.DeleteSession(t.Context(), agentsession.DeleteTarget{SessionID: targetID, StorageID: oldTarget.StorageID, ExpectedRecordRevision: oldTarget.RecordRevision}); err != nil {
		t.Fatal(err)
	}
}

func TestControllerTargetLockFailurePreservesCurrentGenerationAndTranscript(t *testing.T) {
	root, workspaceRoot := t.TempDir(), t.TempDir()
	secrets := &controllerSecretBackend{}
	model := &controllerModel{}
	server := &controllerServer{generation: api.MemoryGenerationStamp{LearnerGeneration: 1, MemoryGeneration: 1}}
	provider := Provider{Name: "ollama", Endpoint: "http://127.0.0.1:11434/v1", Model: "local"}
	current, err := Start(t.Context(), controllerDependencies(controllerStore(t, root, secrets), model, server, workspaceRoot, provider), false)
	if err != nil {
		t.Fatal(err)
	}
	defer current.Close()
	if _, err := current.Send(t.Context(), "必须保留"); err != nil {
		t.Fatal(err)
	}
	target, err := Start(t.Context(), controllerDependencies(controllerStore(t, root, secrets), model, server, workspaceRoot, provider), false)
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	items, err := current.ListSessions(t.Context(), SessionListRequest{})
	if err != nil {
		t.Fatal(err)
	}
	var locked agentsession.Summary
	for _, item := range items {
		if item.Summary.SessionID == target.SessionID() {
			locked = item.Summary
		}
	}
	if !locked.Locked {
		t.Fatalf("target was not marked locked: %+v", locked)
	}
	currentID, generation := current.SessionID(), current.Generation()
	_, err = current.CommitSwitch(t.Context(), current.PlanSwitch(locked), SwitchConfirmation{})
	if !errors.Is(err, agentsession.ErrInUse) || current.SessionID() != currentID || current.Generation() != generation {
		t.Fatalf("err=%v current=%s generation=%d", err, current.SessionID(), current.Generation())
	}
	found := false
	for _, entry := range current.SessionTranscript().Entries {
		found = found || entry.Kind == agentsession.TranscriptKindUser && entry.Text == "必须保留"
	}
	if !found {
		t.Fatal("target failure lost current transcript")
	}
}

func TestControllerPersistsAndResumesStableConversation(t *testing.T) {
	root, workspaceRoot := t.TempDir(), t.TempDir()
	secrets := &controllerSecretBackend{}
	model := &controllerModel{}
	server := &controllerServer{generation: api.MemoryGenerationStamp{LearnerGeneration: 3, MemoryGeneration: 4}}
	provider := Provider{Name: "ollama", Endpoint: "http://127.0.0.1:11434/v1", Model: "local"}

	store := controllerStore(t, root, secrets)
	controller, err := Start(t.Context(), controllerDependencies(store, model, server, workspaceRoot, provider), false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := controller.Send(t.Context(), "请复习一元一次方程"); err != nil {
		t.Fatal(err)
	}
	sessionID := controller.SessionID()
	controller.Close()

	store = controllerStore(t, root, secrets)
	closedHandle, closedLoaded, err := store.OpenSession(t.Context(), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	closedRevision := closedLoaded.Record.RecordRevision
	closedLastOpened := closedLoaded.Record.LastOpenedAt
	if err := closedHandle.Close(); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store = controllerStore(t, root, secrets)
	resumed, err := Resume(t.Context(), controllerDependencies(store, model, server, workspaceRoot, provider), ResumeOptions{
		SessionID: sessionID, CurrentWorkspace: workspaceRoot,
	})
	if err != nil {
		t.Fatal(err)
	}
	resumed.mu.Lock()
	activeRecord := resumed.record
	resumed.mu.Unlock()
	if activeRecord.RecordRevision <= closedRevision || activeRecord.CheckpointRevision != closedLoaded.Record.CheckpointRevision || activeRecord.LastOpenedAt.Before(closedLastOpened) || activeRecord.Lifecycle != "active" {
		t.Fatalf("resume metadata was not published before interaction: closed=%+v active=%+v", closedLoaded.Record, activeRecord)
	}
	if _, err := resumed.Send(t.Context(), "继续出一道题"); err != nil {
		t.Fatal(err)
	}
	resumed.Close()

	model.mu.Lock()
	defer model.mu.Unlock()
	foundHistory := false
	for _, request := range model.requests {
		for _, message := range request.Messages {
			if message.Role == "user" && message.Content == "请复习一元一次方程" && len(request.Messages) > 2 {
				foundHistory = true
			}
		}
	}
	if !foundHistory {
		t.Fatalf("resumed model requests did not contain committed history: %+v", model.requests)
	}
}

func TestControllerFileReceiptsFollowStableCheckpointAndEvents(t *testing.T) {
	tests := []struct {
		name       string
		result     workspace.Result
		outcome    string
		stableCode string
		hash       string
		invalidate bool
	}{
		{
			name: "completed",
			result: workspace.Result{
				Value:     map[string]any{"path": "notes.md", "operation": "write_replace", "complete": true, "publication_outcome": string(workspace.PublicationCompleted)},
				Reference: &workspace.Reference{Path: "notes.md", Kind: "file", ContentHash: "sha256:" + strings.Repeat("c", 64)}, Publication: workspace.PublicationCompleted,
			},
			outcome: agentsession.NoticeOutcomeCompleted, stableCode: agentsession.FilePublicationCompletedCode,
			hash: "sha256:" + strings.Repeat("c", 64),
		},
		{
			name: "unknown",
			result: workspace.Result{
				Value:     map[string]any{"error": workspace.CodeOutcomeUnknown, "code": workspace.CodeOutcomeUnknown, "path": "notes.md", "operation": "write_replace", "complete": false, "publication_outcome": string(workspace.PublicationUnknown)},
				Reference: &workspace.Reference{Path: "notes.md", Kind: "file", InvalidateObserved: true}, Publication: workspace.PublicationUnknown,
			},
			outcome: agentsession.NoticeOutcomeUnknown, stableCode: agentsession.FilePublicationUnknownCode, invalidate: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root, workspaceRoot := t.TempDir(), t.TempDir()
			secrets := &controllerSecretBackend{}
			model := &scriptedControllerModel{responses: []modelclient.Response{
				{Message: modelclient.Message{Role: "assistant", ToolCalls: []modelclient.ToolCall{{
					ID: "write-call", Type: "function", Function: modelclient.ToolFunction{Name: workspace.ToolWrite, Arguments: `{"path":"notes.md","mode":"replace","expected_hash":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","content":"new"}`},
				}}}},
				{Message: modelclient.Message{Role: "assistant", Content: "文件处理结束"}},
			}}
			executor := &controllerWorkspaceExecutor{
				status: workspace.Status{Available: true, Label: "workspace"},
				prepared: map[string]*workspace.PreparedMutation{workspace.ToolWrite: {
					Presentation: workspace.MutationPresentation{Tool: workspace.ToolWrite, Operation: "write_replace", Path: "notes.md"},
				}},
				commitResults: []workspace.Result{test.result},
			}
			server := &controllerServer{generation: api.MemoryGenerationStamp{LearnerGeneration: 1, MemoryGeneration: 1}}
			provider := Provider{Name: "ollama", Endpoint: "http://127.0.0.1:11434/v1", Model: "local"}
			store := controllerStore(t, root, secrets)
			dependencies := controllerDependencies(store, model, server, workspaceRoot, provider)
			dependencies.LoopOptions.Workspace = executor
			dependencies.LoopOptions.WorkspaceStatus = executor.status
			controller, err := Start(t.Context(), dependencies, false)
			if err != nil {
				t.Fatal(err)
			}
			pending, err := controller.Send(t.Context(), "更新文件")
			if err != nil || pending.PendingFileMutation == nil {
				t.Fatalf("pending=%+v err=%v", pending, err)
			}
			if _, err := controller.ResolveFileMutation(t.Context(), "write-call", agentloop.FileMutationApprove); err != nil {
				t.Fatal(err)
			}
			controller.mu.Lock()
			receipts := append([]agentsession.FileReceipt(nil), controller.record.FileReceipts...)
			controller.mu.Unlock()
			if len(receipts) != 1 {
				t.Fatalf("receipts=%+v", receipts)
			}
			receipt := receipts[0]
			if receipt.ToolCallID != "write-call" || receipt.Effect.Operation != "write_replace" || receipt.Effect.Target.Path != "notes.md" || receipt.Effect.Target.Kind != "file" ||
				receipt.Outcome != test.outcome || receipt.StableCode != test.stableCode || receipt.Effect.Target.Version != test.hash || receipt.InvalidateObserved != test.invalidate {
				t.Fatalf("receipt=%+v", receipt)
			}
			sessionID := controller.SessionID()
			if err := controller.Shutdown(t.Context()); err != nil {
				t.Fatal(err)
			}
			store = controllerStore(t, root, secrets)
			handle, loaded, err := store.OpenSession(t.Context(), sessionID)
			if err != nil {
				t.Fatal(err)
			}
			defer handle.Close()
			defer store.Close()
			if len(loaded.Record.FileReceipts) != 1 || loaded.Record.FileReceipts[0] != receipt {
				t.Fatalf("persisted receipts=%+v want=%+v", loaded.Record.FileReceipts, receipt)
			}
		})
	}
}

func TestControllerCrashRecoveryInvalidatesSamePathAndPersistsUnknownReceipt(t *testing.T) {
	root, workspaceRoot := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(workspaceRoot, "notes.md"), []byte("old target body"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspaceRoot, "other.md"), []byte("other path body"), 0o600); err != nil {
		t.Fatal(err)
	}
	secrets := &controllerSecretBackend{}
	targetHash := "sha256:" + strings.Repeat("a", 64)
	otherHash := "sha256:" + strings.Repeat("b", 64)
	model := &scriptedControllerModel{responses: []modelclient.Response{
		{Message: modelclient.Message{Role: "assistant", ToolCalls: []modelclient.ToolCall{
			{ID: "read-target", Type: "function", Function: modelclient.ToolFunction{Name: workspace.ToolRead, Arguments: `{"path":"notes.md"}`}},
			{ID: "read-other", Type: "function", Function: modelclient.ToolFunction{Name: workspace.ToolRead, Arguments: `{"path":"other.md"}`}},
		}}},
		{Message: modelclient.Message{Role: "assistant", Content: "已读取"}},
	}}
	executor := &controllerWorkspaceExecutor{
		status: workspace.Status{Available: true, Label: "workspace"},
		executeResults: []workspace.Result{
			{Value: map[string]any{"path": "notes.md", "content": "old target body", "content_hash": targetHash, "complete": true}, Reference: &workspace.Reference{Path: "notes.md", Kind: "file", ContentHash: targetHash}},
			{Value: map[string]any{"path": "other.md", "content": "other path body", "content_hash": otherHash, "complete": true}, Reference: &workspace.Reference{Path: "other.md", Kind: "file", ContentHash: otherHash}},
		},
	}
	server := &controllerServer{generation: api.MemoryGenerationStamp{LearnerGeneration: 1, MemoryGeneration: 1}}
	provider := Provider{Name: "ollama", Endpoint: "http://127.0.0.1:11434/v1", Model: "local"}
	store := controllerStore(t, root, secrets)
	dependencies := controllerDependencies(store, model, server, workspaceRoot, provider)
	dependencies.LoopOptions.Workspace = executor
	dependencies.LoopOptions.WorkspaceStatus = executor.status
	controller, err := Start(t.Context(), dependencies, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := controller.Send(t.Context(), "读取文件"); err != nil {
		t.Fatal(err)
	}
	if err := controller.BeginTurn(t.Context(), agentloop.DirtyIntent{TurnSequence: 2, OperationClass: "agent-turn"}); err != nil {
		t.Fatal(err)
	}
	if controller.dirty == nil || controller.dirty.TurnSequence != 2 || controller.dirty.MayHaveSideEffect {
		t.Fatalf("ordinary dirty marker=%+v", controller.dirty)
	}
	if err := controller.BeforeFilePublication(t.Context(), agentloop.FileWriteAhead{
		ToolCallID: "write-crash", Effect: fileeffects.New("write_replace", "", "notes.md", "file"),
	}); err != nil {
		t.Fatal(err)
	}
	if controller.dirty == nil || !controller.dirty.MayHaveSideEffect || controller.dirty.File == nil ||
		controller.dirty.File.StableCode != agentsession.FilePublicationUnknownCode {
		t.Fatalf("file dirty marker=%+v", controller.dirty)
	}
	sessionID := controller.SessionID()
	controller.abort()

	store = controllerStore(t, root, secrets)
	resumed, err := Resume(t.Context(), controllerDependencies(store, &controllerModel{}, server, workspaceRoot, provider), ResumeOptions{SessionID: sessionID, CurrentWorkspace: workspaceRoot})
	if err != nil {
		t.Fatal(err)
	}
	resumed.mu.Lock()
	receipts := append([]agentsession.FileReceipt(nil), resumed.record.FileReceipts...)
	resumed.mu.Unlock()
	if len(receipts) != 1 || receipts[0].Effect.Target.Path != "notes.md" || receipts[0].Effect.Target.Kind != "file" ||
		receipts[0].Outcome != agentsession.NoticeOutcomeUnknown || receipts[0].StableCode != agentsession.FilePublicationUnknownCode || !receipts[0].InvalidateObserved {
		t.Fatalf("recovery receipts=%+v", receipts)
	}
	checkpoint, err := resumed.loop.ExportCheckpoint()
	if err != nil {
		t.Fatal(err)
	}
	var target, other *agentloop.WorkspaceReference
	for _, current := range checkpoint.WorkspaceReferences {
		value := current.Value
		switch current.Key {
		case "read-target":
			target = &value
		case "read-other":
			other = &value
		}
	}
	if target == nil || target.ContentHash != "" || !target.InvalidateObserved || other == nil || other.ContentHash != otherHash || other.InvalidateObserved {
		t.Fatalf("workspace references target=%+v other=%+v", target, other)
	}
	for _, history := range checkpoint.ToolHistory {
		if history.Key == "read-target" && (strings.Contains(history.Value, "old target body") || !strings.Contains(history.Value, agentsession.FilePublicationUnknownCode)) {
			t.Fatalf("target history=%s", history.Value)
		}
		if history.Key == "read-other" && !strings.Contains(history.Value, "other path body") {
			t.Fatalf("other history=%s", history.Value)
		}
	}
	foundTargetSource := false
	for _, source := range checkpoint.Context.Sources {
		if source.WorkspaceReference != nil && source.WorkspaceReference.Path == "notes.md" {
			foundTargetSource = true
			if source.Freshness != agentloop.FreshnessWorkspaceSuperseded || source.SourceAvailable || source.RecallText != "" || strings.Contains(source.ModelMessage.Content, "old target body") {
				t.Fatalf("target source=%+v", source)
			}
		}
	}
	if !foundTargetSource {
		t.Fatal("matching workspace source was not retained as stale metadata")
	}
	if err := resumed.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
	store = controllerStore(t, root, secrets)
	handle, loaded, err := store.OpenSession(t.Context(), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	defer handle.Close()
	defer store.Close()
	if loaded.Interrupted != nil || len(loaded.Record.FileReceipts) != 1 || loaded.Record.FileReceipts[0].StableCode != agentsession.FilePublicationUnknownCode {
		t.Fatalf("persisted recovery state=%+v", loaded)
	}
}

func TestControllerPersistsDirtyWriteAheadAndConsumesItOnResume(t *testing.T) {
	root, workspaceRoot := t.TempDir(), t.TempDir()
	secrets := &controllerSecretBackend{}
	model := &controllerModel{}
	server := &controllerServer{generation: api.MemoryGenerationStamp{LearnerGeneration: 1, MemoryGeneration: 1}}
	provider := Provider{Name: "ollama", Endpoint: "http://127.0.0.1:11434/v1", Model: "local"}

	store := controllerStore(t, root, secrets)
	controller, err := Start(t.Context(), controllerDependencies(store, model, server, workspaceRoot, provider), false)
	if err != nil {
		t.Fatal(err)
	}
	if err := controller.BeginTurn(t.Context(), agentloop.DirtyIntent{TurnSequence: 1, OperationClass: "agent-turn"}); err != nil {
		t.Fatal(err)
	}
	if controller.dirty == nil || controller.dirty.TurnSequence != 1 || controller.dirty.MayHaveSideEffect {
		t.Fatalf("ordinary marker=%+v", controller.dirty)
	}
	if err := controller.BeforePreferenceWrite(t.Context(), agentloop.PreferenceWriteAhead{
		ToolCallID: "call-pref", CreateOperationID: "10000000-0000-4000-8000-000000000001",
		AdmitOperationID: "10000000-0000-4000-8000-000000000002", RejectOperationID: "10000000-0000-4000-8000-000000000003",
		Payload: agentloop.PreferencePayload{Content: "图示", Reason: "用户明确要求", Category: "interaction_preference", Sensitivity: "non_sensitive", Stability: "stable", ValidUntil: time.Date(2036, 1, 1, 0, 0, 0, 0, time.UTC)},
		Stage:   agentloop.PreferenceStageCreate, StableCode: "preference_create_pending",
	}); err != nil {
		t.Fatal(err)
	}
	if controller.dirty == nil || !controller.dirty.MayHaveSideEffect || controller.dirty.Preference == nil {
		t.Fatalf("preference marker=%+v", controller.dirty)
	}
	sessionID := controller.SessionID()
	controller.abort()

	store = controllerStore(t, root, secrets)
	handle, loaded, err := store.OpenSession(t.Context(), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Interrupted == nil || loaded.Interrupted.Preference == nil || loaded.Interrupted.Preference.CreateOperationID != "10000000-0000-4000-8000-000000000001" {
		t.Fatalf("write-ahead receipt missing: %+v", loaded.Interrupted)
	}
	_ = handle.Close()
	_ = store.Close()

	store = controllerStore(t, root, secrets)
	resumed, err := Resume(t.Context(), controllerDependencies(store, model, server, workspaceRoot, provider), ResumeOptions{SessionID: sessionID, CurrentWorkspace: workspaceRoot})
	if err != nil {
		t.Fatal(err)
	}
	if len(resumed.Status().Notices) == 0 {
		t.Fatal("interrupted resume notice missing")
	}
	resumed.Close()

	store = controllerStore(t, root, secrets)
	handle, loaded, err = store.OpenSession(t.Context(), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	defer handle.Close()
	defer store.Close()
	if loaded.Interrupted != nil || len(loaded.Record.PreferenceReceipts) != 1 || loaded.Record.PreferenceReceipts[0].Outcome != agentsession.NoticeOutcomeUnknown {
		t.Fatalf("dirty receipt was not converted to retry-only recovery state: %+v", loaded)
	}
}

func TestControllerNormalPreferenceSaveCommitsCompletedTypedReceipt(t *testing.T) {
	root, workspaceRoot := t.TempDir(), t.TempDir()
	secrets := &controllerSecretBackend{}
	model := &scriptedControllerModel{responses: []modelclient.Response{
		{
			Message: modelclient.Message{
				Role: "assistant",
				ToolCalls: []modelclient.ToolCall{{
					ID: "call-pref", Type: "function",
					Function: modelclient.ToolFunction{Name: "remember_preference", Arguments: `{"content":"回答先给结论","reason":"用户明确要求","category":"interaction_preference","sensitivity":"non_sensitive","stability":"stable"}`},
				}},
			},
		},
		{Message: modelclient.Message{Role: "assistant", Content: "已保存偏好"}},
	}}
	server := &controllerServer{generation: api.MemoryGenerationStamp{LearnerGeneration: 1, MemoryGeneration: 1}}
	provider := Provider{Name: "ollama", Endpoint: "http://127.0.0.1:11434/v1", Model: "local"}
	store := controllerStore(t, root, secrets)
	controller, err := Start(t.Context(), controllerDependencies(store, model, server, workspaceRoot, provider), false)
	if err != nil {
		t.Fatal(err)
	}
	result, err := controller.Send(t.Context(), "请记住")
	if err != nil || result.Pending == nil {
		t.Fatalf("pending=%+v err=%v", result, err)
	}
	if _, err := controller.ResolvePreference(t.Context(), agentloop.PreferenceSave); err != nil {
		t.Fatal(err)
	}
	controller.mu.Lock()
	receipts := append([]agentsession.PreferenceReceipt(nil), controller.record.PreferenceReceipts...)
	controller.mu.Unlock()
	if len(receipts) != 1 || receipts[0].Outcome != agentsession.NoticeOutcomeCompleted || receipts[0].Stage != agentsession.PreferenceStageAdmit ||
		receipts[0].StableCode != "preference_saved" || receipts[0].CandidateID == "" || receipts[0].CandidateRevision < 2 {
		t.Fatalf("completed receipt=%+v", receipts)
	}
	if err := controller.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestControllerRetriesPersistedPreferenceReceiptWithExactTypedState(t *testing.T) {
	root, workspaceRoot := t.TempDir(), t.TempDir()
	secrets := &controllerSecretBackend{}
	model := &controllerModel{}
	server := &controllerServer{
		generation: api.MemoryGenerationStamp{LearnerGeneration: 1, MemoryGeneration: 1},
		candidate:  api.MemoryCandidate{ID: "candidate-1", Status: "pending_review", Revision: 1},
	}
	provider := Provider{Name: "ollama", Endpoint: "http://127.0.0.1:11434/v1", Model: "local"}
	validUntil := time.Date(2036, 1, 1, 0, 0, 0, 0, time.UTC)
	writeAhead := agentloop.PreferenceWriteAhead{
		ToolCallID: "call-pref", CreateOperationID: "10000000-0000-4000-8000-000000000001",
		AdmitOperationID: "10000000-0000-4000-8000-000000000002", RejectOperationID: "10000000-0000-4000-8000-000000000003",
		Payload: agentloop.PreferencePayload{Content: "回答先给结论", Reason: "用户明确要求", Category: "interaction_preference", Sensitivity: "non_sensitive", Stability: "stable", ValidUntil: validUntil},
		Stage:   agentloop.PreferenceStageCreate, StableCode: "preference_create_pending",
	}

	store := controllerStore(t, root, secrets)
	controller, err := Start(t.Context(), controllerDependencies(store, model, server, workspaceRoot, provider), false)
	if err != nil {
		t.Fatal(err)
	}
	if err := controller.BeginTurn(t.Context(), agentloop.DirtyIntent{TurnSequence: 1, OperationClass: "agent-turn"}); err != nil {
		t.Fatal(err)
	}
	if err := controller.BeforePreferenceWrite(t.Context(), writeAhead); err != nil {
		t.Fatal(err)
	}
	sessionID := controller.SessionID()
	controller.abort()

	store = controllerStore(t, root, secrets)
	resumed, err := Resume(t.Context(), controllerDependencies(store, model, server, workspaceRoot, provider), ResumeOptions{SessionID: sessionID, CurrentWorkspace: workspaceRoot})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := resumed.loop.ExportCheckpoint(); err != nil {
		t.Fatalf("retry receipt restored executable pending state: %v", err)
	}
	server.createErr = errors.New("temporary transport failure")
	if err := resumed.RetryPreferenceReceipt(t.Context(), writeAhead.CreateOperationID); err == nil {
		t.Fatal("unknown create retry unexpectedly succeeded")
	}
	resumed.mu.Lock()
	unknown := resumed.record.PreferenceReceipts[0]
	resumed.mu.Unlock()
	if unknown.Outcome != agentsession.NoticeOutcomeUnknown || unknown.Stage != agentsession.PreferenceStageCreate {
		t.Fatalf("unknown retry receipt changed terminal state: %+v", unknown)
	}
	server.createErr = nil
	if err := resumed.RetryPreferenceReceipt(t.Context(), writeAhead.CreateOperationID); err != nil {
		t.Fatal(err)
	}
	if len(server.createRequests) != 2 || len(server.decisionRequests) != 1 ||
		server.createRequests[0].OperationID != writeAhead.CreateOperationID || server.createRequests[1].OperationID != writeAhead.CreateOperationID ||
		server.createRequests[0].Content != writeAhead.Payload.Content || server.createRequests[1].Content != writeAhead.Payload.Content ||
		!server.createRequests[0].ValidUntil.Equal(validUntil) || server.decisionRequests[0].OperationID != writeAhead.AdmitOperationID ||
		server.decisionRequests[0].Decision != "admit" {
		t.Fatalf("retry changed typed payload or operation IDs: create=%+v decide=%+v", server.createRequests, server.decisionRequests)
	}
	if err := resumed.RetryPreferenceReceipt(t.Context(), writeAhead.CreateOperationID); !errors.Is(err, ErrPreferenceReceiptNotFound) {
		t.Fatalf("completed receipt was retryable: %v", err)
	}
	if err := resumed.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}

	store = controllerStore(t, root, secrets)
	handle, loaded, err := store.OpenSession(t.Context(), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	defer handle.Close()
	defer store.Close()
	if len(loaded.Record.PreferenceReceipts) != 1 {
		t.Fatalf("receipts=%+v", loaded.Record.PreferenceReceipts)
	}
	receipt := loaded.Record.PreferenceReceipts[0]
	if receipt.Outcome != agentsession.NoticeOutcomeCompleted || receipt.Stage != agentsession.PreferenceStageAdmit ||
		receipt.CandidateID != "candidate-1" || receipt.CandidateRevision != 2 || receipt.RejectOperationID != writeAhead.RejectOperationID ||
		receipt.Payload.Content != writeAhead.Payload.Content || !receipt.Payload.ValidUntil.Equal(validUntil) {
		t.Fatalf("completed retry receipt=%+v", receipt)
	}
}

func TestControllerRetryPreferenceRejectUsesOriginalRejectOperationID(t *testing.T) {
	root, workspaceRoot := t.TempDir(), t.TempDir()
	secrets := &controllerSecretBackend{}
	model := &controllerModel{}
	server := &controllerServer{
		generation: api.MemoryGenerationStamp{LearnerGeneration: 1, MemoryGeneration: 1},
		candidate:  api.MemoryCandidate{ID: "candidate-reject", Status: "pending_review", Revision: 5},
	}
	provider := Provider{Name: "ollama", Endpoint: "http://127.0.0.1:11434/v1", Model: "local"}
	writeAhead := agentloop.PreferenceWriteAhead{
		ToolCallID: "call-reject", CreateOperationID: "20000000-0000-4000-8000-000000000001",
		AdmitOperationID: "20000000-0000-4000-8000-000000000002", RejectOperationID: "20000000-0000-4000-8000-000000000003",
		Payload:     agentloop.PreferencePayload{Content: "回答简洁", Reason: "用户明确要求", Category: "interaction_preference", Sensitivity: "non_sensitive", Stability: "stable", ValidUntil: time.Date(2036, 1, 1, 0, 0, 0, 0, time.UTC)},
		CandidateID: "candidate-reject", CandidateRevision: 4, Stage: agentloop.PreferenceStageReject, StableCode: "admission_forbidden",
	}
	store := controllerStore(t, root, secrets)
	controller, err := Start(t.Context(), controllerDependencies(store, model, server, workspaceRoot, provider), false)
	if err != nil {
		t.Fatal(err)
	}
	if err := controller.BeginTurn(t.Context(), agentloop.DirtyIntent{TurnSequence: 1, OperationClass: "agent-turn"}); err != nil {
		t.Fatal(err)
	}
	if err := controller.BeforePreferenceWrite(t.Context(), writeAhead); err != nil {
		t.Fatal(err)
	}
	sessionID := controller.SessionID()
	controller.abort()

	store = controllerStore(t, root, secrets)
	resumed, err := Resume(t.Context(), controllerDependencies(store, model, server, workspaceRoot, provider), ResumeOptions{SessionID: sessionID, CurrentWorkspace: workspaceRoot})
	if err != nil {
		t.Fatal(err)
	}
	if err := resumed.RetryPreferenceReceipt(t.Context(), writeAhead.CreateOperationID); err != nil {
		t.Fatal(err)
	}
	if len(server.createRequests) != 0 || len(server.decisionRequests) != 1 || server.decisionRequests[0].OperationID != writeAhead.RejectOperationID || server.decisionRequests[0].ExpectedRevision != 5 || server.decisionRequests[0].Decision != "reject" {
		t.Fatalf("reject retry changed stage or ID: create=%+v decision=%+v", server.createRequests, server.decisionRequests)
	}
	resumed.mu.Lock()
	receipt := resumed.record.PreferenceReceipts[0]
	resumed.mu.Unlock()
	if receipt.Outcome != agentsession.NoticeOutcomeRejected || receipt.StableCode != "admission_forbidden" || receipt.CandidateRevision != 6 {
		t.Fatalf("rejected receipt=%+v", receipt)
	}
	if err := resumed.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestControllerPrivacyFailureQuarantinesAndLaterRevalidates(t *testing.T) {
	root, workspaceRoot := t.TempDir(), t.TempDir()
	secrets := &controllerSecretBackend{}
	model := &controllerModel{}
	provider := Provider{Name: "ollama", Endpoint: "http://127.0.0.1:11434/v1", Model: "local"}
	server := &controllerServer{generation: api.MemoryGenerationStamp{LearnerGeneration: 7, MemoryGeneration: 9}}

	store := controllerStore(t, root, secrets)
	controller, err := Start(t.Context(), controllerDependencies(store, model, server, workspaceRoot, provider), false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := controller.Send(t.Context(), "需要隐私围栏的历史问题"); err != nil {
		t.Fatal(err)
	}
	sessionID := controller.SessionID()
	controller.Close()

	server.exportErr = errors.New("offline")
	store = controllerStore(t, root, secrets)
	quarantined, err := Resume(t.Context(), controllerDependencies(store, model, server, workspaceRoot, provider), ResumeOptions{SessionID: sessionID, CurrentWorkspace: workspaceRoot})
	if err != nil {
		t.Fatal(err)
	}
	quarantined.Close()

	store = controllerStore(t, root, secrets)
	handle, loaded, err := store.OpenSession(t.Context(), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Record.PrivacyVerified || len(loaded.Record.QuarantinedCheckpoint) == 0 {
		t.Fatalf("privacy quarantine was not retained: verified=%t protected=%d", loaded.Record.PrivacyVerified, len(loaded.Record.QuarantinedCheckpoint))
	}
	_ = handle.Close()
	_ = store.Close()

	server.exportErr = nil
	store = controllerStore(t, root, secrets)
	revalidated, err := Resume(t.Context(), controllerDependencies(store, model, server, workspaceRoot, provider), ResumeOptions{SessionID: sessionID, CurrentWorkspace: workspaceRoot})
	if err != nil {
		t.Fatal(err)
	}
	if len(revalidated.Status().Notices) == 0 {
		t.Fatal("privacy revalidation notice missing")
	}
	revalidated.Close()

	store = controllerStore(t, root, secrets)
	handle, loaded, err = store.OpenSession(t.Context(), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	defer handle.Close()
	defer store.Close()
	if !loaded.Record.PrivacyVerified || len(loaded.Record.QuarantinedCheckpoint) != 0 {
		t.Fatalf("privacy quarantine was not cleared after matching generation: verified=%t protected=%d", loaded.Record.PrivacyVerified, len(loaded.Record.QuarantinedCheckpoint))
	}
}

func TestControllerProviderChangeBlocksHistoricalTransmissionUntilConfirmed(t *testing.T) {
	root, workspaceRoot := t.TempDir(), t.TempDir()
	secrets := &controllerSecretBackend{}
	model := &controllerModel{}
	server := &controllerServer{generation: api.MemoryGenerationStamp{LearnerGeneration: 1, MemoryGeneration: 1}}
	original := Provider{Name: "ollama", Endpoint: "http://127.0.0.1:11434/v1", Model: "local"}

	store := controllerStore(t, root, secrets)
	controller, err := Start(t.Context(), controllerDependencies(store, model, server, workspaceRoot, original), false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := controller.Send(t.Context(), "历史问题"); err != nil {
		t.Fatal(err)
	}
	sessionID := controller.SessionID()
	controller.Close()

	changed := Provider{Name: "custom", Endpoint: "https://gateway.example/v1", Model: "new"}
	store = controllerStore(t, root, secrets)
	resumed, err := Resume(t.Context(), controllerDependencies(store, model, server, workspaceRoot, changed), ResumeOptions{SessionID: sessionID, CurrentWorkspace: workspaceRoot})
	if err != nil {
		t.Fatal(err)
	}
	defer resumed.Close()
	if !resumed.Status().ProviderConfirmationRequired {
		t.Fatal("provider gate was not raised")
	}
	if _, err := resumed.Send(t.Context(), "不得发送"); !errors.Is(err, ErrProviderConfirmationRequired) {
		t.Fatalf("send error=%v, want provider gate", err)
	}
	if err := resumed.ConfirmProvider(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := resumed.Send(t.Context(), "确认后发送"); err != nil {
		t.Fatal(err)
	}
}

func TestControllerSwitchProviderChangeCanInstallLocallyWithoutTransmission(t *testing.T) {
	root, workspaceRoot := t.TempDir(), t.TempDir()
	secrets := &controllerSecretBackend{}
	server := &controllerServer{generation: api.MemoryGenerationStamp{LearnerGeneration: 1, MemoryGeneration: 1}}
	originalModel := &controllerModel{}
	original := Provider{Name: "ollama", Endpoint: "http://127.0.0.1:11434/v1", Model: "local"}

	target, err := Start(t.Context(), controllerDependencies(controllerStore(t, root, secrets), originalModel, server, workspaceRoot, original), false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := target.Send(t.Context(), "仅本地查看的历史正文"); err != nil {
		t.Fatal(err)
	}
	targetID := target.SessionID()
	if err := target.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
	deleteCandidate, err := Start(t.Context(), controllerDependencies(controllerStore(t, root, secrets), originalModel, server, workspaceRoot, original), false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := deleteCandidate.Send(t.Context(), "待本地删除的 Session"); err != nil {
		t.Fatal(err)
	}
	deleteCandidateID := deleteCandidate.SessionID()
	if err := deleteCandidate.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}

	changedModel := &controllerModel{}
	changed := Provider{Name: "custom", Endpoint: "https://gateway.example/v1", Model: "new"}
	current, err := Start(t.Context(), controllerDependencies(controllerStore(t, root, secrets), changedModel, server, workspaceRoot, changed), false)
	if err != nil {
		t.Fatal(err)
	}
	defer current.Close()
	items, err := current.ListSessions(t.Context(), SessionListRequest{})
	if err != nil {
		t.Fatal(err)
	}
	var summary agentsession.Summary
	for _, item := range items {
		if item.Summary.SessionID == targetID {
			summary = item.Summary
		}
	}
	plan := current.PlanSwitch(summary)
	if !plan.NeedProviderConfirm || plan.NeedWorkspaceConfirm {
		t.Fatalf("unexpected switch plan: %+v", plan)
	}
	generation, err := current.CommitSwitch(t.Context(), plan, SwitchConfirmation{Provider: false})
	if err != nil {
		t.Fatal(err)
	}
	if generation != 2 || current.SessionID() != targetID || !current.Status().ProviderConfirmationRequired {
		t.Fatalf("generation=%d session=%s status=%+v", generation, current.SessionID(), current.Status())
	}
	foundHistoricalText := false
	for _, entry := range current.SessionTranscript().Entries {
		foundHistoricalText = foundHistoricalText || entry.Kind == agentsession.TranscriptKindUser && entry.Text == "仅本地查看的历史正文"
	}
	if !foundHistoricalText {
		t.Fatalf("local transcript was not installed: %+v", current.SessionTranscript().Entries)
	}

	items, err = current.ListSessions(t.Context(), SessionListRequest{})
	if err != nil {
		t.Fatal(err)
	}
	var currentSummary agentsession.Summary
	for _, item := range items {
		if item.Current {
			currentSummary = item.Summary
		}
	}
	renamed, err := current.RenameSession(t.Context(), targetID, "本地历史", currentSummary.RecordRevision)
	if err != nil || renamed.Title != "本地历史" {
		t.Fatalf("renamed=%+v err=%v", renamed, err)
	}
	items, err = current.ListSessions(t.Context(), SessionListRequest{})
	if err != nil {
		t.Fatal(err)
	}
	var deleteSummary agentsession.Summary
	for _, item := range items {
		if item.Summary.SessionID == deleteCandidateID {
			deleteSummary = item.Summary
		}
	}
	if deleteSummary.SessionID == "" {
		t.Fatal("delete candidate missing while provider gate is active")
	}
	if err := current.DeleteSession(t.Context(), agentsession.DeleteTarget{SessionID: deleteSummary.SessionID, StorageID: deleteSummary.StorageID, ExpectedRecordRevision: deleteSummary.RecordRevision}); err != nil {
		t.Fatalf("delete while provider-blocked: %v", err)
	}
	if _, err := current.Send(t.Context(), "确认前不得发送"); !errors.Is(err, ErrProviderConfirmationRequired) {
		t.Fatalf("send error=%v, want provider gate", err)
	}
	time.Sleep(20 * time.Millisecond)
	changedModel.mu.Lock()
	requestsBeforeConfirmation := len(changedModel.requests)
	changedModel.mu.Unlock()
	if requestsBeforeConfirmation != 0 {
		t.Fatalf("new provider received %d model/title requests before confirmation", requestsBeforeConfirmation)
	}

	if err := current.ConfirmProvider(t.Context()); err != nil {
		t.Fatal(err)
	}
	if current.Status().ProviderConfirmationRequired {
		t.Fatal("provider gate remained after explicit confirmation")
	}
	if _, err := current.Send(t.Context(), "确认后允许发送"); err != nil {
		t.Fatal(err)
	}
	changedModel.mu.Lock()
	requestsAfterConfirmation := len(changedModel.requests)
	changedModel.mu.Unlock()
	if requestsAfterConfirmation == 0 {
		t.Fatal("explicit provider confirmation did not enable model transmission")
	}

	if _, err := current.NewSession(t.Context()); err != nil {
		t.Fatal(err)
	}
	items, err = current.ListSessions(t.Context(), SessionListRequest{})
	if err != nil {
		t.Fatal(err)
	}
	var oldTarget agentsession.Summary
	for _, item := range items {
		if item.Summary.SessionID == targetID {
			oldTarget = item.Summary
		}
	}
	if oldTarget.SessionID == "" {
		t.Fatal("locally opened Session missing before delete")
	}
	if err := current.DeleteSession(t.Context(), agentsession.DeleteTarget{SessionID: targetID, StorageID: oldTarget.StorageID, ExpectedRecordRevision: oldTarget.RecordRevision}); err != nil {
		t.Fatal(err)
	}
}

func TestControllerSwitchLocalProviderViewDoesNotBypassWorkspaceConfirmation(t *testing.T) {
	root, currentWorkspace, targetWorkspace := t.TempDir(), t.TempDir(), t.TempDir()
	secrets := &controllerSecretBackend{}
	server := &controllerServer{generation: api.MemoryGenerationStamp{LearnerGeneration: 1, MemoryGeneration: 1}}
	original := Provider{Name: "ollama", Endpoint: "http://127.0.0.1:11434/v1", Model: "local"}
	target, err := Start(t.Context(), controllerDependencies(controllerStore(t, root, secrets), &controllerModel{}, server, targetWorkspace, original), false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := target.Send(t.Context(), "跨工作区历史正文"); err != nil {
		t.Fatal(err)
	}
	targetID := target.SessionID()
	if err := target.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}

	changedModel := &controllerModel{}
	changed := Provider{Name: "custom", Endpoint: "https://gateway.example/v1", Model: "new"}
	current, err := Start(t.Context(), controllerDependencies(controllerStore(t, root, secrets), changedModel, server, currentWorkspace, changed), false)
	if err != nil {
		t.Fatal(err)
	}
	defer current.Close()
	items, err := current.ListSessions(t.Context(), SessionListRequest{All: true})
	if err != nil {
		t.Fatal(err)
	}
	var summary agentsession.Summary
	for _, item := range items {
		if item.Summary.SessionID == targetID {
			summary = item.Summary
		}
	}
	plan := current.PlanSwitch(summary)
	if !plan.NeedProviderConfirm || !plan.NeedWorkspaceConfirm {
		t.Fatalf("unexpected switch plan: %+v", plan)
	}
	currentID, generation := current.SessionID(), current.Generation()
	if _, err := current.CommitSwitch(t.Context(), plan, SwitchConfirmation{Provider: false, Workspace: false}); !errors.Is(err, ErrWorkspaceConfirmationRequired) {
		t.Fatalf("workspace refusal error=%v", err)
	}
	if current.SessionID() != currentID || current.Generation() != generation {
		t.Fatal("workspace refusal changed the active Session")
	}
	if _, err := current.CommitSwitch(t.Context(), plan, SwitchConfirmation{Provider: false, Workspace: true}); err != nil {
		t.Fatal(err)
	}
	if current.SessionID() != targetID || !current.Status().ProviderConfirmationRequired {
		t.Fatalf("local cross-workspace target not installed safely: session=%s status=%+v", current.SessionID(), current.Status())
	}
	changedModel.mu.Lock()
	requestCount := len(changedModel.requests)
	changedModel.mu.Unlock()
	if requestCount != 0 {
		t.Fatalf("new provider received %d requests during local cross-workspace install", requestCount)
	}
}

func TestControllerSwitchProviderConfirmationStillEnablesTransmission(t *testing.T) {
	root, workspaceRoot := t.TempDir(), t.TempDir()
	secrets := &controllerSecretBackend{}
	server := &controllerServer{generation: api.MemoryGenerationStamp{LearnerGeneration: 1, MemoryGeneration: 1}}
	original := Provider{Name: "ollama", Endpoint: "http://127.0.0.1:11434/v1", Model: "local"}
	target, err := Start(t.Context(), controllerDependencies(controllerStore(t, root, secrets), &controllerModel{}, server, workspaceRoot, original), false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := target.Send(t.Context(), "待确认历史"); err != nil {
		t.Fatal(err)
	}
	targetID := target.SessionID()
	if err := target.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}

	changedModel := &controllerModel{}
	changed := Provider{Name: "custom", Endpoint: "https://gateway.example/v1", Model: "new"}
	current, err := Start(t.Context(), controllerDependencies(controllerStore(t, root, secrets), changedModel, server, workspaceRoot, changed), false)
	if err != nil {
		t.Fatal(err)
	}
	defer current.Close()
	items, err := current.ListSessions(t.Context(), SessionListRequest{})
	if err != nil {
		t.Fatal(err)
	}
	var summary agentsession.Summary
	for _, item := range items {
		if item.Summary.SessionID == targetID {
			summary = item.Summary
		}
	}
	if _, err := current.CommitSwitch(t.Context(), current.PlanSwitch(summary), SwitchConfirmation{Provider: true}); err != nil {
		t.Fatal(err)
	}
	if current.Status().ProviderConfirmationRequired {
		t.Fatal("provider-confirmed switch remained blocked")
	}
	if _, err := current.Send(t.Context(), "切换时已确认"); err != nil {
		t.Fatal(err)
	}
	changedModel.mu.Lock()
	requestCount := len(changedModel.requests)
	changedModel.mu.Unlock()
	if requestCount == 0 {
		t.Fatal("provider-confirmed switch did not enable transmission")
	}
}

func TestControllerClassifiesCheckpointVersionAndBrokenReferences(t *testing.T) {
	tests := []struct {
		name       string
		mutate     func(map[string]any)
		want       error
		stableCode string
	}{
		{
			name: "future schema",
			mutate: func(value map[string]any) {
				value["schema_version"] = agentloop.SessionCheckpointSchemaVersion + 1
			},
			want:       agentsession.ErrVersionUnsupported,
			stableCode: agentsession.ErrorCodeVersionUnsupported,
		},
		{
			name: "broken message reference",
			mutate: func(value map[string]any) {
				turnIDs := value["message_turn_ids"].([]any)
				turnIDs[0] = "turn-999"
			},
			want:       agentsession.ErrCorrupt,
			stableCode: agentsession.ErrorCodeCorrupt,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root, workspaceRoot := t.TempDir(), t.TempDir()
			secrets := &controllerSecretBackend{}
			model := &controllerModel{}
			server := &controllerServer{generation: api.MemoryGenerationStamp{LearnerGeneration: 1, MemoryGeneration: 1}}
			provider := Provider{Name: "ollama", Endpoint: "http://127.0.0.1:11434/v1", Model: "local"}

			controller, err := Start(t.Context(), controllerDependencies(controllerStore(t, root, secrets), model, server, workspaceRoot, provider), false)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := controller.Send(t.Context(), "建立稳定检查点"); err != nil {
				t.Fatal(err)
			}
			sessionID := controller.SessionID()
			if err := controller.Shutdown(t.Context()); err != nil {
				t.Fatal(err)
			}

			store := controllerStore(t, root, secrets)
			handle, loaded, err := store.OpenSession(t.Context(), sessionID)
			if err != nil {
				t.Fatal(err)
			}
			var checkpoint map[string]any
			if err := json.Unmarshal(loaded.Record.Checkpoint, &checkpoint); err != nil {
				t.Fatal(err)
			}
			test.mutate(checkpoint)
			mutated, err := json.Marshal(checkpoint)
			if err != nil {
				t.Fatal(err)
			}
			candidate := loaded.Record
			candidate.Checkpoint = mutated
			if _, err := handle.Save(t.Context(), loaded.Record.RecordRevision, candidate); err != nil {
				t.Fatal(err)
			}
			if err := handle.Close(); err != nil {
				t.Fatal(err)
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}

			_, err = Resume(t.Context(), controllerDependencies(controllerStore(t, root, secrets), model, server, workspaceRoot, provider), ResumeOptions{SessionID: sessionID, CurrentWorkspace: workspaceRoot})
			if !errors.Is(err, test.want) {
				t.Fatalf("resume error=%v, want %v", err, test.want)
			}
			if code := agentsession.StableErrorCode(err); code != test.stableCode {
				t.Fatalf("stable code=%q, want %q", code, test.stableCode)
			}
			if strings.Contains(err.Error(), "turn-999") || strings.Contains(err.Error(), "message_turn_ids") || strings.Contains(err.Error(), "schema_version") {
				t.Fatalf("raw checkpoint detail leaked: %v", err)
			}
		})
	}
}

func TestControllerGeneratesToolFreeAutomaticTitleAfterToolTurn(t *testing.T) {
	root, workspaceRoot := t.TempDir(), t.TempDir()
	secrets := &controllerSecretBackend{}
	model := &toolTitleModel{}
	server := &controllerServer{
		generation: api.MemoryGenerationStamp{LearnerGeneration: 1, MemoryGeneration: 1},
		retrieveResult: api.KnowledgeRetrievalResult{
			Hits: []api.RetrievalHit{{CanonicalSlice: "服务端工具结果正文"}},
		},
	}
	provider := Provider{Name: "ollama", Endpoint: "http://127.0.0.1:11434/v1", Model: "local"}

	store := controllerStore(t, root, secrets)
	controller, err := Start(t.Context(), controllerDependencies(store, model, server, workspaceRoot, provider), false)
	if err != nil {
		t.Fatal(err)
	}
	defer controller.Close()
	if _, err := controller.Send(t.Context(), "首个工具问题"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for {
		controller.mu.Lock()
		title := controller.record.Title
		controller.mu.Unlock()
		if title == "代数复习" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("automatic title was not saved, current=%q", title)
		}
		time.Sleep(5 * time.Millisecond)
	}

	model.mu.Lock()
	requests := append([]modelclient.Request(nil), model.requests...)
	model.mu.Unlock()
	found := false
	for _, request := range requests {
		if request.MaxTokens != 96 || request.ReasoningEffort != modelclient.ReasoningEffortNone {
			continue
		}
		found = true
		if request.Tools != nil || len(request.Messages) != 2 {
			t.Fatalf("unsafe automatic title request: %+v", request)
		}
		if input := request.Messages[1].Content; input != "当前标题：首个工具问题\n用户：首个工具问题" {
			t.Fatalf("tool turn contaminated title input: %q", input)
		}
	}
	if !found {
		t.Fatalf("tool-free title request missing: %+v", requests)
	}
}

func TestTitleInputIncludesOnlySafeCommittedUserAndFinalAssistantTurns(t *testing.T) {
	checkpoint := agentloop.SessionCheckpoint{
		Turns: []agentloop.CheckpointTurn{
			{ID: "turn-1", Completed: true}, {ID: "turn-2", Completed: true},
			{ID: "turn-3", Completed: true}, {ID: "turn-4", Completed: true, Protected: true},
		},
		Messages: []modelclient.Message{
			{Role: "user", Content: "首条安全问题"}, {Role: "assistant", Content: "首条安全回答"},
			{Role: "user", Content: "工具问题"}, {Role: "assistant", Content: "相关工具助手", ToolCalls: []modelclient.ToolCall{{ID: "call-1", Type: "function", Function: modelclient.ToolFunction{Name: "search_knowledge", Arguments: `{}`}}}}, {Role: "tool", ToolCallID: "call-1", Content: "服务端正文"},
			{Role: "user", Content: "最近安全问题"}, {Role: "assistant", Content: "最近安全回答"},
			{Role: "user", Content: "受保护问题"}, {Role: "assistant", Content: "受保护回答"},
		},
		MessageTurnIDs: []string{"turn-1", "turn-1", "turn-2", "turn-2", "turn-2", "turn-3", "turn-3", "turn-4", "turn-4"},
		Context:        agentloop.CheckpointContext{Sources: []agentloop.CheckpointSource{{TurnID: "turn-4", ServerReference: &agentloop.ServerReference{}}}},
	}
	input := titleInput(checkpoint, "当前自动标题", agentsession.DefaultLimits())
	for _, wanted := range []string{"当前标题：当前自动标题", "用户：首条安全问题", "助手：首条安全回答", "用户：工具问题", "用户：最近安全问题", "助手：最近安全回答", "用户：受保护问题"} {
		if !strings.Contains(input, wanted) {
			t.Fatalf("title input missing %q: %q", wanted, input)
		}
	}
	for _, forbidden := range []string{"服务端正文", "search_knowledge", "相关工具助手", "受保护回答"} {
		if strings.Contains(input, forbidden) {
			t.Fatalf("title input leaked %q: %q", forbidden, input)
		}
	}
}

func TestTitleInputIsUTF8BoundedAndSkipsCredentialLikeMessages(t *testing.T) {
	input := titleInput(agentloop.SessionCheckpoint{
		Turns: []agentloop.CheckpointTurn{{ID: "turn-1", Completed: true}, {ID: "turn-2", Completed: true}},
		Messages: []modelclient.Message{
			{Role: "user", Content: "Authorization: Bearer secret"}, {Role: "assistant", Content: "不得进入"},
			{Role: "user", Content: strings.Repeat("学", 4000)}, {Role: "assistant", Content: strings.Repeat("答", 4000)},
		},
		MessageTurnIDs: []string{"turn-1", "turn-1", "turn-2", "turn-2"},
	}, "Authorization: Bearer title-secret", agentsession.DefaultLimits())
	if !utf8.ValidString(input) || len(input) > 6000 || strings.Contains(input, "secret") || strings.Contains(input, "不得进入") {
		t.Fatalf("unsafe title input len=%d valid=%t value=%q", len(input), utf8.ValidString(input), input)
	}
}

func TestInjectedLimitsBoundPickerSearchSummaryManualTitleAndReceipts(t *testing.T) {
	limits := agentsession.DefaultLimits()
	limits.PickerQueryRunes = 3
	limits.SearchSummaryRunes = 3
	limits.SearchSummaryBytes = 5
	limits.ManualTitleBytes = 5
	limits.ManualTitleRunes = 3
	limits.ManualTitleColumns = 3
	limits.AutoTitleResponseBytes = 20
	limits.NoticeCount = 1
	limits.ReceiptCount = 1

	if _, err := normalizeSearchQuery("四五六七", limits); !errors.Is(err, agentsession.ErrInvalid) {
		t.Fatalf("query error=%v", err)
	}
	if got := boundedSearchSummary("甲乙丙丁", limits); got != "甲" {
		t.Fatalf("summary=%q", got)
	}
	if _, _, err := normalizeManualTitle("甲乙", agentsession.TranscriptV1{}, limits); !errors.Is(err, agentsession.ErrInvalid) {
		t.Fatalf("manual title error=%v", err)
	}
	if _, err := parseTitle(`{"title":"甲乙丙丁"}`, limits); err == nil {
		t.Fatal("oversized automatic title response succeeded")
	}

	preferences := appendBoundedPreference(nil, agentsession.PreferenceReceipt{CreateOperationID: "one"}, limits.ReceiptCount)
	preferences = appendBoundedPreference(preferences, agentsession.PreferenceReceipt{CreateOperationID: "two"}, limits.ReceiptCount)
	if len(preferences) != 1 || preferences[0].CreateOperationID != "two" {
		t.Fatalf("preferences=%+v", preferences)
	}
	files := appendBoundedFile(nil, agentsession.FileReceipt{ToolCallID: "one"}, limits.ReceiptCount)
	files = appendBoundedFile(files, agentsession.FileReceipt{ToolCallID: "two"}, limits.ReceiptCount)
	if len(files) != 1 || files[0].ToolCallID != "two" {
		t.Fatalf("files=%+v", files)
	}
	controller := &Controller{limits: limits}
	controller.appendStatusNoticeLocked("one")
	controller.appendStatusNoticeLocked("two")
	if len(controller.notices) != 1 || controller.notices[0] != "two" {
		t.Fatalf("notices=%+v", controller.notices)
	}
}

func TestSelectorCapsPickerResultsWithStoreLimitsAndRequestLimit(t *testing.T) {
	root := t.TempDir()
	secrets := &controllerSecretBackend{}
	limits := agentsession.DefaultLimits()
	limits.PickerResults = 2
	store := controllerStoreWithLimits(t, root, secrets, limits)
	defer store.Close()
	for _, title := range []string{"one", "two", "three"} {
		handle, _, err := store.Create(t.Context(), agentsession.CreateInput{Title: title, Checkpoint: []byte(`{"v":1}`)})
		if err != nil {
			t.Fatal(err)
		}
		if err := handle.Close(); err != nil {
			t.Fatal(err)
		}
	}
	selector := NewSelector(store, "", Provider{})
	items, err := selector.ListSessions(t.Context(), SessionListRequest{All: true})
	if err != nil || len(items) != 2 {
		t.Fatalf("items=%+v err=%v", items, err)
	}
	items, err = selector.ListSessions(t.Context(), SessionListRequest{All: true, Limit: 1})
	if err != nil || len(items) != 1 {
		t.Fatalf("limited items=%+v err=%v", items, err)
	}
}

func TestControllerUsesStoreLimitsForAutomaticTitleRequest(t *testing.T) {
	root, workspaceRoot := t.TempDir(), t.TempDir()
	secrets := &controllerSecretBackend{}
	limits := agentsession.DefaultLimits()
	limits.AutoTitleInputBytes = 32
	limits.AutoTitlePartBytes = 9
	limits.AutoTitleResponseBytes = 64
	limits.AutoTitleMaxTokens = 7
	model := &controllerModel{}
	controller, err := Start(t.Context(), controllerDependencies(
		controllerStoreWithLimits(t, root, secrets, limits), model,
		&controllerServer{generation: api.MemoryGenerationStamp{LearnerGeneration: 1, MemoryGeneration: 1}}, workspaceRoot,
		Provider{Name: "ollama", Endpoint: "http://127.0.0.1:11434/v1", Model: "local"},
	), false)
	if err != nil {
		t.Fatal(err)
	}
	defer controller.Close()
	if _, err := controller.Send(t.Context(), "abcdefghijklmno"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for {
		model.mu.Lock()
		requests := append([]modelclient.Request(nil), model.requests...)
		model.mu.Unlock()
		for _, request := range requests {
			if len(request.Messages) > 0 && strings.Contains(request.Messages[0].Content, "会话标题") {
				if request.MaxTokens != 7 || len(request.Messages[1].Content) > 32 {
					t.Fatalf("title request=%+v", request)
				}
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("title request missing: %+v", requests)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestAutomaticTitleSaveUsesInjectedTimeoutAndPublishesFailure(t *testing.T) {
	root, workspaceRoot := t.TempDir(), t.TempDir()
	secrets := &controllerSecretBackend{}
	limits := agentsession.DefaultLimits()
	limits.AutoTitleSaveTimeout = 10 * time.Millisecond
	model := &generationTitleModel{titleStarted: make(chan struct{}, 1), releaseTitle: make(chan struct{})}
	controller, err := Start(t.Context(), controllerDependencies(
		controllerStoreWithLimits(t, root, secrets, limits), model,
		&controllerServer{generation: api.MemoryGenerationStamp{LearnerGeneration: 1, MemoryGeneration: 1}}, workspaceRoot,
		Provider{Name: "ollama", Endpoint: "http://127.0.0.1:11434/v1", Model: "local"},
	), false)
	if err != nil {
		t.Fatal(err)
	}
	defer controller.Close()
	if _, err := controller.Send(t.Context(), "触发标题保存"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-model.titleStarted:
	case <-time.After(time.Second):
		t.Fatal("automatic title request did not start")
	}
	lock, err := filelock.Acquire(t.Context(), filepath.Join(root, "profile.lock"), filelock.Exclusive, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	close(model.releaseTitle)
	deadline := time.Now().Add(time.Second)
	for controller.Status().TitleFailureCode != sessionTitleFailedCode {
		if time.Now().After(deadline) {
			_ = lock.Close()
			t.Fatal("title save did not honor injected timeout")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	if title := controller.SessionTitle(); title != "触发标题保存" {
		t.Fatalf("title save failure changed fallback: %q", title)
	}
	assertSingleTitleFailure(t, controller)
	if result, err := controller.Send(t.Context(), "标题保存失败后主对话继续"); err != nil || result.Text != "已回答" {
		t.Fatalf("main conversation after title save failure result=%+v err=%v", result, err)
	}
	assertSingleTitleFailure(t, controller)
}

func TestParseTitleRejectsUnsafeAndOpaqueValues(t *testing.T) {
	for _, value := range []string{
		`{"title":"安全标题","extra":true}`,
		"{\"title\":\"两行\\n标题\"}",
		`{"title":"sk-1234567890abcdef1234567890"}`,
	} {
		if _, err := parseTitle(value, agentsession.DefaultLimits()); err == nil {
			t.Fatalf("parseTitle(%q) succeeded", value)
		}
	}
	if title, err := parseTitle(`{"title":"函数与导数复习"}`, agentsession.DefaultLimits()); err != nil || title != "函数与导数复习" {
		t.Fatalf("title=%q err=%v", title, err)
	}
}
