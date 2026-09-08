//go:build darwin

package localexec

import (
	"os/exec"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

func TestDarwinSessionMembershipUnreapedLeader(t *testing.T) {
	// A different unreaped session must not taint this session's cleanup.
	unrelated := exec.Command("/bin/sh", "-c", "exit 0")
	unrelated.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := unrelated.Start(); err != nil {
		t.Fatal(err)
	}
	defer unrelated.Wait()
	if err := observeExit(unrelated.Process.Pid); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("/bin/sh", "-c", "exit 0")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Wait()
	if err := observeExit(cmd.Process.Pid); err != nil {
		t.Fatal(err)
	}
	for _, liveOnly := range []bool{true, false} {
		members, err := sessionHasMembers(cmd.Process.Pid, liveOnly)
		if err != nil || members {
			t.Fatalf("empty anchored session (liveOnly=%v): members=%v err=%v", liveOnly, members, err)
		}
	}
}

func setPTYTestAttributes(fd int, attributes *unix.Termios) error {
	return unix.IoctlSetTermios(fd, unix.TIOCSETA, attributes)
}
