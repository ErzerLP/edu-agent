package agentcontroller

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/agentloop"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/agentsession"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/fileeffects"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/securefile"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/workspace"
)

func TestArchivePurgeCrashAndUncertainSettlementNeverReplay(t *testing.T) {
	for _, stage := range []string{"identity_failure", "pending_only", "unconfirmed_actual"} {
		t.Run(stage, func(t *testing.T) {
			f, c, model := purgeControllerFixture(t, false)
			owner, provider := c.SessionID(), c.provider
			fault := &copyJournalFaultStore{fileArtifactStore: fileArtifactStore{c.handle}, identity: stage == "identity_failure"}
			if stage != "pending_only" {
				if err := c.fileBatches.Bind(t.Context(), c.artifactOwner, fault); err != nil {
					t.Fatal(err)
				}
			}
			pending, err := c.Send(t.Context(), "永久清理")
			if err != nil || pending.PendingFileMutation == nil {
				t.Fatal(pending, err)
			}
			id := ""
			if stage == "pending_only" {
				w, err := workspace.Open(f.workspace)
				if err != nil {
					t.Fatal(err)
				}
				raw, _ := json.Marshal(map[string]any{"path": model.path, "expected_version": model.version})
				p, r := w.PrepareMutation(t.Context(), workspace.ToolPurgeArchive, string(raw))
				if p == nil {
					t.Fatal(r)
				}
				if err := c.BeforeFilePublication(t.Context(), agentloop.FileWriteAhead{ToolCallID: model.callID, Effect: p.FileEffect()}); err != nil {
					t.Fatal(err)
				}
				plan := fileeffects.BatchPlan{Root: p.FileEffect()}
				for _, item := range p.PurgeItems() {
					entry := fileeffects.BatchItem{Source: fileeffects.Endpoint{Path: item.Path, Kind: string(item.Kind), Version: item.Version}, Target: fileeffects.Endpoint{Path: item.Path, Kind: "absent"}}
					if item.Kind == securefile.EntryFile {
						entry.Bytes = item.Size
					}
					plan.Items = append(plan.Items, entry)
				}
				id, err = c.fileBatches.Begin(t.Context(), c.artifactOwner, model.callID, plan)
				if err != nil {
					t.Fatal(err)
				}
				if err := c.fileBatches.Pending(t.Context(), c.artifactOwner, id, 0); err != nil {
					t.Fatal(err)
				}
				_ = w.Close()
				// Crash after confirmed per-item intent, before unlink or any settlement.
			} else {
				_, _ = c.ResolveFileMutation(t.Context(), model.callID, agentloop.FileMutationApprove)
				if fault.failures != 1 || c.fileJournalErr == nil || c.dirty == nil {
					t.Fatal("missing persistence fence", fault.failures, c.fileJournalErr)
				}
				if stage == "unconfirmed_actual" {
					items, err := c.fileBatches.List(t.Context(), c.artifactOwner)
					if err != nil || len(items) != 1 {
						t.Fatal(items, err)
					}
					id = items[0].ID
					live, err := c.fileBatches.Status(c.artifactOwner, id)
					if err != nil || live.Completed != 1 || live.Started != 1 || live.Bytes <= live.SavedBytes || live.PersistenceError == "" {
						t.Fatal("actual deletion lost", live, err)
					}
				}
			}
			first := filepath.Join(f.workspace, model.path, fmt.Sprintf("%s-039.bin", purgePrivateName))
			remaining := filepath.Join(f.workspace, model.path, fmt.Sprintf("%s-000.bin", purgePrivateName))
			if _, err := os.Stat(first); stage == "unconfirmed_actual" && !os.IsNotExist(err) || stage != "unconfirmed_actual" && err != nil {
				t.Fatal("wrong first deletion", err)
			}
			if _, err := os.Stat(remaining); err != nil {
				t.Fatal("later item was deleted", err)
			}
			c.abort()
			fresh := &purgeControllerModel{path: model.path, callID: model.callID}
			w, err := workspace.Open(f.workspace)
			if err != nil {
				t.Fatal(err)
			}
			fresh.version = w.Execute(t.Context(), "stat", `{"path":"`+model.path+`"}`).Value.(map[string]any)["entry_version"].(string)
			_ = w.Close()
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
			if fresh.calls != 0 {
				t.Fatal("recovery invoked the model")
			}
			if stage == "identity_failure" {
				// The executor durably settled unchanged before the identity-write
				// failure latched. Keep its call identity without inventing an effect.
				if len(restored.record.FileReceipts) != 0 {
					t.Fatal("known not-started operation became an effect", restored.record.FileReceipts)
				}
			} else if len(restored.record.FileReceipts) != 1 || restored.record.FileReceipts[0].Outcome != agentsession.NoticeOutcomeUnknown || !restored.record.FileReceipts[0].Effect.IsArchivePurge() {
				t.Fatal("WAL promoted/replayed", restored.record.FileReceipts)
			}
			name, expected := localCallMarker(model.callID)
			data, err := restored.handle.ReadArtifact(t.Context(), name)
			if err != nil || !bytes.Equal(data, expected) {
				t.Fatal("lost independent call identity", err)
			}
			if stage == "identity_failure" {
				if restored.fileBatches.HasCall(restored.artifactOwner, model.callID) {
					t.Fatal("unexpected batch identity")
				}
			} else {
				status, err := restored.fileBatches.Status(restored.artifactOwner, id)
				if err != nil || !status.Restored || status.Started != 1 || status.Unknown != 1 || status.Completed != 0 || status.Items != 42 {
					t.Fatal("unconfirmed result promoted", status, err)
				}
				log := readRecursiveCopyLog(t, restored, id)
				if bytes.Contains(log, []byte(`"type":"actual"`)) || !bytes.Contains(log, []byte(`"type":"pending"`)) {
					t.Fatal("orphan promoted")
				}
			}
			if err := restored.Shutdown(t.Context()); err != nil {
				t.Fatal(err)
			}
			restored = open()
			defer restored.Close()
			summary := leaseSummary(t, restored, owner)
			if _, err := restored.RenameSession(t.Context(), owner, "purge replay test", summary.RecordRevision); err != nil {
				t.Fatal(err)
			}
			pending, err = restored.Send(t.Context(), "旧调用身份重提")
			if err != nil || pending.PendingFileMutation == nil {
				t.Fatal(pending, err)
			}
			if _, err = restored.ResolveFileMutation(t.Context(), model.callID, agentloop.FileMutationApprove); err != nil || restored.saveFailed != nil {
				t.Fatal("fence blocked checkpoint", err, restored.saveFailed)
			}
			if _, err := os.Stat(remaining); err != nil {
				t.Fatal("old call replayed", err)
			}
			fresh.mu.Lock()
			fresh.callID = "fresh-purge"
			fresh.mu.Unlock()
			pending, err = restored.Send(t.Context(), "明确发起新的清理")
			if err != nil || pending.PendingFileMutation == nil {
				t.Fatal(pending, err)
			}
			if _, err = restored.ResolveFileMutation(t.Context(), "fresh-purge", agentloop.FileMutationApprove); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(filepath.Join(f.workspace, model.path)); !os.IsNotExist(err) {
				t.Fatal("new identity was blocked", err)
			}
		})
	}
}
