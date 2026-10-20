package mentorrun

import (
	"context"
	"encoding/json"
	"github.com/edu-agent/edu-agent/packages/agentcore/modelclient"
	"github.com/edu-agent/edu-agent/server/internal/integrations/websearch"
	"github.com/edu-agent/edu-agent/server/internal/learningspace"
	"github.com/google/uuid"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestWebDisableDuringRoutingStillAllowsTheAnswer(t *testing.T) {
	routeStarted := make(chan struct{})
	var started sync.Once
	routing, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := fixture(t, func(w http.ResponseWriter, r *http.Request, _ int) {
		var request modelclient.Request
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		if strings.Contains(request.Messages[0].Content, "公开检索判断") {
			started.Do(func() { close(routeStarted) })
			<-routing.Done()
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": modelclient.Message{Role: "assistant", Content: `{"search_needed":true}`}, "finish_reason": "stop"}}})
		} else {
			stream(w, modelclient.Message{Role: "assistant", Content: "联网已关闭，继续依据已有资料回答。"})
		}
	})
	var sent atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { sent.Add(1); w.Write([]byte(`{"web":{"results":[]}}`)) }))
	defer provider.Close()
	f.service.search = func(wrap func(http.RoundTripper) http.RoundTripper) (websearch.Adapter, string, error) {
		var transport http.RoundTripper = http.DefaultTransport
		if wrap != nil {
			transport = wrap(transport)
		}
		return websearch.NewBrave(provider.URL, "fixture", &http.Client{Transport: transport}), "fixture", nil
	}
	conversation := newHistory(t, f, true, f.goal)
	f.create = historyRequest(t, f, conversation, "请读取 https://example.org/article 的最新说明")
	receipt := f.accept(t)
	done := make(chan error, 1)
	go func() { _, err := f.service.RunOnce(context.Background()); done <- err }()
	select {
	case <-routeStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("routing did not start")
	}
	if _, err := f.service.ChangeWebSearch(context.Background(), f.actor, learningspace.DefaultID, conversation, WebSearchCommand{OperationID: uuid.NewString(), ExpectedVersion: 1, Enabled: false}); err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("answer did not finish")
	}
	snapshot := f.snapshot(t, receipt.RunID)
	if snapshot.Status != "succeeded" || snapshot.Output != "联网已关闭，继续依据已有资料回答。" || sent.Load() != 0 {
		t.Fatalf("closing web access interrupted the ordinary answer: %+v sent=%d", snapshot, sent.Load())
	}
}
