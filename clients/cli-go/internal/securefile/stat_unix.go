//go:build linux || darwin

package securefile

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

func unixEntryInfo(stat unix.Stat_t) EntryInfo {
	kind := unixEntryType(uint32(stat.Mode))
	identity := fmt.Sprintf("unix:%x:%x", uint64(stat.Dev), uint64(stat.Ino))
	version := archiveMetadataVersion(fmt.Sprintf("%s|%s|%d|%d|%d:%d|%d:%d|%d|%d|%d",
		identity, kind, stat.Size, stat.Mode, stat.Mtim.Sec, stat.Mtim.Nsec,
		stat.Ctim.Sec, stat.Ctim.Nsec, stat.Nlink, stat.Uid, stat.Gid))
	return EntryInfo{Kind: kind, Size: stat.Size, ModTime: time.Unix(stat.Mtim.Sec, stat.Mtim.Nsec).UTC(), Identity: identity, Version: version}
}

func openUnixStatParent(ctx context.Context, root *Root, components []string) (*os.File, error) {
	parent, err := openDirectoryWithinRoot(root, nil)
	if err != nil {
		return nil, archiveUnixError(err)
	}
	for _, component := range components {
		if err := ctx.Err(); err != nil {
			_ = parent.Close()
			return nil, err
		}
		if err := verifyUnixArchiveLocation(root.file, parent, nil); err != nil {
			_ = parent.Close()
			return nil, err
		}
		next, openErr := openUnixArchiveDirectory(parent, component)
		// Even missing paths must not be reported from a relocated parent.
		verifyErr := verifyUnixArchiveLocation(root.file, parent, nil)
		closeErr := parent.Close()
		if err := errors.Join(verifyErr, openErr, closeErr); err != nil {
			if next != nil {
				_ = next.Close()
			}
			// A failed confinement check takes precedence over ENOENT.
			if verifyErr != nil {
				return nil, verifyErr
			}
			if closeErr != nil {
				return nil, closeErr
			}
			return nil, openErr
		}
		parent = next
	}
	if err := verifyUnixArchiveLocation(root.file, parent, nil); err != nil {
		_ = parent.Close()
		return nil, err
	}
	return parent, nil
}

func statWithinRoot(ctx context.Context, root *Root, components []string) (entry EntryInfo, err error) {
	if len(components) == 0 {
		var stat unix.Stat_t
		if err := unix.Fstat(int(root.file.Fd()), &stat); err != nil {
			return entry, archiveUnixError(err)
		}
		return unixEntryInfo(stat), ctx.Err()
	}
	parent, err := openUnixStatParent(ctx, root, components[:len(components)-1])
	if err != nil {
		return entry, err
	}
	defer func() {
		if closeErr := parent.Close(); closeErr != nil {
			err = closeErr
		}
	}()
	stat, statErr := unixArchiveStat(parent, components[len(components)-1])
	if err := verifyUnixArchiveLocation(root.file, parent, nil); err != nil {
		return entry, err
	}
	if err := ctx.Err(); err != nil {
		return entry, err
	}
	if statErr != nil {
		return entry, statErr
	}
	return unixEntryInfo(stat), nil
}

func hashEntryWithinRoot(ctx context.Context, root *Root, components []string, expected EntryInfo, limit int64) (hash string, err error) {
	parent, err := openUnixStatParent(ctx, root, components[:len(components)-1])
	if err != nil {
		return "", err
	}
	defer func() { err = errors.Join(err, parent.Close()) }()
	if err := ctx.Err(); err != nil {
		return "", err
	}
	// O_NONBLOCK prevents an attacker replacing a regular file with a FIFO
	// between metadata inspection and open from blocking the tool.
	fd, err := unix.Openat(int(parent.Fd()), components[len(components)-1], unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return "", archiveUnixError(err)
	}
	file := os.NewFile(uintptr(fd), components[len(components)-1])
	defer func() { err = errors.Join(err, file.Close()) }()
	if err := verifyUnixArchiveLocation(root.file, parent, nil); err != nil {
		return "", err
	}
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return "", archiveUnixError(err)
	}
	before := unixEntryInfo(stat)
	if before.Kind != EntryFile {
		return "", ErrNotRegular
	}
	if before != expected {
		return "", ErrChanged
	}
	hash, err = readEntryHash(ctx, file, expected, limit)
	if err != nil {
		return "", err
	}
	if err := unix.Fstat(fd, &stat); err != nil {
		return "", archiveUnixError(err)
	}
	if unixEntryInfo(stat) != expected {
		return "", ErrChanged
	}
	after, err := statWithinRoot(ctx, root, components)
	if err != nil {
		return "", err
	}
	if after != expected {
		return "", ErrChanged
	}
	return hash, nil
}
