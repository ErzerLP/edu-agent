package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestChatAndCapabilityProbe(t *testing.T) {
	server := fakeServer(t, "success", "test-key")
	defer server.Close()
	client := newTestClient(t, server.URL, 500*time.Millisecond)
	capabilities := client.Probe(context.Background())
	if !capabilities.Compatible || !capabilities.SystemUserAssistant || !capabilities.StructuredJSON || !capabilities.NativeJSONSchema || capabilities.ToolCalls {
		t.Fatalf("unexpected capabilities: %+v", capabilities)
	}
}

func TestFakeServerContractFailures(t *testing.T) {
	tests := []struct {
		mode     string
		category ErrorCategory
		timeout  time.Duration
	}{
		{mode: "invalid-json", category: ErrorInvalidResponse, timeout: 500 * time.Millisecond},
		{mode: "schema-mismatch", category: ErrorSchemaMismatch, timeout: 500 * time.Millisecond},
		{mode: "unauthorized", category: ErrorUnauthorized, timeout: 500 * time.Millisecond},
		{mode: "rate-limited", category: ErrorRateLimited, timeout: 500 * time.Millisecond},
		{mode: "server-error", category: ErrorUpstream, timeout: 500 * time.Millisecond},
		{mode: "timeout", category: ErrorTimeout, timeout: 10 * time.Millisecond},
	}
	for _, test := range tests {
		t.Run(test.mode, func(t *testing.T) {
			server := fakeServer(t, test.mode, "test-key")
			defer server.Close()
			client := newTestClient(t, server.URL, test.timeout)
			capabilities := client.Probe(context.Background())
			if capabilities.Compatible || len(capabilities.IncompatibilityReasons) != 1 || capabilities.IncompatibilityReasons[0] != string(test.category) {
				t.Fatalf("unexpected probe result: %+v", capabilities)
			}
		})
	}
}

func TestProbeCachesSuccessfulCapabilityCheck(t *testing.T) {
	var capabilityRequests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		capabilityRequests++
		writeEnvelope(w, `{"capability_probe":true}`)
	}))
	defer server.Close()
	client := newTestClient(t, server.URL, time.Second)
	first := client.Probe(context.Background())
	second := client.Probe(context.Background())
	if !first.Compatible || !second.Compatible || capabilityRequests != 1 {
		t.Fatalf("expected one cached capability request, requests=%d first=%+v second=%+v", capabilityRequests, first, second)
	}
}

func TestCachedProbeDetectsEndpointOutage(t *testing.T) {
	server := fakeServer(t, "success", "test-key")
	client := newTestClient(t, server.URL, 100*time.Millisecond)
	if result := client.Probe(context.Background()); !result.Compatible {
		t.Fatalf("initial probe failed: %+v", result)
	}
	server.Close()
	result := client.Probe(context.Background())
	if result.Compatible || len(result.IncompatibilityReasons) != 1 || result.IncompatibilityReasons[0] != string(ErrorUnavailable) {
		t.Fatalf("cached probe masked endpoint outage: %+v", result)
	}
}

func TestCanceledProbeIsNotCached(t *testing.T) {
	var capabilityRequests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		capabilityRequests++
		writeEnvelope(w, `{"capability_probe":true}`)
	}))
	defer server.Close()
	client := newTestClient(t, server.URL, time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if result := client.Probe(ctx); result.Compatible {
		t.Fatalf("canceled probe unexpectedly succeeded: %+v", result)
	}
	if result := client.Probe(context.Background()); !result.Compatible || capabilityRequests != 1 {
		t.Fatalf("canceled result poisoned cache: requests=%d result=%+v", capabilityRequests, result)
	}
}

func TestOptionalNativeJSONSchemaIsNegotiated(t *testing.T) {
	server := fakeServer(t, "no-native-schema", "test-key")
	defer server.Close()
	client := newTestClient(t, server.URL, 500*time.Millisecond)
	capabilities := client.Probe(context.Background())
	if !capabilities.Compatible || !capabilities.StructuredJSON || capabilities.NativeJSONSchema {
		t.Fatalf("native schema rejection must not fail the core profile: %+v", capabilities)
	}
}

func TestConnectionFailureIsUnavailable(t *testing.T) {
	server := fakeServer(t, "success", "test-key")
	endpoint := server.URL
	server.Close()
	client := newTestClient(t, endpoint, 100*time.Millisecond)
	result := client.Probe(context.Background())
	if result.Compatible || len(result.IncompatibilityReasons) != 1 || result.IncompatibilityReasons[0] != string(ErrorUnavailable) {
		t.Fatalf("unexpected result: %+v", result)
	}
}

func TestErrorsDoNotLeakCredentialOrResponseBody(t *testing.T) {
	const secret = "super-secret-model-key"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("upstream-secret-body"))
	}))
	defer server.Close()
	client := newTestClientWithKey(t, server.URL, secret, time.Second)
	_, err := client.Chat(context.Background(), ChatRequest{Messages: []Message{{Role: RoleUser, Content: "test"}}})
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "upstream-secret-body") {
		t.Fatalf("error leaked confidential data: %v", err)
	}
}

func TestChatRejectsSchemaMismatch(t *testing.T) {
	server := fakeServer(t, "schema-mismatch", "test-key")
	defer server.Close()
	client := newTestClient(t, server.URL, time.Second)
	schema := json.RawMessage(`{"type":"object","properties":{"capability_probe":{"type":"boolean"}},"required":["capability_probe"]}`)
	_, err := client.Chat(context.Background(), ChatRequest{Messages: []Message{{Role: RoleUser, Content: "test"}}, Schema: schema})
	var modelErr *Error
	if !errors.As(err, &modelErr) || modelErr.Category != ErrorSchemaMismatch {
		t.Fatalf("expected schema mismatch, got %v", err)
	}
}

func newTestClient(t *testing.T, base string, timeout time.Duration) *Client {
	return newTestClientWithKey(t, base, "test-key", timeout)
}

func newTestClientWithKey(t *testing.T, base, key string, timeout time.Duration) *Client {
	t.Helper()
	parsed, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	client, err := New(Options{BaseURL: parsed, Model: "fake", APIKey: key, ContextWindow: 8192, MinimumContext: 4096, Timeout: timeout})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func fakeServer(t *testing.T, mode, expectedKey string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if mode != "unauthorized" && r.Header.Get("Authorization") != "Bearer "+expectedKey {
			t.Errorf("authorization header did not carry configured key")
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		var request struct {
			Messages       []Message `json:"messages"`
			Stream         bool      `json:"stream"`
			ResponseFormat struct {
				Type string `json:"type"`
			} `json:"response_format"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode request: %v", err)
		}
		switch mode {
		case "unauthorized":
			w.WriteHeader(http.StatusUnauthorized)
		case "rate-limited":
			w.WriteHeader(http.StatusTooManyRequests)
		case "server-error":
			w.WriteHeader(http.StatusBadGateway)
		case "timeout":
			time.Sleep(100 * time.Millisecond)
			writeEnvelope(w, `{"capability_probe":true}`)
		case "invalid-json":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"choices":`))
		case "schema-mismatch":
			writeEnvelope(w, `{"capability_probe":"yes"}`)
		case "no-native-schema":
			if request.ResponseFormat.Type == "json_schema" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			fallthrough
		default:
			roles := map[Role]bool{}
			for _, message := range request.Messages {
				roles[message.Role] = true
			}
			if request.Stream {
				t.Errorf("probe unexpectedly requested streaming: %+v", request)
			}
			if request.ResponseFormat.Type != "json_schema" && (!roles[RoleSystem] || !roles[RoleUser] || !roles[RoleAssistant]) {
				t.Errorf("core probe did not exercise required roles: %+v", request)
			}
			writeEnvelope(w, `{"capability_probe":true}`)
		}
	}))
}

func writeEnvelope(w http.ResponseWriter, content string) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = fmt.Fprintf(w, `{"choices":[{"message":{"role":"assistant","content":%q}}]}`, content)
}
