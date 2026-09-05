//go:build linux || darwin

package localexec

import (
	"errors"
	"os/exec"
	"syscall"

	"golang.org/x/sys/unix"
)

func platformSupported() bool             { return true }
func configureProcessGroup(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} }

func signalProcessGroup(pid int, force bool) error {
	signal := unix.SIGTERM
	if force {
		signal = unix.SIGKILL
	}
	err := unix.Kill(-pid, signal)
	if errors.Is(err, unix.ESRCH) {
		return nil
	}
	return err
}

func processGroupExists(pid int) (bool, error) {
	err := unix.Kill(-pid, 0)
	if errors.Is(err, unix.ESRCH) {
		return false, nil
	}
	if errors.Is(err, unix.EPERM) {
		return true, err
	}
	return err == nil, err
}
