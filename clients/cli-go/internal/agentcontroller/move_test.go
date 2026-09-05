package agentcontroller

import (
	"encoding/json"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/agentloop"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/agentsession"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/api"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/fileeffects"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/modelclient"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/workspace"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestControllerMoveCompletedUnknownSaveResumeAndStrictReceipts(t *testing.T) {
	for _, unknown := range []bool{false, true} {
		t.Run(map[bool]string{false: "completed", true: "unknown"}[unknown], func(t *testing.T) {
			storeRoot, root := t.TempDir(), t.TempDir()
			if err := os.MkdirAll(filepath.Join(root, "source/child"), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, "source/child/file"), []byte("move-body\x00\xff"), 0600); err != nil {
				t.Fatal(err)
			}
			executor, err := workspace.Open(root)
			if err != nil {
				t.Fatal(err)
			}
			stat := executor.Execute(t.Context(), "stat", `{"path":"source"}`)
			version := stat.Value.(map[string]any)["entry_version"].(string)
			raw, _ := json.Marshal(map[string]any{"source": "source", "destination": "target", "expected_version": version})
			secrets := &controllerSecretBackend{}
			model := &scriptedControllerModel{responses: []modelclient.Response{{Message: modelclient.Message{Role: "assistant", ToolCalls: []modelclient.ToolCall{{ID: "move-call", Type: "function", Function: modelclient.ToolFunction{Name: "move", Arguments: string(raw)}}}}}, {Message: modelclient.Message{Role: "assistant", Content: "已移动"}}}}
			server := &controllerServer{generation: api.MemoryGenerationStamp{LearnerGeneration: 1, MemoryGeneration: 1}}
			provider := Provider{Name: "ollama", Endpoint: "http://127.0.0.1:11434/v1", Model: "local"}
			deps := controllerDependencies(controllerStore(t, storeRoot, secrets), model, server, root, provider)
			deps.LoopOptions.Workspace = &controllerCopyOutcomeExecutor{Executor: executor, unknown: unknown}
			c, err := Start(t.Context(), deps, false)
			if err != nil {
				t.Fatal(err)
			}
			defer c.abort()
			pending, err := c.Send(t.Context(), "移动目录")
			if err != nil || pending.PendingFileMutation == nil {
				t.Fatal(pending, err)
			}
			result, err := c.ResolveFileMutation(t.Context(), "move-call", agentloop.FileMutationApprove)
			if err != nil {
				t.Fatal(err)
			}
			c.mu.Lock()
			receipts := append([]agentsession.FileReceipt(nil), c.record.FileReceipts...)
			c.mu.Unlock()
			if len(receipts) != 1 {
				t.Fatal(receipts)
			}
			receipt := receipts[0]
			expected := agentsession.NoticeOutcomeCompleted
			if unknown {
				expected = agentsession.NoticeOutcomeUnknown
			}
			if receipt.Outcome != expected || receipt.Effect.Source.Path != "source" || receipt.Effect.Target.Path != "target" || receipt.Effect.Source.Version != version || receipt.Effect.Target.Version != "" || receipt.Effect.Source.Kind != "directory" || receipt.Effect.Scope != "subtree" || !receipt.InvalidateObserved {
				t.Fatal(receipt)
			}
			if _, err = os.Stat(filepath.Join(root, "source")); !os.IsNotExist(err) {
				t.Fatal("source not moved", err)
			}
			checkpoint, err := c.loop.ExportCheckpoint()
			if err != nil {
				t.Fatal(err)
			}
			ahead := agentsession.FileWriteAhead{ToolCallID: "move-call", Effect: receipt.Effect}
			for _, mutate := range []func(*fileeffects.Effect){func(e *fileeffects.Effect) { e.Source.Path = "other" }, func(e *fileeffects.Effect) { e.Source.Version = "entry-v1:" + strings.Repeat("b", 64) }, func(e *fileeffects.Effect) { e.Target.Path = "other" }, func(e *fileeffects.Effect) { e.Source.Kind = "file" }} {
				bad := ahead
				mutate(&bad.Effect)
				if _, _, err := fileReceiptFromCheckpoint(bad, checkpoint, result.Events); err == nil {
					t.Fatal("mismatching move plan accepted")
				}
			}
			cpBytes, _ := json.Marshal(checkpoint)
			for _, fault := range []string{"missing_effect", "hash_reference", "invalidation", "event", "turn"} {
				var cp agentloop.SessionCheckpoint
				if err = json.Unmarshal(cpBytes, &cp); err != nil {
					t.Fatal(err)
				}
				events := append([]agentloop.Event(nil), result.Events...)
				switch fault {
				case "missing_effect":
					for i := range cp.Context.Sources {
						if cp.Context.Sources[i].ModelMessage.ToolCallID == "move-call" {
							cp.Context.Sources[i].RecallText = `{"operation":"move"}`
							cp.Context.Sources[i].ModelMessage.Content = `{"operation":"move"}`
						}
					}
					for i := range cp.Messages {
						if cp.Messages[i].ToolCallID == "move-call" {
							cp.Messages[i].Content = `{"operation":"move"}`
						}
					}
					for i := range cp.ToolHistory {
						if cp.ToolHistory[i].Key == "move-call" {
							cp.ToolHistory[i].Value = `{"operation":"move"}`
						}
					}
				case "hash_reference":
					for i := range cp.WorkspaceReferences {
						if cp.WorkspaceReferences[i].Key == "move-call" {
							cp.WorkspaceReferences[i].Value.ContentHash = "sha256:" + strings.Repeat("c", 64)
						}
					}
				case "invalidation":
					for i := range cp.WorkspaceReferences {
						if cp.WorkspaceReferences[i].Key == "move-call" {
							cp.WorkspaceReferences[i].Value.InvalidateObserved = false
						}
					}
				case "event":
					events = nil
				case "turn":
					for i := range cp.Turns {
						cp.Turns[i].FileEffectCallID = "other"
					}
				}
				_, found, err := fileReceiptFromCheckpoint(ahead, cp, events)
				if err == nil && (fault != "turn" || found) {
					t.Fatal("incomplete move receipt accepted", fault)
				}
			}
			id := c.SessionID()
			if err = c.Shutdown(t.Context()); err != nil {
				t.Fatal(err)
			}
			// User action in temporary fixture; recovery must not restore the old target.
			if err = os.Rename(filepath.Join(root, "target"), filepath.Join(root, "user-location")); err != nil {
				t.Fatal(err)
			}
			resumed, err := Resume(t.Context(), controllerDependencies(controllerStore(t, storeRoot, secrets), &controllerModel{}, server, root, provider), ResumeOptions{SessionID: id, CurrentWorkspace: root})
			if err != nil {
				t.Fatal(err)
			}
			defer resumed.Close()
			resumed.mu.Lock()
			saved := append([]agentsession.FileReceipt(nil), resumed.record.FileReceipts...)
			resumed.mu.Unlock()
			if len(saved) != 1 || saved[0] != receipt {
				t.Fatal("lost receipt", saved)
			}
			for _, name := range []string{"source", "target"} {
				if _, err = os.Stat(filepath.Join(root, name)); !os.IsNotExist(err) {
					t.Fatal("replayed move", name, err)
				}
			}
			data, err := os.ReadFile(filepath.Join(root, "user-location/child/file"))
			if err != nil || string(data) != "move-body\x00\xff" {
				t.Fatal("user bytes changed", err)
			}
			if unknown {
				outcomes := resumed.UnknownOutcomes()
				if len(outcomes) != 1 {
					t.Fatal(outcomes)
				}
				for _, want := range []string{"source", "target", "不会自动重试", "核查两端"} {
					if !strings.Contains(outcomes[0].Label, want) {
						t.Fatal(outcomes)
					}
				}
				if strings.Contains(outcomes[0].Label, "未修改源") {
					t.Fatal("false copy claim", outcomes)
				}
			}
		})
	}
}
func TestControllerMoveWALCrashRecoveryDoesNotReplay(t *testing.T) {
	storeRoot, root := t.TempDir(), t.TempDir()
	secrets := &controllerSecretBackend{}
	server := &controllerServer{generation: api.MemoryGenerationStamp{LearnerGeneration: 1, MemoryGeneration: 1}}
	provider := Provider{Name: "ollama", Endpoint: "http://127.0.0.1:11434/v1", Model: "local"}
	if err := os.WriteFile(filepath.Join(root, "source"), []byte("preserved"), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := Start(t.Context(), controllerDependencies(controllerStore(t, storeRoot, secrets), &controllerModel{}, server, root, provider), false)
	if err != nil {
		t.Fatal(err)
	}
	if err = c.BeginTurn(t.Context(), agentloop.DirtyIntent{TurnSequence: 1, OperationClass: "agent-turn"}); err != nil {
		c.abort()
		t.Fatal(err)
	}
	effect := fileeffects.New("move", "source", "never", "file")
	effect.Source.Version = "entry-v1:" + strings.Repeat("a", 64)
	if err = c.BeforeFilePublication(t.Context(), agentloop.FileWriteAhead{ToolCallID: "crash-move", Effect: effect}); err != nil {
		c.abort()
		t.Fatal(err)
	}
	id := c.SessionID()
	c.abort()
	resumed, err := Resume(t.Context(), controllerDependencies(controllerStore(t, storeRoot, secrets), &controllerModel{}, server, root, provider), ResumeOptions{SessionID: id, CurrentWorkspace: root})
	if err != nil {
		t.Fatal(err)
	}
	defer resumed.Close()
	entries, _ := os.ReadDir(root)
	if len(entries) != 1 || entries[0].Name() != "source" {
		t.Fatal("replayed move WAL", entries)
	}
	unknown := resumed.UnknownOutcomes()
	if len(unknown) != 1 || !strings.Contains(unknown[0].Label, "source") || !strings.Contains(unknown[0].Label, "never") {
		t.Fatal(unknown)
	}
	resumed.mu.Lock()
	receipts := append([]agentsession.FileReceipt(nil), resumed.record.FileReceipts...)
	resumed.mu.Unlock()
	if len(receipts) != 1 || receipts[0].Effect != effect || receipts[0].Outcome != agentsession.NoticeOutcomeUnknown {
		t.Fatal(receipts)
	}
}
