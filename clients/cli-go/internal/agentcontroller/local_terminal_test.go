package agentcontroller

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/localexec"
)

func TestPTYLocalTerminalPortsAndEncryptedRecovery(t *testing.T) {
	f := outputFixture(t, false)
	c := f.controller
	owner := c.SessionID()
	task, err := f.manager.Start(t.Context(), owner, "direct-pty", localexec.StartArgs{Command: "stty -echo; printf READY; read input; stty size; printf END", CWD: f.workspace, Shell: "/bin/sh", PTY: true})
	if err != nil {
		t.Fatal(err)
	}
	ready := false
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		page, err := c.ReadLocalTask(task.TaskID, "stdout", 0, 64)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(page.Data, []byte("READY")) {
			ready = true
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !ready {
		t.Fatal("PTY did not reach echo-disabled input prompt")
	}
	if _, err = c.ResizeLocalTask(task.TaskID, 22, 91); err != nil {
		t.Fatal(err)
	}
	inputCtx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	content := []byte("DIRECT_INPUT_PRIVATE_907\n")
	input, err := c.InputLocalTask(inputCtx, task.TaskID, content)
	if err != nil || input.Written != len(content) || input.Outcome != "written" {
		t.Fatalf("input=%+v err=%v", input, err)
	}
	finished, err := f.manager.Wait(t.Context(), owner, task.TaskID, 2*time.Second)
	if err != nil || finished.State != "exited" || finished.ExitCode == nil || *finished.ExitCode != 0 {
		t.Fatalf("finished=%+v err=%v", finished, err)
	}
	page, err := c.ReadLocalTask(task.TaskID, "stdout", 0, 64)
	if err != nil || !bytes.Contains(page.Data, []byte("22 91")) || bytes.Contains(page.Data, content[:len(content)-1]) {
		t.Fatalf("output=%q err=%v", page.Data, err)
	}
	if c.model.(*outputTestModel).calls != 0 || c.dirty != nil {
		t.Fatal("human input entered model/tool intent queue")
	}
	checkpoint, err := c.loop.ExportCheckpoint()
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, content[:len(content)-1]) {
		t.Fatal("human input was saved as chat/source")
	}
	if err = c.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
	manager := localexec.New(localexec.Options{})
	dependencies := controllerDependencies(f.openStore(t), &outputTestModel{}, f.server, f.workspace, c.provider)
	dependencies.LoopOptions.LocalExec = manager
	restored, err := Resume(t.Context(), dependencies, ResumeOptions{SessionID: owner, CurrentWorkspace: f.workspace})
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	tasks, err := restored.LocalTasks()
	if err != nil || len(tasks) != 1 || !tasks[0].PTY || !tasks[0].Restored || tasks[0].Controllable || tasks[0].Rows != 22 || tasks[0].Cols != 91 {
		t.Fatalf("history=%+v err=%v", tasks, err)
	}
	page, err = restored.ReadLocalTask(task.TaskID, "stdout", 0, 64)
	if err != nil || !page.Historical || page.Availability != "saved" || !bytes.Contains(page.Data, []byte("22 91")) {
		t.Fatalf("restored page=%+v err=%v", page, err)
	}
	if _, err = restored.InputLocalTask(t.Context(), task.TaskID, []byte("do-not-send")); err == nil {
		t.Fatal("input accepted by history")
	}
	if _, err = restored.InterruptLocalTask(t.Context(), task.TaskID); err == nil {
		t.Fatal("interrupt accepted by history")
	}
	if _, err = restored.EOFLocalTask(t.Context(), task.TaskID); err == nil {
		t.Fatal("EOF accepted by history")
	}
	if _, err = restored.ResizeLocalTask(task.TaskID, 30, 100); err == nil {
		t.Fatal("resize accepted by history")
	}
}
