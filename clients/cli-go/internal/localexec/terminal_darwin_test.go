//go:build darwin

package localexec

import "golang.org/x/sys/unix"

func setPTYTestAttributes(fd int, attributes *unix.Termios) error {
	return unix.IoctlSetTermios(fd, unix.TIOCSETA, attributes)
}
