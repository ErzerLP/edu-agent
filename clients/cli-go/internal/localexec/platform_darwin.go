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

// Modern Darwin does not export e_sess kernel pointers through sysctl. Use
// numeric session IDs instead, while the unreaped leader still pins the SID.
// A failed lookup is only harmless if the process really disappeared.
func sessionHasMembers(sid int, liveOnly bool) (bool, error) {
	processes, err := unix.SysctlKinfoProcSlice("kern.proc.all")
	if err != nil {
		return false, err
	}
	anchored := false
	for _, process := range processes {
		if int(process.Proc.P_pid) == sid {
			anchored = true
			break
		}
	}
	if !anchored {
		return false, unix.ESRCH
	}
	var uncertain error
	for _, process := range processes {
		pid := int(process.Proc.P_pid)
		if pid <= 0 || pid == sid || liveOnly && process.Proc.P_stat == 5 {
			continue
		}
		// The anchored root group belongs to this session, including zombies.
		if int(process.Eproc.Pgid) == sid {
			return true, nil
		}
		session, err := unix.Getsid(pid)
		if errors.Is(err, unix.ESRCH) {
			remaining, checkErr := unix.SysctlKinfoProcSlice("kern.proc.pid", pid)
			if checkErr == nil && len(remaining) == 0 {
				continue
			}
			// Darwin getsid cannot reference a zombie. Root-group zombies
			// were counted above; a zombie elsewhere cannot be a live escaped
			// job and must not make every unrelated PTY session unclean.
			if checkErr == nil && len(remaining) == 1 && remaining[0].Proc.P_stat == 5 && int(remaining[0].Eproc.Pgid) != sid {
				continue
			}
		}
		if err != nil {
			uncertain = err
			continue
		}
		if session == sid {
			return true, nil
		}
	}
	return false, uncertain
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
