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
	"github.com/edu-agent/edu-agent/server/internal/platform/health"
)

type fakeIdentity struct {
	exchange    identity.IssuedCredential
	exchangeErr error
	auth        identity.Credential
	authErr     error
	devices     []identity.Device
	revoked     string
	revokeErr   error
}

func (f *fakeIdentity) ExchangePairingCode(context.Context, string, string) (identity.IssuedCredential, error) {
	return f.exchange, f.exchangeErr
}
func (f *fakeIdentity) Authenticate(context.Context, string, string) (identity.Credential, error) {
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

func newTestAPI(t *testing.T, id *fakeIdentity, pairLimit, authLimit int, logs *bytes.Buffer) http.Handler {
	t.Helper()
	logger := slog.New(slog.NewJSONHandler(logs, nil))
	handler, err := New(Options{
		Identity: id, Model: fakeModel{result: llm.Capabilities{Compatible: true}},
		Readiness: fakeReadiness{report: health.Report{Status: health.StatusHealthy, Components: map[string]health.Component{}}},
		Logger:    logger, PairLimiter: NewFixedWindowLimiter(pairLimit, time.Minute), AuthLimiter: NewFixedWindowLimiter(authLimit, time.Minute),
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
	handler, err := New(Options{Identity: id, Readiness: fakeReadiness{report: health.Report{Status: health.StatusNotReady, Components: map[string]health.Component{"postgresql": {Status: health.StatusNotReady, Reason: "unavailable"}}}}, Logger: logger, PairLimiter: NewFixedWindowLimiter(1, time.Minute), AuthLimiter: NewFixedWindowLimiter(1, time.Minute)})
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

var _ = errors.Is
