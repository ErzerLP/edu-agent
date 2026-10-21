package httpapi

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/edu-agent/edu-agent/server/internal/mentorrun"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestWebImportFailureLogsSafeCategory(t *testing.T) {
	const private = "private document text and title"
	for _, tc := range []struct {
		name, category string
		err            error
		status         int
	}{
		{"constraint", "database_constraint", fmt.Errorf("%s: %w", private, &pgconn.PgError{Code: "23514", Message: private, Detail: private, ConstraintName: private}), 503},
		{"serialization", "database_transaction_conflict", &pgconn.PgError{Code: "40001", Message: private}, 503},
		{"lock timeout", "database_transaction_conflict", &pgconn.PgError{Code: "55P03", Message: private}, 503},
		{"internal", "internal", errors.New(private), 503},
		{"cancelled", "cancelled", fmt.Errorf("%s: %w", private, context.Canceled), 503},
		{"version", "version_conflict", fmt.Errorf("%s: %w", private, mentorrun.ErrConflict), 409},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var logs bytes.Buffer
			a := &API{logger: slog.New(slog.NewJSONHandler(&logs, nil))}
			response := httptest.NewRecorder()
			request := httptest.NewRequest("POST", "/v1/learning/web-imports", nil)
			a.writeWebImportFailure(response, request, tc.err)
			if response.Code != tc.status || !strings.Contains(logs.String(), `"error_category":"`+tc.category+`"`) || strings.Contains(logs.String(), private) || strings.Contains(response.Body.String(), private) {
				t.Fatalf("unsafe or unclassified failure: status=%d logs=%s body=%s", response.Code, logs.String(), response.Body.String())
			}
		})
	}
}
