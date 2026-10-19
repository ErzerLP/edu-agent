//go:build linux || darwin

package localexec

import (
	"context"
	"errors"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestStopSettlementOverridesSignalAttemptError(t *testing.T) {
	original := signalTaskGroup
	defer func() { signalTaskGroup = original }()
	signalTaskGroup = func(pid int, force bool) error {
		if err := original(pid, force); err != nil {
			return err
		}
		// A syscall outcome is not proof that the group remains alive. This
		// models a reported error followed by independently confirmed exit.
		return errors.New("signal attempt reported an error")
	}
	m := testManager(t, Options{})
	s := launch(t, m, "owner", "stop-error", "exec sleep 30", false)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	s, err := m.Stop(ctx, "owner", s.TaskID)
	if err != nil || s.State != StateCanceled || s.Controllable || s.CleanupIncomplete {
		t.Fatalf("settled process misreported: %+v err=%v", s, err)
	}
}

func TestPTYCleanupWaitsForSessionRetirement(t *testing.T) {
	for _, tc := range []struct {
		name       string
		members    bool
		probeErr   error
		persistent bool
	}{
		{name: "pending_reap", members: true},
		{name: "uncertain_probe", probeErr: unix.EAGAIN},
		{name: "remaining_member", members: true, persistent: true},
		{name: "unresolved_probe", probeErr: unix.EPERM, persistent: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			original := taskSessionHasMembers
			t.Cleanup(func() { taskSessionHasMembers = original })
			first := true
			taskSessionHasMembers = func(pid int, liveOnly bool) (bool, error) {
				if !liveOnly && (first || tc.persistent) {
					// Model the kernel's gap between exit observation and a
					// confirmed final session scan, without delaying the child.
					first = false
					return tc.members, tc.probeErr
				}
				return original(pid, liveOnly)
			}
			m := testManager(t, Options{})
			s, err := m.Start(t.Context(), "owner", "retirement", StartArgs{
				PTY: true, Shell: "/bin/sh", CWD: t.TempDir(), Command: "printf ready; read line",
			})
			if err != nil {
				t.Fatal(err)
			}
			awaitPTY(t, m, s.TaskID, "ready")
			ctx, cancel := context.WithTimeout(t.Context(), 4*time.Second)
			defer cancel()
			s, err = m.Stop(ctx, "owner", s.TaskID)
			if err != nil || s.State != StateCanceled || s.Controllable || s.CleanupIncomplete != tc.persistent {
				t.Fatalf("session retirement: %+v err=%v", s, err)
			}
		})
	}
}
