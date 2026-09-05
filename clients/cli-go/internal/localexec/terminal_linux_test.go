//go:build linux

package localexec

import "golang.org/x/sys/unix"

func setPTYTestAttributes(fd int, attributes *unix.Termios) error {
	return unix.IoctlSetTermios(fd, unix.TCSETS, attributes)
}
