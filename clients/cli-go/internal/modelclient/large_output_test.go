package modelclient

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/agentlimits"
)

func largeOutputFrame(text string) string {
	data, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": text}}}})
	return sse(string(data))
}

func TestLargeOutputCompleteAndFineGrainedStream(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		name := "complete"
		if streaming {
			name = "stream"
		}
		t.Run(name, func(t *testing.T) {
			// 8193 events/16386 lines exceeds both old SSE limits.
			text := strings.Repeat("long-answer-data", 8193)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var request completionRequest
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
					return
				}
				if request.MaxTokens != 128000 || request.Stream != streaming {
					t.Errorf("wire tokens=%d stream=%v", request.MaxTokens, request.Stream)
				}
				if !streaming {
					w.Header().Set("Content-Type", "application/json")
					_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": Message{Role: "assistant", Content: text}, "finish_reason": "stop"}}})
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				for i := 0; i < 8193; i++ {
					_, _ = io.WriteString(w, largeOutputFrame("long-answer-data"))
				}
				_, _ = io.WriteString(w, sse(`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`)+sse(`[DONE]`))
			}))
			defer server.Close()
			client := newTestClient(t, server.URL)
			request := Request{Messages: []Message{{Role: "user", Content: "hello"}}, MaxTokens: 128000}
			var response Response
			var err error
			var displayed strings.Builder
			if streaming {
				response, err = client.Stream(t.Context(), request, func(event StreamEvent) error {
					if event.Kind == StreamEventTextDelta {
						displayed.WriteString(event.Text)
					}
					return nil
				})
			} else {
				response, err = client.Complete(t.Context(), request)
			}
			if err != nil || response.Message.Content != text {
				t.Fatalf("len=%d err=%v", len(response.Message.Content), err)
			}
			if streaming && displayed.String() != text {
				t.Fatal("stream display lost text")
			}
		})
	}
}

func TestLargeOutputBodyAndSerializationBounds(t *testing.T) {
	for _, size := range []int{agentlimits.MaxAssistantTextBytes, agentlimits.MaxAssistantTextBytes + 1} {
		text := strings.Repeat("<", size) // Sixfold JSON expansion is not sixfold source text.
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": Message{Role: "assistant", Content: text}, "finish_reason": "stop"}}})
		}))
		response, err := newTestClient(t, server.URL).Complete(t.Context(), Request{Messages: []Message{{Role: "user", Content: "hello"}}, MaxTokens: 128000})
		server.Close()
		if size == agentlimits.MaxAssistantTextBytes {
			if err != nil || response.Message.Content != text {
				t.Fatalf("legal escaped body rejected: %v", err)
			}
		} else if err == nil {
			t.Fatal("oversize complete accepted")
		}
	}
	assembler := streamAssembler{tools: make(map[int]*assembledToolCall)}
	for i := 0; i < 16; i++ {
		if _, err := assembler.apply([]byte(strings.TrimSuffix(strings.TrimPrefix(largeOutputFrame(strings.Repeat("x", 64<<10)), "data: "), "\n\n"))); err != nil {
			t.Fatal(err)
		}
	}
	data := strings.TrimSuffix(strings.TrimPrefix(largeOutputFrame("x"), "data: "), "\n\n")
	if _, err := assembler.apply([]byte(data)); StableErrorCode(err) != ErrorCodeStreamResponseTooLarge {
		t.Fatalf("stream oversize err=%v", err)
	}
}

func TestLargeOutputEmptyFloodAndTransportBounds(t *testing.T) {
	assembler := streamAssembler{tools: make(map[int]*assembledToolCall)}
	for i := 0; i < 4096; i++ {
		if _, err := assembler.apply([]byte(`{"choices":[{"index":0,"delta":{}}]}`)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := assembler.apply([]byte(`{"choices":[{"index":0,"delta":{}}]}`)); StableErrorCode(err) != ErrorCodeStreamProtocol {
		t.Fatalf("empty flood: %v", err)
	}
	for _, reader := range []*sseReader{
		{reader: bufio.NewReader(strings.NewReader("data: {}\n\n")), events: maxSSEEvents},
		{reader: bufio.NewReader(strings.NewReader(":\n")), lines: maxSSELines},
		{reader: bufio.NewReader(strings.NewReader(":\n")), total: maxStreamResponseBytes},
	} {
		if _, err := reader.next(); err == nil {
			t.Fatal("unbounded transport")
		}
	}
}

func TestLargeOutputStreamCancellation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for i := 0; i < 10000; i++ {
			if _, err := io.WriteString(w, largeOutputFrame(strings.Repeat("x", 16))); err != nil {
				return
			}
		}
	}))
	defer server.Close()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	bytes := 0
	_, err := newTestClient(t, server.URL).Stream(ctx, Request{Messages: []Message{{Role: "user", Content: "hello"}}, MaxTokens: 128000}, func(event StreamEvent) error {
		bytes += len(event.Text)
		if bytes > 64<<10 {
			cancel()
		}
		return nil
	})
	if !errors.Is(err, context.Canceled) || bytes <= 64<<10 {
		t.Fatalf("cancel err=%v bytes=%d", err, bytes)
	}
}
