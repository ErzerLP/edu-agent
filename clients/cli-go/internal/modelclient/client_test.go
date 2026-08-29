package modelclient

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestCompleteSendsOpenAICompatibleToolRequest(t *testing.T) {
	var received completionRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer model-token" {
			t.Fatalf("request path=%s authorization=%q", r.URL.Path, r.Header.Get("Authorization"))
		}
		if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
			t.Fatal(err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"call-1","type":"function","function":{"name":"get_learning_progress","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`)
	}))
	defer server.Close()
	client, err := New(server.URL+"/v1", "test-model", "model-token", time.Second, nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Complete(t.Context(), Request{
		Messages: []Message{{Role: "user", Content: "查看进度"}},
		Tools:    []Tool{{Type: "function", Function: ToolDefinition{Name: "get_learning_progress", Parameters: json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`)}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if received.Model != "test-model" || received.Stream || len(response.Message.ToolCalls) != 1 {
		t.Fatalf("request=%+v response=%+v", received, response)
	}
}

func TestCompleteRefusesRedirectAndRedactsProviderBody(t *testing.T) {
	var redirected bool
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		redirected = true
		if r.Header.Get("Authorization") != "" {
			t.Fatal("authorization forwarded to redirect target")
		}
	}))
	defer destination.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, destination.URL, http.StatusFound)
	}))
	defer source.Close()
	client, _ := New(source.URL, "model", "private-model-token", time.Second, nil)
	_, err := client.Complete(t.Context(), Request{Messages: []Message{{Role: "user", Content: "hello"}}})
	if err == nil || redirected {
		t.Fatalf("error=%v redirected=%t", err, redirected)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"code":"invalid_api_key","message":"private-model-token"}}`)
	}))
	defer server.Close()
	client, _ = New(server.URL, "model", "private-model-token", time.Second, nil)
	_, err = client.Complete(t.Context(), Request{Messages: []Message{{Role: "user", Content: "hello"}}})
	if err == nil || strings.Contains(err.Error(), "private-model-token") {
		t.Fatalf("unsafe error=%v", err)
	}
}
