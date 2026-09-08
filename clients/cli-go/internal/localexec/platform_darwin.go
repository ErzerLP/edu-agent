//go:build darwin

package localexec

import (
	"errors"
	"unsafe"

	"golang.org/x/sys/unix"
)

// x/sys has no Darwin Waitid wrapper. Darwin's waitid_nocancel accepts idtype
// P_PID=1 and a siginfo_t buffer (104 bytes on supported 64-bit Darwin ABIs).
// Reserve an aligned, larger buffer; we only need WNOWAIT notification, not the
// platform-specific siginfo layout. The process is reaped later by exec.Cmd.Wait.
func observeExit(pid int) error {
	var info [16]uint64
	for {
		_, _, errno := unix.Syscall6(unix.SYS_WAITID_NOCANCEL, 1, uintptr(pid), uintptr(unsafe.Pointer(&info[0])), unix.WEXITED|unix.WNOWAIT, 0, 0)
		if errno == 0 {
			return nil
		}
		if !errors.Is(errno, unix.EINTR) {
			return errno
		}
	}
}

func getTerminalAttributes(fd int) (*unix.Termios, error) {
	return unix.IoctlGetTermios(fd, unix.TIOCGETA)
}

const disabledTerminalByte = 0xff // Darwin _POSIX_VDISABLE.

// Darwin returns ordinary EOF on terminal hangup; other errors stay incomplete.
func terminalEOF(error) bool { return false }

// All entries from one sysctl snapshot share the same session-pointer identity.
// The unreaped leader must still be present; otherwise cleanup is unprovable.
func sessionHasMembers(sid int, liveOnly bool) (bool, error) {
	processes, err := unix.SysctlKinfoProcSlice("kern.proc.all")
	if err != nil {
		return false, err
	}
	var session uintptr
	for _, process := range processes {
		if int(process.Proc.P_pid) == sid {
			session = process.Eproc.Sess
			break
		}
	}
	if session == 0 {
		return false, unix.ESRCH
	}
	for _, process := range processes {
		if int(process.Proc.P_pid) != sid && process.Eproc.Sess == session && (!liveOnly || process.Proc.P_stat != 5) {
			return true, nil
		}
	}
	return false, nil
}

func groupHasLiveMembers(pgid int) (bool, error) {
	processes, err := unix.SysctlKinfoProcSlice("kern.proc.pgrp", pgid)
	if err != nil {
		return false, err
	}
	for _, process := range processes {
		// Darwin SZOMB=5. Zombies cannot be signaled away and are accounted for
		// by the final group-existence check, not mistaken for running children.
		if int(process.Proc.P_pid) != pgid && int(process.Eproc.Pgid) == pgid && process.Proc.P_stat != 5 {
			return true, nil
		}
	}
	return false, nil
}
