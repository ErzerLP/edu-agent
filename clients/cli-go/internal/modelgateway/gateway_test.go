package modelgateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/modelclient"
)

const configJSON = `{"protocol_version":1,"invoke_allowed":true,"available":true,"privacy_generation":7,"models":[{"target":"teaching","display_name":"教学模型","model_name":"demo","configuration_revision":"config-1","destination_revision":"dest-1","context_tokens":32768,"output_tokens":128}],"limits":{"idle_timeout_seconds":60,"request_timeout_seconds":900},"capabilities":{"stream":true,"tools":true},"admission":{"per_device":1,"queue":false}}`

func gatewayModel(t *testing.T, server *httptest.Server) *Model {
	t.Helper()
	client, err := New(Options{ServerURL: server.URL, DeviceID: "device-1", Token: "device-token", Select: func(_ context.Context, c Configuration) (Connection, error) { return c.Models[0], nil }})
	if err != nil {
		t.Fatal(err)
	}
	model, err := client.Bind(t.Context(), Identity{SessionID: "11111111-1111-4111-8111-111111111111", TurnID: "22222222-2222-4222-8222-222222222222", SpaceID: "33333333-3333-4333-8333-333333333333"})
	if err != nil {
		t.Fatal(err)
	}
	return model
}

func TestGatewayTextAndToolRequestsCarryIdentity(t *testing.T) {
	var requests []http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/model-gateway/config" {
			fmt.Fprint(w, configJSON)
			return
		}
		if r.URL.Path != "/v1/model-gateway/chat/completions" {
			t.Errorf("path %s", r.URL.Path)
		}
		requests = append(requests, r.Header.Clone())
		var body struct {
			Model     string                `json:"model"`
			MaxTokens int                   `json:"max_tokens"`
			Messages  []modelclient.Message `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if body.Model != "teaching" || body.MaxTokens != 128 {
			t.Errorf("body = %+v", body)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		if len(requests) == 1 {
			fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call1\",\"type\":\"function\",\"function\":{\"name\":\"read\",\"arguments\":\"{}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n")
		} else {
			if len(body.Messages) != 3 || body.Messages[2].ToolCallID != "call1" {
				t.Errorf("lost tool result: %+v", body.Messages)
			}
			fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"完成\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
		}
	}))
	defer server.Close()
	model := gatewayModel(t, server)
	messages := []modelclient.Message{{Role: "user", Content: "读取"}}
	first, err := model.Stream(t.Context(), modelclient.Request{Messages: messages}, func(modelclient.StreamEvent) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	messages = append(messages, first.Message, modelclient.Message{Role: "tool", ToolCallID: "call1", Content: "文件正文"})
	second, err := model.Stream(t.Context(), modelclient.Request{Messages: messages}, func(modelclient.StreamEvent) error { return nil })
	if err != nil || second.Message.Content != "完成" {
		t.Fatalf("%+v %v", second, err)
	}
	for _, h := range requests {
		for k, want := range map[string]string{"Authorization": "Bearer device-token", "X-Model-Gateway-Version": "1", "X-Model-Configuration": "config-1", "X-Learning-Generation": "7", "X-CLI-Session-ID": model.Identity.SessionID, "X-CLI-Turn-ID": model.Identity.TurnID, "X-CLI-Model-Stage": "foreground", "X-Learning-Space-ID": model.Identity.SpaceID} {
			if h.Get(k) != want {
				t.Errorf("%s=%q", k, h.Get(k))
			}
		}
	}
	if len(requests) != 2 || requests[0].Get("X-CLI-Request-ID") == requests[1].Get("X-CLI-Request-ID") || len(requests[0].Get("X-CLI-Request-ID")) != 36 {
		t.Fatalf("request IDs: %v", requests)
	}
}

func TestGatewayErrorsDoNotRetryOrExposeHTML(t *testing.T) {
	for _, tc := range []struct {
		name                string
		status              int
		body, content, want string
	}{
		{"busy", 429, `{"error":{"type":"gateway_error","code":"gateway_busy","message":"secret"}}`, "application/json", "繁忙"},
		{"html", 404, "<html>proxy secret</html>", "text/html", "部署"},
		{"html502", 502, "<html>proxy secret</html>", "text/html", "部署"},
		{"no compatibility retry", 400, `{"error":{"code":"stream_not_supported","message":"secret"}}`, "application/json", "无法"},
		{"stream error", 200, "event: error\ndata: {\"error\":{\"type\":\"gateway_error\",\"code\":\"device_revoked\",\"message\":\"secret\"}}\n\n", "text/event-stream", "撤销"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1/model-gateway/config" {
					fmt.Fprint(w, configJSON)
					return
				}
				calls.Add(1)
				w.Header().Set("Content-Type", tc.content)
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.body)
			}))
			defer server.Close()
			model := gatewayModel(t, server)
			_, err := model.Stream(t.Context(), modelclient.Request{Messages: []modelclient.Message{{Role: "user", Content: "hi"}}}, func(modelclient.StreamEvent) error { return nil })
			if err == nil || !strings.Contains(err.Error(), tc.want) || strings.Contains(err.Error(), "secret") || calls.Load() != 1 {
				t.Fatalf("calls=%d err=%v", calls.Load(), err)
			}
		})
	}
}

func TestGatewayCancelAndSerialInternalRequests(t *testing.T) {
	started := make(chan struct{})
	cancelled := make(chan http.Header, 1)
	var active, maxActive atomic.Int32
	var mu sync.Mutex
	var stages []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		if r.URL.Path == "/v1/model-gateway/config" {
			fmt.Fprint(w, configJSON)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/cancel") {
			cancelled <- r.Header.Clone()
			fmt.Fprint(w, `{"state":"cancelled"}`)
			return
		}
		n := active.Add(1)
		defer active.Add(-1)
		for old := maxActive.Load(); n > old; old = maxActive.Load() {
			if maxActive.CompareAndSwap(old, n) {
				break
			}
		}
		stage := r.Header.Get("X-CLI-Model-Stage")
		mu.Lock()
		stages = append(stages, stage)
		mu.Unlock()
		if stage == "foreground" {
			close(started)
			<-r.Context().Done()
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}]}`)
	}))
	defer server.Close()
	model := gatewayModel(t, server)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	req := modelclient.Request{Messages: []modelclient.Message{{Role: "user", Content: "hi"}}}
	done := make(chan error, 1)
	go func() { _, err := model.Complete(ctx, req); done <- err }()
	<-started
	title := make(chan error, 1)
	go func() { _, err := model.Complete(WithStage(t.Context(), "session_title"), req); title <- err }()
	cancel()
	if err := <-done; err == nil {
		t.Fatal("cancel succeeded")
	}
	select {
	case h := <-cancelled:
		if h.Get("X-Learning-Generation") != "7" || h.Get("Authorization") != "Bearer device-token" {
			t.Fatal(h)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("missing cancel")
	}
	if err := <-title; err != nil {
		t.Fatal(err)
	}
	if maxActive.Load() != 1 {
		t.Fatalf("concurrent requests: %d", maxActive.Load())
	}
	mu.Lock()
	defer mu.Unlock()
	if len(stages) != 2 || stages[1] != "session_title" {
		t.Fatal(stages)
	}
}

func TestGatewayRejectsRedirectAndDoesNotLeakToken(t *testing.T) {
	var leaked atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { leaked.Add(1) }))
	defer other.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/model-gateway/config" {
			fmt.Fprint(w, configJSON)
			return
		}
		http.Redirect(w, r, other.URL, http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	model := gatewayModel(t, server)
	_, err := model.Complete(t.Context(), modelclient.Request{Messages: []modelclient.Message{{Role: "user", Content: "private"}}})
	if err == nil || leaked.Load() != 0 {
		t.Fatalf("redirect leaked request: %v %d", err, leaked.Load())
	}
}

func TestGatewayNewTurnCancelsQueuedOldWork(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/model-gateway/config" {
			fmt.Fprint(w, configJSON)
			return
		}
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}]}`)
	}))
	defer server.Close()
	model := gatewayModel(t, server)
	_, err := model.Prepare(t.Context(), Identity{SessionID: model.Identity.SessionID, TurnID: "44444444-4444-4444-8444-444444444444"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = model.Complete(WithStage(t.Context(), "session_title"), modelclient.Request{Messages: []modelclient.Message{{Role: "user", Content: "old title"}}})
	if err == nil || calls.Load() != 0 {
		t.Fatalf("stale background task sent: %v calls=%d", err, calls.Load())
	}
}

func TestGatewayPartialUsageDoesNotCalibrateAsComplete(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1/model-gateway/config" {
					fmt.Fprint(w, configJSON)
					return
				}
				if stream {
					w.Header().Set("Content-Type", "text/event-stream")
					fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"完成\"},\"finish_reason\":\"stop\"}]}\n\ndata: {\"choices\":[],\"usage\":{\"completion_tokens\":3}}\n\ndata: [DONE]\n\n")
				} else {
					w.Header().Set("Content-Type", "application/json")
					fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"完成"},"finish_reason":"stop"}],"usage":{"completion_tokens":3}}`)
				}
			}))
			defer server.Close()
			model := gatewayModel(t, server)
			request := modelclient.Request{Messages: []modelclient.Message{{Role: "user", Content: "hi"}}}
			var response modelclient.Response
			var err error
			if stream {
				response, err = model.Stream(t.Context(), request, func(modelclient.StreamEvent) error { return nil })
			} else {
				response, err = model.Complete(t.Context(), request)
			}
			if err != nil || response.Message.Content != "完成" || response.Usage != nil {
				t.Fatalf("partial usage treated as complete: %+v %v", response, err)
			}
		})
	}
}

func TestGatewayOverallDeadlineCancelsWithoutRetry(t *testing.T) {
	var calls, cancels atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/model-gateway/config" {
			fmt.Fprint(w, strings.Replace(configJSON, `"request_timeout_seconds":900`, `"request_timeout_seconds":1`, 1))
			return
		}
		_, _ = io.Copy(io.Discard, r.Body)
		if strings.HasSuffix(r.URL.Path, "/cancel") {
			cancels.Add(1)
			fmt.Fprint(w, `{"state":"cancelled"}`)
			return
		}
		calls.Add(1)
		<-r.Context().Done()
	}))
	defer server.Close()
	model := gatewayModel(t, server)
	start := time.Now()
	_, err := model.Complete(t.Context(), modelclient.Request{Messages: []modelclient.Message{{Role: "user", Content: "hi"}}})
	if err == nil || !strings.Contains(err.Error(), "model_request_timeout") || time.Since(start) > 3*time.Second || calls.Load() != 1 || cancels.Load() != 1 {
		t.Fatalf("deadline/retry: %v calls=%d cancel=%d elapsed=%s", err, calls.Load(), cancels.Load(), time.Since(start))
	}
}

func TestGatewayStreamWaitsIdleTimeoutPlusFiveSeconds(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/model-gateway/config" {
			fmt.Fprint(w, strings.Replace(configJSON, `"idle_timeout_seconds":60`, `"idle_timeout_seconds":5`, 1))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.(http.Flusher).Flush()
		select {
		case <-time.After(6 * time.Second):
		case <-r.Context().Done():
			return
		}
		fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"完成\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer server.Close()
	model := gatewayModel(t, server)
	result, err := model.Stream(t.Context(), modelclient.Request{Messages: []modelclient.Message{{Role: "user", Content: "hi"}}}, func(modelclient.StreamEvent) error { return nil })
	if err != nil || result.Message.Content != "完成" {
		t.Fatalf("did not allow idle+5: %v %+v", err, result)
	}
}

func TestGatewayUsesServerCapacityWithoutInventingOutputRatio(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/model-gateway/config" {
			fmt.Fprint(w, strings.Replace(configJSON, `"output_tokens":128`, `"output_tokens":32768`, 1))
			return
		}
		var body struct {
			MaxTokens int `json:"max_tokens"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if body.MaxTokens != 32 {
			t.Errorf("planned output changed: %d", body.MaxTokens)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"完成"},"finish_reason":"stop"}]}`)
	}))
	defer server.Close()
	model := gatewayModel(t, server)
	if model.Selected.OutputTokens != 32768 {
		t.Fatal("server capacity clamped")
	}
	if _, err := model.Complete(t.Context(), modelclient.Request{Messages: []modelclient.Message{{Role: "user", Content: "hi"}}, MaxTokens: 32}); err != nil {
		t.Fatal(err)
	}
}

func TestGatewayConfigurationRefreshCarriesKnownGeneration(t *testing.T) {
	var configs atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if configs.Add(1) > 1 && (r.Header.Get("X-Model-Gateway-Version") != "1" || r.Header.Get("X-Learning-Generation") != "7") {
			t.Error("configuration refresh omitted known protocol/generation")
		}
		fmt.Fprint(w, configJSON)
	}))
	defer server.Close()
	model := gatewayModel(t, server)
	if _, err := model.Prepare(t.Context(), model.Identity); err != nil {
		t.Fatal(err)
	}
}
