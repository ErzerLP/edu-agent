package mentorrun

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/edu-agent/edu-agent/packages/agentcore/modelclient"
)

func TestRunFailureLogClassifiesTruncationWithoutBodyOrSecrets(t *testing.T) {
	const sensitive = "private-key-and-source-sentinel"
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"choices":[{"finish_reason":"length","message":{"role":"assistant","content":%q}}]}`, sensitive)
	}))
	defer provider.Close()
	client, err := modelclient.New(provider.URL, "test-model", sensitive, time.Second, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, cause := client.Complete(t.Context(), modelclient.Request{MaxTokens: 2048, Messages: []modelclient.Message{{Role: "user", Content: sensitive}}})
	if modelclient.StableErrorCode(cause) != modelclient.ErrorCodeResponseTruncated {
		t.Fatal("fixture did not reproduce output truncation")
	}
	assertRunFailureLog(t, fmt.Errorf("%s: %w", sensitive, cause), "response_truncated", sensitive)
}

func TestRunFailureLogUsesOnlyFixedCategories(t *testing.T) {
	const sensitive = "private-key-and-error-sentinel"
	for _, test := range []struct {
		name  string
		cause error
		want  string
	}{
		{"protocol", &modelclient.ClientError{Code: modelclient.ErrorCodeResponseProtocol, Message: sensitive}, "response_protocol_error"},
		{"filtered", &modelclient.ClientError{Code: modelclient.ErrorCodeContentFiltered, Message: sensitive}, "content_filtered"},
		{"unknown_code", &modelclient.ClientError{Code: modelclient.ErrorCode(sensitive), Message: sensitive}, "model_failed"},
		{"unclassified", errors.New(sensitive), "model_failed"},
		{"timeout", fmt.Errorf("%s: %w", sensitive, context.DeadlineExceeded), "timeout"},
		{"cancelled", context.Canceled, "cancelled"},
		{"citation", errCitation, "invalid_source_citation"},
		{"context_limit", ErrLimit, "history_context_limit"},
		{"search", &searchFailure{reason: sensitive}, "research_search_failed"},
		{"context_changed", ErrInactive, "context_changed"},
	} {
		t.Run(test.name, func(t *testing.T) {
			assertRunFailureLog(t, test.cause, test.want, sensitive)
		})
	}
}

func assertRunFailureLog(t *testing.T, cause error, category, sensitive string) {
	t.Helper()
	var output bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&output, nil)))
	defer slog.SetDefault(previous)
	logRunFailure(t.Context(), row{Meta: Meta{RunID: "00000000-0000-4000-8000-000000000001", Kind: "start_learning", Stage: "waiting_model"}, body: Body{Output: sensitive}}, cause)
	if strings.Contains(output.String(), sensitive) {
		t.Fatal("failure log leaked a secret, source body, or raw error")
	}
	var entry map[string]any
	if json.Unmarshal(output.Bytes(), &entry) != nil {
		t.Fatal("failure did not produce a single structured log")
	}
	if entry["level"] != "WARN" || entry["msg"] != "mentor_run_failed" || entry["error_category"] != category || entry["stage"] != "waiting_model" || entry["kind"] != "start_learning" || entry["run_id"] != "00000000-0000-4000-8000-000000000001" {
		t.Fatalf("missing failure diagnostics: %v", entry)
	}
	if len(entry) != 7 {
		t.Fatal("failure log contains fields outside the diagnostic whitelist")
	}
}
