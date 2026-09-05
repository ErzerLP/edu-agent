package agentcontroller

import (
	"bytes"
	"context"
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

type controllerCopyOutcomeExecutor struct {
	workspace.Executor
	unknown bool
}

func (w *controllerCopyOutcomeExecutor) CommitMutation(ctx context.Context, p *workspace.PreparedMutation) workspace.Result {
	r := w.Executor.CommitMutation(ctx, p)
	if w.unknown && r.Publication == workspace.PublicationCompleted {
		r.Publication = workspace.PublicationUnknown
		r.Effect.Target.Version = ""
		r.Reference.ContentHash = ""
		r.Reference.InvalidateObserved = true
		v := r.Value.(map[string]any)
		v["publication_outcome"] = "unknown"
		v["complete"] = false
		v["file_effect"] = *r.Effect
		v["error"], v["code"] = workspace.CodeOutcomeUnknown, workspace.CodeOutcomeUnknown
		delete(v, "content_hash")
	}
	return r
}
func TestControllerCopyCompletedUnknownSaveResumeAndReceiptMatching(t *testing.T) {
	for _, unknown := range []bool{false, true} {
		t.Run(map[bool]string{false: "completed", true: "unknown"}[unknown], func(t *testing.T) {
			storeRoot, root := t.TempDir(), t.TempDir()
			data := bytes.Repeat([]byte("\x00\xffcopy-body"), 120000)
			if err := os.WriteFile(filepath.Join(root, "source.bin"), data, 0600); err != nil {
				t.Fatal(err)
			}
			executor, err := workspace.Open(root)
			if err != nil {
				t.Fatal(err)
			}
			stat := executor.Execute(t.Context(), "stat", `{"path":"source.bin"}`)
			version := stat.Value.(map[string]any)["entry_version"].(string)
			raw, _ := json.Marshal(map[string]any{"source": "source.bin", "destination": "copied.bin", "expected_version": version})
			secrets := &controllerSecretBackend{}
			model := &scriptedControllerModel{responses: []modelclient.Response{{Message: modelclient.Message{Role: "assistant", ToolCalls: []modelclient.ToolCall{{ID: "copy-call", Type: "function", Function: modelclient.ToolFunction{Name: "copy", Arguments: string(raw)}}}}}, {Message: modelclient.Message{Role: "assistant", Content: "已复制"}}}}
			server := &controllerServer{generation: api.MemoryGenerationStamp{LearnerGeneration: 1, MemoryGeneration: 1}}
			provider := Provider{Name: "ollama", Endpoint: "http://127.0.0.1:11434/v1", Model: "local"}
			deps := controllerDependencies(controllerStore(t, storeRoot, secrets), model, server, root, provider)
			deps.LoopOptions.Workspace = &controllerCopyOutcomeExecutor{Executor: executor, unknown: unknown}
			c, err := Start(t.Context(), deps, false)
			if err != nil {
				t.Fatal(err)
			}
			pending, err := c.Send(t.Context(), "复制文件")
			if err != nil || pending.PendingFileMutation == nil {
				c.abort()
				t.Fatal(pending, err)
			}
			result, err := c.ResolveFileMutation(t.Context(), "copy-call", agentloop.FileMutationApprove)
			if err != nil {
				c.abort()
				t.Fatal(err)
			}
			c.mu.Lock()
			receipts := append([]agentsession.FileReceipt(nil), c.record.FileReceipts...)
			c.mu.Unlock()
			if len(receipts) != 1 {
				c.abort()
				t.Fatal(receipts)
			}
			receipt := receipts[0]
			if receipt.Effect.Source.Path != "source.bin" || receipt.Effect.Source.Version != version || receipt.Effect.Target.Path != "copied.bin" || receipt.Effect.Operation != "copy" {
				c.abort()
				t.Fatal(receipt)
			}
			expected := agentsession.NoticeOutcomeCompleted
			if unknown {
				expected = agentsession.NoticeOutcomeUnknown
			}
			if receipt.Outcome != expected || unknown && receipt.Effect.Target.Version != "" || !unknown && receipt.Effect.Target.Version == "" {
				c.abort()
				t.Fatal(receipt)
			}
			checkpoint, err := c.loop.ExportCheckpoint()
			if err != nil {
				c.abort()
				t.Fatal(err)
			}
			ahead := agentsession.FileWriteAhead{ToolCallID: "copy-call", Effect: receipt.Effect}
			ahead.Effect.Target.Version = ""
			for _, mutate := range []func(*fileeffects.Effect){func(e *fileeffects.Effect) { e.Source.Path = "other.bin" }, func(e *fileeffects.Effect) { e.Source.Version = "entry-v1:" + strings.Repeat("b", 64) }, func(e *fileeffects.Effect) { e.Target.Path = "other.bin" }} {
				bad := ahead
				mutate(&bad.Effect)
				if _, _, err := fileReceiptFromCheckpoint(bad, checkpoint, result.Events); err == nil {
					c.abort()
					t.Fatal("mismatching copy receipt accepted")
				}
			}
			cpBytes, _ := json.Marshal(checkpoint)
			var noEffect agentloop.SessionCheckpoint
			if err = json.Unmarshal(cpBytes, &noEffect); err != nil {
				c.abort()
				t.Fatal(err)
			}
			for i := range noEffect.Context.Sources {
				if noEffect.Context.Sources[i].ModelMessage.ToolCallID == "copy-call" {
					noEffect.Context.Sources[i].RecallText = `{"operation":"copy"}`
					noEffect.Context.Sources[i].ModelMessage.Content = `{"operation":"copy"}`
				}
			}
			for i := range noEffect.Messages {
				if noEffect.Messages[i].ToolCallID == "copy-call" {
					noEffect.Messages[i].Content = `{"operation":"copy"}`
				}
			}
			for i := range noEffect.ToolHistory {
				if noEffect.ToolHistory[i].Key == "copy-call" {
					noEffect.ToolHistory[i].Value = `{"operation":"copy"}`
				}
			}
			if _, _, err = fileReceiptFromCheckpoint(ahead, noEffect, result.Events); err == nil {
				c.abort()
				t.Fatal("missing full copy effect accepted")
			}
			id := c.SessionID()
			if err = c.Shutdown(t.Context()); err != nil {
				t.Fatal(err)
			}
			if err = os.Remove(filepath.Join(root, "copied.bin")); err != nil {
				t.Fatal(err)
			} // User cleanup in fixture: resume must not replay.
			resumed, err := Resume(t.Context(), controllerDependencies(controllerStore(t, storeRoot, secrets), &controllerModel{}, server, root, provider), ResumeOptions{SessionID: id, CurrentWorkspace: root})
			if err != nil {
				t.Fatal(err)
			}
			defer resumed.Close()
			resumed.mu.Lock()
			saved := append([]agentsession.FileReceipt(nil), resumed.record.FileReceipts...)
			resumed.mu.Unlock()
			if len(saved) != 1 || saved[0] != receipt {
				t.Fatal(saved)
			}
			if _, err = os.Stat(filepath.Join(root, "copied.bin")); !os.IsNotExist(err) {
				t.Fatal("restore replayed copy", err)
			}
			source, err := os.ReadFile(filepath.Join(root, "source.bin"))
			if err != nil || !bytes.Equal(source, data) {
				t.Fatal("restore changed source", err)
			}
			if unknown {
				outcomes := resumed.UnknownOutcomes()
				if len(outcomes) != 1 {
					t.Fatal(outcomes)
				}
				for _, want := range []string{"source.bin", "copied.bin", "未修改源", "不会自动重试"} {
					if !strings.Contains(outcomes[0].Label, want) {
						t.Fatal(outcomes)
					}
				}
			}
		})
	}
}
func TestControllerCopyWALCrashRecoveryDoesNotCopy(t *testing.T) {
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
	effect := fileeffects.New("copy", "source", "never", "file")
	effect.Source.Version = "entry-v1:" + strings.Repeat("a", 64)
	if err = c.BeforeFilePublication(t.Context(), agentloop.FileWriteAhead{ToolCallID: "crash-copy", Effect: effect}); err != nil {
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
		t.Fatal("replayed WAL", entries)
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
