//go:build linux || darwin

package localexec

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDefaultShellSelectionAndExplicitFailure(t *testing.T) {
	m := testManager(t, Options{})
	cwd := t.TempDir()
	preferred := filepath.Join(cwd, "user-shell")
	if err := os.Symlink("/bin/sh", preferred); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SHELL", preferred)
	s, err := m.Start(context.Background(), "owner", "preferred", StartArgs{CWD: cwd, Command: "exit 0", Env: map[string]*string{"SHELL": nil}})
	if err != nil || s.Shell != preferred {
		t.Fatalf("preferred shell: %+v %v", s, err)
	}
	finish(t, m, "owner", s.TaskID)
	t.Setenv("SHELL", filepath.Join(cwd, "unavailable"))
	s, err = m.Start(context.Background(), "owner", "fallback", StartArgs{CWD: cwd, Command: "exit 0"})
	if err != nil || s.Shell != "/bin/sh" {
		t.Fatalf("fallback shell: %+v %v", s, err)
	}
	finish(t, m, "owner", s.TaskID)
	_, err = m.Start(context.Background(), "owner", "explicit", StartArgs{CWD: cwd, Shell: os.Getenv("SHELL"), Command: "exit 0"})
	requireCode(t, err, "shell_unavailable")
}

func TestOutputContinuesDrainingAfterQuota(t *testing.T) {
	m := testManager(t, Options{OutputBytesPerTask: 1024})
	s := launch(t, m, "owner", "drain", "dd if=/dev/zero bs=1024 count=256 2>/dev/null; printf tail >&2", false)
	s = finish(t, m, "owner", s.TaskID)
	if s.State != StateExited || s.ExitCode == nil || *s.ExitCode != 0 || s.StdoutBytes != 256<<10 || s.StderrBytes != 4 || s.StdoutRetained+s.StderrRetained != 1024 || s.OutputState != "truncated" {
		t.Fatalf("quota must not block or kill command: %+v", s)
	}
}

func TestCanceledStopAndCloseWaitStillPerformCleanup(t *testing.T) {
	for _, operation := range []string{"stop", "close"} {
		t.Run(operation, func(t *testing.T) {
			m := testManager(t, Options{StopGrace: 50 * time.Millisecond})
			s := launch(t, m, "owner", operation, "trap '' TERM; printf ready; while :; do :; done", false)
			awaitOutput(t, m, "owner", s.TaskID, "ready")
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			if operation == "stop" {
				_, err := m.Stop(ctx, "owner", s.TaskID)
				requireCode(t, err, "stop_wait_canceled")
			} else {
				err := m.Close(ctx)
				requireCode(t, err, "close_wait_canceled")
			}
			ended := finish(t, m, "owner", s.TaskID)
			if ended.State != StateCanceled || ended.CleanupIncomplete {
				t.Fatalf("canceled cleanup wait: %+v", ended)
			}
			ctx, cancel = context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if err := m.Close(ctx); err != nil {
				t.Fatal(err)
			}
		})
	}
}
