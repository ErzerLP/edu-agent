package agentcontroller

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/agentloop"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/api"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/modelclient"
)

const liveReasoningPrivateMarker = "PRIVATE-REASONING-NEVER-PERSIST-4321"

type liveReasoningControllerModel struct{ controllerModel }

func (m *liveReasoningControllerModel) Stream(ctx context.Context, request modelclient.Request, observe func(modelclient.StreamEvent) error) (modelclient.Response, error) {
	for _, event := range []modelclient.StreamEvent{{Kind: modelclient.StreamEventResponseStarted}, {Kind: modelclient.StreamEventReasoningDelta, Text: liveReasoningPrivateMarker}} {
		if err := observe(event); err != nil {
			return modelclient.Response{}, err
		}
	}
	return m.Complete(ctx, request)
}

func TestLiveReasoningEncryptedRecoveryAndNoSaveIsolation(t *testing.T) {
	for _, noSave := range []bool{false, true} {
		root, workspaceRoot := t.TempDir(), t.TempDir()
		secrets := &controllerSecretBackend{}
		server := &controllerServer{generation: api.MemoryGenerationStamp{LearnerGeneration: 1, MemoryGeneration: 1}}
		provider := Provider{Name: "ollama", Endpoint: "http://127.0.0.1:11434/v1", Model: "local"}
		model := &liveReasoningControllerModel{}
		c, err := Start(t.Context(), controllerDependencies(controllerStore(t, root, secrets), model, server, workspaceRoot, provider), noSave)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		owner := c.SessionID()
		received := 0
		ctx := agentloop.WithActivityReporter(t.Context(), func(a agentloop.Activity) {
			if a.Kind == agentloop.ActivityReasoningDelta && a.Delta == liveReasoningPrivateMarker {
				received++
			}
		})
		for range 2 {
			if _, err := c.Send(ctx, "公开问题"); err != nil {
				t.Fatal(err)
			}
		}
		if received != 2 {
			t.Fatal("live reasoning not forwarded", received)
		}
		if !noSave {
			deadline := time.Now().Add(time.Second)
			for {
				model.mu.Lock()
				found := false
				for _, request := range model.requests {
					if isTitleRequest(request) {
						found = true
					}
				}
				model.mu.Unlock()
				if found {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("automatic title request was not exercised")
				}
				time.Sleep(time.Millisecond)
			}
		}
		assertStored := func(current *Controller) {
			t.Helper()
			loaded, err := current.handle.Load()
			if err != nil {
				t.Fatal(err)
			}
			encoded, err := json.Marshal(loaded.Record)
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Contains(encoded, []byte(liveReasoningPrivateMarker)) || bytes.Contains(loaded.Record.Checkpoint, []byte(liveReasoningPrivateMarker)) || bytes.Contains(loaded.Record.Transcript, []byte(liveReasoningPrivateMarker)) {
				t.Fatal("reasoning persisted in record/checkpoint/source/transcript/title")
			}
		}
		if noSave {
			if c.handle != nil {
				t.Fatal("no-save acquired durable handle")
			}
		} else {
			assertStored(c)
		}
		if err := c.Shutdown(t.Context()); err != nil {
			t.Fatal(err)
		}
		if !noSave {
			restored, err := Resume(t.Context(), controllerDependencies(controllerStore(t, root, secrets), model, server, workspaceRoot, provider), ResumeOptions{SessionID: owner, CurrentWorkspace: workspaceRoot})
			if err != nil {
				t.Fatal(err)
			}
			assertStored(restored)
			if _, err := restored.Send(t.Context(), "恢复后继续"); err != nil {
				t.Fatal(err)
			}
			if err := restored.Shutdown(t.Context()); err != nil {
				t.Fatal(err)
			}
		}
		model.mu.Lock()
		requests, err := json.Marshal(model.requests)
		model.mu.Unlock()
		if err != nil || bytes.Contains(requests, []byte(liveReasoningPrivateMarker)) {
			t.Fatal("reasoning fed into foreground or title request")
		}
	}
}
