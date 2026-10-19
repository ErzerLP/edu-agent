//go:build darwin

package localexec

import (
	"os/exec"
	"syscall"
	"testing"
	"time"

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
	// The leader is known exited, but unrelated processes can retire between
	// the sysctl snapshot and getsid. Await a conclusive scan, just as cleanup
	// does; a reported member in this childless session is still a failure.
	deadline := time.NewTimer(4 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for _, liveOnly := range []bool{true, false} {
		for {
			members, err := sessionHasMembers(cmd.Process.Pid, liveOnly)
			if members {
				t.Fatalf("empty anchored session (liveOnly=%v): members=%v err=%v", liveOnly, members, err)
			}
			if err == nil {
				break
			}
			select {
			case <-deadline.C:
				t.Fatalf("session scan did not settle (liveOnly=%v): %v", liveOnly, err)
			case <-ticker.C:
			}
		}
	}
}

func setPTYTestAttributes(fd int, attributes *unix.Termios) error {
	return unix.IoctlSetTermios(fd, unix.TIOCSETA, attributes)
}
