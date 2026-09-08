//go:build darwin

package securefile

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

func purgeEntryOpenFlags(kind EntryType) int {
	if kind == EntryDirectory {
		return unix.O_RDONLY | unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_CLOEXEC
	}
	if kind == EntryLink {
		// O_SYMLINK opens the link itself. Combining O_NOFOLLOW with it
		// rejects the link with ELOOP on Darwin. Ancestors remain no-follow.
		return unix.O_EVTONLY | unix.O_SYMLINK | unix.O_CLOEXEC
	}
	return unix.O_EVTONLY | unix.O_NOFOLLOW | unix.O_CLOEXEC
}

func purgeNativeMount(file *os.File) (string, error) {
	var stat unix.Statfs_t
	if err := unix.Fstatfs(int(file.Fd()), &stat); err != nil {
		return "", errors.Join(ErrArchiveUnsupported, err)
	}
	if stat.Fsid.Val[0] == 0 && stat.Fsid.Val[1] == 0 {
		return "", ErrArchiveUnsupported
	}
	// The native filesystem ID and mount location distinguish mounted views;
	// this is not a fallback to the device field of stat(2).
	return fmt.Sprintf("darwin-mount:%x:%x:%x", stat.Fsid.Val[0], stat.Fsid.Val[1], stat.Mntonname), nil
}
