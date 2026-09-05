package agentcontroller

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/agentloop"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/agentsession"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/api"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/modelclient"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/workspace"
)

func TestControllerArchiveCompletedReceiptSurvivesResume(t *testing.T) {
	for _, kind := range []string{"file", "directory"} {
		t.Run(kind, func(t *testing.T) {
			storeRoot, root := t.TempDir(), t.TempDir()
			source := filepath.Join(root, "notes")
			if kind == "directory" {
				if err := os.Mkdir(source, 0o700); err != nil {
					t.Fatal(err)
				}
				source = filepath.Join(source, "note.md")
			}
			if err := os.WriteFile(source, []byte("preserved bytes"), 0o600); err != nil {
				t.Fatal(err)
			}
			executor, err := workspace.Open(root)
			if err != nil {
				t.Fatal(err)
			}
			secrets := &controllerSecretBackend{}
			model := &scriptedControllerModel{responses: []modelclient.Response{
				{Message: modelclient.Message{Role: "assistant", ToolCalls: []modelclient.ToolCall{{ID: "archive-call", Type: "function", Function: modelclient.ToolFunction{Name: workspace.ToolArchive, Arguments: `{"path":"notes"}`}}}}},
				{Message: modelclient.Message{Role: "assistant", Content: "归档已完成，由你手动清理。"}},
			}}
			server := &controllerServer{generation: api.MemoryGenerationStamp{LearnerGeneration: 1, MemoryGeneration: 1}}
			provider := Provider{Name: "ollama", Endpoint: "http://127.0.0.1:11434/v1", Model: "local"}
			dependencies := controllerDependencies(controllerStore(t, storeRoot, secrets), model, server, root, provider)
			dependencies.LoopOptions.Workspace = executor
			controller, err := Start(t.Context(), dependencies, false)
			if err != nil {
				t.Fatal(err)
			}
			pending, err := controller.Send(t.Context(), "归档 notes")
			if err != nil || pending.PendingFileMutation == nil {
				controller.abort()
				t.Fatalf("pending=%+v err=%v", pending, err)
			}
			destination := pending.PendingFileMutation.ArchivePath
			if _, err := controller.ResolveFileMutation(t.Context(), "archive-call", agentloop.FileMutationApprove); err != nil {
				controller.abort()
				t.Fatal(err)
			}
			controller.mu.Lock()
			receipts := append([]agentsession.FileReceipt(nil), controller.record.FileReceipts...)
			controller.mu.Unlock()
			if len(receipts) != 1 || receipts[0].Operation != workspace.ToolArchive || receipts[0].Path != "notes" || receipts[0].ArchivePath != destination || receipts[0].Kind != kind || receipts[0].Outcome != agentsession.NoticeOutcomeCompleted || !receipts[0].InvalidateObserved || receipts[0].ContentHash != "" {
				controller.abort()
				t.Fatalf("receipts=%+v", receipts)
			}
			id := controller.SessionID()
			if err := controller.Shutdown(t.Context()); err != nil {
				t.Fatal(err)
			}
			resumed, err := Resume(t.Context(), controllerDependencies(controllerStore(t, storeRoot, secrets), &controllerModel{}, server, root, provider), ResumeOptions{SessionID: id, CurrentWorkspace: root})
			if err != nil {
				t.Fatal(err)
			}
			defer resumed.Close()
			resumed.mu.Lock()
			persisted := append([]agentsession.FileReceipt(nil), resumed.record.FileReceipts...)
			resumed.mu.Unlock()
			if len(persisted) != 1 || persisted[0] != receipts[0] {
				t.Fatalf("persisted=%+v", persisted)
			}
			if _, err := os.Stat(filepath.Join(root, "notes")); !os.IsNotExist(err) {
				t.Fatalf("source resurrected: %v", err)
			}
			archived := filepath.Join(root, filepath.FromSlash(destination))
			if kind == "directory" {
				archived = filepath.Join(archived, "note.md")
			}
			data, err := os.ReadFile(archived)
			if err != nil || string(data) != "preserved bytes" {
				t.Fatalf("archive changed after resume: %q %v", data, err)
			}
		})
	}
}

func TestControllerArchiveCrashRecoveryKeepsBothPathsWithoutReplay(t *testing.T) {
	storeRoot, root := t.TempDir(), t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "notes"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "notes", "a.md"), []byte("old child evidence"), 0o600); err != nil {
		t.Fatal(err)
	}
	executor, err := workspace.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	secrets := &controllerSecretBackend{}
	model := &scriptedControllerModel{responses: []modelclient.Response{
		{Message: modelclient.Message{Role: "assistant", ToolCalls: []modelclient.ToolCall{{ID: "read-child", Type: "function", Function: modelclient.ToolFunction{Name: workspace.ToolRead, Arguments: `{"path":"notes/a.md"}`}}}}},
		{Message: modelclient.Message{Role: "assistant", Content: "已读"}},
	}}
	server := &controllerServer{generation: api.MemoryGenerationStamp{LearnerGeneration: 1, MemoryGeneration: 1}}
	provider := Provider{Name: "ollama", Endpoint: "http://127.0.0.1:11434/v1", Model: "local"}
	dependencies := controllerDependencies(controllerStore(t, storeRoot, secrets), model, server, root, provider)
	dependencies.LoopOptions.Workspace = executor
	controller, err := Start(t.Context(), dependencies, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := controller.Send(t.Context(), "读取笔记"); err != nil {
		controller.abort()
		t.Fatal(err)
	}
	if err := controller.BeginTurn(t.Context(), agentloop.DirtyIntent{TurnSequence: 2, OperationClass: "agent-turn"}); err != nil {
		controller.abort()
		t.Fatal(err)
	}
	const destination = ".edu-agent-archive/20260905-test/notes"
	if err := controller.BeforeFilePublication(t.Context(), agentloop.FileWriteAhead{ToolCallID: "archive-crash", Operation: workspace.ToolArchive, Path: "notes", ArchivePath: destination, Kind: "directory"}); err != nil {
		controller.abort()
		t.Fatal(err)
	}
	id := controller.SessionID()
	controller.abort() // Simulate loss between write-ahead and an observed publication outcome.
	resumed, err := Resume(t.Context(), controllerDependencies(controllerStore(t, storeRoot, secrets), &controllerModel{}, server, root, provider), ResumeOptions{SessionID: id, CurrentWorkspace: root})
	if err != nil {
		t.Fatal(err)
	}
	defer resumed.Close()
	resumed.mu.Lock()
	receipts := append([]agentsession.FileReceipt(nil), resumed.record.FileReceipts...)
	resumed.mu.Unlock()
	if len(receipts) != 1 || receipts[0].ArchivePath != destination || receipts[0].Kind != "directory" || receipts[0].Outcome != agentsession.NoticeOutcomeUnknown {
		t.Fatalf("recovery receipts=%+v", receipts)
	}
	if _, err := os.Stat(filepath.Join(root, "notes", "a.md")); err != nil {
		t.Fatalf("source moved on resume: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, workspace.ArchiveDirectory)); !os.IsNotExist(err) {
		t.Fatalf("replayed archive created destination: %v", err)
	}
	checkpoint, err := resumed.loop.ExportCheckpoint()
	if err != nil {
		t.Fatal(err)
	}
	for _, history := range checkpoint.ToolHistory {
		if history.Key == "read-child" && (strings.Contains(history.Value, "old child evidence") || !strings.Contains(history.Value, "requires_reread")) {
			t.Fatalf("stale child remained live: %s", history.Value)
		}
	}
	outcomes := resumed.UnknownOutcomes()
	if len(outcomes) != 1 || !strings.Contains(outcomes[0].Label, destination) || !strings.Contains(outcomes[0].Label, "不会自动重试") {
		t.Fatalf("unknown outcomes=%+v", outcomes)
	}
}
