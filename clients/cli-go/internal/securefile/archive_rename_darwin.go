//go:build darwin

package securefile

import "golang.org/x/sys/unix"

func renameArchiveNoReplace(sourceFD int, source string, destinationFD int, destination string) error {
	return unix.RenameatxNp(sourceFD, source, destinationFD, destination, unix.RENAME_EXCL)
}
