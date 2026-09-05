package agentcontroller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/agentsession"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/modelclient"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/workspace"
)

func TestLargeFileEditDurableReceiptAndResume(t *testing.T) {
	original := "OLD_MARKER\n" + strings.Repeat("large-file-padding\n", 70000)
	digest := sha256.Sum256([]byte(original))
	hash := "sha256:" + hex.EncodeToString(digest[:])
	model := journalModel(journalCall("large-edit", "edit", map[string]any{"path": "large.txt", "expected_hash": hash, "edits": []map[string]string{{"old_text": "OLD_MARKER", "new_text": "NEW_MARKER"}}}))
	c, executor, root, resume := newJournalController(t, model, agentsession.DefaultLimits())
	summary := leaseSummary(t, c, c.SessionID())
	if _, err := c.RenameSession(t.Context(), summary.SessionID, "large edit", summary.RecordRevision); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "large.txt")
	if err := os.WriteFile(path, []byte(original), 0640); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Send(t.Context(), "edit"); err != nil {
		t.Fatal(err)
	}
	want := strings.Replace(original, "OLD_MARKER", "NEW_MARKER", 1)
	data, err := os.ReadFile(path)
	if err != nil || string(data) != want {
		t.Fatal("large publication failed")
	}
	receipts := journalReceipts(t, c, agentsession.NoticeOutcomeCompleted)
	if executor.commits != 1 || receipts[0].Effect.Operation != "edit" {
		t.Fatal("actual settlement missing")
	}
	saved := resume()
	journalReceipts(t, saved, agentsession.NoticeOutcomeCompleted)
	data, err = os.ReadFile(path)
	if err != nil || string(data) != want {
		t.Fatal("resume changed file")
	}
	checkpoint, err := json.Marshal(saved.record)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(checkpoint), strings.Repeat("large-file-padding\\n", 10000)) {
		t.Fatal("full file entered recovery record")
	}
}

type largeEditBudgetModel struct {
	hash    string
	calls   int
	results []map[string]any
}

func (m *largeEditBudgetModel) Complete(_ context.Context, request modelclient.Request) (modelclient.Response, error) {
	if isTitleRequest(request) {
		return modelclient.Response{Message: modelclient.Message{Role: "assistant", Content: `{"title":"edit"}`}}, nil
	}
	last := request.Messages[len(request.Messages)-1]
	if last.Role == "user" {
		m.calls++
		call := journalCall(fmt.Sprintf("edit-%d", m.calls), "edit", map[string]any{"path": "file", "expected_hash": m.hash, "edits": []map[string]string{{"old_text": "OLD", "new_text": "NEW"}}})
		return modelclient.Response{Message: modelclient.Message{Role: "assistant", ToolCalls: []modelclient.ToolCall{call}}}, nil
	}
	if last.Role == "tool" {
		var result map[string]any
		if err := json.Unmarshal([]byte(last.Content), &result); err != nil {
			return modelclient.Response{}, err
		}
		m.results = append(m.results, result)
	}
	return modelclient.Response{Message: modelclient.Message{Role: "assistant", Content: "checked"}}, nil
}

func TestLargeFileEditCurrentBudgetSurvivesSessionSwitch(t *testing.T) {
	f := outputFixture(t, false)
	original := strings.Repeat("x", 1500) + "\nOLD\n"
	if err := os.WriteFile(filepath.Join(f.workspace, "file"), []byte(original), 0600); err != nil {
		t.Fatal(err)
	}
	owner := f.controller.SessionID()
	f.controller.mu.Lock()
	f.controller.loopOptions.WorkspaceEditFileBytes = 4096
	f.controller.mu.Unlock()
	if err := f.controller.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte(original))
	hash := "sha256:" + hex.EncodeToString(digest[:])
	model := &largeEditBudgetModel{hash: hash}
	deps := controllerDependencies(f.openStore(t), model, f.server, f.workspace, f.controller.provider)
	deps.LoopOptions.ContextWindow = 32768
	deps.LoopOptions.WorkspaceEditFileBytes = 128
	c, err := Resume(t.Context(), deps, ResumeOptions{SessionID: owner, CurrentWorkspace: f.workspace})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	check := func() {
		t.Helper()
		if result, err := c.Send(t.Context(), "edit"); err != nil || result.PendingFileMutation != nil {
			t.Fatalf("runtime bypassed edit budget: %+v %v", result, err)
		}
		if len(model.results) == 0 || model.results[len(model.results)-1]["error"] != workspace.CodeFileTooLarge {
			t.Fatal("runtime did not enforce budget")
		}
	}
	check()
	if _, err := c.NewSession(t.Context()); err != nil {
		t.Fatal(err)
	}
	summary := leaseSummary(t, c, c.SessionID())
	if _, err := c.RenameSession(t.Context(), summary.SessionID, "edit new", summary.RecordRevision); err != nil {
		t.Fatal(err)
	}
	check()
	if _, err := c.CommitSwitch(t.Context(), c.PlanSwitch(leaseSummary(t, c, owner)), SwitchConfirmation{}); err != nil {
		t.Fatal(err)
	}
	check()
	data, err := os.ReadFile(filepath.Join(f.workspace, "file"))
	if err != nil || string(data) != original {
		t.Fatal("budget rejection changed file")
	}
	if !strings.Contains(string(c.record.Checkpoint), workspace.CodeFileTooLarge) {
		t.Fatal("budget rejection not retained")
	}
}
