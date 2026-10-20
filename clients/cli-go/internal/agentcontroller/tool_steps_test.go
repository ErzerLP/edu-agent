package agentcontroller

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/api"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/modelclient"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/modelgateway"
)

func TestGatewayToolStepsRefreshAndFailClosed(t *testing.T) {
	var limit, configCalls, modelCalls, summaries atomic.Int32
	var unavailable atomic.Bool
	var firstTurn atomic.Value
	limit.Store(3)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/model-gateway/config" {
			configCalls.Add(1)
			if unavailable.Load() {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			v := "null"
			if limit.Load() != 0 {
				v = fmt.Sprint(limit.Load())
			}
			fmt.Fprintf(w, `{"protocol_version":1,"invoke_allowed":true,"available":true,"privacy_generation":7,"models":[{"target":"teaching","display_name":"教学模型","model_name":"demo","configuration_revision":"c1","destination_revision":"d1","context_tokens":32768,"output_tokens":128}],"limits":{"max_tool_steps":%s,"idle_timeout_seconds":60,"request_timeout_seconds":900}}`, v)
			return
		}
		if r.URL.Path != "/v1/model-gateway/chat/completions" {
			fmt.Fprint(w, `{}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		stage := r.Header.Get("X-CLI-Model-Stage")
		if stage == "session_title" {
			fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"{\"title\":\"学习计划\"}"},"finish_reason":"stop"}]}`)
			return
		}
		var req modelclient.Request
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
		}
		if stage == "tool_limit_summary" {
			summaries.Add(1)
			if len(req.Tools) != 0 {
				t.Error("summary tools must be withdrawn")
			}
			fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"已完成本轮查询。"},"finish_reason":"stop"}]}`)
			return
		}
		n := modelCalls.Add(1)
		turn := r.Header.Get("X-CLI-Turn-ID")
		if n == 1 {
			firstTurn.Store(turn)
		}
		if n == 2 && turn != firstTurn.Load() {
			t.Error("continue changed the original usage turn")
		}
		if n == 3 && turn == firstTurn.Load() {
			t.Error("new input reused the previous usage turn")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		if n%2 == 0 {
			fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"已完成\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
			return
		}
		// Change the remote setting after the round snapshot was read.
		limit.Store(0)
		calls := make([]map[string]any, 4)
		for i := range calls {
			calls[i] = map[string]any{"index": i, "id": fmt.Sprintf("call-%d-%d", n, i), "type": "function", "function": modelclient.ToolFunction{Name: "get_learning_progress", Arguments: `{}`}}
		}
		payload, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"role": "assistant", "tool_calls": calls}, "finish_reason": "tool_calls"}}})
		fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", payload)
	}))
	defer server.Close()
	client, err := modelgateway.New(modelgateway.Options{ServerURL: server.URL, DeviceID: "device", Token: "token", Select: func(_ context.Context, c modelgateway.Configuration) (modelgateway.Connection, error) {
		return c.Models[0], nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	model, err := client.Bind(t.Context(), modelgateway.Identity{})
	if err != nil {
		t.Fatal(err)
	}
	deps := controllerDependencies(controllerStore(t, t.TempDir(), &controllerSecretBackend{}), model, &controllerServer{generation: api.MemoryGenerationStamp{LearnerGeneration: 1, MemoryGeneration: 1}}, t.TempDir(), GatewayBinding{Target: "teaching", ServerURL: server.URL, DeviceID: "device", ModelName: "demo", DestinationRevision: "d1"})
	deps.LoopOptions.ContextWindow, deps.LoopOptions.MaxTokens = 32768, 128
	c, err := Start(t.Context(), deps, false)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	r, err := c.Send(t.Context(), "分步查询")
	if err != nil || len(r.Events) != 3 || summaries.Load() != 1 || !strings.Contains(r.Text, "继续") {
		t.Fatalf("snapshot limit not applied: result=%+v err=%v summaries=%d", r, err, summaries.Load())
	}
	beforeConfig := configCalls.Load()
	unavailable.Store(true)
	if _, err = c.Send(t.Context(), "继续"); err == nil {
		t.Fatal("used cached limit when refresh failed")
	}
	if modelCalls.Load() != 1 || configCalls.Load() != beforeConfig+1 {
		t.Fatal("refresh failure dispatched model or skipped config")
	}
	unavailable.Store(false)
	r, err = c.Send(t.Context(), "继续")
	if err != nil || r.Text != "已完成" || len(r.Events) != 4 || summaries.Load() != 1 {
		t.Fatalf("resume lost cursor or new unlimited setting: %+v %v", r, err)
	}
	limit.Store(3)
	unavailable.Store(true)
	if _, err = c.Send(t.Context(), "下一轮"); err == nil {
		t.Fatal("new round used cached configuration")
	}
	if modelCalls.Load() != 2 {
		t.Fatal("new round dispatched on configuration failure")
	}
	unavailable.Store(false)
	r, err = c.Send(t.Context(), "下一轮")
	if err != nil || len(r.Events) != 3 || summaries.Load() != 2 {
		t.Fatalf("new round did not refresh setting: %+v %v", r, err)
	}
}
