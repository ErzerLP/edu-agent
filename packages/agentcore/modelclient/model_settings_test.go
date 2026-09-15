package modelclient

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

type modelSettingsTransport func(*http.Request) (*http.Response, error)

func (f modelSettingsTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestModelSettingsLongTimeoutAndOutputArePassedThrough(t *testing.T) {
	for _, stream := range []bool{false, true} {
		name := "complete"
		if stream {
			name = "stream"
		}
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				calls := 0
				transport := modelSettingsTransport(func(r *http.Request) (*http.Response, error) {
					calls++
					var body completionRequest
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Fatal(err)
					}
					if body.MaxTokens != 256000 || body.Stream != stream {
						t.Fatalf("changed request: %+v", body)
					}
					// Virtual time: no wall-clock delay or real provider request.
					time.Sleep(11 * time.Minute)
					if err := r.Context().Err(); err != nil {
						t.Fatalf("configured 30m was shortened: %v", err)
					}
					contentType := "application/json"
					data := `{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`
					if stream {
						contentType = "text/event-stream"
						data = "data: {\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"
					}
					return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{contentType}}, Body: io.NopCloser(strings.NewReader(data))}, nil
				})
				// A server client's timeout must not replace the model setting.
				client, err := New("https://model.example/v1", "test", "", 30*time.Minute, &http.Client{Transport: transport, Timeout: time.Millisecond})
				if err != nil {
					t.Fatal(err)
				}
				request := Request{Messages: []Message{{Role: "user", Content: "hello"}}, MaxTokens: 256000}
				var response Response
				if stream {
					response, err = client.Stream(t.Context(), request, func(StreamEvent) error { return nil })
				} else {
					response, err = client.Complete(t.Context(), request)
				}
				if err != nil || response.Message.Content != "ok" || calls != 1 {
					t.Fatalf("response=%+v error=%v calls=%d", response, err, calls)
				}
			})
		})
	}
}
