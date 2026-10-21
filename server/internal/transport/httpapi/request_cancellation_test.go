package httpapi

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/edu-agent/edu-agent/server/internal/memory"
	"github.com/edu-agent/edu-agent/server/internal/privacy"
)

func TestRequestCancellationIsNotInternalError(t *testing.T) {
	for _, operation := range []string{"source", "usage"} {
		for _, tc := range []struct {
			name      string
			cancelled bool
			err       error
			wantError bool
		}{
			{"client cancelled", true, fmt.Errorf("query interrupted: %w", context.Canceled), false},
			{"unexpected cancellation", false, context.Canceled, true},
			{"internal failure", false, errors.New("sensitive database detail"), true},
			{"internal failure after cancel", true, errors.New("sensitive database detail"), true},
		} {
			t.Run(operation+"/"+tc.name, func(t *testing.T) {
				var logs bytes.Buffer
				api := &API{logger: slog.New(slog.NewJSONHandler(&logs, nil))}
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				if tc.cancelled {
					cancel()
				}
				r := httptest.NewRequest(http.MethodGet, "/", nil).WithContext(ctx)
				w := newBufferedHTTPResponse()
				if operation == "source" {
					api.writeKnowledgeFailure(w, r, operation, tc.err)
				} else {
					api.writeLearningFailure(w, r, operation, tc.err)
				}
				if strings.Contains(logs.String(), `"level":"ERROR"`) != tc.wantError {
					t.Fatalf("wrong cancellation log level: %s", logs.String())
				}
				if !tc.wantError && (w.status != 0 || w.body.Len() != 0 || !strings.Contains(logs.String(), "request_cancelled")) {
					t.Fatalf("cancelled request wrote an error response: status=%d logs=%s", w.status, logs.String())
				}
				if strings.Contains(logs.String(), "sensitive database detail") {
					t.Fatal("leaked internal detail")
				}
			})
		}
	}
}

func TestResponsePermitClientCancellationLog(t *testing.T) {
	var logs bytes.Buffer
	api := &API{logger: slog.New(slog.NewJSONHandler(&logs, nil)), readPermits: privacy.NewReadPermitManager()}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	handler := api.handleResponseReadPermit(memory.CodeContentRedacted, privacy.OwnerLearning)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { cancel(); w.Write([]byte("private response")) }))
	w := newBufferedHTTPResponse()
	handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil).WithContext(ctx))
	if w.status != 0 || w.body.Len() != 0 || strings.Contains(logs.String(), `"level":"ERROR"`) || !strings.Contains(logs.String(), "request_cancelled") {
		t.Fatalf("client cancellation misreported or flushed: status=%d logs=%s", w.status, logs.String())
	}
}
