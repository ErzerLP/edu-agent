package agentloop

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/modelclient"
)

func TestLiveReasoningProductionSSEDoesNotEnterCheckpointOrNextRequest(t *testing.T) {
	const marker = "private-reasoning-marker-123"
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		if strings.Contains(string(body), marker) {
			t.Error("reasoning fed back into next request")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"reasoning_content\":\""+marker+"\"},\"finish_reason\":null}]}\n\n")
		if calls.Add(1) == 1 {
			io.WriteString(w, `data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call-live","type":"function","function":{"name":"search_knowledge","arguments":"{\"query\":\"graph\"}"}}]},"finish_reason":"tool_calls"}]}`+"\n\n")
		} else {
			io.WriteString(w, `data: {"choices":[{"index":0,"delta":{"content":"public answer"},"finish_reason":"stop"}]}`+"\n\n")
		}
		io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer server.Close()
	client, err := modelclient.New(server.URL, "test", "", time.Second, nil)
	if err != nil {
		t.Fatal(err)
	}
	session := newTestSession(t, client, &fakeServer{})
	var activities []Activity
	result, err := session.Send(WithActivityReporter(t.Context(), func(a Activity) { activities = append(activities, a) }), "hello")
	if err != nil || result.Text != "public answer" || calls.Load() != 2 {
		t.Fatalf("result=%+v error=%v calls=%d", result, err, calls.Load())
	}
	reasoning := 0
	latest := map[string]Activity{}
	for _, a := range activities {
		if a.Kind == ActivityThinking {
			latest[a.Event.ID] = a
		}
		if a.Kind == ActivityReasoningDelta {
			reasoning++
			if a.Delta != marker || latest[a.Event.ID].Event.Status != EventRunning {
				t.Fatal("reasoning lost active request identity")
			}
			if a.Event.Summary != "" || a.Event.Detail != "" {
				t.Fatal("body entered metadata")
			}
		}
	}
	if reasoning != 2 {
		t.Fatalf("reasoning events=%d", reasoning)
	}
	for _, a := range latest {
		if a.Event.Status == EventRunning {
			t.Fatal("unsettled thinking")
		}
	}
	cp, err := session.ExportCheckpoint()
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := EncodeSessionCheckpoint(cp)
	if err != nil {
		t.Fatal(err)
	}
	eventJSON, _ := json.Marshal(activities)
	resultJSON, _ := json.Marshal(result)
	if strings.Contains(string(encoded)+string(eventJSON)+string(resultJSON), marker) {
		t.Fatal("private reasoning in checkpoint/source/serializable activity/result")
	}
}
