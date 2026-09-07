package agentcontroller

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/agentloop"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/modelclient"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/workspace"
)

const recursiveCopyPrivate = "copy_private_2c13ff"

type recursiveCopyModel struct {
	version, destination, callID string
	calls                        int
	result                       map[string]any
	batchID, page                string
	found                        bool
}

func (m *recursiveCopyModel) Complete(_ context.Context, request modelclient.Request) (modelclient.Response, error) {
	if isTitleRequest(request) {
		return modelclient.Response{Message: modelclient.Message{Role: "assistant", Content: `{"title":"copy test"}`}}, nil
	}
	m.calls++
	last := request.Messages[len(request.Messages)-1]
	name, id := "", ""
	var args map[string]any
	if last.Role == "user" {
		name, id = "copy", m.callID
		args = map[string]any{"source": "source", "destination": m.destination, "expected_version": m.version}
	} else if last.Role == "tool" {
		value := map[string]any{}
		if err := json.Unmarshal([]byte(last.Content), &value); err != nil {
			return modelclient.Response{}, err
		}
		if last.ToolCallID == m.callID {
			m.result = value
			m.batchID, _ = value["batch_id"].(string)
			if m.batchID != "" {
				name, id = "artifact", "copy-log-search"
				args = map[string]any{"action": "search", "id": m.batchID, "needle": `"type":"actual"`, "limit": 1}
			}
		} else if last.ToolCallID == "copy-log-search" {
			matches, _ := value["matches"].([]any)
			m.found = len(matches) == 1
			name, id = "artifact", "copy-log-read"
			args = map[string]any{"action": "read", "id": m.batchID, "limit": 4096}
		} else if last.ToolCallID == "copy-log-read" {
			m.page, _ = value["data"].(string)
		}
	}
	if name == "" {
		return modelclient.Response{Message: modelclient.Message{Role: "assistant", Content: "done"}}, nil
	}
	raw, _ := json.Marshal(args)
	return modelclient.Response{Message: modelclient.Message{Role: "assistant", ToolCalls: []modelclient.ToolCall{{ID: id, Type: "function", Function: modelclient.ToolFunction{Name: name, Arguments: string(raw)}}}}}, nil
}

func recursiveCopyFixture(t *testing.T, noSave bool) (*leaseFixture, *Controller, *recursiveCopyModel) {
	t.Helper()
	f := outputFixture(t, noSave)
	owner, provider := f.controller.SessionID(), f.controller.provider
	if err := f.controller.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(f.workspace, "source", "empty"), 0700); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 40; i++ {
		name := fmt.Sprintf("%s-%03d.bin", recursiveCopyPrivate, i)
		if err := os.WriteFile(filepath.Join(f.workspace, "source", name), []byte("binary\x00\xff"), 0640); err != nil {
			t.Fatal(err)
		}
	}
	w, err := workspace.Open(f.workspace)
	if err != nil {
		t.Fatal(err)
	}
	version := w.Execute(t.Context(), "stat", `{"path":"source"}`).Value.(map[string]any)["entry_version"].(string)
	w.Close()
	model := &recursiveCopyModel{version: version, destination: "target", callID: "saved-tree-copy"}
	deps := controllerDependencies(nil, model, f.server, f.workspace, provider)
	deps.LoopOptions.ContextWindow = 32768
	deps.LoopOptions.ToolTimeout = 30 * time.Second
	var c *Controller
	if noSave {
		opened, openErr := openWorkspaceForClient(f.workspace, deps.LoopOptions)
		if openErr != nil {
			t.Fatal(openErr)
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

func readRecursiveCopyLog(t *testing.T, c *Controller, id string) []byte {
	t.Helper()
	var data []byte
	for offset := int64(0); ; {
		page, err := c.ReadLocalArtifact(t.Context(), id, offset, 4096)
		if err != nil {
			t.Fatal(err)
		}
		if page.NextOffset != offset+int64(len(page.Data)) {
			t.Fatal("not the returned raw prefix")
		}
		data = append(data, page.Data...)
		if !page.More {
			if page.Info.Bytes != int64(len(data)) || page.Info.Hash != fmt.Sprintf("sha256:%x", sha256.Sum256(data)) {
				t.Fatal("incomplete log/hash", page.Info)
			}
			break
		}
		offset = page.NextOffset
	}
	return data
}

func TestRecursiveCopyEncryptedRecoveryAndNoSave(t *testing.T) {
	for _, noSave := range []bool{false, true} {
		t.Run(fmt.Sprintf("noSave=%t", noSave), func(t *testing.T) {
			f, c, model := recursiveCopyFixture(t, noSave)
			pending, err := c.Send(t.Context(), "copy")
			if err != nil || pending.PendingFileMutation == nil || pending.PendingFileMutation.PlanID == "" || pending.PendingFileMutation.PlanSaved == noSave {
				t.Fatalf("pending=%+v err=%v", pending, err)
			}
			if _, err = c.ResolveFileMutation(t.Context(), pending.PendingFileMutation.CallID, agentloop.FileMutationApprove); err != nil {
				t.Fatal(err)
			}
			if !model.found || !strings.Contains(model.page, recursiveCopyPrivate) || model.result["item_count"] != float64(42) || model.result["completed"] != float64(42) {
				t.Fatalf("model routes/settlement: %+v", model)
			}
			status, err := c.fileBatches.Status(c.artifactOwner, model.batchID)
			if err != nil || !status.Finished || status.Items != 42 || status.Completed != 42 || (status.SavedBytes == 0) != noSave {
				t.Fatalf("status=%+v err=%v", status, err)
			}
			if !noSave && (len(c.record.FileReceipts) != 1 || !c.record.FileReceipts[0].Effect.IsDirectoryCopy()) {
				t.Fatal("root receipt or old 32-item journal bypass failed", c.record.FileReceipts)
			}
			for i := 0; i < 40; i++ {
				data, err := os.ReadFile(filepath.Join(f.workspace, "target", fmt.Sprintf("%s-%03d.bin", recursiveCopyPrivate, i)))
				if err != nil || !bytes.Equal(data, []byte("binary\x00\xff")) {
					t.Fatal("wrong copied bytes", i, err)
				}
			}
			if info, err := os.Stat(filepath.Join(f.workspace, "target", "empty")); err != nil || !info.IsDir() {
				t.Fatal("lost empty directory", err)
			}
			full := readRecursiveCopyLog(t, c, model.batchID)
			if bytes.Count(full, []byte(`"type":"actual"`)) != 42 {
				t.Fatal("not a full per-item receipt")
			}
			cp, err := c.loop.ExportCheckpoint()
			if err != nil {
				t.Fatal(err)
			}
			encoded, _ := json.Marshal(cp)
			if bytes.Contains(encoded, []byte(recursiveCopyPrivate)) || bytes.Contains(encoded, []byte("plan_only_not_executed")) {
				t.Fatal("full manifest or raw log leaked to checkpoint")
			}
			if !noSave {
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
					if bytes.Contains(blob, []byte(recursiveCopyPrivate)) {
						t.Fatal("plaintext manifest/log on disk")
					}
				}
			}
			owner, id := c.SessionID(), model.batchID
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
			fresh := &recursiveCopyModel{}
			deps := controllerDependencies(f.openStore(t), fresh, f.server, f.workspace, Provider{Name: "changed", Endpoint: "https://different.example/v1", Model: "test"})
			deps.LoopOptions.ContextWindow = 32768
			restored, err := Resume(t.Context(), deps, ResumeOptions{SessionID: owner, CurrentWorkspace: f.workspace})
			if err != nil {
				t.Fatal(err)
			}
			defer restored.Close()
			if !restored.Status().ProviderConfirmationRequired || fresh.calls != 0 {
				t.Fatal("provider preflight bypassed")
			}
			if got := readRecursiveCopyLog(t, restored, id); !bytes.Equal(got, full) {
				t.Fatal("saved log changed on recovery")
			}
			status, err = restored.fileBatches.Status(restored.artifactOwner, id)
			if err != nil || !status.Restored || status.Completed != 42 {
				t.Fatalf("restored status=%+v %v", status, err)
			}
			if err := restored.fileBatches.Pending(t.Context(), restored.artifactOwner, id, 0); err == nil {
				t.Fatal("recovery resumed writer")
			}
			page, err := restored.SearchLocalArtifact(t.Context(), id, recursiveCopyPrivate, 0, 1)
			if err != nil || len(page.Offsets) != 1 || fresh.calls != 0 {
				t.Fatal("manual search called model or lost data", err)
			}
			external := f.openStore(t)
			defer external.Close()
			if err := external.Clear(t.Context()); err != nil {
				t.Fatal(err)
			}
			if _, err := restored.ReadLocalArtifact(t.Context(), id, int64(len(full)), 32); err == nil {
				t.Fatal("EOF bypassed privacy revocation")
			}
			if _, err := restored.LocalArtifacts(); err == nil {
				t.Fatal("catalog bypassed privacy revocation")
			}
		})
	}
}
