package agentloop

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/modelclient"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/modelgateway"
)

func TestGatewayContextJobsUseBoundTurnAndOutputLimit(t *testing.T) {
	var active, peak atomic.Int32
	var mu sync.Mutex
	stages := map[string]bool{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/model-gateway/config" {
			fmt.Fprint(w, `{"protocol_version":1,"invoke_allowed":true,"available":true,"privacy_generation":7,"models":[{"target":"mentor","display_name":"AI 助手模型","model_name":"demo","configuration_revision":"c1","destination_revision":"d1","context_tokens":32768,"output_tokens":128}],"limits":{"idle_timeout_seconds":60,"request_timeout_seconds":900}}`)
			return
		}
		n := active.Add(1)
		defer active.Add(-1)
		for old := peak.Load(); n > old; old = peak.Load() {
			if peak.CompareAndSwap(old, n) {
				break
			}
		}
		stage := r.Header.Get("X-CLI-Model-Stage")
		mu.Lock()
		stages[stage] = true
		mu.Unlock()
		if r.Header.Get("X-CLI-Session-ID") != "11111111-1111-4111-8111-111111111111" || r.Header.Get("X-CLI-Turn-ID") != "22222222-2222-4222-8222-222222222222" {
			t.Error("lost captured identity")
		}
		var body struct {
			Model     string `json:"model"`
			MaxTokens int    `json:"max_tokens"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if body.Model != "mentor" || body.MaxTokens != 128 {
			t.Errorf("wrong internal request: %+v", body)
		}
		w.Header().Set("Content-Type", "application/json")
		switch stage {
		case "context_observer":
			fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"observe","type":"function","function":{"name":"record_session_observations","arguments":"{\"covers_up_to_id\":\"source\",\"observations\":[]}"}}]},"finish_reason":"tool_calls"}]}`)
		case "context_reflector":
			fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"reflect","type":"function","function":{"name":"record_session_reflections","arguments":"{\"reflections\":[]}"}}]},"finish_reason":"tool_calls"}]}`)
		default:
			fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"完成"},"finish_reason":"stop"}]}`)
		}
	}))
	defer server.Close()
	client, err := modelgateway.New(modelgateway.Options{ServerURL: server.URL, DeviceID: "device", Token: "token", Select: func(_ context.Context, c modelgateway.Configuration) (modelgateway.Connection, error) {
		return c.Models[0], nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	model, err := client.Bind(t.Context(), modelgateway.Identity{SessionID: "11111111-1111-4111-8111-111111111111", TurnID: "22222222-2222-4222-8222-222222222222"})
	if err != nil {
		t.Fatal(err)
	}
	estimator := NewTokenEstimator()
	errs := make(chan error, 3)
	go func() {
		_, err := runObserver(t.Context(), model, estimator, 32768, observerSnapshot{Sources: []SourceEntry{{ID: "source", RecallText: "整理这轮对话"}}, SourcePositions: map[string]int{"source": 0}})
		errs <- err
	}()
	go func() {
		_, err := runReflector(t.Context(), model, estimator, 32768, reflectorSnapshot{Observations: []Observation{{ID: "observation", Content: "学习计划"}}})
		errs <- err
	}()
	go func() {
		_, err := model.Complete(t.Context(), modelclient.Request{Messages: []modelclient.Message{{Role: "user", Content: "前台"}}})
		errs <- err
	}()
	for i := 0; i < 3; i++ {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	if peak.Load() != 1 || !stages["context_observer"] || !stages["context_reflector"] || !stages["foreground"] {
		t.Fatalf("concurrency=%d stages=%v", peak.Load(), stages)
	}
}

func gatewayFixtureConfig(t *testing.T, w http.ResponseWriter, r *http.Request) bool {
	t.Helper()
	if r.URL.Path == "/v1/model-gateway/config" {
		fmt.Fprint(w, `{"protocol_version":1,"invoke_allowed":true,"available":true,"privacy_generation":7,"models":[{"target":"teaching","display_name":"教学模型","model_name":"fixture","configuration_revision":"c1","destination_revision":"d1","context_tokens":272000,"output_tokens":128000}],"limits":{"idle_timeout_seconds":60,"request_timeout_seconds":900}}`)
		return true
	}
	if r.URL.Path != "/v1/model-gateway/chat/completions" || r.Header.Get("Authorization") != "Bearer fixture-device-token" || r.Header.Get("X-CLI-Session-ID") == "" {
		t.Errorf("not a gateway request: %s %+v", r.URL.Path, r.Header)
	}
	return false
}
func newGatewayFixtureModel(t *testing.T, server *httptest.Server) *modelgateway.Model {
	t.Helper()
	client, err := modelgateway.New(modelgateway.Options{ServerURL: server.URL, DeviceID: "fixture-device", Token: "fixture-device-token", Select: func(_ context.Context, c modelgateway.Configuration) (modelgateway.Connection, error) {
		return c.Models[0], nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	model, err := client.Bind(t.Context(), modelgateway.Identity{SessionID: "11111111-1111-4111-8111-111111111111", TurnID: "22222222-2222-4222-8222-222222222222"})
	if err != nil {
		t.Fatal(err)
	}
	return model
}
