package httpapi

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5/middleware"
)

func TestModelGatewayAuditRedactsUntrustedPathAndRequestID(t *testing.T) {
	var logs bytes.Buffer
	a := &API{logger: slog.New(slog.NewJSONHandler(&logs, nil))}
	h := middleware.RequestID(a.audit(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(404) })))
	r := httptest.NewRequest("GET", "/v1/model-gateway/requests/private-path-marker?private-query-marker", nil)
	r.Header.Set("X-Request-ID", "private-header-marker")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if strings.Contains(logs.String(), "private-") || strings.Contains(logs.String(), "remote_ip") {
		t.Fatalf("gateway audit contains private input: %s", logs.String())
	}
	if !strings.Contains(logs.String(), `"status":404`) || w.Header().Get("X-Request-ID") == "private-header-marker" {
		t.Fatal("safe correlation/status missing")
	}
}
