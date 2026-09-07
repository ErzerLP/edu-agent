package agentcontroller

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/agentsession"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/api"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/localexec"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/modelclient"
)

const outputMarker = "OUTPUT_TAIL_6af730"
const outputCommand = "printf prefix; awk 'BEGIN { for(i=0;i<6000;i++) printf \"output-row\\n\" }'; printf " + outputMarker + "; printf diagnostic_marker >&2"

type outputTestModel struct{ calls int }

func (m *outputTestModel) Complete(_ context.Context, request modelclient.Request) (modelclient.Response, error) {
	m.calls++
	if isTitleRequest(request) {
		return modelclient.Response{Message: modelclient.Message{Role: "assistant", Content: `{"title":"output"}`}}, nil
	}
	last := request.Messages[len(request.Messages)-1]
	if last.Role == "user" && last.Content == "emit" {
		args, _ := json.Marshal(map[string]any{"command": outputCommand, "wait_ms": 1000})
		return modelclient.Response{Message: modelclient.Message{Role: "assistant", ToolCalls: []modelclient.ToolCall{{ID: "output-call", Type: "function", Function: modelclient.ToolFunction{Name: "shell", Arguments: string(args)}}}}}, nil
	}
	return modelclient.Response{Message: modelclient.Message{Role: "assistant", Content: "done"}}, nil
}

func outputFixture(t *testing.T, noSave bool) *leaseFixture {
	t.Helper()
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("requires native local shell")
	}
	t.Setenv("SHELL", "/bin/sh")
	f := &leaseFixture{root: t.TempDir(), workspace: t.TempDir(), secrets: &controllerSecretBackend{}, manager: localexec.New(localexec.Options{OutputBytesPerTask: 64, OutputBytesTotal: 128, StopGrace: 40 * time.Millisecond})}
	f.server = &leaseTestServer{controllerServer: controllerServer{generation: api.MemoryGenerationStamp{LearnerGeneration: 1, MemoryGeneration: 1}}}
	var store *agentsession.Store
	if !noSave {
		store = f.openStore(t)
	}
	dependencies := controllerDependencies(store, &outputTestModel{}, f.server, f.workspace, Provider{Name: "local", Endpoint: "http://localhost:11434/v1", Model: "test"})
	dependencies.LoopOptions.LocalExec = f.manager
	dependencies.LoopOptions.ContextWindow = 32768
	var err error
	f.controller, err = Start(t.Context(), dependencies, noSave)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(f.controller.Close)
	if !noSave {
		summary := leaseSummary(t, f.controller, f.controller.SessionID())
		if _, err = f.controller.RenameSession(t.Context(), summary.SessionID, "output test", summary.RecordRevision); err != nil {
			t.Fatal(err)
		}
	}
	return f
}
func outputRun(t *testing.T, f *leaseFixture) localexec.Snapshot {
	t.Helper()
	if _, err := f.controller.Send(t.Context(), "emit"); err != nil {
		t.Fatal(err)
	}
	tasks, err := f.controller.LocalTasks()
	if err != nil || len(tasks) != 1 {
		t.Fatalf("tasks=%+v err=%v", tasks, err)
	}
	snapshot, err := f.manager.Wait(t.Context(), f.controller.localOwner, tasks[0].TaskID, 10*time.Second)
	if err != nil || snapshot.Controllable || snapshot.ExitCode == nil || *snapshot.ExitCode != 0 {
		t.Fatalf("task=%+v err=%v", snapshot, err)
	}
	return snapshot
}

func TestLocalOutputEncryptedResumeBeyondMemory(t *testing.T) {
	f := outputFixture(t, false)
	c := f.controller
	task := outputRun(t, f)
	want := "prefix" + strings.Repeat("output-row\n", 6000) + outputMarker
	if task.StdoutSaved != int64(len(want)) || task.Persistence != "saved" {
		t.Fatalf("retention=%+v", task)
	}
	checkpoint, err := c.loop.ExportCheckpoint()
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, []byte(outputMarker)) || bytes.Contains(encoded, []byte(outputCommand)) {
		t.Fatal("raw output/command entered checkpoint")
	}
	entries, err := os.ReadDir(f.root)
	if err != nil {
		t.Fatal(err)
	}
	artifacts := 0
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), "artifact-") {
			continue
		}
		artifacts++
		data, readErr := os.ReadFile(filepath.Join(f.root, entry.Name()))
		if readErr != nil {
			t.Fatal(readErr)
		}
		if bytes.Contains(data, []byte(outputMarker)) || bytes.Contains(data, []byte("output-row")) {
			t.Fatal("plaintext output on disk")
		}
	}
	if artifacts < 3 {
		t.Fatalf("missing independent metadata/streams: %d", artifacts)
	}
	owner := c.SessionID()
	if err := c.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
	freshModel := &outputTestModel{}
	manager := localexec.New(localexec.Options{OutputBytesPerTask: 64, OutputBytesTotal: 128})
	dependencies := controllerDependencies(f.openStore(t), freshModel, f.server, f.workspace, Provider{Name: "changed", Endpoint: "https://different.example/v1", Model: "test"})
	dependencies.LoopOptions.LocalExec = manager
	restored, err := Resume(t.Context(), dependencies, ResumeOptions{SessionID: owner, CurrentWorkspace: f.workspace})
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	if !restored.Status().ProviderConfirmationRequired || freshModel.calls != 0 {
		t.Fatal("historical output triggered provider before consent")
	}
	tasks, err := restored.LocalTasks()
	if err != nil || len(tasks) != 1 || tasks[0].TaskID != task.TaskID || !tasks[0].Restored || tasks[0].Controllable || tasks[0].State != localexec.StateExited {
		t.Fatalf("restored=%+v err=%v", tasks, err)
	}
	var output []byte
	for offset := int64(0); offset < int64(len(want)); {
		page, err := restored.ReadLocalTask(task.TaskID, "stdout", offset, 4096)
		if err != nil || page.NextOffset <= offset || !page.Historical || page.Availability != "saved" || page.Saved != int64(len(want)) {
			t.Fatalf("page=%+v err=%v", page, err)
		}
		output = append(output, page.Data...)
		offset = page.NextOffset
	}
	if string(output) != want {
		t.Fatalf("output mismatch: got %d want %d", len(output), len(want))
	}
	found, err := restored.SearchLocalTask(t.Context(), task.TaskID, "stdout", outputMarker, 0, 1)
	if err != nil || len(found.Offsets) != 1 || found.Offsets[0] != int64(len(want)-len(outputMarker)) {
		t.Fatalf("search=%+v err=%v", found, err)
	}
	stderr, err := restored.ReadLocalTask(task.TaskID, "stderr", 0, 4096)
	if err != nil || string(stderr.Data) != "diagnostic_marker" || freshModel.calls != 0 {
		t.Fatalf("stderr=%+v err=%v", stderr, err)
	}
}

func TestLocalOutputNoSaveCreatesNoArtifacts(t *testing.T) {
	f := outputFixture(t, true)
	task := outputRun(t, f)
	if task.Persistence != "memory_only" || task.StdoutSaved != 0 || task.StdoutRetained > 64 {
		t.Fatalf("no-save retention=%+v", task)
	}
	if err := f.controller.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(f.root)
	if err != nil || len(entries) != 0 {
		t.Fatalf("no-save created files: %v err=%v", entries, err)
	}
}

func TestLocalOutputUncommittedArchiveSurvivesResumeClose(t *testing.T) {
	f := outputFixture(t, false)
	c := f.controller
	owner := c.SessionID()
	summary := leaseSummary(t, c, owner)
	if _, err := c.RenameSession(t.Context(), owner, "", summary.RecordRevision); err != nil {
		t.Fatal(err)
	}
	marker, err := c.handle.MarkDirty(t.Context(), c.record.RecordRevision, 1, "agent", false)
	if err != nil {
		t.Fatal(err)
	}
	marker.MayHaveSideEffect = true
	marker.LocalEffects = []agentsession.LocalEffectIntent{{ToolCallID: "crash-call", Operation: "shell"}}
	if _, err = c.handle.UpdateDirty(t.Context(), marker); err != nil {
		t.Fatal(err)
	}
	task, err := f.manager.Start(t.Context(), owner, "crash-call", localexec.StartArgs{CWD: f.workspace, Command: "printf preserved-before-checkpoint", Shell: "/bin/sh"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.manager.Wait(t.Context(), owner, task.TaskID, time.Second); err != nil {
		t.Fatal(err)
	}
	// Simulate loss before the turn's checkpoint: close resources without Save.
	c.abort()
	manager := localexec.New(localexec.Options{})
	dependencies := controllerDependencies(f.openStore(t), &outputTestModel{}, f.server, f.workspace, c.provider)
	dependencies.LoopOptions.LocalExec = manager
	restored, err := Resume(t.Context(), dependencies, ResumeOptions{SessionID: owner, CurrentWorkspace: f.workspace})
	if err != nil {
		t.Fatal(err)
	}
	if restored.record.CommittedUserTurns != 0 || restored.record.TitleSource == "manual" {
		t.Fatal("fixture is not a zero-committed-turn Session")
	}
	if err = restored.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
	store := f.openStore(t)
	defer store.Close()
	handle, _, err := store.OpenSession(t.Context(), owner)
	if err != nil {
		t.Fatalf("task output was automatically deleted as empty: %v", err)
	}
	defer handle.Close()
	names, err := handle.ListArtifacts(t.Context(), "task_")
	if err != nil || len(names) != 1 {
		t.Fatalf("retained metadata=%v err=%v", names, err)
	}
}

func TestLocalOutputClearRevokesPersistedBytes(t *testing.T) {
	f := outputFixture(t, false)
	task := outputRun(t, f)
	external := f.openStore(t)
	defer external.Close()
	if err := external.Clear(t.Context()); err != nil {
		t.Fatal(err)
	}
	for _, check := range []struct {
		name   string
		offset int64
	}{
		{"memory_prefix", 0}, {"eof", task.StdoutBytes},
		{"saved_segment", 1024}, {"memory_after_saved_failure", 0},
	} {
		t.Run(check.name, func(t *testing.T) {
			page, err := f.controller.ReadLocalTask(task.TaskID, "stdout", check.offset, 32)
			var unavailable *localexec.Error
			if !errors.As(err, &unavailable) || unavailable.Code != "output_unavailable" || len(page.Data) != 0 {
				t.Fatalf("old generation remained readable: page=%+v err=%v", page, err)
			}
			found, err := f.controller.SearchLocalTask(t.Context(), task.TaskID, "stdout", "prefix", check.offset, 1)
			if !errors.As(err, &unavailable) || unavailable.Code != "output_unavailable" || len(found.Offsets) != 0 {
				t.Fatalf("old generation remained searchable: page=%+v err=%v", found, err)
			}
		})
	}
	if err := f.manager.BindArchive(f.controller.localOwner, nil); err != nil {
		t.Fatal(err)
	}
	if page, err := f.controller.ReadLocalTask(task.TaskID, "stdout", 0, 32); err == nil || len(page.Data) != 0 {
		t.Fatalf("storage detach revived revoked output: page=%+v err=%v", page, err)
	}
}
