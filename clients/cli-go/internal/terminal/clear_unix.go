//go:build !windows

package terminal

import "io"

func clearScreen(out io.Writer) error {
	_, err := io.WriteString(out, "\x1b[2J\x1b[H> ")
	return err
}
