package agentcontroller

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/agentloop"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/agentsession"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/modelclient"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/workspace"
)

const purgePrivateName = "purge_private_child_1e43"

type purgeControllerModel struct {
	mu                    sync.Mutex
	path, version, callID string
	calls                 int
	result                map[string]any
	batchID, page         string
	found                 bool
}

func (m *purgeControllerModel) Complete(_ context.Context, r modelclient.Request) (modelclient.Response, error) {
	if isTitleRequest(r) {
		return modelclient.Response{Message: modelclient.Message{Role: "assistant", Content: `{"title":"purge test"}`}}, nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls++
	last := r.Messages[len(r.Messages)-1]
	name, id := "", ""
	var args map[string]any
	if last.Role == "user" {
		name, id = "purge_archive", m.callID
		args = map[string]any{"path": m.path, "expected_version": m.version}
	} else if last.Role == "tool" {
		value := map[string]any{}
		if err := json.Unmarshal([]byte(last.Content), &value); err != nil {
			return modelclient.Response{}, err
		}
		switch last.ToolCallID {
		case m.callID:
			m.result = value
			m.batchID, _ = value["batch_id"].(string)
			if m.batchID != "" {
				name, id = "artifact", "purge-log-search"
				args = map[string]any{"action": "search", "id": m.batchID, "needle": `"type":"actual"`, "limit": 1}
			}
		case "purge-log-search":
			matches, _ := value["matches"].([]any)
			m.found = len(matches) == 1
			name, id = "artifact", "purge-log-read"
			args = map[string]any{"action": "read", "id": m.batchID, "limit": 4096}
		case "purge-log-read":
			m.page, _ = value["data"].(string)
		}
	}
	if name == "" {
		return modelclient.Response{Message: modelclient.Message{Role: "assistant", Content: "done"}}, nil
	}
	raw, _ := json.Marshal(args)
	return modelclient.Response{Message: modelclient.Message{Role: "assistant", ToolCalls: []modelclient.ToolCall{{ID: id, Type: "function", Function: modelclient.ToolFunction{Name: name, Arguments: string(raw)}}}}}, nil
}

func purgeControllerFixture(t *testing.T, noSave bool) (*leaseFixture, *Controller, *purgeControllerModel) {
	t.Helper()
	f := outputFixture(t, noSave)
	owner, provider := f.controller.SessionID(), f.controller.provider
	if err := f.controller.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(f.workspace, "old-tree", "empty"), 0700); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 40; i++ {
		if err := os.WriteFile(filepath.Join(f.workspace, "old-tree", fmt.Sprintf("%s-%03d.bin", purgePrivateName, i)), []byte("private-purge-bytes\x00\xff"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	w, err := workspace.Open(f.workspace)
	if err != nil {
		t.Fatal(err)
	}
	plan, r := w.PrepareMutation(t.Context(), workspace.ToolArchive, `{"path":"old-tree"}`)
	if plan == nil {
		t.Fatal(r)
	}
	r = w.CommitMutation(t.Context(), plan)
	if r.Publication != workspace.PublicationCompleted {
		t.Fatal(r)
	}
	path := r.Value.(map[string]any)["archive_path"].(string)
	version := w.Execute(t.Context(), "stat", `{"path":"`+path+`"}`).Value.(map[string]any)["entry_version"].(string)
	_ = w.Close()
	model := &purgeControllerModel{path: path, version: version, callID: "saved-purge"}
	deps := controllerDependencies(nil, model, f.server, f.workspace, provider)
	deps.LoopOptions.ContextWindow = 32768
	deps.LoopOptions.ToolTimeout = 30 * time.Second
	var c *Controller
	if noSave {
		opened, err := openWorkspaceForClient(f.workspace, deps.LoopOptions)
		if err != nil {
			t.Fatal(err)
		}
		deps.LoopOptions.Workspace, deps.LoopOptions.WorkspaceStatus = opened, opened.Status()
		c, err = Start(t.Context(), deps, true)
	} else {
		deps.Store = f.openStore(t)
		c, err = Resume(t.Context(), deps, ResumeOptions{SessionID: owner, CurrentWorkspace: f.workspace})
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	return f, c, model
}

func TestArchivePurgeEncryptedRecoveryAndNoSave(t *testing.T) {
	for _, noSave := range []bool{false, true} {
		t.Run(fmt.Sprint(noSave), func(t *testing.T) {
			f, c, model := purgeControllerFixture(t, noSave)
			if err := c.SetFileAuthorizationMode(agentloop.FileAuthorizationYOLO); err != nil {
				t.Fatal(err)
			}
			pending, err := c.Send(t.Context(), "清理归档")
			if err != nil || pending.PendingFileMutation == nil || pending.PendingFileMutation.PlanID == "" || pending.PendingFileMutation.PlanSaved == noSave {
				t.Fatal(pending, err)
			}
			if _, err := os.Stat(filepath.Join(f.workspace, model.path)); err != nil {
				t.Fatal("YOLO bypassed explicit confirmation", err)
			}
			if _, err = c.ResolveFileMutation(t.Context(), model.callID, agentloop.FileMutationApprove); err != nil {
				t.Fatal(err)
			}
			if !model.found || !strings.Contains(model.page, purgePrivateName) || model.result["item_count"] != float64(42) || model.result["completed"] != float64(42) || model.result["physical_bytes_reclaimed"] != "unknown" {
				t.Fatal(model)
			}
			if _, err := os.Stat(filepath.Join(f.workspace, model.path)); !os.IsNotExist(err) {
				t.Fatal("scope not deleted", err)
			}
			if _, err := os.Stat(filepath.Dir(filepath.Join(f.workspace, model.path))); err != nil {
				t.Fatal("unselected parent removed", err)
			}
			full := readRecursiveCopyLog(t, c, model.batchID)
			if bytes.Count(full, []byte(`"type":"actual"`)) != 42 {
				t.Fatal("incomplete cleanup receipt")
			}
			cp, err := c.loop.ExportCheckpoint()
			if err != nil {
				t.Fatal(err)
			}
			encoded, _ := json.Marshal(cp)
			for _, secret := range []string{purgePrivateName, "private-purge-bytes", "plan_only_not_executed", `\"type\":\"actual\"`} {
				if bytes.Contains(encoded, []byte(secret)) {
					t.Fatal("private plan/log in checkpoint", secret)
				}
			}
			if !noSave {
				if len(c.record.FileReceipts) != 1 || !c.record.FileReceipts[0].Effect.IsArchivePurge() || c.record.FileReceipts[0].Outcome != agentsession.NoticeOutcomeCompleted {
					t.Fatal(c.record.FileReceipts)
				}
				entries, _ := os.ReadDir(f.root)
				for _, entry := range entries {
					if entry.IsDir() {
						continue
					}
					blob, err := os.ReadFile(filepath.Join(f.root, entry.Name()))
					if err != nil {
						t.Fatal(err)
					}
					if bytes.Contains(blob, []byte(model.path)) || bytes.Contains(blob, []byte(purgePrivateName)) {
						t.Fatal("unencrypted history")
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
					t.Fatal("no-save wrote history", entries, err)
				}
				return
			}
			fresh := &purgeControllerModel{}
			deps := controllerDependencies(f.openStore(t), fresh, f.server, f.workspace, Provider{Name: "changed", Endpoint: "https://different.example/v1", Model: "test"})
			deps.LoopOptions.ContextWindow = 32768
			restored, err := Resume(t.Context(), deps, ResumeOptions{SessionID: owner, CurrentWorkspace: f.workspace})
			if err != nil {
				t.Fatal(err)
			}
			defer restored.Close()
			if !restored.Status().ProviderConfirmationRequired || fresh.calls != 0 || !bytes.Equal(full, readRecursiveCopyLog(t, restored, model.batchID)) {
				t.Fatal("restore/provider/bytes changed")
			}
			if err := restored.fileBatches.Pending(t.Context(), restored.artifactOwner, model.batchID, 0); err == nil {
				t.Fatal("history became executable")
			}
			name, expected := localCallMarker(model.callID)
			marker, err := restored.handle.ReadArtifact(t.Context(), name)
			if err != nil || !bytes.Equal(marker, expected) {
				t.Fatal("missing call identity", err)
			}
			external := f.openStore(t)
			defer external.Close()
			if err := external.Clear(t.Context()); err != nil {
				t.Fatal(err)
			}
			if _, err := restored.ReadLocalArtifact(t.Context(), model.batchID, int64(len(full)), 1); err == nil {
				t.Fatal("EOF bypassed clear")
			}
			if _, err := restored.handle.ReadArtifact(t.Context(), name); err == nil {
				t.Fatal("identity bypassed clear")
			}
			if _, err := os.Stat(filepath.Dir(filepath.Join(f.workspace, model.path))); err != nil {
				t.Fatal("session clear changed archive", err)
			}
		})
	}
}
