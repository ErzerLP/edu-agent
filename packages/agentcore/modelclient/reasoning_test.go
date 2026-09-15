package modelclient

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestReasoningEffortSerializationAndValidation(t *testing.T) {
	var received []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			return
		}
		received = append(received, body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
	}))
	defer server.Close()
	client, err := New(server.URL, "model", "", time.Second, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, effort := range []ReasoningEffort{"", ReasoningEffortAuto, ReasoningEffortNone, ReasoningEffortMinimal, ReasoningEffortLow, ReasoningEffortMedium, ReasoningEffortHigh, ReasoningEffortXHigh, ReasoningEffortMax} {
		if _, err := client.Complete(t.Context(), Request{Messages: []Message{{Role: "user", Content: "hello"}}, ReasoningEffort: effort}); err != nil {
			t.Fatalf("effort=%q error=%v", effort, err)
		}
	}
	if len(received) != 9 {
		t.Fatalf("requests=%d", len(received))
	}
	for index, body := range received {
		value, present := body["reasoning_effort"]
		if index < 2 {
			if present {
				t.Fatalf("request %d unexpectedly sent reasoning_effort=%v", index, value)
			}
			continue
		}
		if !present {
			t.Fatalf("request %d omitted reasoning_effort", index)
		}
	}

	var calls atomic.Int32
	guard := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
	defer guard.Close()
	client, _ = New(guard.URL, "model", "", time.Second, nil)
	_, err = client.Complete(t.Context(), Request{Messages: []Message{{Role: "user", Content: "hello"}}, ReasoningEffort: "turbo"})
	if StableErrorCode(err) != ErrorCodeInvalidReasoningEffort || calls.Load() != 0 {
		t.Fatalf("error=%v code=%q calls=%d", err, StableErrorCode(err), calls.Load())
	}
}

func TestStreamOmitsAutoReasoningEffort(t *testing.T) {
	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, sse(`{"choices":[{"index":0,"delta":{"content":"ok"},"finish_reason":"stop"}]}`)+sse(`[DONE]`))
	}))
	defer server.Close()
	_, err := newTestClient(t, server.URL).Stream(t.Context(), Request{Messages: []Message{{Role: "user", Content: "hello"}}, ReasoningEffort: ReasoningEffortAuto}, func(StreamEvent) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if _, present := body["reasoning_effort"]; present {
		t.Fatalf("auto reasoning_effort was sent: %#v", body)
	}
}

func TestReasoningEffortUnsupportedRequiresExactParamAndUnsupportedCode(t *testing.T) {
	tests := []struct {
		name string
		body string
		want ErrorCode
	}{
		{
			name: "exact code and param",
			body: `{"error":{"code":"unsupported_parameter","type":"invalid_request_error","param":"reasoning_effort","message":"provider secret"}}`,
			want: ErrorCodeReasoningEffortUnsupported,
		},
		{
			name: "ordinary bad request",
			body: `{"error":{"code":"invalid_request_error","type":"invalid_request_error","param":"reasoning_effort","message":"provider secret"}}`,
		},
		{
			name: "unsupported different param",
			body: `{"error":{"code":"unsupported_parameter","param":"temperature","message":"provider secret"}}`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, test.body)
			}))
			defer server.Close()
			client, _ := New(server.URL, "model", "", time.Second, nil)
			_, err := client.Complete(t.Context(), Request{Messages: []Message{{Role: "user", Content: "hello"}}, ReasoningEffort: ReasoningEffortHigh})
			if StableErrorCode(err) != test.want {
				t.Fatalf("error=%v code=%q want=%q", err, StableErrorCode(err), test.want)
			}
			if err == nil || containsProviderSecret(err.Error()) {
				t.Fatalf("unsafe error=%v", err)
			}
		})
	}
}

func TestStreamReasoningUnsupportedDoesNotFallback(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"code":"unsupported_value","param":"reasoning_effort","message":"provider secret"}}`)
	}))
	defer server.Close()
	_, err := newTestClient(t, server.URL).Stream(t.Context(), Request{Messages: []Message{{Role: "user", Content: "hello"}}, ReasoningEffort: ReasoningEffortMax}, func(StreamEvent) error { return nil })
	if StableErrorCode(err) != ErrorCodeReasoningEffortUnsupported || calls.Load() != 1 {
		t.Fatalf("error=%v code=%q calls=%d", err, StableErrorCode(err), calls.Load())
	}
}

func containsProviderSecret(value string) bool {
	return strings.Contains(value, "provider secret")
}
