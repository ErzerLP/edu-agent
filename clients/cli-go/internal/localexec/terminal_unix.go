//go:build linux || darwin

package localexec

import (
	"errors"
	"os"
	"os/exec"
	"syscall"

	"github.com/creack/pty"
	"golang.org/x/sys/unix"
)

// withTerminalFD pins the os.File for the whole ioctl. Never use Fd followed by
// an ioctl: Close could recycle that number, and Fd also disables nonblocking IO.
func withTerminalFD(file *os.File, action func(int) error) error {
	raw, err := file.SyscallConn()
	if err != nil {
		return err
	}
	var actionErr error
	if err := raw.Control(func(fd uintptr) { actionErr = action(int(fd)) }); err != nil {
		return err
	}
	return actionErr
}

// creack/pty owns portable PTY allocation. Its master is replaced by independently
// closable, CLOEXEC duplicates made nonblocking BEFORE os.NewFile registers them
// with Go's poller. Only the child slave is used by exec (and remains blocking).
func openPTY(cmd *exec.Cmd, rows, cols int) (*processPipes, error) {
	master, slave, err := pty.Open()
	if err != nil {
		return nil, err
	}
	defer master.Close()
	p := &processPipes{stdinReader: slave}
	dup := func() (*os.File, error) {
		fd := -1
		err := withTerminalFD(master, func(source int) error {
			var err error
			fd, err = unix.FcntlInt(uintptr(source), unix.F_DUPFD_CLOEXEC, 0)
			if err != nil {
				return err
			}
			return unix.SetNonblock(fd, true)
		})
		if err != nil {
			if fd >= 0 {
				_ = unix.Close(fd)
			}
			return nil, err
		}
		return os.NewFile(uintptr(fd), "localexec-pty"), nil
	}
	p.stdoutReader, err = dup()
	if err == nil {
		p.stdinWriter, err = dup()
	}
	if err == nil {
		err = resizeTerminal(p.stdoutReader, rows, cols)
	}
	if err != nil {
		p.closeAll()
		return nil, err
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	return p, nil
}

func resizeTerminal(file *os.File, rows, cols int) error {
	return withTerminalFD(file, func(fd int) error {
		return unix.IoctlSetWinsize(fd, unix.TIOCSWINSZ, &unix.Winsize{Row: uint16(rows), Col: uint16(cols)})
	})
}

func terminalControlByte(file *os.File, control string) (byte, error) {
	var value byte
	err := withTerminalFD(file, func(fd int) error {
		termios, err := getTerminalAttributes(fd)
		if err != nil {
			return failure("pty_control_failed")
		}
		index := unix.VINTR
		if control == "eof" {
			index = unix.VEOF
		}
		value = termios.Cc[index]
		if value == disabledTerminalByte {
			return failure("pty_control_disabled")
		}
		return nil
	})
	if err != nil {
		var stable *Error
		if errors.As(err, &stable) {
			return 0, err
		}
		return 0, failure("pty_not_active")
	}
	return value, nil
}
