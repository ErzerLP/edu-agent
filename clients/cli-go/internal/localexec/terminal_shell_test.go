package localexec

import (
	"bytes"
	"context"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestPTYInteractiveShellRetainsStateOnlyWithinTask(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("native PTY required")
	}
	manager := New(Options{StopGrace: 30 * time.Millisecond})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := manager.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	cwd, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	destination, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	args := StartArgs{Command: "exec /bin/sh -i", Shell: "/bin/sh", CWD: cwd, PTY: true, Env: map[string]*string{"ENV": nil, "EDU_PTY_STATE_0127": nil}}
	task, err := manager.Start(t.Context(), "owner", "interactive-state", args)
	if err != nil {
		t.Fatal(err)
	}
	first := "export EDU_PTY_STATE_0127=retained; cd '" + strings.ReplaceAll(destination, "'", "'\\''") + "'\n"
	second := "printf 'STATE:%s:%s\\n' \"$EDU_PTY_STATE_0127\" \"$PWD\"; exit\n"
	for _, text := range []string{first, second} {
		result, err := manager.WriteInput(t.Context(), "owner", task.TaskID, []byte(text))
		if err != nil || result.Written != len(text) {
			t.Fatalf("input=%+v err=%v", result, err)
		}
	}
	final, err := manager.Wait(t.Context(), "owner", task.TaskID, 2*time.Second)
	if err != nil || final.ExitCode == nil || *final.ExitCode != 0 || final.Controllable {
		t.Fatalf("final=%+v err=%v", final, err)
	}
	page, err := manager.Read("owner", task.TaskID, "stdout", 0, 4096)
	if err != nil || !bytes.Contains(page.Data, []byte("STATE:retained:"+destination)) {
		t.Fatalf("output=%q err=%v", page.Data, err)
	}
	args.Command = "printf 'NEW:%s:%s' \"${EDU_PTY_STATE_0127-unset}\" \"$PWD\""
	fresh, err := manager.Start(t.Context(), "owner", "fresh-state", args)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = manager.Wait(t.Context(), "owner", fresh.TaskID, 2*time.Second); err != nil {
		t.Fatal(err)
	}
	page, err = manager.Read("owner", fresh.TaskID, "stdout", 0, 4096)
	if err != nil || !bytes.Contains(page.Data, []byte("NEW:unset:"+cwd)) {
		t.Fatalf("state leaked across tasks: %q err=%v", page.Data, err)
	}
}
