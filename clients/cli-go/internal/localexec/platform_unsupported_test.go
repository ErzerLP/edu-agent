//go:build !linux && !darwin

package localexec

import (
	"context"
	"errors"
	"testing"
)

func TestUnsupportedPlatformHasStableFailureIdentity(t *testing.T) {
	m := New(Options{})
	s, err := m.Start(context.Background(), "owner", "call", StartArgs{Command: "sensitive", CWD: "irrelevant"})
	var stable *Error
	if !errors.As(err, &stable) || stable.Code != "unsupported_platform" || s.TaskID == "" || s.State != StateFailed {
		t.Fatalf("unsupported: %+v %v", s, err)
	}
	again, err := m.Start(context.Background(), "owner", "call", StartArgs{})
	if !errors.As(err, &stable) || stable.Code != "unsupported_platform" || again.TaskID != s.TaskID {
		t.Fatalf("dedup: %+v %v", again, err)
	}
	if err := m.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}
