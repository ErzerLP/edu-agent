//go:build linux || darwin

package localexec

import (
	"context"
	"errors"
	"testing"
	"time"
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
