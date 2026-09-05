//go:build linux

package securefile

import "golang.org/x/sys/unix"

func renameArchiveNoReplace(sourceFD int, source string, destinationFD int, destination string) error {
	return unix.Renameat2(sourceFD, source, destinationFD, destination, unix.RENAME_NOREPLACE)
}
