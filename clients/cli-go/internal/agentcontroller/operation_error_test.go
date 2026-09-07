package agentcontroller

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/agentsession"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/api"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/modelclient"
)

type operationErrorThenSuccessModel struct {
	mu      sync.Mutex
	failure error
	calls   int
}

func (m *operationErrorThenSuccessModel) Complete(_ context.Context, request modelclient.Request) (modelclient.Response, error) {
	if isTitleRequest(request) {
		return modelclient.Response{Message: modelclient.Message{Role: "assistant", Content: `{"title":"恢复测试"}`}}, nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls++
	if m.calls == 1 {
		return modelclient.Response{}, m.failure
	}
	return modelclient.Response{Message: modelclient.Message{Role: "assistant", Content: "可继续对话"}}, nil
}

func TestOperationErrorTranscriptPersistsWithoutBlockingContinuation(t *testing.T) {
	for _, tc := range []struct {
		name        string
		failure     error
		code, state string
	}{
		{"provider", errors.New("PRIVATE_PROVIDER_FAILURE token=not-for-history"), "agent_request_failed", agentsession.AssistantStateFailed},
		{"model_cancelled", context.Canceled, "context_cancelled", agentsession.AssistantStateStopped},
		{"model_deadline", context.DeadlineExceeded, "context_deadline_exceeded", agentsession.AssistantStateStopped},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, workspaceRoot := t.TempDir(), t.TempDir()
			secrets := &controllerSecretBackend{}
			server := &controllerServer{generation: api.MemoryGenerationStamp{LearnerGeneration: 1, MemoryGeneration: 1}}
			provider := Provider{Name: "ollama", Endpoint: "http://127.0.0.1:11434/v1", Model: "local"}
			model := &operationErrorThenSuccessModel{failure: tc.failure}
			c, err := Start(t.Context(), controllerDependencies(controllerStore(t, root, secrets), model, server, workspaceRoot, provider), false)
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			owner := c.SessionID()
			_, err = c.Send(t.Context(), "可恢复的失败轮次")
			if !errors.Is(err, tc.failure) || c.saveFailed != nil || c.dirty != nil {
				t.Fatalf("operation error became persistence failure: operation=%v save=%v dirty=%v", err, c.saveFailed, c.dirty != nil)
			}
			assertTranscript := func(controller *Controller) {
				t.Helper()
				loaded, err := controller.handle.Load()
				if err != nil {
					t.Fatal(err)
				}
				transcript, err := agentsession.DecodeTranscript(loaded.Record.Transcript, agentsession.DefaultLimits())
				if err != nil {
					t.Fatal(err)
				}
				errorsFound, stoppedFound := 0, 0
				for _, entry := range transcript.Entries {
					if entry.Kind == agentsession.TranscriptKindError {
						errorsFound++
						if entry.Error == nil || entry.Error.Code != tc.code || entry.PresentationOnly || entry.ModelCommitted {
							t.Fatal("error card lost its protected non-model role", entry)
						}
					}
					if entry.Kind == agentsession.TranscriptKindAssistant && entry.AssistantState == tc.state {
						stoppedFound++
						if entry.ModelCommitted {
							t.Fatal("failed draft committed to model", entry)
						}
					}
				}
				if errorsFound != 1 || stoppedFound != 1 {
					t.Fatal("missing stable error/draft", transcript)
				}
				for _, secret := range []string{"PRIVATE_PROVIDER_FAILURE", "not-for-history"} {
					if bytes.Contains(loaded.Record.Transcript, []byte(secret)) || bytes.Contains(loaded.Record.Checkpoint, []byte(secret)) {
						t.Fatal("raw provider error persisted", secret)
					}
				}
			}
			assertTranscript(c)
			if result, err := c.Send(t.Context(), "继续对话"); err != nil || result.Text != "可继续对话" {
				t.Fatal(result, err)
			}
			if err := c.Shutdown(t.Context()); err != nil {
				t.Fatal(err)
			}
			restored, err := Resume(t.Context(), controllerDependencies(controllerStore(t, root, secrets), &controllerModel{}, server, workspaceRoot, provider), ResumeOptions{SessionID: owner, CurrentWorkspace: workspaceRoot})
			if err != nil {
				t.Fatal(err)
			}
			defer restored.Close()
			assertTranscript(restored)
			if _, err := restored.Send(t.Context(), "重启后继续"); err != nil {
				t.Fatal(err)
			}
		})
	}
}
