//go:build linux

package localexec

import (
	"errors"
	"os"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

func getTerminalAttributes(fd int) (*unix.Termios, error) {
	return unix.IoctlGetTermios(fd, unix.TCGETS)
}

const disabledTerminalByte = 0 // Linux _POSIX_VDISABLE.

// Linux reports last-slave-close as EIO; explicit close/deadline is not EOF.
func terminalEOF(err error) bool { return errors.Is(err, unix.EIO) }

func observeExit(pid int) error {
	var info unix.Siginfo
	for {
		err := unix.Waitid(unix.P_PID, pid, &info, unix.WEXITED|unix.WNOWAIT, nil)
		if !errors.Is(err, unix.EINTR) {
			return err
		}
	}
}

// Only called while an unreaped leader pins this active task's PGID. /proc
// enumeration distinguishes that anchor from descendants without guessing from
// pipe EOF. Vanished processes are normal; inaccessible entries make the result
// conservative. Final cleanup still requires killpg(0) == ESRCH, including zombies.
func groupHasLiveMembers(pgid int) (bool, error) {
	return procHasMembers(pgid, false, true)
}

// Session scans are used only while the unreaped session leader pins its SID.
// Checking all pgrps catches job-control children outside the shell's root pgrp.
func sessionHasMembers(sid int, liveOnly bool) (bool, error) {
	return procHasMembers(sid, true, liveOnly)
}

func procHasMembers(id int, session, liveOnly bool) (bool, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return false, err
	}
	var uncertain error
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid == id {
			continue
		}
		data, err := os.ReadFile("/proc/" + entry.Name() + "/stat")
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			uncertain = err
			continue
		}
		// comm can contain spaces, parentheses, and newlines. Fields after its
		// LAST ')' start with state, ppid, pgrp; Fields(data) alone is incorrect.
		end := strings.LastIndexByte(string(data), ')')
		if end < 0 {
			uncertain = unix.EIO
			continue
		}
		fields := strings.Fields(string(data[end+1:]))
		if len(fields) < 4 {
			uncertain = unix.EIO
			continue
		}
		field := 2
		if session {
			field = 3
		}
		group, err := strconv.Atoi(fields[field])
		if err != nil {
			uncertain = err
			continue
		}
		if group == id && (!liveOnly || fields[0] != "Z" && fields[0] != "X") {
			return true, nil
		}
	}
	return false, uncertain
}
