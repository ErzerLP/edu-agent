//go:build darwin

package terminal

import (
	"os"

	"golang.org/x/sys/unix"
)

func nativeEchoDisabled(file *os.File) (bool, error) {
	state, err := unix.IoctlGetTermios(int(file.Fd()), unix.TIOCGETA)
	if err != nil {
		return false, err
	}
	return state.Lflag&unix.ECHO == 0, nil
}
