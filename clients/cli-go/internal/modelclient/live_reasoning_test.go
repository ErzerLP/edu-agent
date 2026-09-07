package modelclient

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func withoutTransportActivity(events []StreamEvent) []StreamEvent {
	var result []StreamEvent
	for _, event := range events {
		if event.Kind != StreamEventResponseActivity {
			result = append(result, event)
		}
	}
	return result
}

func TestLiveReasoningReadableFieldsAndOpaqueIsolation(t *testing.T) {
	for _, tc := range []struct{ raw, want string }{
		{`{"reasoning_content":"first","reasoning":"duplicate"}`, "first"},
		{`{"reasoning":"second"}`, "second"},
		{`{"thinking":"third"}`, "third"},
		{`{"reasoning_details":[{"type":"reasoning.text","text":"text"},{"type":"reasoning.summary","summary":"summary"},{"type":"reasoning.encrypted","data":"SECRET"},{"type":"unknown","text":"SECRET"}]}`, "textsummary"},
		{`{"thinking":{"signature":"SECRET","text":"SECRET"},"reasoning_content":null}`, ""},
	} {
		var delta streamDelta
		if err := json.Unmarshal([]byte(tc.raw), &delta); err != nil {
			t.Fatal(err)
		}
		if got := readableReasoning(delta); got != tc.want {
			t.Fatalf("readable result=%q want=%q", got, tc.want)
		}
	}
}

func TestLiveReasoningSSESeparateFromAnswerAndMalformedFrames(t *testing.T) {
	for _, malformed := range []bool{false, true} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			writeSSE(t, w, `{"choices":[{"index":0,"delta":{"reasoning_content":"private-reasoning-marker"},"finish_reason":null}]}`)
			if malformed {
				writeSSE(t, w, `{"choices":[{"index":2,"delta":{"reasoning":"INVALID-MARKER"},"finish_reason":null}]}`)
			} else {
				writeSSE(t, w, `{"choices":[{"index":0,"delta":{"content":"answer"},"finish_reason":"stop"}]}`)
				writeSSE(t, w, `[DONE]`)
			}
		}))
		var events []StreamEvent
		response, err := newTestClient(t, server.URL).Stream(t.Context(), Request{Messages: []Message{{Role: "user", Content: "hello"}}}, func(event StreamEvent) error { events = append(events, event); return nil })
		server.Close()
		if (err != nil) != malformed {
			t.Fatalf("malformed=%t error=%v", malformed, err)
		}
		if !malformed && response.Message.Content != "answer" {
			t.Fatal("answer changed")
		}
		reasoning, activity := 0, 0
		for _, event := range events {
			if event.Kind == StreamEventReasoningDelta {
				reasoning++
				if event.Text != "private-reasoning-marker" {
					t.Fatal("invalid frame displayed")
				}
			}
			if event.Kind == StreamEventResponseActivity {
				activity++
				if event.ReceivedAt.IsZero() || event.Text != "" {
					t.Fatal("invalid transport activity")
				}
			}
		}
		if reasoning != 1 || activity == 0 {
			t.Fatalf("reasoning=%d progress=%d", reasoning, activity)
		}
		encoded, _ := json.Marshal(response)
		serializedEvents, _ := json.Marshal(events)
		if strings.Contains(string(encoded)+string(serializedEvents), "private-reasoning-marker") {
			t.Fatal("reasoning entered serializable response/events")
		}
	}
}

func TestLiveReasoningObserverErrorStopsStream(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		writeSSE(t, w, `{"choices":[{"index":0,"delta":{"reasoning_content":"partial"},"finish_reason":null}]}`)
		writeSSE(t, w, `{"choices":[{"index":0,"delta":{"content":"answer"},"finish_reason":"stop"}]}`)
		writeSSE(t, w, `[DONE]`)
	}))
	defer server.Close()
	sentinel := errors.New("observer stopped")
	_, err := newTestClient(t, server.URL).Stream(t.Context(), Request{Messages: []Message{{Role: "user", Content: "hello"}}}, func(event StreamEvent) error {
		if event.Kind == StreamEventReasoningDelta {
			return sentinel
		}
		return nil
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("error=%v", err)
	}
}
