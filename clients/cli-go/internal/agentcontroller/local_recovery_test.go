package agentcontroller

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/agentsession"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/localexec"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/modelclient"
)

type unavailableOutputWriter struct{ localOutputStore }

func (unavailableOutputWriter) WriteArtifact(context.Context, string, []byte) error {
	return &localexec.Error{Code: "output_save_failed"}
}

type replayLocalCallModel struct{}

func (m *replayLocalCallModel) Complete(_ context.Context, request modelclient.Request) (modelclient.Response, error) {
	if isTitleRequest(request) {
		return modelclient.Response{Message: modelclient.Message{Role: "assistant", Content: `{"title":"恢复核查"}`}}, nil
	}
	last := request.Messages[len(request.Messages)-1]
	if last.Role == "user" {
		id := "lost-first-call"
		if last.Content == "fresh" {
			id = "fresh-call"
		}
		return modelclient.Response{Message: modelclient.Message{Role: "assistant", ToolCalls: []modelclient.ToolCall{{ID: id, Type: "function", Function: modelclient.ToolFunction{Name: "shell", Arguments: `{"command":"printf x >> replay-count","wait_ms":1000}`}}}}}, nil
	}
	return modelclient.Response{Message: modelclient.Message{Role: "assistant", Content: "done"}}, nil
}

func TestLocalRecoveryWALOnlyCallIdentityPreventsReplay(t *testing.T) {
	f := outputFixture(t, false)
	c, owner := f.controller, f.controller.SessionID()
	summary := leaseSummary(t, c, owner)
	if _, err := c.RenameSession(t.Context(), owner, "", summary.RecordRevision); err != nil {
		t.Fatal(err)
	}
	marker, err := c.handle.MarkDirty(t.Context(), c.record.RecordRevision, 1, "agent", false)
	if err != nil {
		t.Fatal(err)
	}
	marker.MayHaveSideEffect = true
	marker.LocalEffects = []agentsession.LocalEffectIntent{{ToolCallID: "lost-first-call", Operation: "shell"}}
	if _, err = c.handle.UpdateDirty(t.Context(), marker); err != nil {
		t.Fatal(err)
	}
	if err = f.manager.BindArchive(owner, unavailableOutputWriter{localOutputStore{handle: c.handle}}); err != nil {
		t.Fatal(err)
	}
	task, err := f.manager.Start(t.Context(), owner, "lost-first-call", localexec.StartArgs{Command: "printf x >> replay-count", Shell: "/bin/sh", CWD: f.workspace})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.manager.Wait(t.Context(), owner, task.TaskID, time.Second); err != nil {
		t.Fatal(err)
	}
	c.abort() // Crash-equivalent: no stable turn checkpoint, only the task intent.
	manager := localexec.New(localexec.Options{})
	dependencies := controllerDependencies(f.openStore(t), &replayLocalCallModel{}, f.server, f.workspace, c.provider)
	dependencies.LoopOptions.LocalExec = manager
	dependencies.LoopOptions.ContextWindow = 32768
	restored, err := Resume(t.Context(), dependencies, ResumeOptions{SessionID: owner, CurrentWorkspace: f.workspace})
	if err != nil {
		t.Fatal(err)
	}
	// An immediate close must not delete this zero-committed-turn Session,
	// and a later restart must still remember the consumed WAL's identities.
	if err = restored.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
	dependencies.Store = f.openStore(t)
	dependencies.LoopOptions.LocalExec = localexec.New(localexec.Options{})
	restored, err = Resume(t.Context(), dependencies, ResumeOptions{SessionID: owner, CurrentWorkspace: f.workspace})
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	summary = leaseSummary(t, restored, owner)
	if _, err = restored.RenameSession(t.Context(), owner, "replay test", summary.RecordRevision); err != nil {
		t.Fatal(err)
	}
	_, _ = restored.Send(t.Context(), "continue")
	data, err := os.ReadFile(filepath.Join(f.workspace, "replay-count"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "x" {
		t.Fatalf("WAL-only call identity replayed the external effect: %q", data)
	}
	if _, err = restored.Send(t.Context(), "fresh"); err != nil {
		t.Fatal(err)
	}
	data, err = os.ReadFile(filepath.Join(f.workspace, "replay-count"))
	if err != nil || string(data) != "xx" {
		t.Fatalf("new call was incorrectly blocked: %q err=%v", data, err)
	}
}
