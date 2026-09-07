package agentcontroller

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/agentloop"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/agentsession"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/modelclient"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/workspace"
)

type archiveRestoreModel struct {
	mu                                   sync.Mutex
	source, version, destination, callID string
	calls                                int
}

func (m *archiveRestoreModel) Complete(_ context.Context, request modelclient.Request) (modelclient.Response, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls++
	if len(request.Tools) == 0 {
		return modelclient.Response{Message: modelclient.Message{Role: "assistant", Content: "恢复测试"}}, nil
	}
	if len(request.Messages) > 0 && request.Messages[len(request.Messages)-1].Role == "user" {
		raw, _ := json.Marshal(map[string]any{"source": m.source, "destination": m.destination, "expected_version": m.version})
		return modelclient.Response{Message: modelclient.Message{Role: "assistant", ToolCalls: []modelclient.ToolCall{{ID: m.callID, Type: "function", Function: modelclient.ToolFunction{Name: "restore_archive", Arguments: string(raw)}}}}}, nil
	}
	return modelclient.Response{Message: modelclient.Message{Role: "assistant", Content: "已处理工具结果"}}, nil
}

func archiveRestoreControllerFixture(t *testing.T, noSave bool) (*leaseFixture, *Controller, *archiveRestoreModel) {
	t.Helper()
	f := outputFixture(t, noSave)
	owner, provider := f.controller.SessionID(), f.controller.provider
	if err := f.controller.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.workspace, "original.bin"), []byte("private-controller-restore\x00\xff"), 0600); err != nil {
		t.Fatal(err)
	}
	w, err := workspace.Open(f.workspace)
	if err != nil {
		t.Fatal(err)
	}
	p, result := w.PrepareMutation(t.Context(), workspace.ToolArchive, `{"path":"original.bin"}`)
	if p == nil {
		t.Fatal(result)
	}
	result = w.CommitMutation(t.Context(), p)
	if result.Publication != workspace.PublicationCompleted {
		t.Fatal(result)
	}
	source := result.Value.(map[string]any)["archive_path"].(string)
	version := w.Execute(t.Context(), workspace.ToolStat, `{"path":"`+source+`"}`).Value.(map[string]any)["entry_version"].(string)
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	model := &archiveRestoreModel{source: source, version: version, destination: "recovered", callID: "saved-restore"}
	deps := controllerDependencies(nil, model, f.server, f.workspace, provider)
	deps.LoopOptions.ContextWindow = 32768
	var c *Controller
	if noSave {
		opened, err := workspace.Open(f.workspace)
		if err != nil {
			t.Fatal(err)
		}
		deps.LoopOptions.Workspace = opened
		c, err = Start(t.Context(), deps, true)
		if err != nil {
			t.Fatal(err)
		}
	} else {
		deps.Store = f.openStore(t)
		c, err = Resume(t.Context(), deps, ResumeOptions{SessionID: owner, CurrentWorkspace: f.workspace})
		if err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(c.Close)
	return f, c, model
}

func TestArchiveRestoreEncryptedRecoveryAndNoSave(t *testing.T) {
	for _, noSave := range []bool{false, true} {
		t.Run(fmt.Sprintf("noSave=%t", noSave), func(t *testing.T) {
			f, c, model := archiveRestoreControllerFixture(t, noSave)
			pending, err := c.Send(t.Context(), "恢复归档")
			if err != nil || pending.PendingFileMutation == nil {
				t.Fatal(pending, err)
			}
			if _, err = c.ResolveFileMutation(t.Context(), model.callID, agentloop.FileMutationApprove); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(filepath.Join(f.workspace, "recovered"))
			if err != nil || string(data) != "private-controller-restore\x00\xff" {
				t.Fatal(data, err)
			}
			cp, err := c.loop.ExportCheckpoint()
			if err != nil {
				t.Fatal(err)
			}
			encoded, _ := json.Marshal(cp)
			if bytes.Contains(encoded, []byte("private-controller-restore")) {
				t.Fatal("file body in checkpoint")
			}
			if !noSave {
				if len(c.record.FileReceipts) != 1 {
					t.Fatal(c.record.FileReceipts)
				}
				r := c.record.FileReceipts[0]
				if !r.Effect.IsArchiveRestore() || r.Effect.Source.Path != model.source || r.Effect.Source.Version != model.version || r.Effect.Target.Path != "recovered" || r.Effect.Target.Version != "" || r.Outcome != agentsession.NoticeOutcomeCompleted || !r.InvalidateObserved {
					t.Fatal(r)
				}
				entries, err := os.ReadDir(f.root)
				if err != nil {
					t.Fatal(err)
				}
				for _, entry := range entries {
					if entry.IsDir() {
						continue
					}
					blob, err := os.ReadFile(filepath.Join(f.root, entry.Name()))
					if err != nil {
						t.Fatal(err)
					}
					if bytes.Contains(blob, []byte(model.source)) || bytes.Contains(blob, []byte("private-controller-restore")) {
						t.Fatal("unencrypted recovery data")
					}
				}
			}
			owner := c.SessionID()
			if err := c.Shutdown(t.Context()); err != nil {
				t.Fatal(err)
			}
			if noSave {
				entries, err := os.ReadDir(f.root)
				if err != nil || len(entries) != 0 {
					t.Fatal("no-save history", entries, err)
				}
				return
			}
			fresh := &archiveRestoreModel{}
			deps := controllerDependencies(f.openStore(t), fresh, f.server, f.workspace, Provider{Name: "changed", Endpoint: "https://different.example/v1", Model: "test"})
			deps.LoopOptions.ContextWindow = 32768
			restored, err := Resume(t.Context(), deps, ResumeOptions{SessionID: owner, CurrentWorkspace: f.workspace})
			if err != nil {
				t.Fatal(err)
			}
			defer restored.Close()
			if !restored.Status().ProviderConfirmationRequired || fresh.calls != 0 || len(restored.record.FileReceipts) != 1 || !restored.record.FileReceipts[0].Effect.IsArchiveRestore() {
				t.Fatal("lost receipt/provider gate")
			}
			name, expected := localCallMarker(model.callID)
			marker, err := restored.handle.ReadArtifact(t.Context(), name)
			if err != nil || !bytes.Equal(marker, expected) {
				t.Fatal("lost non-replay identity", err)
			}
			external := f.openStore(t)
			defer external.Close()
			if err := external.Clear(t.Context()); err != nil {
				t.Fatal(err)
			}
			if _, err := restored.handle.ReadArtifact(t.Context(), name); err == nil {
				t.Fatal("identity bypassed privacy revocation")
			}
			if _, err := os.Stat(filepath.Join(f.workspace, "recovered")); err != nil {
				t.Fatal("privacy clear modified restored file", err)
			}
		})
	}
}

func TestArchiveRestoreWALRecoveryNeverReplays(t *testing.T) {
	for _, published := range []bool{false, true} {
		t.Run(fmt.Sprintf("published=%t", published), func(t *testing.T) {
			f, c, model := archiveRestoreControllerFixture(t, false)
			owner, provider := c.SessionID(), c.provider
			pending, err := c.Send(t.Context(), "恢复归档")
			if err != nil || pending.PendingFileMutation == nil {
				t.Fatal(pending, err)
			}
			w, err := workspace.Open(f.workspace)
			if err != nil {
				t.Fatal(err)
			}
			raw, _ := json.Marshal(map[string]any{"source": model.source, "destination": model.destination, "expected_version": model.version})
			plan, result := w.PrepareMutation(t.Context(), workspace.ToolRestoreArchive, string(raw))
			if plan == nil {
				t.Fatal(result)
			}
			if err := c.BeforeFilePublication(t.Context(), agentloop.FileWriteAhead{ToolCallID: model.callID, Effect: plan.FileEffect()}); err != nil {
				t.Fatal(err)
			}
			// Simulate a crash either before rename or after rename but before any
			// executor settlement. Disk location must not be promoted into a receipt.
			if published {
				if result := w.CommitMutation(t.Context(), plan); result.Publication != workspace.PublicationCompleted {
					t.Fatal(result)
				}
			}
			if err := w.Close(); err != nil {
				t.Fatal(err)
			}
			c.abort()
			fresh := &archiveRestoreModel{source: model.source, version: model.version, destination: "must-not-replay", callID: model.callID}
			open := func() *Controller {
				t.Helper()
				deps := controllerDependencies(f.openStore(t), fresh, f.server, f.workspace, provider)
				deps.LoopOptions.ContextWindow = 32768
				next, err := Resume(t.Context(), deps, ResumeOptions{SessionID: owner, CurrentWorkspace: f.workspace})
				if err != nil {
					t.Fatal(err)
				}
				return next
			}
			restored := open()
			if len(restored.record.FileReceipts) != 1 || restored.record.FileReceipts[0].Outcome != agentsession.NoticeOutcomeUnknown || fresh.calls != 0 {
				t.Fatal("WAL promoted or replayed", restored.record.FileReceipts)
			}
			name, expected := localCallMarker(model.callID)
			data, err := restored.handle.ReadArtifact(t.Context(), name)
			if err != nil || !bytes.Equal(data, expected) {
				t.Fatal("WAL identity lost", err)
			}
			if err := restored.Shutdown(t.Context()); err != nil {
				t.Fatal(err)
			}
			restored = open()
			defer restored.Close()
			if published {
				if _, err := os.Stat(filepath.Join(f.workspace, "recovered")); err != nil {
					t.Fatal(err)
				}
				return
			}
			pending, err = restored.Send(t.Context(), "重提旧调用身份")
			if err != nil || pending.PendingFileMutation == nil {
				t.Fatal(pending, err)
			}
			if _, err = restored.ResolveFileMutation(t.Context(), model.callID, agentloop.FileMutationApprove); err != nil || restored.saveFailed != nil {
				t.Fatal("replay fence treated as persistence failure", err, restored.saveFailed)
			}
			if _, err := os.Stat(filepath.Join(f.workspace, "must-not-replay")); !os.IsNotExist(err) {
				t.Fatal("replayed old identity", err)
			}
			fresh.mu.Lock()
			fresh.callID, fresh.destination = "fresh-restore", "fresh-target"
			fresh.mu.Unlock()
			pending, err = restored.Send(t.Context(), "显式新恢复")
			if err != nil || pending.PendingFileMutation == nil {
				t.Fatal(pending, err)
			}
			if _, err = restored.ResolveFileMutation(t.Context(), "fresh-restore", agentloop.FileMutationApprove); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(filepath.Join(f.workspace, "fresh-target")); err != nil {
				t.Fatal("fresh identity blocked", err)
			}
		})
	}
}
