package agentcontroller

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/api"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/modelgateway"
)

func TestGatewayControllerTitleAndResumeIdentity(t *testing.T) {
	var mu sync.Mutex
	var headers []http.Header
	title := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/model-gateway/config" {
			fmt.Fprint(w, `{"protocol_version":1,"invoke_allowed":true,"available":true,"privacy_generation":7,"models":[{"target":"teaching","display_name":"教学模型","model_name":"demo","configuration_revision":"c1","destination_revision":"d1","context_tokens":32768,"output_tokens":128}],"limits":{"idle_timeout_seconds":60,"request_timeout_seconds":900}}`)
			return
		}
		if r.URL.Path != "/v1/model-gateway/chat/completions" {
			fmt.Fprint(w, `{"state":"cancelled"}`)
			return
		}
		mu.Lock()
		headers = append(headers, r.Header.Clone())
		mu.Unlock()
		if r.Header.Get("X-CLI-Model-Stage") == "session_title" {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"{\"title\":\"学习计划\"}"},"finish_reason":"stop"}]}`)
			title <- struct{}{}
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"完成\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
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
	root, workspace := t.TempDir(), t.TempDir()
	secrets := &controllerSecretBackend{}
	learning := &controllerServer{generation: api.MemoryGenerationStamp{LearnerGeneration: 1, MemoryGeneration: 1}}
	provider := GatewayBinding{Target: "teaching", ServerURL: server.URL, ModelName: "demo", DeviceID: "device", DestinationRevision: "d1"}
	deps := controllerDependencies(controllerStore(t, root, secrets), model, learning, workspace, provider)
	deps.LoopOptions.ContextWindow = 32768
	deps.LoopOptions.MaxTokens = 128
	c, err := Start(t.Context(), deps, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.Send(t.Context(), "制定学习计划"); err != nil {
		c.Close()
		t.Fatal(err)
	}
	select {
	case <-title:
	case <-time.After(2 * time.Second):
		c.Close()
		t.Fatal("missing title")
	}
	session := c.SessionID()
	c.Close()
	deps.Store = controllerStore(t, root, secrets)
	restored, err := Resume(t.Context(), deps, ResumeOptions{SessionID: session, CurrentWorkspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	mu.Lock()
	before := len(headers)
	mu.Unlock()
	if before != 2 {
		t.Fatalf("restore unexpectedly invoked: %d", before)
	}
	if _, err = restored.Send(t.Context(), "继续学习"); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	if len(headers) < 3 {
		t.Fatal(headers)
	}
	first, second, last := headers[0], headers[1], headers[2]
	if first.Get("X-CLI-Session-ID") == "" || first.Get("X-CLI-Session-ID") != last.Get("X-CLI-Session-ID") {
		t.Fatal("session identity lost")
	}
	if first.Get("X-CLI-Turn-ID") != second.Get("X-CLI-Turn-ID") || first.Get("X-CLI-Turn-ID") == last.Get("X-CLI-Turn-ID") {
		t.Fatal("wrong title/new turn attribution")
	}
	mu.Unlock()
	restored.Close()
	// Declining a device change must not rewrite the original usage association.
	otherDeps := deps
	otherDeps.Store = controllerStore(t, root, secrets)
	otherDeps.Gateway.DeviceID = "other-device"
	blocked, err := Resume(t.Context(), otherDeps, ResumeOptions{SessionID: session, CurrentWorkspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	if !blocked.Status().GatewayConfirmationRequired {
		blocked.Close()
		t.Fatal("device change was not gated")
	}
	blocked.Close()
	deps.Store = controllerStore(t, root, secrets)
	original, err := Resume(t.Context(), deps, ResumeOptions{SessionID: session, CurrentWorkspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	defer original.Close()
	if _, err = original.Send(t.Context(), "原设备继续"); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	var latest http.Header
	for _, h := range headers {
		if h.Get("X-CLI-Model-Stage") == "foreground" {
			latest = h
		}
	}
	if latest.Get("X-CLI-Session-ID") != first.Get("X-CLI-Session-ID") {
		t.Fatal("declined device change rewrote original association")
	}

}
