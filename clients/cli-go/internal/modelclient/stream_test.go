package modelclient

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestStreamSendsCompatibleRequestAndAssemblesTextUsage(t *testing.T) {
	var received completionRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" || r.Header.Get("Accept") != "text/event-stream" || r.Header.Get("Content-Type") != "application/json" || r.Header.Get("Authorization") != "Bearer token" {
			t.Errorf("path=%s accept=%q content-type=%q authorization=%q", r.URL.Path, r.Header.Get("Accept"), r.Header.Get("Content-Type"), r.Header.Get("Authorization"))
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
			t.Error(err)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		flusher := w.(http.Flusher)
		writeSSE(t, w, `{"choices":[{"index":0,"delta":{"role":"assistant","reasoning_content":"hidden-secret"},"finish_reason":null}]}`)
		payload := []byte("data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"你好\"},\"finish_reason\":null}]}\n\n")
		split := bytes.Index(payload, []byte("你")) + 1
		_, _ = w.Write(payload[:split])
		flusher.Flush()
		_, _ = w.Write(payload[split:])
		flusher.Flush()
		writeSSE(t, w, `{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`)
		writeSSE(t, w, `{"choices":[],"usage":{"prompt_tokens":11,"completion_tokens":3,"total_tokens":14,"prompt_tokens_details":{"cached_tokens":7}}}`)
		writeSSE(t, w, `[DONE]`)
	}))
	defer server.Close()

	client := newTestClient(t, server.URL+"/v1")
	var events []StreamEvent
	response, err := client.Stream(t.Context(), Request{
		Messages:        []Message{{Role: "user", Content: "你好"}},
		MaxTokens:       512,
		ReasoningEffort: ReasoningEffortHigh,
	}, func(event StreamEvent) error {
		events = append(events, event)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !received.Stream || received.StreamOptions == nil || !received.StreamOptions.IncludeUsage || received.ReasoningEffort != ReasoningEffortHigh || received.MaxTokens != 512 {
		t.Fatalf("request=%+v", received)
	}
	if response.Usage == nil {
		t.Fatal("response usage is nil")
	}
	cacheRead, cacheReported := response.Usage.CacheReadTokens()
	if response.Message.Role != "assistant" || response.Message.Content != "你好" || response.FinishReason != "stop" || response.Usage.TotalTokens != 14 || !cacheReported || cacheRead != 7 || response.CompatibilityFallback {
		t.Fatalf("response=%+v cache-read=%d reported=%t", response, cacheRead, cacheReported)
	}
	if len(events) != 2 || events[0].Kind != StreamEventResponseStarted || events[1] != (StreamEvent{Kind: StreamEventTextDelta, Text: "你好"}) {
		t.Fatalf("events=%+v", events)
	}
	for _, event := range events {
		if strings.Contains(event.Text, "hidden-secret") {
			t.Fatalf("hidden reasoning leaked: %+v", events)
		}
	}
}

func TestStreamPreservesExplicitZeroPromptCacheUsage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		writeSSE(t, w, `{"choices":[{"index":0,"delta":{"role":"assistant","content":"ok"},"finish_reason":null}]}`)
		writeSSE(t, w, `{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`)
		writeSSE(t, w, `{"choices":[],"usage":{"prompt_tokens":4000,"completion_tokens":10,"total_tokens":4010,"prompt_cache_hit_tokens":0}}`)
		writeSSE(t, w, `[DONE]`)
	}))
	defer server.Close()

	response, err := newTestClient(t, server.URL).Stream(t.Context(), Request{
		Messages: []Message{{Role: "user", Content: "hello"}},
	}, func(StreamEvent) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if response.Usage == nil {
		t.Fatal("response usage is nil")
	}
	cacheRead, cacheReported := response.Usage.CacheReadTokens()
	if !cacheReported || cacheRead != 0 {
		t.Fatalf("usage=%+v cache-read=%d reported=%t", response.Usage, cacheRead, cacheReported)
	}
}

func TestStreamRejectsInvalidPromptCacheUsage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		writeSSE(t, w, `{"choices":[{"index":0,"delta":{"role":"assistant","content":"ok"},"finish_reason":null}]}`)
		writeSSE(t, w, `{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`)
		writeSSE(t, w, `{"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":1,"total_tokens":11,"prompt_cache_hit_tokens":11}}`)
		writeSSE(t, w, `[DONE]`)
	}))
	defer server.Close()

	_, err := newTestClient(t, server.URL).Stream(t.Context(), Request{Messages: []Message{{Role: "user", Content: "hello"}}}, func(StreamEvent) error { return nil })
	if StableErrorCode(err) != ErrorCodeStreamProtocol {
		t.Fatalf("error=%v code=%q", err, StableErrorCode(err))
	}
}

func TestStreamAssemblesMultipleCrossChunkToolCalls(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		writeSSE(t, w, `{"choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call-","type":"function","function":{"name":"get_","arguments":"{\"a\":"}},{"index":1,"id":"call-2","type":"function","function":{"name":"second","arguments":"{\"b\":"}}]},"finish_reason":null}]}`)
		writeSSE(t, w, `{"choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"function":{"arguments":"\"x\"}"}},{"index":0,"id":"1","function":{"name":"progress","arguments":"1}"}}]},"finish_reason":null}]}`)
		writeSSE(t, w, `{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`)
		writeSSE(t, w, `[DONE]`)
	}))
	defer server.Close()

	response, err := newTestClient(t, server.URL).Stream(t.Context(), Request{Messages: []Message{{Role: "user", Content: "tools"}}}, func(StreamEvent) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	want := []ToolCall{
		{ID: "call-1", Type: "function", Function: ToolFunction{Name: "get_progress", Arguments: `{"a":1}`}},
		{ID: "call-2", Type: "function", Function: ToolFunction{Name: "second", Arguments: `{"b":"x"}`}},
	}
	if len(response.Message.ToolCalls) != len(want) {
		t.Fatalf("tool calls=%+v", response.Message.ToolCalls)
	}
	for index := range want {
		if response.Message.ToolCalls[index] != want[index] {
			t.Fatalf("tool[%d]=%+v want=%+v", index, response.Message.ToolCalls[index], want[index])
		}
	}
	if response.FinishReason != "tool_calls" || response.Message.Content != "" {
		t.Fatalf("response=%+v", response)
	}
}

func TestStreamFallbackOnlyBeforeValidIncrement(t *testing.T) {
	t.Run("http rejection", func(t *testing.T) {
		assertStreamFallback(t, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":{"code":"unsupported_parameter","type":"invalid_request_error","param":"stream","message":"secret-provider-detail"}}`)
		})
	})
	t.Run("sse error frame", func(t *testing.T) {
		assertStreamFallback(t, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "event: error\n")
			writeSSE(t, w, `{"error":{"code":"unsupported_parameter","param":"stream","message":"secret-provider-detail"}}`)
		})
	})

	t.Run("ordinary bad request does not fallback", func(t *testing.T) {
		var calls atomic.Int32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			calls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":{"code":"invalid_request_error","param":"stream","message":"secret-provider-detail"}}`)
		}))
		defer server.Close()
		_, err := newTestClient(t, server.URL).Stream(t.Context(), Request{Messages: []Message{{Role: "user", Content: "hello"}}}, func(StreamEvent) error { return nil })
		if err == nil || calls.Load() != 1 || strings.Contains(err.Error(), "secret-provider-detail") {
			t.Fatalf("calls=%d error=%v", calls.Load(), err)
		}
	})

	t.Run("no fallback after increment", func(t *testing.T) {
		var calls atomic.Int32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			calls.Add(1)
			w.Header().Set("Content-Type", "text/event-stream")
			writeSSE(t, w, `{"choices":[{"index":0,"delta":{"content":"partial"},"finish_reason":null}]}`)
			_, _ = io.WriteString(w, "event: error\n")
			writeSSE(t, w, `{"error":{"code":"unsupported_parameter","param":"stream","message":"secret-provider-detail"}}`)
		}))
		defer server.Close()
		_, err := newTestClient(t, server.URL).Stream(t.Context(), Request{Messages: []Message{{Role: "user", Content: "hello"}}}, func(StreamEvent) error { return nil })
		if StableErrorCode(err) != ErrorCodeStreamProtocol || calls.Load() != 1 || strings.Contains(err.Error(), "secret-provider-detail") {
			t.Fatalf("calls=%d error=%v code=%q", calls.Load(), err, StableErrorCode(err))
		}
	})

	t.Run("no fallback after hidden reasoning increment", func(t *testing.T) {
		var calls atomic.Int32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			calls.Add(1)
			w.Header().Set("Content-Type", "text/event-stream")
			writeSSE(t, w, `{"choices":[{"index":0,"delta":{"reasoning_content":"hidden"},"finish_reason":null}]}`)
			_, _ = io.WriteString(w, "event: error\n")
			writeSSE(t, w, `{"error":{"code":"unsupported_parameter","param":"stream"}}`)
		}))
		defer server.Close()
		_, err := newTestClient(t, server.URL).Stream(t.Context(), Request{Messages: []Message{{Role: "user", Content: "hello"}}}, func(StreamEvent) error { return nil })
		if StableErrorCode(err) != ErrorCodeStreamProtocol || calls.Load() != 1 {
			t.Fatalf("calls=%d error=%v code=%q", calls.Load(), err, StableErrorCode(err))
		}
	})
}

func TestStreamRetriesWithoutStreamOptionsBeforeNonStreamingFallback(t *testing.T) {
	for _, mode := range []string{"http", "sse"} {
		t.Run(mode, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				call := calls.Add(1)
				var request completionRequest
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
					return
				}
				if !request.Stream {
					t.Errorf("call %d was not streaming", call)
					return
				}
				if call == 1 {
					if request.StreamOptions == nil || !request.StreamOptions.IncludeUsage {
						t.Errorf("first request=%+v", request)
						return
					}
					if mode == "http" {
						w.Header().Set("Content-Type", "application/json")
						w.WriteHeader(http.StatusBadRequest)
						_, _ = io.WriteString(w, `{"error":{"code":"unsupported_parameter","param":"stream_options","message":"provider secret"}}`)
						return
					}
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, "event: error\n"+sse(`{"error":{"type":"parameter_not_supported","param":"stream_options","message":"provider secret"}}`))
					return
				}
				if call != 2 || request.StreamOptions != nil {
					t.Errorf("retry %d request=%+v", call, request)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, sse(`{"choices":[{"index":0,"delta":{"content":"兼容成功"},"finish_reason":"stop"}]}`)+sse(`[DONE]`))
			}))
			defer server.Close()

			var events []StreamEvent
			response, err := newTestClient(t, server.URL).Stream(t.Context(), Request{Messages: []Message{{Role: "user", Content: "hello"}}}, func(event StreamEvent) error {
				events = append(events, event)
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if calls.Load() != 2 || !response.CompatibilityFallback || response.Message.Content != "兼容成功" {
				t.Fatalf("calls=%d response=%+v", calls.Load(), response)
			}
			var started, fallback, deltas int
			for _, event := range events {
				switch event.Kind {
				case StreamEventResponseStarted:
					started++
				case StreamEventCompatibilityFallback:
					fallback++
				case StreamEventTextDelta:
					deltas++
				}
				if strings.Contains(event.Text, "provider secret") {
					t.Fatalf("provider detail leaked: %+v", events)
				}
			}
			if started != 1 || fallback != 1 || deltas != 1 {
				t.Fatalf("events=%+v", events)
			}
		})
	}
}

func TestStreamOptionsRetryThenStreamUnsupportedFallsBackToComplete(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call := calls.Add(1)
		var request completionRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		switch call {
		case 1:
			if !request.Stream || request.StreamOptions == nil {
				t.Errorf("first request=%+v", request)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":{"code":"unsupported_parameter","param":"stream_options","message":"provider secret"}}`)
		case 2:
			if !request.Stream || request.StreamOptions != nil {
				t.Errorf("second request=%+v", request)
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "event: error\n"+sse(`{"error":{"code":"unsupported_parameter","param":"stream","message":"provider secret"}}`))
		case 3:
			if request.Stream || request.StreamOptions != nil {
				t.Errorf("complete request=%+v", request)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"最终回答"},"finish_reason":"stop"}]}`)
		default:
			t.Errorf("unexpected call %d", call)
		}
	}))
	defer server.Close()

	var events []StreamEvent
	response, err := newTestClient(t, server.URL).Stream(t.Context(), Request{Messages: []Message{{Role: "user", Content: "hello"}}}, func(event StreamEvent) error {
		events = append(events, event)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 3 || !response.CompatibilityFallback || response.Message.Content != "最终回答" {
		t.Fatalf("calls=%d response=%+v", calls.Load(), response)
	}
	fallbackEvents := 0
	for _, event := range events {
		if event.Kind == StreamEventCompatibilityFallback {
			fallbackEvents++
		}
		if strings.Contains(event.Text, "provider secret") {
			t.Fatalf("provider detail leaked: %+v", events)
		}
	}
	if fallbackEvents != 1 {
		t.Fatalf("events=%+v", events)
	}
}

func TestStreamCompatibilityFallbackRejectsInvalidUTF8BeforeJSONDecode(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call := calls.Add(1)
		var request completionRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		switch call {
		case 1:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":{"code":"unsupported_parameter","param":"stream_options","message":"unsupported"}}`)
		case 2:
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "event: error\n"+sse(`{"error":{"code":"unsupported_parameter","param":"stream","message":"unsupported"}}`))
		case 3:
			if request.Stream {
				t.Error("compatibility fallback remained streaming")
			}
			w.Header().Set("Content-Type", "application/json")
			body := append([]byte(`{"choices":[{"message":{"role":"assistant","content":"`), byte(0xff))
			body = append(body, []byte(`"},"finish_reason":"stop"}]}`)...)
			_, _ = w.Write(body)
		default:
			t.Errorf("unexpected call %d", call)
		}
	}))
	defer server.Close()

	_, err := newTestClient(t, server.URL).Stream(t.Context(), Request{Messages: []Message{{Role: "user", Content: "hello"}}}, func(StreamEvent) error { return nil })
	if StableErrorCode(err) != ErrorCodeResponseProtocol || calls.Load() != 3 {
		t.Fatalf("calls=%d error=%v code=%q", calls.Load(), err, StableErrorCode(err))
	}
}

func TestStreamCompatibilityRetriesAreBounded(t *testing.T) {
	t.Run("stream options only retries once", func(t *testing.T) {
		var calls atomic.Int32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			calls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":{"code":"unsupported_parameter","param":"stream_options","message":"provider secret"}}`)
		}))
		defer server.Close()
		_, err := newTestClient(t, server.URL).Stream(t.Context(), Request{Messages: []Message{{Role: "user", Content: "hello"}}}, func(StreamEvent) error { return nil })
		if err == nil || calls.Load() != 2 || strings.Contains(err.Error(), "provider secret") {
			t.Fatalf("calls=%d error=%v", calls.Load(), err)
		}
	})

	t.Run("complete fallback only runs once", func(t *testing.T) {
		var calls atomic.Int32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			calls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":{"code":"unsupported_parameter","param":"stream","message":"provider secret"}}`)
		}))
		defer server.Close()
		_, err := newTestClient(t, server.URL).Stream(t.Context(), Request{Messages: []Message{{Role: "user", Content: "hello"}}}, func(StreamEvent) error { return nil })
		if err == nil || calls.Load() != 2 || strings.Contains(err.Error(), "provider secret") {
			t.Fatalf("calls=%d error=%v", calls.Load(), err)
		}
	})

	t.Run("valid delta prevents stream options retry", func(t *testing.T) {
		var calls atomic.Int32
		var deltas []string
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			calls.Add(1)
			w.Header().Set("Content-Type", "text/event-stream")
			writeSSE(t, w, `{"choices":[{"index":0,"delta":{"content":"partial"},"finish_reason":null}]}`)
			_, _ = io.WriteString(w, "event: error\n"+sse(`{"error":{"code":"unsupported_parameter","param":"stream_options","message":"provider secret"}}`))
		}))
		defer server.Close()
		_, err := newTestClient(t, server.URL).Stream(t.Context(), Request{Messages: []Message{{Role: "user", Content: "hello"}}}, func(event StreamEvent) error {
			if event.Kind == StreamEventTextDelta {
				deltas = append(deltas, event.Text)
			}
			return nil
		})
		if StableErrorCode(err) != ErrorCodeStreamProtocol || calls.Load() != 1 || len(deltas) != 1 || deltas[0] != "partial" || strings.Contains(err.Error(), "provider secret") {
			t.Fatalf("calls=%d deltas=%v error=%v code=%q", calls.Load(), deltas, err, StableErrorCode(err))
		}
	})
}

func TestStreamRejectsIncompleteFinishReasonsAfterDeliveringDeltas(t *testing.T) {
	for _, test := range []struct {
		reason string
		code   ErrorCode
	}{
		{reason: "length", code: ErrorCodeResponseTruncated},
		{reason: "content_filter", code: ErrorCodeContentFiltered},
	} {
		t.Run(test.reason, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, sse(`{"choices":[{"index":0,"delta":{"content":"partial"},"finish_reason":"`+test.reason+`"}]}`)+sse(`[DONE]`))
			}))
			defer server.Close()
			var deltas []string
			response, err := newTestClient(t, server.URL).Stream(t.Context(), Request{Messages: []Message{{Role: "user", Content: "hello"}}}, func(event StreamEvent) error {
				if event.Kind == StreamEventTextDelta {
					deltas = append(deltas, event.Text)
				}
				return nil
			})
			if StableErrorCode(err) != test.code || response.Message.Content != "" || len(response.Message.ToolCalls) != 0 || response.FinishReason != "" || response.Usage != nil || response.CompatibilityFallback || len(deltas) != 1 || deltas[0] != "partial" {
				t.Fatalf("response=%+v deltas=%v error=%v code=%q", response, deltas, err, StableErrorCode(err))
			}
		})
	}
}

func TestStreamInactivityTimeoutResetsOnAnyResponseBytes(t *testing.T) {
	const timeout = 500 * time.Millisecond
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		parts := []string{
			": keepalive\n\n",
			`data: {"choices":[{"index":0,"delta":{"role":"assistant","reasoning_content":"`,
			`hidden"},"finish_reason":null}]}` + "\n\n",
			": keepalive\n\n",
			": keepalive\n\n",
			sse(`{"choices":[{"index":0,"delta":{"content":"完成"},"finish_reason":null}]}`),
			sse(`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`) + sse(`[DONE]`),
		}
		for index, part := range parts {
			if _, err := io.WriteString(w, part); err != nil {
				return
			}
			flusher.Flush()
			if index < len(parts)-1 {
				time.Sleep(100 * time.Millisecond)
			}
		}
	}))
	defer server.Close()

	client, err := New(server.URL, "test-model", "token", timeout, nil)
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	var events []StreamEvent
	response, err := client.Stream(t.Context(), Request{Messages: []Message{{Role: "user", Content: "hello"}}}, func(event StreamEvent) error {
		events = append(events, event)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if time.Since(started) <= timeout {
		t.Fatalf("stream completed before proving timeout renewal: elapsed=%s timeout=%s", time.Since(started), timeout)
	}
	if response.Message.Content != "完成" || len(events) != 2 || events[0].Kind != StreamEventResponseStarted || events[1] != (StreamEvent{Kind: StreamEventTextDelta, Text: "完成"}) {
		t.Fatalf("response=%+v events=%+v", response, events)
	}
}

func TestStreamConfiguredInactivityTimeoutRequiresResponseBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer server.Close()

	client, err := New(server.URL, "test-model", "token", 80*time.Millisecond, nil)
	if err != nil {
		t.Fatal(err)
	}
	var events []StreamEvent
	_, err = client.Stream(t.Context(), Request{Messages: []Message{{Role: "user", Content: "hello"}}}, func(event StreamEvent) error {
		events = append(events, event)
		return nil
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error=%v", err)
	}
	if len(events) != 0 {
		t.Fatalf("response headers incorrectly started SSE presentation: %+v", events)
	}
}

func TestStreamCancellationAndObserverErrorArePreserved(t *testing.T) {
	t.Run("context cancellation", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			writeSSE(t, w, `{"choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`)
			<-r.Context().Done()
		}))
		defer server.Close()
		ctx, cancel := context.WithCancel(t.Context())
		client := newTestClient(t, server.URL)
		_, err := client.Stream(ctx, Request{Messages: []Message{{Role: "user", Content: "hello"}}}, func(event StreamEvent) error {
			if event.Kind == StreamEventResponseStarted {
				cancel()
			}
			return nil
		})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error=%v", err)
		}
	})

	t.Run("observer error", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			writeSSE(t, w, `{"choices":[{"index":0,"delta":{"content":"hello"},"finish_reason":null}]}`)
		}))
		defer server.Close()
		observerErr := errors.New("stop observer")
		_, err := newTestClient(t, server.URL).Stream(t.Context(), Request{Messages: []Message{{Role: "user", Content: "hello"}}}, func(event StreamEvent) error {
			if event.Kind == StreamEventTextDelta {
				return observerErr
			}
			return nil
		})
		if err != observerErr {
			t.Fatalf("error=%v", err)
		}
	})
}

func TestStreamFailsClosedOnProtocolErrors(t *testing.T) {
	tests := []struct {
		name        string
		contentType string
		body        string
	}{
		{name: "wrong content type", contentType: "application/json", body: `{"secret":"provider-body"}`},
		{name: "multiple choices", contentType: "text/event-stream", body: sse(`{"choices":[{"index":0,"delta":{"content":"a"}},{"index":1,"delta":{"content":"b"}}]}`)},
		{name: "wrong choice index", contentType: "text/event-stream", body: sse(`{"choices":[{"index":1,"delta":{"content":"a"},"finish_reason":null}]}`)},
		{name: "invalid utf8", contentType: "text/event-stream", body: "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"\xff\"}}]}\n\n"},
		{name: "invalid finish", contentType: "text/event-stream", body: sse(`{"choices":[{"index":0,"delta":{"content":"a"},"finish_reason":"unknown"}]}`)},
		{name: "done before finish", contentType: "text/event-stream", body: sse(`[DONE]`)},
		{name: "missing done", contentType: "text/event-stream", body: sse(`{"choices":[{"index":0,"delta":{"content":"a"},"finish_reason":"stop"}]}`)},
		{name: "incomplete tool json", contentType: "text/event-stream", body: sse(`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call","type":"function","function":{"name":"tool","arguments":"{"}}]},"finish_reason":"tool_calls"}]}`) + sse(`[DONE]`)},
		{name: "provider error", contentType: "text/event-stream", body: "event: error\n" + sse(`{"error":{"code":"provider_failed","message":"provider-body"}}`)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", test.contentType)
				_, _ = io.WriteString(w, test.body)
			}))
			defer server.Close()
			_, err := newTestClient(t, server.URL).Stream(t.Context(), Request{Messages: []Message{{Role: "user", Content: "hello"}}}, func(StreamEvent) error { return nil })
			if StableErrorCode(err) != ErrorCodeStreamProtocol || strings.Contains(err.Error(), "provider-body") {
				t.Fatalf("error=%v code=%q", err, StableErrorCode(err))
			}
		})
	}
}

func TestStreamEnforcesLineEventTextArgumentAndToolBounds(t *testing.T) {
	t.Run("line", func(t *testing.T) {
		reader := &sseReader{reader: bufio.NewReader(strings.NewReader(strings.Repeat("x", maxSSELineBytes+1) + "\n"))}
		_, err := reader.next()
		if StableErrorCode(err) != ErrorCodeStreamResponseTooLarge {
			t.Fatalf("error=%v", err)
		}
	})
	t.Run("response", func(t *testing.T) {
		reader := &sseReader{reader: bufio.NewReader(strings.NewReader("x\n")), total: maxResponseBytes}
		_, err := reader.next()
		if StableErrorCode(err) != ErrorCodeStreamResponseTooLarge {
			t.Fatalf("error=%v", err)
		}
	})
	t.Run("events", func(t *testing.T) {
		reader := &sseReader{reader: bufio.NewReader(strings.NewReader("data: {}\n\n")), events: maxSSEEvents}
		_, err := reader.next()
		if StableErrorCode(err) != ErrorCodeStreamProtocol {
			t.Fatalf("error=%v", err)
		}
	})
	t.Run("text delta", func(t *testing.T) {
		assembler := streamAssembler{tools: make(map[int]*assembledToolCall)}
		content := strings.Repeat("x", maxStreamTextDeltaBytes+1)
		data, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": content}}}})
		_, err := assembler.apply(data)
		if StableErrorCode(err) != ErrorCodeStreamResponseTooLarge {
			t.Fatalf("error=%v", err)
		}
	})
	t.Run("argument delta", func(t *testing.T) {
		assembler := streamAssembler{tools: make(map[int]*assembledToolCall)}
		index := 0
		err := assembler.applyToolDelta(streamToolCallDelta{Index: &index, ID: "call", Type: "function", Function: streamFunctionDelta{Name: "tool", Arguments: strings.Repeat("x", maxStreamArgumentDelta+1)}})
		if StableErrorCode(err) != ErrorCodeStreamResponseTooLarge {
			t.Fatalf("error=%v", err)
		}
	})
	t.Run("tool count", func(t *testing.T) {
		assembler := streamAssembler{tools: make(map[int]*assembledToolCall)}
		index := maxStreamToolCalls
		err := assembler.applyToolDelta(streamToolCallDelta{Index: &index, ID: "call"})
		if StableErrorCode(err) != ErrorCodeStreamProtocol {
			t.Fatalf("error=%v", err)
		}
	})
}

func assertStreamFallback(t *testing.T, reject func(http.ResponseWriter, *http.Request)) {
	t.Helper()
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call := calls.Add(1)
		var request completionRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		if call == 1 {
			if !request.Stream {
				t.Error("first request was not streaming")
			}
			reject(w, r)
			return
		}
		if request.Stream || request.StreamOptions != nil {
			t.Errorf("fallback request=%+v", request)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"fallback"},"finish_reason":"stop"}]}`)
	}))
	defer server.Close()

	var events []StreamEvent
	response, err := newTestClient(t, server.URL).Stream(t.Context(), Request{Messages: []Message{{Role: "user", Content: "hello"}}}, func(event StreamEvent) error {
		events = append(events, event)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 || !response.CompatibilityFallback || response.Message.Content != "fallback" {
		t.Fatalf("calls=%d response=%+v", calls.Load(), response)
	}
	if len(events) == 0 || events[len(events)-1].Kind != StreamEventCompatibilityFallback {
		t.Fatalf("events=%+v", events)
	}
}

func newTestClient(t *testing.T, baseURL string) *Client {
	t.Helper()
	client, err := New(baseURL, "test-model", "token", 2*time.Second, nil)
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func writeSSE(t *testing.T, w io.Writer, data string) {
	t.Helper()
	if _, err := io.WriteString(w, sse(data)); err != nil {
		t.Error(err)
	}
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
}

func sse(data string) string {
	return "data: " + data + "\n\n"
}
