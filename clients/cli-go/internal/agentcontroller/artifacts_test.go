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

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/agentloop"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/localartifact"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/modelclient"
)

const artifactPrivateMarker = "PRIVATE_DIFF_CONTENT_5b627f"

type artifactTestModel struct {
	calls  int
	diffID string
	read   string
}

func (m *artifactTestModel) Complete(_ context.Context, r modelclient.Request) (modelclient.Response, error) {
	if isTitleRequest(r) {
		return modelclient.Response{Message: modelclient.Message{Role: "assistant", Content: `{"title":"patch result"}`}}, nil
	}
	m.calls++
	last := map[string]any{}
	if r.Messages[len(r.Messages)-1].Role == "tool" {
		if err := json.Unmarshal([]byte(r.Messages[len(r.Messages)-1].Content), &last); err != nil {
			return modelclient.Response{}, err
		}
	}
	name, id, args := "", "", map[string]any{}
	switch m.calls {
	case 1:
		name, id = "apply_patch", "saved-patch"
		args = map[string]any{"patch": "*** Begin Patch\n*** Add File: result.txt\n+" + artifactPrivateMarker + strings.Repeat("z", 12000) + "\n*** Add File: another.txt\n+second file\n*** Delete File: remove.txt\n*** End Patch", "expected_hashes": map[string]string{"remove.txt": fmt.Sprintf("sha256:%x", sha256.Sum256([]byte("remove me\n")))}}
	case 2:
		m.diffID, _ = last["diff_id"].(string)
		name, id = "artifact", "find-diff"
		args = map[string]any{"action": "search", "id": m.diffID, "needle": artifactPrivateMarker, "limit": 1}
	case 3:
		matches, _ := last["matches"].([]any)
		offset := float64(0)
		if len(matches) > 0 {
			offset, _ = matches[0].(float64)
		}
		name, id = "artifact", "read-diff"
		args = map[string]any{"action": "read", "id": m.diffID, "offset": offset, "limit": len(artifactPrivateMarker)}
	case 4:
		m.read, _ = last["data"].(string)
	}
	if name == "" {
		return modelclient.Response{Message: modelclient.Message{Role: "assistant", Content: "done"}}, nil
	}
	raw, _ := json.Marshal(args)
	return modelclient.Response{Message: modelclient.Message{Role: "assistant", ToolCalls: []modelclient.ToolCall{{ID: id, Type: "function", Function: modelclient.ToolFunction{Name: name, Arguments: string(raw)}}}}}, nil
}

func artifactControllerFixture(t *testing.T, noSave bool) (*leaseFixture, *Controller, *artifactTestModel) {
	t.Helper()
	f := outputFixture(t, noSave)
	owner := f.controller.SessionID()
	provider := f.controller.provider
	if err := f.controller.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
	model := &artifactTestModel{}
	dependencies := controllerDependencies(nil, model, f.server, f.workspace, provider)
	dependencies.LoopOptions.ContextWindow = 32768
	var c *Controller
	var err error
	if noSave {
		opened, openErr := openWorkspaceForClient(f.workspace, dependencies.LoopOptions)
		if openErr != nil {
			t.Fatal(openErr)
		}
		dependencies.LoopOptions.Workspace = opened
		dependencies.LoopOptions.WorkspaceStatus = opened.Status()
		c, err = Start(t.Context(), dependencies, true)
	} else {
		dependencies.Store = f.openStore(t)
		c, err = Resume(t.Context(), dependencies, ResumeOptions{SessionID: owner, CurrentWorkspace: f.workspace})
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	if err := os.WriteFile(filepath.Join(f.workspace, "remove.txt"), []byte("remove me\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return f, c, model
}

func TestFileArtifactEncryptedPatchRecovery(t *testing.T) {
	f, c, model := artifactControllerFixture(t, false)
	result, err := c.Send(t.Context(), "patch")
	if err != nil || result.PendingFileMutation == nil || !result.PendingFileMutation.DiffSaved {
		t.Fatalf("pending=%+v err=%v", result, err)
	}
	if _, err := c.ResolveFileMutation(t.Context(), result.PendingFileMutation.CallID, agentloop.FileMutationApprove); err != nil {
		t.Fatal(err)
	}
	if model.calls != 4 || model.read != artifactPrivateMarker {
		t.Fatalf("model artifact route: %+v", model)
	}
	if len(c.record.FileReceipts) != 3 {
		t.Fatalf("per-file receipt missing: %+v", c.record.FileReceipts)
	}
	checkpoint, err := c.loop.ExportCheckpoint()
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(checkpoint)
	if bytes.Contains(data, []byte(artifactPrivateMarker)) || bytes.Contains(data, []byte("*** Begin Patch")) {
		t.Fatal("raw diff/arguments entered checkpoint")
	}
	entries, err := os.ReadDir(f.root)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "artifact-") {
			count++
			blob, err := os.ReadFile(filepath.Join(f.root, entry.Name()))
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Contains(blob, []byte(artifactPrivateMarker)) {
				t.Fatal("plaintext sidecar")
			}
		}
	}
	if count < 4 {
		t.Fatalf("independent encrypted diff/receipt absent: %d", count)
	}
	owner, diffID := c.SessionID(), model.diffID
	if err := c.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
	fresh := &artifactTestModel{}
	dependencies := controllerDependencies(f.openStore(t), fresh, f.server, f.workspace, Provider{Name: "changed", Endpoint: "https://different.example/v1", Model: "test"})
	dependencies.LoopOptions.ContextWindow = 32768
	restored, err := Resume(t.Context(), dependencies, ResumeOptions{SessionID: owner, CurrentWorkspace: f.workspace})
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	if !restored.Status().ProviderConfirmationRequired || fresh.calls != 0 {
		t.Fatal("provider preflight bypassed")
	}
	infos, err := restored.LocalArtifacts()
	if err != nil || len(infos) != 2 {
		t.Fatalf("catalog=%+v err=%v", infos, err)
	}
	var full strings.Builder
	for offset := int64(0); ; {
		page, err := restored.ReadLocalArtifact(t.Context(), diffID, offset, 4096)
		if err != nil {
			t.Fatal(err)
		}
		if !page.Info.Saved {
			t.Fatal("saved mode lost")
		}
		full.Write(page.Data)
		offset = page.NextOffset
		if !page.More {
			break
		}
	}
	if !strings.Contains(full.String(), artifactPrivateMarker+strings.Repeat("z", 12000)) {
		t.Fatal("recovered diff incomplete")
	}
	found, err := restored.SearchLocalArtifact(t.Context(), diffID, artifactPrivateMarker, 0, 1)
	if err != nil || len(found.Offsets) != 1 || fresh.calls != 0 {
		t.Fatalf("manual search=%+v err=%v calls=%d", found, err, fresh.calls)
	}
	external := f.openStore(t)
	defer external.Close()
	if err := external.Clear(t.Context()); err != nil {
		t.Fatal(err)
	}
	if page, err := restored.ReadLocalArtifact(t.Context(), diffID, 0, 4096); err == nil {
		t.Fatalf("clear did not revoke body: %+v", page)
	}
}

func TestFileArtifactNoSaveStaysInMemory(t *testing.T) {
	f, c, _ := artifactControllerFixture(t, true)
	result, err := c.Send(t.Context(), "patch")
	if err != nil || result.PendingFileMutation == nil {
		t.Fatalf("%+v %v", result, err)
	}
	if result.PendingFileMutation.DiffSaved {
		t.Fatal("no-save claimed persistence")
	}
	if _, err = c.ResolveFileMutation(t.Context(), result.PendingFileMutation.CallID, agentloop.FileMutationApprove); err != nil {
		t.Fatal(err)
	}
	infos, err := c.LocalArtifacts()
	if err != nil || len(infos) != 2 {
		t.Fatalf("%+v %v", infos, err)
	}
	for _, info := range infos {
		if info.Saved {
			t.Fatal("memory artifact marked saved")
		}
	}
	if err := c.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(f.root)
	if err != nil || len(entries) != 0 {
		t.Fatalf("no-save created history files: %v %v", entries, err)
	}
}

func TestFileArtifactOrphanEvidencePreventsEmptySessionCleanup(t *testing.T) {
	f := outputFixture(t, false)
	c := f.controller
	owner := c.SessionID()
	summary := leaseSummary(t, c, owner)
	if _, err := c.RenameSession(t.Context(), owner, "", summary.RecordRevision); err != nil {
		t.Fatal(err)
	}
	const name = "result_d_r_unconfirmed_0"
	if err := c.handle.WriteArtifact(t.Context(), name, []byte("unconfirmed encrypted segment")); err != nil {
		t.Fatal(err)
	}
	c.abort()
	dependencies := controllerDependencies(f.openStore(t), &artifactTestModel{}, f.server, f.workspace, c.provider)
	restored, err := Resume(t.Context(), dependencies, ResumeOptions{SessionID: owner, CurrentWorkspace: f.workspace})
	if err != nil {
		t.Fatal(err)
	}
	if restored.record.CommittedUserTurns != 0 || restored.record.TitleSource == "manual" {
		t.Fatal("not an empty-conversation fixture")
	}
	if infos, err := restored.LocalArtifacts(); err != nil || len(infos) != 0 {
		t.Fatal("orphan segment must not be promoted to a complete artifact")
	}
	if err := restored.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
	store := f.openStore(t)
	defer store.Close()
	handle, _, err := store.OpenSession(t.Context(), owner)
	if err != nil {
		t.Fatalf("orphan evidence was automatically deleted: %v", err)
	}
	defer handle.Close()
	if data, err := handle.ReadArtifact(t.Context(), name); err != nil || string(data) != "unconfirmed encrypted segment" {
		t.Fatalf("lost evidence: %q %v", data, err)
	}
}

var _ localartifact.Store = fileArtifactStore{}
