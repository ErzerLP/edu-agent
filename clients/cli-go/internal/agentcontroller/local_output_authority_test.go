package agentcontroller

import (
	"errors"
	"testing"
	"time"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/localexec"
)

func TestLocalOutputNewGenerationNeverReusesOwner(t *testing.T) {
	f := outputFixture(t, false)
	old := outputRun(t, f)
	oldOwner := f.controller.localOwner
	external := f.openStore(t)
	defer external.Close()
	if err := external.Clear(t.Context()); err != nil {
		t.Fatal(err)
	}
	dependencies := controllerDependencies(f.openStore(t), &outputTestModel{}, f.server, f.workspace, f.controller.provider)
	dependencies.LoopOptions.LocalExec = f.manager
	// Even a stale internal option cannot override the new persisted identity.
	dependencies.LoopOptions.LocalExecOwner = oldOwner
	dependencies.LoopOptions.ContextWindow = 32768
	current, err := Start(t.Context(), dependencies, false)
	if err != nil {
		t.Fatal(err)
	}
	defer current.Close()
	if current.localOwner == oldOwner || current.localOwner != current.record.SessionID {
		t.Fatalf("new generation reused an old task owner: %q", current.localOwner)
	}
	if _, err := current.Send(t.Context(), "emit"); err != nil {
		t.Fatal(err)
	}
	// A tool's wait budget can expire before its archive has finished writing.
	// Retire the new owner's writer before checking the old authority's fence.
	tasks, err := current.LocalTasks()
	if err != nil || len(tasks) != 1 {
		t.Fatalf("new owner tasks=%+v err=%v", tasks, err)
	}
	if task, err := f.manager.Wait(t.Context(), current.localOwner, tasks[0].TaskID, 10*time.Second); err != nil || localTaskUnsettled(task) {
		t.Fatalf("new owner output not settled: %+v err=%v", task, err)
	}
	var unavailable *localexec.Error
	if page, err := f.controller.ReadLocalTask(old.TaskID, "stdout", 0, 32); !errors.As(err, &unavailable) || unavailable.Code != "output_unavailable" || len(page.Data) != 0 {
		t.Fatalf("new Session authority revived old cache: %+v %v", page, err)
	}
	if page, err := current.ReadLocalTask(old.TaskID, "stdout", 0, 32); err == nil || len(page.Data) != 0 {
		t.Fatalf("new owner acquired old task output: %+v %v", page, err)
	}
}
