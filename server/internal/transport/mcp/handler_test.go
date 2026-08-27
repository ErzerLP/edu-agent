package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/identity"
	"github.com/edu-agent/edu-agent/server/internal/knowledge"
	"github.com/edu-agent/edu-agent/server/internal/learning"
	"github.com/edu-agent/edu-agent/server/internal/memory"
	"github.com/edu-agent/edu-agent/server/internal/privacy"
	"github.com/edu-agent/edu-agent/server/internal/transport/problem"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	testDeviceID = "90000000-0000-4000-8000-000000000001"
	testToken    = "valid-device-token"
)

type testIdentity struct {
	mu         sync.Mutex
	credential identity.Credential
	revoked    bool
	calls      int
}

func (f *testIdentity) Authenticate(_ context.Context, token, _ string) (identity.Credential, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if token != testToken || f.revoked {
		return identity.Credential{}, identity.ErrUnauthenticated
	}
	return f.credential, nil
}

func (f *testIdentity) authCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *testIdentity) ExchangePairingCode(context.Context, string, string) (identity.IssuedCredential, error) {
	return identity.IssuedCredential{}, errors.New("not implemented")
}
func (f *testIdentity) ListDevices(context.Context) ([]identity.Device, error) { return nil, nil }
func (f *testIdentity) RevokeDevice(context.Context, string) error             { return nil }

type testLimiter struct {
	mu     sync.Mutex
	limit  int
	counts map[string]int
}

func newTestLimiter(limit int) *testLimiter {
	return &testLimiter{limit: limit, counts: map[string]int{}}
}
func (l *testLimiter) Allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.counts[key]++
	return l.limit > 0 && l.counts[key] <= l.limit
}
func (l *testLimiter) Limited(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.limit <= 0 || l.counts[key] >= l.limit
}

type testKnowledge struct {
	head       *knowledge.KnowledgeRevision
	retrieve   knowledge.RetrievalResult
	err        error
	proposal   knowledge.Proposal
	page       knowledge.ProposalPage
	create     knowledge.CreateProposalCommand
	rollback   knowledge.CreateRollbackCommand
	list       knowledge.ProposalListCommand
	getID      string
	retrieveFn func(context.Context, knowledge.RetrievalCommand) (knowledge.RetrievalResult, error)
}

func (f *testKnowledge) Head(context.Context) (*knowledge.KnowledgeRevision, error) {
	return f.head, f.err
}
func (f *testKnowledge) Tree(context.Context, string) (knowledge.TreeResult, error) {
	return knowledge.TreeResult{}, f.err
}
func (f *testKnowledge) Export(context.Context, string) (knowledge.ExportResult, error) {
	return knowledge.ExportResult{}, f.err
}
func (f *testKnowledge) Retrieve(ctx context.Context, command knowledge.RetrievalCommand) (knowledge.RetrievalResult, error) {
	if f.retrieveFn != nil {
		return f.retrieveFn(ctx, command)
	}
	return f.retrieve, f.err
}
func (f *testKnowledge) Create(_ context.Context, command knowledge.CreateProposalCommand) (knowledge.Proposal, error) {
	f.create = command
	return f.proposal, f.err
}
func (f *testKnowledge) CreateRollback(_ context.Context, command knowledge.CreateRollbackCommand) (knowledge.Proposal, error) {
	f.rollback = command
	return f.proposal, f.err
}
func (f *testKnowledge) List(_ context.Context, command knowledge.ProposalListCommand) (knowledge.ProposalPage, error) {
	f.list = command
	return f.page, f.err
}
func (f *testKnowledge) Get(_ context.Context, proposalID string) (knowledge.Proposal, error) {
	f.getID = proposalID
	return f.proposal, f.err
}

type testLearning struct {
	mu          sync.Mutex
	actor       string
	method      string
	calls       int
	err         error
	current     learning.SessionView
	timeline    learning.TimelinePage
	projection  learning.ProjectionStatus
	operation   learning.OperationResult
	proposal    learning.ProposalArtifact
	lastGoal    learning.GoalCommand
	lastSession learning.SessionCommand
	lastAction  learning.ActionCommand
}

func (f *testLearning) record(method, actor string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.method, f.actor = method, actor
	f.calls++
}
func (f *testLearning) CreateGoal(_ context.Context, actor string, command learning.GoalCommand) (learning.OperationResult, error) {
	f.record("create_goal", actor)
	f.mu.Lock()
	f.lastGoal = command
	f.mu.Unlock()
	return f.operation, f.err
}
func (f *testLearning) CreateSession(_ context.Context, actor string, command learning.SessionCommand) (learning.OperationResult, error) {
	f.record("create_session", actor)
	f.mu.Lock()
	f.lastSession = command
	f.mu.Unlock()
	return f.operation, f.err
}
func (f *testLearning) Propose(_ context.Context, actor string, _ learning.ProposalRequest) (learning.ProposalArtifact, error) {
	f.record("propose", actor)
	return f.proposal, f.err
}
func (f *testLearning) ApplyAction(_ context.Context, actor, _ string, command learning.ActionCommand) (learning.OperationResult, error) {
	f.record("apply_action", actor)
	f.mu.Lock()
	f.lastAction = command
	f.mu.Unlock()
	return f.operation, f.err
}
func (f *testLearning) Decide(context.Context, string, string, learning.AssessmentDecisionCommand) (learning.OperationResult, error) {
	return learning.OperationResult{}, f.err
}
func (f *testLearning) CurrentSession(context.Context) (learning.SessionView, error) {
	return f.current, f.err
}
func (f *testLearning) Session(context.Context, string) (learning.SessionView, error) {
	return f.current, f.err
}
func (f *testLearning) Timeline(context.Context, learning.TimelineQuery) (learning.TimelinePage, error) {
	return f.timeline, f.err
}
func (f *testLearning) Routes(context.Context, learning.CursorPageRequest) (learning.RoutesPage, error) {
	return learning.RoutesPage{}, f.err
}
func (f *testLearning) Node(context.Context, string) (learning.NodeView, error) {
	return learning.NodeView{}, f.err
}
func (f *testLearning) Evidence(context.Context, learning.EvidenceQuery) (learning.EvidencePage, error) {
	return learning.EvidencePage{}, f.err
}
func (f *testLearning) Reviews(context.Context, learning.ReviewQuery) (learning.ReviewsPage, error) {
	return learning.ReviewsPage{}, f.err
}
func (f *testLearning) ProjectionStatus(context.Context) (learning.ProjectionStatus, error) {
	return f.projection, f.err
}

type testMemory struct {
	err         error
	records     memory.RecordPage
	detail      memory.RecordDetail
	export      memory.ExportPage
	listCalls   int
	detailCalls int
	exportCalls int
}

func (f *testMemory) ListRecords(context.Context, memory.PageRequest) (memory.RecordPage, error) {
	f.listCalls++
	return f.records, f.err
}
func (f *testMemory) Detail(context.Context, string) (memory.RecordDetail, error) {
	f.detailCalls++
	return f.detail, f.err
}
func (f *testMemory) Export(context.Context, memory.PageRequest) (memory.ExportPage, error) {
	f.exportCalls++
	return f.export, f.err
}

type bearerTransport struct {
	token string
	base  http.RoundTripper
}

func (t bearerTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	clone := request.Clone(request.Context())
	clone.Header = request.Header.Clone()
	clone.Header.Set("Authorization", "Bearer "+t.token)
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(clone)
}

type responseRecordingTransport struct {
	mu     sync.Mutex
	bodies [][]byte
}

func (t *responseRecordingTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	response, err := http.DefaultTransport.RoundTrip(request)
	if err != nil {
		return nil, err
	}
	if response.StatusCode >= http.StatusBadRequest {
		data, readErr := io.ReadAll(response.Body)
		closeErr := response.Body.Close()
		if readErr != nil {
			return nil, readErr
		}
		if closeErr != nil {
			return nil, closeErr
		}
		t.mu.Lock()
		t.bodies = append(t.bodies, append([]byte(nil), data...))
		t.mu.Unlock()
		response.Body = io.NopCloser(bytes.NewReader(data))
		return response, nil
	}
	var body bytes.Buffer
	response.Body = &recordingReadCloser{Reader: io.TeeReader(response.Body, &body), close: response.Body.Close, done: func() {
		t.mu.Lock()
		t.bodies = append(t.bodies, append([]byte(nil), body.Bytes()...))
		t.mu.Unlock()
	}}
	return response, nil
}

func (t *responseRecordingTransport) lastBody() []byte {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.bodies) == 0 {
		return nil
	}
	return append([]byte(nil), t.bodies[len(t.bodies)-1]...)
}

type recordingReadCloser struct {
	io.Reader
	close func() error
	done  func()
}

func (r *recordingReadCloser) Close() error {
	err := r.close()
	r.done()
	return err
}

func newProtocolFixture(t *testing.T, scopes []string) (*Handler, *testIdentity, *testKnowledge, *testLearning, *testMemory, *bytes.Buffer) {
	t.Helper()
	logs := &bytes.Buffer{}
	id := &testIdentity{credential: identity.Credential{Device: identity.Device{ID: testDeviceID}, Scopes: append([]string(nil), scopes...)}}
	knowledgeService := &testKnowledge{head: &knowledge.KnowledgeRevision{ID: "10000000-0000-4000-8000-000000000001"}, retrieve: knowledge.RetrievalResult{KnowledgeRevisionID: "10000000-0000-4000-8000-000000000001", Hits: []knowledge.RetrievalHit{}}}
	learningService := &testLearning{operation: learning.OperationResult{Status: "committed", AggregateType: "goal", AggregateID: "20000000-0000-4000-8000-000000000001", AggregateVersion: 1}}
	memoryService := &testMemory{}
	handler, err := New(Options{
		Identity: id, Knowledge: knowledgeService, Learning: learningService,
		Memory: memoryService, MemoryExporter: memoryService,
		ReadPermits: privacy.NewReadPermitManager(), Logger: slog.New(slog.NewJSONHandler(logs, nil)),
		AuthLimiter: newTestLimiter(1000), DeviceLimiter: newTestLimiter(1000),
	})
	if err != nil {
		t.Fatal(err)
	}
	return handler, id, knowledgeService, learningService, memoryService, logs
}

func connectSDKClient(t *testing.T, endpoint, token string) *sdkmcp.ClientSession {
	t.Helper()
	return connectSDKClientWithTransport(t, endpoint, bearerTransport{token: token})
}

func connectSDKClientWithTransport(t *testing.T, endpoint string, transport http.RoundTripper) *sdkmcp.ClientSession {
	t.Helper()
	client := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "edu-agent-test", Version: "1"}, &sdkmcp.ClientOptions{Capabilities: &sdkmcp.ClientCapabilities{}})
	session, err := client.Connect(context.Background(), &sdkmcp.StreamableClientTransport{
		Endpoint: endpoint, HTTPClient: &http.Client{Transport: transport},
		DisableStandaloneSSE: true, MaxRetries: -1,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session
}

func TestOfficialSDKDiscoversExactSurfaceAndInvokesCallbacks(t *testing.T) {
	handler, identityService, _, _, _, _ := newProtocolFixture(t, []string{"knowledge:read", "learning:read", "learning:write", "memory:read"})
	server := httptest.NewServer(handler)
	defer server.Close()
	session := connectSDKClient(t, server.URL, testToken)
	if session.ID() != "" {
		t.Fatalf("stateless MCP session ID = %q", session.ID())
	}
	if session.InitializeResult() == nil || session.InitializeResult().ProtocolVersion == "" {
		t.Fatalf("missing protocol negotiation result: %+v", session.InitializeResult())
	}

	tools, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	toolNames := make([]string, 0, len(tools.Tools))
	for _, tool := range tools.Tools {
		toolNames = append(toolNames, tool.Name)
	}
	sort.Strings(toolNames)
	wantTools := []string{
		"knowledge.maintenance.get", "knowledge.maintenance.list", "knowledge.maintenance.propose",
		"knowledge.retrieve", "learning.create_goal", "learning.list_evidence", "learning.list_reviews", "learning.list_routes",
		"learning.list_timeline", "memory.list_records", "tutoring.apply_action", "tutoring.create_session", "tutoring.propose",
	}
	sort.Strings(wantTools)
	if !equalStrings(toolNames, wantTools) {
		t.Fatalf("tools = %v", toolNames)
	}

	resources, err := session.ListResources(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	resourceURIs := make([]string, 0, len(resources.Resources))
	for _, resource := range resources.Resources {
		resourceURIs = append(resourceURIs, resource.URI)
	}
	sort.Strings(resourceURIs)
	wantResources := []string{
		"edu-agent://knowledge/head", "edu-agent://learning/projections/status",
		"edu-agent://memory/export", "edu-agent://tutoring/sessions/current",
	}
	sort.Strings(wantResources)
	if !equalStrings(resourceURIs, wantResources) {
		t.Fatalf("resources = %v", resourceURIs)
	}

	templates, err := session.ListResourceTemplates(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	templateURIs := make([]string, 0, len(templates.ResourceTemplates))
	for _, resource := range templates.ResourceTemplates {
		templateURIs = append(templateURIs, resource.URITemplate)
	}
	sort.Strings(templateURIs)
	wantTemplates := []string{
		"edu-agent://knowledge/revisions/{revision_id}/export", "edu-agent://knowledge/revisions/{revision_id}/tree",
		"edu-agent://learning/nodes/{node_revision_id}", "edu-agent://memory/records/{memory_id}",
		"edu-agent://tutoring/sessions/{session_id}",
	}
	sort.Strings(wantTemplates)
	if !equalStrings(templateURIs, wantTemplates) {
		t.Fatalf("resource templates = %v", templateURIs)
	}

	resource, err := session.ReadResource(context.Background(), &sdkmcp.ReadResourceParams{URI: "edu-agent://knowledge/head"})
	if err != nil || len(resource.Contents) != 1 || !bytes.Contains([]byte(resource.Contents[0].Text), []byte("10000000-0000-4000-8000-000000000001")) {
		t.Fatalf("knowledge head: result=%+v err=%v", resource, err)
	}
	result, err := session.CallTool(context.Background(), &sdkmcp.CallToolParams{Name: "knowledge.retrieve", Arguments: map[string]any{"query": "context"}})
	if err != nil || result.IsError {
		t.Fatalf("knowledge retrieve: result=%+v err=%v", result, err)
	}
	if identityService.authCalls() < 6 {
		t.Fatalf("Authenticate calls = %d; stateless requests were not reauthenticated", identityService.authCalls())
	}

	if _, err := session.CallTool(context.Background(), &sdkmcp.CallToolParams{Name: "knowledge.import", Arguments: map[string]any{}}); err == nil {
		t.Fatal("forbidden knowledge.import reached the MCP surface")
	}
	if _, err := session.ReadResource(context.Background(), &sdkmcp.ReadResourceParams{URI: "edu-agent://privacy/erasures"}); err == nil {
		t.Fatal("forbidden privacy resource reached the MCP surface")
	}
}

func TestOfficialSDKRequestCancellationReachesApplicationCallback(t *testing.T) {
	handler, _, knowledgeService, _, _, _ := newProtocolFixture(t, []string{"knowledge:read"})
	started := make(chan struct{})
	cancelled := make(chan struct{})
	knowledgeService.retrieveFn = func(ctx context.Context, _ knowledge.RetrievalCommand) (knowledge.RetrievalResult, error) {
		close(started)
		<-ctx.Done()
		close(cancelled)
		return knowledge.RetrievalResult{}, ctx.Err()
	}
	server := httptest.NewServer(handler)
	defer server.Close()
	session := connectSDKClient(t, server.URL, testToken)
	ctx, cancel := context.WithCancel(context.Background())
	callDone := make(chan error, 1)
	go func() {
		_, err := session.CallTool(ctx, &sdkmcp.CallToolParams{Name: "knowledge.retrieve", Arguments: map[string]any{"query": "cancel"}})
		callDone <- err
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("application callback did not start")
	}
	cancel()
	select {
	case <-cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("request cancellation did not reach application callback")
	}
	select {
	case <-callDone:
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled SDK call did not finish")
	}
}

func TestAssessmentActionPreservesApplicationDispositionWithoutDecisionSurface(t *testing.T) {
	handler, _, _, learningService, _, _ := newProtocolFixture(t, []string{"learning:write"})
	learningService.operation = learning.OperationResult{
		Status: "committed", AggregateType: "session", AggregateID: "27000000-0000-4000-8000-000000000001",
		AggregateVersion: 5, EvidenceDisposition: learning.DispositionProvisional,
	}
	server := httptest.NewServer(handler)
	defer server.Close()
	session := connectSDKClient(t, server.URL, testToken)
	result, err := session.CallTool(context.Background(), &sdkmcp.CallToolParams{Name: "tutoring.apply_action", Arguments: map[string]any{
		"session_id":   "27000000-0000-4000-8000-000000000001",
		"operation_id": "28000000-0000-4000-8000-000000000001", "payload_schema_version": 1,
		"aggregate_type": "session", "aggregate_id": "27000000-0000-4000-8000-000000000001",
		"expected_version": 4, "action": "record_assessment", "proposal_id": "29000000-0000-4000-8000-000000000001",
	}})
	if err != nil || result.IsError {
		t.Fatalf("assessment action result=%+v err=%v", result, err)
	}
	structured := result.StructuredContent.(map[string]any)
	if structured["evidence_disposition"] != string(learning.DispositionProvisional) {
		t.Fatalf("assessment disposition=%v", structured["evidence_disposition"])
	}
	learningService.mu.Lock()
	if learningService.lastAction.Action != "record_assessment" || learningService.actor != testDeviceID {
		learningService.mu.Unlock()
		t.Fatalf("assessment command=%+v actor=%q", learningService.lastAction, learningService.actor)
	}
	learningService.mu.Unlock()
	tools, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range tools.Tools {
		name := strings.ToLower(tool.Name)
		if strings.Contains(name, "assessment") || strings.Contains(name, "confirm") || strings.Contains(name, "override") || strings.Contains(name, "invalidate") {
			t.Fatalf("assessment decision capability exposed as %q", tool.Name)
		}
	}
}

func TestMemoryDescriptorsUseComposedReadServiceAndExporterOnly(t *testing.T) {
	handler, _, _, _, memoryService, _ := newProtocolFixture(t, []string{"memory:read"})
	memoryService.records.Items = []memory.Record{}
	memoryService.export.ReasonCodes = []string{}
	server := httptest.NewServer(handler)
	defer server.Close()
	session := connectSDKClient(t, server.URL, testToken)
	result, err := session.CallTool(context.Background(), &sdkmcp.CallToolParams{Name: "memory.list_records", Arguments: map[string]any{"limit": 10}})
	if err != nil || result.IsError {
		t.Fatalf("memory list result=%+v err=%v", result, err)
	}
	if _, err := session.ReadResource(context.Background(), &sdkmcp.ReadResourceParams{URI: "edu-agent://memory/records/30000000-0000-4000-8000-000000000001"}); err != nil {
		t.Fatal(err)
	}
	if _, err := session.ReadResource(context.Background(), &sdkmcp.ReadResourceParams{URI: "edu-agent://memory/export"}); err != nil {
		t.Fatal(err)
	}
	if memoryService.listCalls != 1 || memoryService.detailCalls != 1 || memoryService.exportCalls != 1 {
		t.Fatalf("memory calls list=%d detail=%d export=%d", memoryService.listCalls, memoryService.detailCalls, memoryService.exportCalls)
	}
}

func TestActorInjectionAndSensitiveIdentityFieldsAreNotAccepted(t *testing.T) {
	handler, _, _, learningService, _, _ := newProtocolFixture(t, []string{"knowledge:read", "learning:read", "learning:write", "memory:read"})
	server := httptest.NewServer(handler)
	defer server.Close()
	session := connectSDKClient(t, server.URL, testToken)

	tools, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range tools.Tools {
		schema, err := json.Marshal(tool.InputSchema)
		if err != nil {
			t.Fatal(err)
		}
		for _, forbidden := range []string{"actor", "device_id", "token_id", "principal", "namespace"} {
			if bytes.Contains(bytes.ToLower(schema), []byte(forbidden)) {
				t.Fatalf("tool %s schema exposes %q: %s", tool.Name, forbidden, schema)
			}
		}
	}

	goalArgs := map[string]any{
		"operation_id": "21000000-0000-4000-8000-000000000001", "payload_schema_version": 1,
		"aggregate_type": "goal", "aggregate_id": "22000000-0000-4000-8000-000000000001",
		"expected_version": 0, "text": "Learn concurrency", "source": "mcp-test",
	}
	result, err := session.CallTool(context.Background(), &sdkmcp.CallToolParams{Name: "learning.create_goal", Arguments: goalArgs})
	if err != nil || result.IsError {
		t.Fatalf("create goal result=%+v err=%v", result, err)
	}
	assertRecordedActor(t, learningService, "create_goal")

	sessionArgs := map[string]any{
		"operation_id": "21000000-0000-4000-8000-000000000002", "payload_schema_version": 1,
		"aggregate_type": "session", "aggregate_id": "23000000-0000-4000-8000-000000000001",
		"expected_version": 0, "goal_revision_id": "24000000-0000-4000-8000-000000000001",
	}
	result, err = session.CallTool(context.Background(), &sdkmcp.CallToolParams{Name: "tutoring.create_session", Arguments: sessionArgs})
	if err != nil || result.IsError {
		t.Fatalf("create session result=%+v err=%v", result, err)
	}
	assertRecordedActor(t, learningService, "create_session")

	actionArgs := map[string]any{
		"session_id":   "23000000-0000-4000-8000-000000000001",
		"operation_id": "21000000-0000-4000-8000-000000000003", "payload_schema_version": 1,
		"aggregate_type": "session", "aggregate_id": "23000000-0000-4000-8000-000000000001",
		"expected_version": 1, "action": "start_diagnostic",
	}
	result, err = session.CallTool(context.Background(), &sdkmcp.CallToolParams{Name: "tutoring.apply_action", Arguments: actionArgs})
	if err != nil || result.IsError {
		t.Fatalf("apply action result=%+v err=%v", result, err)
	}
	assertRecordedActor(t, learningService, "apply_action")

	learningService.mu.Lock()
	beforeActionExtra := learningService.calls
	learningService.mu.Unlock()
	actionArgs["answer"] = "ignored payload"
	result, err = session.CallTool(context.Background(), &sdkmcp.CallToolParams{Name: "tutoring.apply_action", Arguments: actionArgs})
	if err != nil || !result.IsError {
		t.Fatalf("irrelevant action field was not rejected: result=%+v err=%v", result, err)
	}
	learningService.mu.Lock()
	afterActionExtra := learningService.calls
	learningService.mu.Unlock()
	if afterActionExtra != beforeActionExtra {
		t.Fatalf("irrelevant action field reached application service: before=%d after=%d", beforeActionExtra, afterActionExtra)
	}
	delete(actionArgs, "answer")

	learningService.mu.Lock()
	before := learningService.calls
	learningService.mu.Unlock()
	goalArgs["device_id"] = "attacker-device"
	result, err = session.CallTool(context.Background(), &sdkmcp.CallToolParams{Name: "learning.create_goal", Arguments: goalArgs})
	if err != nil || !result.IsError {
		t.Fatalf("identity override was not rejected: result=%+v err=%v", result, err)
	}
	learningService.mu.Lock()
	after := learningService.calls
	learningService.mu.Unlock()
	if after != before {
		t.Fatalf("application service called for identity override: before=%d after=%d", before, after)
	}
}

func assertRecordedActor(t *testing.T, service *testLearning, method string) {
	t.Helper()
	service.mu.Lock()
	defer service.mu.Unlock()
	if service.method != method || service.actor != testDeviceID {
		t.Fatalf("recorded method=%q actor=%q", service.method, service.actor)
	}
}

func TestAuthenticationScopeRevocationAndSharedLimitSemantics(t *testing.T) {
	t.Run("scope", func(t *testing.T) {
		handler, _, _, learningService, _, _ := newProtocolFixture(t, []string{"knowledge:read"})
		server := httptest.NewServer(handler)
		defer server.Close()
		session := connectSDKClient(t, server.URL, testToken)
		_, err := session.CallTool(context.Background(), &sdkmcp.CallToolParams{Name: "learning.create_goal", Arguments: map[string]any{
			"operation_id": "25000000-0000-4000-8000-000000000001", "payload_schema_version": 1,
			"aggregate_type": "goal", "aggregate_id": "26000000-0000-4000-8000-000000000001",
			"expected_version": 0, "text": "Scope", "source": "test",
		}})
		if err == nil || !strings.Contains(err.Error(), "Forbidden") {
			t.Fatalf("scope error = %v", err)
		}
		learningService.mu.Lock()
		defer learningService.mu.Unlock()
		if learningService.calls != 0 {
			t.Fatalf("scope failure reached application service: %d", learningService.calls)
		}
	})

	t.Run("revocation", func(t *testing.T) {
		handler, identityService, _, _, _, _ := newProtocolFixture(t, []string{"knowledge:read"})
		server := httptest.NewServer(handler)
		defer server.Close()
		session := connectSDKClient(t, server.URL, testToken)
		identityService.mu.Lock()
		identityService.revoked = true
		identityService.mu.Unlock()
		if _, err := session.ListTools(context.Background(), nil); err == nil || !strings.Contains(err.Error(), "Unauthorized") {
			t.Fatalf("revoked credential error = %v", err)
		}
	})

	t.Run("authentication failure IP limit", func(t *testing.T) {
		handler, _, _, _, _, _ := newProtocolFixture(t, []string{"knowledge:read"})
		handler.authLimiter = newTestLimiter(1)
		body := `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`
		first := directMCPRequest(handler, http.MethodPost, body, "invalid")
		second := directMCPRequest(handler, http.MethodPost, body, "invalid")
		if first.Code != http.StatusUnauthorized || second.Code != http.StatusTooManyRequests || !strings.Contains(first.Body.String(), "authentication_failed") || !strings.Contains(second.Body.String(), "rate_limited") {
			t.Fatalf("auth limit statuses = %d, %d bodies=%s / %s", first.Code, second.Code, first.Body.String(), second.Body.String())
		}
	})

	t.Run("device invocation limit", func(t *testing.T) {
		handler, _, _, _, _, _ := newProtocolFixture(t, []string{"knowledge:read"})
		handler.deviceLimiter = newTestLimiter(1)
		server := httptest.NewServer(handler)
		defer server.Close()
		session := connectSDKClient(t, server.URL, testToken)
		if _, err := session.ListTools(context.Background(), nil); err == nil || !strings.Contains(err.Error(), "Too Many Requests") {
			t.Fatalf("device limit error = %v", err)
		}
	})
}

func TestGatewayRejectsHostOriginOversizedBodiesAndUnauthorizedMethods(t *testing.T) {
	handler, _, _, _, _, _ := newProtocolFixture(t, []string{"knowledge:read"})
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`

	request := httptest.NewRequest(http.MethodPost, "http://localhost/mcp", strings.NewReader(body))
	request = request.WithContext(context.WithValue(request.Context(), http.LocalAddrContextKey, &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 8080}))
	request.Host = "attacker.example"
	request.Header.Set("Authorization", "Bearer "+testToken)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("invalid host status=%d body=%s", response.Code, response.Body.String())
	}

	request = httptest.NewRequest(http.MethodPost, "http://localhost/mcp", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+testToken)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")
	request.Header.Set("Origin", "https://attacker.example")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("cross origin status=%d body=%s", response.Code, response.Body.String())
	}

	handler.maxRequestBody = int64(len(body))
	response = directMCPRequest(handler, http.MethodPost, body+" ", testToken)
	if response.Code != http.StatusRequestEntityTooLarge || !strings.Contains(response.Body.String(), "payload_too_large") {
		t.Fatalf("oversized status=%d body=%s", response.Code, response.Body.String())
	}
	handler.maxRequestBody = DefaultMaxRequestBodyBytes

	descriptorBody := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"knowledge.retrieve","arguments":{"query":"` + strings.Repeat("x", int(defaultToolInputLimit)) + `"}}}`
	response = directMCPRequest(handler, http.MethodPost, descriptorBody, testToken)
	if response.Code != http.StatusRequestEntityTooLarge || !strings.Contains(response.Body.String(), "payload_too_large") {
		t.Fatalf("oversized descriptor arguments status=%d body=%s", response.Code, response.Body.String())
	}

	response = directMCPRequest(handler, http.MethodGet, "", "")
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized GET status=%d body=%s", response.Code, response.Body.String())
	}
	response = directMCPRequest(handler, http.MethodGet, "", testToken)
	if response.Code != http.StatusMethodNotAllowed || response.Header().Get("Allow") != "POST" {
		t.Fatalf("authorized GET status=%d allow=%q body=%s", response.Code, response.Header().Get("Allow"), response.Body.String())
	}
	response = directMCPRequest(handler, http.MethodDelete, "", testToken)
	if response.Code != http.StatusMethodNotAllowed {
		t.Fatalf("authorized DELETE status=%d body=%s", response.Code, response.Body.String())
	}

	response = directMCPRequest(handler, http.MethodPost, `{"jsonrpc":"2.0","id":1,"method":"privacy/erase","params":{}}`, testToken)
	if response.Code != http.StatusNotFound || !strings.Contains(response.Body.String(), "not_found") {
		t.Fatalf("unknown method status=%d body=%s", response.Code, response.Body.String())
	}
	response = directMCPRequest(handler, http.MethodPost, `[{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}]`, testToken)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "invalid_request") {
		t.Fatalf("batch status=%d body=%s", response.Code, response.Body.String())
	}

	request = httptest.NewRequest(http.MethodPost, "http://localhost/mcp", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+testToken)
	request.Header.Set("Content-Type", "text/plain")
	request.Header.Set("Accept", "application/json, text/event-stream")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("content type status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestResourceErrorDataUsesStructuredEnvelope(t *testing.T) {
	encoded, err := json.Marshal(resourceError(problem.DescriptorNotFound(), "request-1"))
	if err != nil {
		t.Fatal(err)
	}
	var response struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(encoded, &response); err != nil {
		t.Fatal(err)
	}
	failure, ok := response.Data["error"].(map[string]any)
	if !ok || failure["code"] != "not_found" || failure["request_id"] != "request-1" {
		t.Fatalf("structured resource error=%s", encoded)
	}
}

func directMCPRequest(handler http.Handler, method, body, token string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, "http://localhost/mcp", strings.NewReader(body))
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	if method == http.MethodPost {
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Accept", "application/json, text/event-stream")
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func TestPrivacyPermitCoversSDKResponseWriteForEveryContentOwner(t *testing.T) {
	tests := []struct {
		name       string
		uri        string
		descriptor string
		owners     []privacy.OwnerKind
		secret     string
		prepare    func(*testKnowledge, *testLearning, *testMemory)
	}{
		{name: "knowledge", uri: "edu-agent://knowledge/head", descriptor: "knowledge.head", owners: []privacy.OwnerKind{privacy.OwnerKnowledge}, secret: "private-knowledge", prepare: func(value *testKnowledge, _ *testLearning, _ *testMemory) {
			value.head.CreatedByDeviceID = "private-knowledge"
		}},
		{name: "learning tutoring", uri: "edu-agent://learning/projections/status", descriptor: "learning.projection_status", owners: []privacy.OwnerKind{privacy.OwnerLearning, privacy.OwnerTutoring}, secret: "private-learning", prepare: func(_ *testKnowledge, value *testLearning, _ *testMemory) {
			value.projection.ActiveGenerationID = "private-learning"
		}},
		{name: "memory", uri: "edu-agent://memory/export", descriptor: "memory.export", owners: []privacy.OwnerKind{privacy.OwnerMemory}, secret: "private-memory", prepare: func(_ *testKnowledge, _ *testLearning, value *testMemory) {
			value.export.ReasonCodes = []string{"private-memory"}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			handler, _, knowledgeService, learningService, memoryService, _ := newProtocolFixture(t, []string{"knowledge:read", "learning:read", "memory:read"})
			test.prepare(knowledgeService, learningService, memoryService)
			server := httptest.NewServer(handler)
			defer server.Close()
			responses := &responseRecordingTransport{}
			session := connectSDKClientWithTransport(t, server.URL, bearerTransport{token: testToken, base: responses})

			generated := make(chan struct{})
			var once sync.Once
			handler.beforeResponseWrite = func(ctx context.Context) {
				invocation, ok := invocationFromContext(ctx)
				if !ok || invocation.Descriptor.Name != test.descriptor {
					return
				}
				once.Do(func() { close(generated) })
				<-ctx.Done()
			}
			callResult := make(chan error, 1)
			go func() {
				_, err := session.ReadResource(context.Background(), &sdkmcp.ReadResourceParams{URI: test.uri})
				callResult <- err
			}()
			select {
			case <-generated:
			case <-time.After(5 * time.Second):
				t.Fatal("resource result was not buffered before response write")
			}
			drainResult := make(chan error, 1)
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				drainResult <- handler.readPermits.CloseAndDrain(ctx, 2, test.owners...)
			}()
			select {
			case err := <-callResult:
				body := responses.lastBody()
				if err == nil || !strings.Contains(err.Error(), "Service Unavailable") || !bytes.Contains(body, []byte(memory.CodeContentRedacted)) || bytes.Contains(body, []byte(test.secret)) {
					t.Fatalf("privacy response error=%v body=%s", err, body)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("resource call did not finish after privacy cancellation")
			}
			if err := <-drainResult; err != nil {
				t.Fatalf("CloseAndDrain: %v", err)
			}
		})
	}
}

func TestAuditIsDescriptorScopedAndRedactsProtocolContent(t *testing.T) {
	handler, _, knowledgeService, _, _, logs := newProtocolFixture(t, []string{"knowledge:read"})
	knowledgeService.retrieve.Hits = []knowledge.RetrievalHit{{CanonicalSlice: "private-output"}}
	server := httptest.NewServer(handler)
	defer server.Close()
	session := connectSDKClient(t, server.URL, testToken)
	result, err := session.CallTool(context.Background(), &sdkmcp.CallToolParams{Name: "knowledge.retrieve", Arguments: map[string]any{"query": "private-query"}})
	if err != nil || result.IsError {
		t.Fatalf("retrieve result=%+v err=%v", result, err)
	}
	logged := logs.String()
	for _, secret := range []string{testToken, "private-query", "private-output", "canonical_slice", "arguments"} {
		if strings.Contains(logged, secret) {
			t.Fatalf("audit log leaked %q: %s", secret, logged)
		}
	}
	for _, required := range []string{`"transport":"mcp"`, `"descriptor":"knowledge_retrieve"`, `"device_id":"` + testDeviceID + `"`, `"result":"success"`, `"peer":`} {
		if !strings.Contains(logged, required) {
			t.Fatalf("audit log missing %s: %s", required, logged)
		}
	}
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
