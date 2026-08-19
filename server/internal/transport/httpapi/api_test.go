package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/identity"
	"github.com/edu-agent/edu-agent/server/internal/integrations/llm"
	"github.com/edu-agent/edu-agent/server/internal/knowledge"
	"github.com/edu-agent/edu-agent/server/internal/platform/health"
)

type fakeIdentity struct {
	exchange    identity.IssuedCredential
	exchangeErr error
	auth        identity.Credential
	authErr     error
	authCalls   int
	devices     []identity.Device
	revoked     string
	revokeErr   error
}

func (f *fakeIdentity) ExchangePairingCode(context.Context, string, string) (identity.IssuedCredential, error) {
	return f.exchange, f.exchangeErr
}
func (f *fakeIdentity) Authenticate(context.Context, string, string) (identity.Credential, error) {
	f.authCalls++
	return f.auth, f.authErr
}
func (f *fakeIdentity) ListDevices(context.Context) ([]identity.Device, error) { return f.devices, nil }
func (f *fakeIdentity) RevokeDevice(_ context.Context, id string) error {
	f.revoked = id
	return f.revokeErr
}

type fakeReadiness struct{ report health.Report }

func (f fakeReadiness) Ready(context.Context) health.Report { return f.report }

type fakeModel struct{ result llm.Capabilities }

func (f fakeModel) Probe(context.Context) llm.Capabilities { return f.result }

type fakeKnowledge struct {
	head          *knowledge.KnowledgeRevision
	importResult  knowledge.ImportResult
	importErr     error
	importCommand knowledge.ImportCommand
	tree          knowledge.TreeResult
	export        knowledge.ExportResult
	retrieval     knowledge.RetrievalResult
}

func (f *fakeKnowledge) Head(context.Context) (*knowledge.KnowledgeRevision, error) {
	return f.head, nil
}
func (f *fakeKnowledge) Import(_ context.Context, command knowledge.ImportCommand) (knowledge.ImportResult, error) {
	f.importCommand = command
	return f.importResult, f.importErr
}
func (f *fakeKnowledge) Tree(context.Context, string) (knowledge.TreeResult, error) {
	return f.tree, nil
}
func (f *fakeKnowledge) Export(context.Context, string) (knowledge.ExportResult, error) {
	return f.export, nil
}
func (f *fakeKnowledge) Retrieve(context.Context, knowledge.RetrievalCommand) (knowledge.RetrievalResult, error) {
	return f.retrieval, nil
}

func newTestAPI(t *testing.T, id *fakeIdentity, pairLimit, authLimit int, logs *bytes.Buffer) http.Handler {
	t.Helper()
	logger := slog.New(slog.NewJSONHandler(logs, nil))
	handler, err := New(Options{
		Identity: id, Model: fakeModel{result: llm.Capabilities{Compatible: true}},
		Readiness: fakeReadiness{report: health.Report{Status: health.StatusHealthy, Components: map[string]health.Component{}}},
		Logger:    logger, PairLimiter: NewFixedWindowLimiter(pairLimit, time.Minute),
		AuthLimiter: NewFixedWindowLimiter(authLimit, time.Minute), DeviceLimiter: NewFixedWindowLimiter(100, time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	return handler
}

func TestHealthAndPairingHTTPContract(t *testing.T) {
	var logs bytes.Buffer
	id := &fakeIdentity{exchange: identity.IssuedCredential{Device: identity.Device{ID: "device-1", DisplayName: "Laptop"}, Token: "one-time-device-token"}}
	handler := newTestAPI(t, id, 2, 2, &logs)

	request := httptest.NewRequest(http.MethodGet, "/livez", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"alive"`) {
		t.Fatalf("livez: %d %s", response.Code, response.Body.String())
	}

	request = httptest.NewRequest(http.MethodPost, "/v1/pairings/exchange", strings.NewReader(`{"code":"pair-secret","display_name":"Laptop"}`))
	request.RemoteAddr = "192.0.2.1:1234"
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusCreated || !strings.Contains(response.Body.String(), "one-time-device-token") {
		t.Fatalf("pair: %d %s", response.Code, response.Body.String())
	}
	if strings.Contains(logs.String(), "pair-secret") || strings.Contains(logs.String(), "one-time-device-token") {
		t.Fatalf("audit log leaked secret: %s", logs.String())
	}
}

func TestAuthenticationAuthorizationAndRateLimits(t *testing.T) {
	var logs bytes.Buffer
	id := &fakeIdentity{authErr: identity.ErrUnauthenticated}
	handler := newTestAPI(t, id, 1, 1, &logs)
	for attempt, expected := range []int{http.StatusUnauthorized, http.StatusTooManyRequests} {
		request := httptest.NewRequest(http.MethodGet, "/v1/devices", nil)
		request.RemoteAddr = "192.0.2.2:1234"
		request.Header.Set("Authorization", "Bearer invalid-secret")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != expected {
			t.Fatalf("attempt %d: got %d body=%s", attempt, response.Code, response.Body.String())
		}
		if !strings.Contains(response.Body.String(), `"request_id"`) {
			t.Fatalf("missing request id: %s", response.Body.String())
		}
	}
	if id.authCalls != 1 {
		t.Fatalf("rate-limited authentication reached the identity service: calls=%d", id.authCalls)
	}
	if strings.Contains(logs.String(), "invalid-secret") {
		t.Fatalf("token leaked to logs: %s", logs.String())
	}

	id.authErr = nil
	id.auth = identity.Credential{Scopes: []string{"model:probe"}}
	request := httptest.NewRequest(http.MethodGet, "/v1/devices", nil)
	request.Header.Set("Authorization", "Bearer valid-secret")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("expected scope failure, got %d", response.Code)
	}
}

func TestDeviceRequestRateLimit(t *testing.T) {
	var logs bytes.Buffer
	id := &fakeIdentity{auth: identity.Credential{Device: identity.Device{ID: "device-1"}, Scopes: []string{"devices:read"}}}
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	handler, err := New(Options{
		Identity: id, Readiness: fakeReadiness{report: health.Report{Status: health.StatusHealthy, Components: map[string]health.Component{}}},
		Logger: logger, PairLimiter: NewFixedWindowLimiter(2, time.Minute), AuthLimiter: NewFixedWindowLimiter(2, time.Minute),
		DeviceLimiter: NewFixedWindowLimiter(1, time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []int{http.StatusOK, http.StatusTooManyRequests} {
		request := httptest.NewRequest(http.MethodGet, "/v1/devices", nil)
		request.Header.Set("Authorization", "Bearer valid")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != expected {
			t.Fatalf("expected %d, got %d: %s", expected, response.Code, response.Body.String())
		}
	}
}

func TestPairingErrorsAreGenericAndLimited(t *testing.T) {
	var logs bytes.Buffer
	id := &fakeIdentity{exchangeErr: identity.ErrInvalidPairingCode}
	handler := newTestAPI(t, id, 1, 2, &logs)
	for _, expected := range []int{http.StatusUnauthorized, http.StatusTooManyRequests} {
		request := httptest.NewRequest(http.MethodPost, "/v1/pairings/exchange", strings.NewReader(`{"code":"unknown","display_name":"Phone"}`))
		request.RemoteAddr = "192.0.2.3:1234"
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != expected {
			t.Fatalf("expected %d, got %d: %s", expected, response.Code, response.Body.String())
		}
	}
}

func TestDeviceRoutesAndCapabilityProbe(t *testing.T) {
	var logs bytes.Buffer
	id := &fakeIdentity{auth: identity.Credential{Scopes: []string{"devices:read", "devices:manage", "model:probe"}}, devices: []identity.Device{{ID: "device-1"}}}
	handler := newTestAPI(t, id, 2, 2, &logs)

	request := httptest.NewRequest(http.MethodGet, "/v1/devices", nil)
	request.Header.Set("Authorization", "Bearer valid")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("list: %d %s", response.Code, response.Body.String())
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || body["devices"] == nil {
		t.Fatalf("invalid list response: %v", err)
	}

	request = httptest.NewRequest(http.MethodDelete, "/v1/devices/device-1", nil)
	request.Header.Set("Authorization", "Bearer valid")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusNoContent || id.revoked != "device-1" {
		t.Fatalf("revoke failed: %d", response.Code)
	}

	request = httptest.NewRequest(http.MethodGet, "/v1/model/capabilities", nil)
	request.Header.Set("Authorization", "Bearer valid")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"compatible":true`) {
		t.Fatalf("probe: %d %s", response.Code, response.Body.String())
	}
}

func TestInvalidDeviceIDReturnsBadRequest(t *testing.T) {
	var logs bytes.Buffer
	id := &fakeIdentity{
		auth:      identity.Credential{Scopes: []string{"devices:manage"}},
		revokeErr: identity.ErrInvalidInput,
	}
	handler := newTestAPI(t, id, 2, 2, &logs)
	request := httptest.NewRequest(http.MethodDelete, "/v1/devices/not-a-uuid", nil)
	request.Header.Set("Authorization", "Bearer valid")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("expected invalid UUID to return 400, got %d: %s", response.Code, response.Body.String())
	}
}

func TestReadinessNotReadyUses503WithoutDetails(t *testing.T) {
	var logs bytes.Buffer
	id := &fakeIdentity{}
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	handler, err := New(Options{
		Identity: id, Readiness: fakeReadiness{report: health.Report{Status: health.StatusNotReady, Components: map[string]health.Component{"postgresql": {Status: health.StatusNotReady, Reason: "unavailable"}}}},
		Logger: logger, PairLimiter: NewFixedWindowLimiter(1, time.Minute), AuthLimiter: NewFixedWindowLimiter(1, time.Minute),
		DeviceLimiter: NewFixedWindowLimiter(1, time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable || strings.Contains(response.Body.String(), "secret") {
		t.Fatalf("readyz: %d %s", response.Code, response.Body.String())
	}
}

func TestRejectsUnknownJSONFields(t *testing.T) {
	var logs bytes.Buffer
	handler := newTestAPI(t, &fakeIdentity{}, 2, 2, &logs)
	request := httptest.NewRequest(http.MethodPost, "/v1/pairings/exchange", strings.NewReader(`{"code":"x","display_name":"x","secret":"leak"}`))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("expected strict JSON error, got %d", response.Code)
	}
}

func TestKnowledgeRoutesScopesStrictJSONAndRedactedLogs(t *testing.T) {
	var logs bytes.Buffer
	revision := knowledge.KnowledgeRevision{ID: "10000000-0000-4000-8000-000000000001", RevisionNo: 1}
	knowledgeService := &fakeKnowledge{head: &revision, importResult: knowledge.ImportResult{Revision: revision}}
	id := &fakeIdentity{auth: identity.Credential{
		Device: identity.Device{ID: "90000000-0000-4000-8000-000000000001"},
		Scopes: []string{"knowledge:read", "knowledge:write"},
	}}
	handler, err := New(Options{
		Identity: id, Knowledge: knowledgeService,
		Readiness: fakeReadiness{report: health.Report{Status: health.StatusHealthy, Components: map[string]health.Component{}}},
		Logger:    slog.New(slog.NewJSONHandler(&logs, nil)), PairLimiter: NewFixedWindowLimiter(10, time.Minute),
		AuthLimiter: NewFixedWindowLimiter(10, time.Minute), DeviceLimiter: NewFixedWindowLimiter(100, time.Minute),
		MaxKnowledgeRequestBody: 4096,
	})
	if err != nil {
		t.Fatal(err)
	}

	request := httptest.NewRequest(http.MethodGet, "/v1/knowledge/revisions/head", nil)
	request.Header.Set("Authorization", "Bearer valid")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), revision.ID) {
		t.Fatalf("head: %d %s", response.Code, response.Body.String())
	}

	const markdownSecret = "private markdown body"
	body := `{"operation_id":"20000000-0000-4000-8000-000000000001","expected_parent_revision_id":null,"source":"test","documents":[{"path":"note.md","markdown":"` + markdownSecret + `"}]}`
	request = httptest.NewRequest(http.MethodPost, "/v1/knowledge/imports", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer valid")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusCreated || knowledgeService.importCommand.ActorDeviceID != id.auth.Device.ID || !knowledgeService.importCommand.ExpectedParentProvided {
		t.Fatalf("import: %d %s command=%+v", response.Code, response.Body.String(), knowledgeService.importCommand)
	}
	if strings.Contains(logs.String(), markdownSecret) {
		t.Fatalf("knowledge audit log leaked Markdown: %s", logs.String())
	}

	request = httptest.NewRequest(http.MethodPost, "/v1/knowledge/imports", strings.NewReader(`{"operation_id":"20000000-0000-4000-8000-000000000002","source":"test","documents":[],"unknown":true}`))
	request.Header.Set("Authorization", "Bearer valid")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), knowledge.CodeInvalidRequest) {
		t.Fatalf("strict import JSON: %d %s", response.Code, response.Body.String())
	}

	tooLongSource, err := json.Marshal(map[string]any{
		"operation_id": "20000000-0000-4000-8000-000000000005",
		"source":       strings.Repeat("知", knowledge.MaxSourceRunes+1),
		"documents":    []map[string]string{{"path": "note.md", "markdown": "body"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	knowledgeService.importErr = &knowledge.Error{Code: knowledge.CodeInvalidRequest}
	request = httptest.NewRequest(http.MethodPost, "/v1/knowledge/imports", bytes.NewReader(tooLongSource))
	request.Header.Set("Authorization", "Bearer valid")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("501-rune source mapping: %d %s", response.Code, response.Body.String())
	}
	knowledgeService.importErr = nil

	id.auth.Scopes = []string{"knowledge:read"}
	request = httptest.NewRequest(http.MethodPost, "/v1/knowledge/imports", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer valid")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("knowledge write scope was not enforced: %d", response.Code)
	}
}

func TestKnowledgeReviewAndBodyLimitHTTPMapping(t *testing.T) {
	var logs bytes.Buffer
	review := &knowledge.IdentityReview{BasisHash: strings.Repeat("a", 64), Documents: []knowledge.DocumentIdentityReview{{Path: "note.md", Locator: strings.Repeat("b", 64)}}}
	knowledgeService := &fakeKnowledge{importErr: &knowledge.Error{Code: knowledge.CodeIdentityReviewRequired, Review: review}}
	id := &fakeIdentity{auth: identity.Credential{Device: identity.Device{ID: "90000000-0000-4000-8000-000000000001"}, Scopes: []string{"knowledge:write"}}}
	handler, err := New(Options{
		Identity: id, Knowledge: knowledgeService,
		Readiness: fakeReadiness{report: health.Report{Status: health.StatusHealthy, Components: map[string]health.Component{}}},
		Logger:    slog.New(slog.NewJSONHandler(&logs, nil)), PairLimiter: NewFixedWindowLimiter(10, time.Minute),
		AuthLimiter: NewFixedWindowLimiter(10, time.Minute), DeviceLimiter: NewFixedWindowLimiter(100, time.Minute),
		MaxKnowledgeRequestBody: 256,
	})
	if err != nil {
		t.Fatal(err)
	}
	body := `{"operation_id":"20000000-0000-4000-8000-000000000003","expected_parent_revision_id":null,"source":"test","documents":[{"path":"note.md","markdown":"changed"}]}`
	request := httptest.NewRequest(http.MethodPost, "/v1/knowledge/imports", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer valid")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "identity_review") || strings.Contains(response.Body.String(), "changed") {
		t.Fatalf("identity review mapping: %d %s", response.Code, response.Body.String())
	}

	knowledgeService.importErr = &knowledge.Error{Code: knowledge.CodeRevisionConflict, CurrentRevisionKnown: true}
	request = httptest.NewRequest(http.MethodPost, "/v1/knowledge/imports", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer valid")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	var conflict map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &conflict); err != nil {
		t.Fatal(err)
	}
	current, exists := conflict["current_revision_id"]
	if response.Code != http.StatusConflict || !exists || current != nil {
		t.Fatalf("empty-head revision conflict did not return explicit null: %d %s", response.Code, response.Body.String())
	}

	large := `{"operation_id":"20000000-0000-4000-8000-000000000004","expected_parent_revision_id":null,"source":"test","documents":[{"path":"note.md","markdown":"` + strings.Repeat("x", 400) + `"}]}`
	request = httptest.NewRequest(http.MethodPost, "/v1/knowledge/imports", strings.NewReader(large))
	request.Header.Set("Authorization", "Bearer valid")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusRequestEntityTooLarge || !strings.Contains(response.Body.String(), knowledge.CodePayloadTooLarge) {
		t.Fatalf("body limit mapping: %d %s", response.Code, response.Body.String())
	}
}

var _ = errors.Is
