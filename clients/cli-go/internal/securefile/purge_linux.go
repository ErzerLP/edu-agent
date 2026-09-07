//go:build linux

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
	// O_PATH pins even an unreadable file or symlink itself, never its target.
	return unix.O_PATH | unix.O_NOFOLLOW | unix.O_CLOEXEC
}

func purgeNativeMount(file *os.File) (string, error) {
	var stat unix.Statx_t
	if err := unix.Statx(int(file.Fd()), "", unix.AT_EMPTY_PATH|unix.AT_SYMLINK_NOFOLLOW, unix.STATX_MNT_ID, &stat); err != nil {
		return "", errors.Join(ErrArchiveUnsupported, err)
	}
	// st_dev is insufficient: a same-device bind mount must also be rejected.
	if stat.Mask&unix.STATX_MNT_ID == 0 || stat.Mnt_id == 0 {
		return "", ErrArchiveUnsupported
	}
	return fmt.Sprintf("linux-mount:%x", stat.Mnt_id), nil
}
