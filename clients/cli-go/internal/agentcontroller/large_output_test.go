package agentcontroller

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/agentloop"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/agentsession"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/api"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/modelclient"
)

type largeOutputControllerModel struct {
	mu       sync.Mutex
	text     string
	requests []modelclient.Request
	title    chan int
}

func (m *largeOutputControllerModel) Complete(_ context.Context, r modelclient.Request) (modelclient.Response, error) {
	if isTitleRequest(r) {
		if m.title != nil {
			select {
			case m.title <- r.MaxTokens:
			default:
			}
		}
		return modelclient.Response{Message: modelclient.Message{Role: "assistant", Content: `{"title":"长回答"}`}}, nil
	}
	if len(r.Tools) == 1 {
		return modelclient.Response{Message: modelclient.Message{Role: "assistant"}}, nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.requests = append(m.requests, r)
	return modelclient.Response{Message: modelclient.Message{Role: "assistant", Content: m.text}}, nil
}

func TestLargeOutputEncryptedSessionSaveResumeAndContinue(t *testing.T) {
	root, workspaceRoot := t.TempDir(), t.TempDir()
	secrets := &controllerSecretBackend{}
	text := " \n" + strings.Repeat("<\n", (1<<19)-1)
	model := &largeOutputControllerModel{text: text, title: make(chan int, 2)}
	server := &controllerServer{generation: api.MemoryGenerationStamp{LearnerGeneration: 3, MemoryGeneration: 4}}
	provider := Provider{Name: "ollama", Endpoint: "http://127.0.0.1:11434/v1", Model: "local"}
	dependencies := controllerDependencies(controllerStore(t, root, secrets), model, server, workspaceRoot, provider)
	dependencies.LoopOptions.ContextWindow = 272000
	dependencies.LoopOptions.MaxTokens = 128000
	controller, err := Start(t.Context(), dependencies, false)
	if err != nil {
		t.Fatal(err)
	}
	result, err := controller.Send(t.Context(), "请保留完整多行长回答")
	if err != nil || result.Text != text {
		controller.Close()
		t.Fatalf("send len=%d err=%v", len(result.Text), err)
	}
	select {
	case tokens := <-model.title:
		if tokens != 96 {
			t.Fatalf("title tokens=%d", tokens)
		}
	case <-time.After(3 * time.Second):
		controller.Close()
		t.Fatal("title request missing")
	}
	id := controller.SessionID()
	if err := controller.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
	store := controllerStore(t, root, secrets)
	handle, loaded, err := store.OpenSession(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	checkpoint, err := agentloop.DecodeSessionCheckpoint(loaded.Record.Checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	if checkpoint.Messages[len(checkpoint.Messages)-1].Content != text {
		t.Fatal("encrypted checkpoint lost body")
	}
	transcript, err := agentsession.DecodeTranscript(loaded.Record.Transcript, store.Limits())
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, entry := range transcript.Entries {
		if entry.Kind == agentsession.TranscriptKindAssistant {
			if entry.Text != text {
				t.Fatal("encrypted transcript lost whitespace or body")
			}
			found = true
		}
	}
	if !found {
		t.Fatal("assistant absent")
	}
	_ = handle.Close()
	continuedModel := &largeOutputControllerModel{text: "continue"}
	dependencies = controllerDependencies(store, continuedModel, server, workspaceRoot, provider)
	dependencies.LoopOptions.ContextWindow = 272000
	dependencies.LoopOptions.MaxTokens = 64000
	resumed, err := Resume(t.Context(), dependencies, ResumeOptions{SessionID: id, CurrentWorkspace: workspaceRoot})
	if err != nil {
		t.Fatal(err)
	}
	defer resumed.Close()
	if _, err := resumed.Send(t.Context(), "继续"); err != nil {
		t.Fatal(err)
	}
	if state, _ := resumed.SessionPersistenceStatus(); state != "saved" {
		t.Fatalf("save state=%s", state)
	}
	continuedModel.mu.Lock()
	requests := append([]modelclient.Request(nil), continuedModel.requests...)
	continuedModel.mu.Unlock()
	if len(requests) != 1 || requests[0].MaxTokens != 64000 {
		t.Fatalf("resume did not use current output limit: requests=%d", len(requests))
	}
	if agentloop.NewTokenEstimator().EstimateRequest(requests[0])+requests[0].MaxTokens+13600 > 272000 {
		t.Fatal("resume exceeds context budget")
	}
	projected := false
	for _, m := range requests[0].Messages {
		if strings.Contains(m.Content, "context_history_projected") && strings.Contains(m.Content, "source_id") {
			projected = true
		}
	}
	if !projected {
		t.Fatal("resumed history has no provenance projection")
	}
}

func TestLargeOutputSessionQuotaFailureIsExplicit(t *testing.T) {
	root, workspaceRoot := t.TempDir(), t.TempDir()
	model := &largeOutputControllerModel{text: strings.Repeat("long line\n", 9000)}
	store := controllerStoreWithLimits(t, root, &controllerSecretBackend{}, agentsession.Limits{TranscriptBytes: 4096})
	dependencies := controllerDependencies(store, model, &controllerServer{generation: api.MemoryGenerationStamp{LearnerGeneration: 1, MemoryGeneration: 1}}, workspaceRoot, Provider{Name: "ollama", Endpoint: "http://127.0.0.1:11434/v1", Model: "local"})
	dependencies.LoopOptions.ContextWindow = 272000
	controller, err := Start(t.Context(), dependencies, false)
	if err != nil {
		t.Fatal(err)
	}
	defer controller.Close()
	result, err := controller.Send(t.Context(), "long answer")
	if !errors.Is(err, agentsession.ErrStoreFull) || result.Text != model.text {
		t.Fatalf("quota err=%v len=%d", err, len(result.Text))
	}
	state, detail := controller.SessionPersistenceStatus()
	if state == "saved" || !strings.Contains(detail, "session_store_full") {
		t.Fatalf("silent quota state=%s detail=%s", state, detail)
	}
}
