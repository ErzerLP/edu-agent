package mentorrun

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/edu-agent/edu-agent/packages/agentcore/modelclient"
	"github.com/edu-agent/edu-agent/server/internal/integrations/llm"
	"github.com/edu-agent/edu-agent/server/internal/learningcontent"
	"github.com/edu-agent/edu-agent/server/internal/research"
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
		{"network_timeout", &net.DNSError{Err: sensitive, IsTimeout: true}, "timeout"},
		{"network_unavailable", fmt.Errorf("%s: %w", sensitive, &net.DNSError{Err: sensitive, IsNotFound: true}), "unavailable"},
		{"citation", errCitation, "invalid_source_citation"},
		{"context_limit", ErrLimit, "history_context_limit"},
		{"search", &searchFailure{reason: sensitive}, "research_search_failed"},
		{"context_changed", ErrInactive, "context_changed"},
		{"content_invalid", learningcontent.ErrInvalid, "invalid_content_candidate"},
		{"content_conflict", learningcontent.ErrConflict, "selection_expired"},
		{"content_unavailable", learningcontent.ErrUnavailable, "content_unavailable"},
		{"research_policy", research.ErrPolicy, "research_policy_rejected"},
	} {
		t.Run(test.name, func(t *testing.T) {
			assertRunFailureLog(t, test.cause, test.want, sensitive)
		})
	}
}

func TestRunFailureLogClassifiesWrappedServerModelErrors(t *testing.T) {
	const sensitive = "private-server-model-error-sentinel"
	for _, category := range []llm.ErrorCategory{
		llm.ErrorUnauthorized, llm.ErrorRateLimited, llm.ErrorUpstream,
		llm.ErrorTimeout, llm.ErrorUnavailable, llm.ErrorInvalidResponse,
		llm.ErrorSchemaMismatch, llm.ErrorIncompatible, llm.ErrorInvalidRequest,
		llm.ErrorOutputBudgetExceeded, llm.ErrorResponseNotCompleted,
	} {
		t.Run(string(category), func(t *testing.T) {
			cause := fmt.Errorf("%s: %w", sensitive, &llm.Error{Category: category, Cause: errors.New(sensitive)})
			assertRunFailureLog(t, cause, string(category), sensitive)
		})
	}
	assertRunFailureLog(t, &llm.Error{Category: llm.ErrorCategory(sensitive)}, "model_failed", sensitive)
}

func TestPostgreSQLModelTransportTimeoutKeepsFailureCategory(t *testing.T) {
	for _, mode := range []string{"complete", "stream"} {
		t.Run(mode, func(t *testing.T) {
			f := fixture(t, func(w http.ResponseWriter, r *http.Request, _ int) {
				select {
				case <-r.Context().Done():
				case <-time.After(100 * time.Millisecond):
				}
			})
			f.accept(t)
			item, err := f.service.claim(t.Context())
			if err != nil || item == nil {
				t.Fatalf("claim: %v", err)
			}
			host := &executionHost{service: f.service, owned: *item, body: item.body}
			host.model, host.limits, _, err = f.settings.MentorClient(func(next http.RoundTripper) http.RoundTripper {
				next.(*http.Transport).ResponseHeaderTimeout = 10 * time.Millisecond
				return budgetTransport{host: host, next: next}
			})
			if err != nil {
				t.Fatal(err)
			}
			request := modelclient.Request{MaxTokens: 64, Messages: []modelclient.Message{{Role: "user", Content: "public probe"}}}
			var cause error
			if mode == "complete" {
				_, cause = (callModel{host}).Complete(t.Context(), request)
			} else {
				_, cause = (callModel{host}).Stream(t.Context(), request, func(modelclient.StreamEvent) error { return nil })
			}
			var networkError net.Error
			if !errors.As(cause, &networkError) || !networkError.Timeout() {
				t.Fatalf("transport timeout lost its type: %T", cause)
			}
			assertRunFailureLog(t, cause, "timeout", "private-timeout-sentinel")
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
	wantFields := 7
	if errors.Is(cause, errCitation) {
		wantFields = 9
		if entry["rejection_reason"] != "invalid_source_citation" || entry["rejection_field"] != "citations" {
			t.Fatal("missing citation rejection reason")
		}
	}
	if len(entry) != wantFields {
		t.Fatal("failure log contains fields outside the diagnostic whitelist")
	}
}
