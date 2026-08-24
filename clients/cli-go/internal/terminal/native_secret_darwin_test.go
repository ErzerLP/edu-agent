//go:build darwin

package terminal

import "os"

func nativeEchoDisabled(_ *os.File) (bool, error) {
	return false, nil
}
