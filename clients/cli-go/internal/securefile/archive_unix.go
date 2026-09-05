//go:build linux || darwin

package securefile

import (
	"context"
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// Kept separate from publication primitives so fault tests cannot accidentally
// exercise an overwrite rename or a content-copy fallback.
var (
	archiveRenameUnix = renameArchiveNoReplace
	archiveSyncUnix   = unix.Fsync
)

func archiveUnixError(err error) error {
	switch {
	case errors.Is(err, unix.ENOENT):
		return ErrNotFound
	case errors.Is(err, unix.EXDEV):
		return ErrCrossDevice
	case errors.Is(err, unix.EEXIST), errors.Is(err, unix.ENOTEMPTY):
		return ErrAlreadyExists
	case errors.Is(err, unix.ENOSYS), errors.Is(err, unix.ENOTSUP), errors.Is(err, unix.EINVAL):
		return ErrArchiveUnsupported
	default:
		return classifyUnixOpenError(err)
	}
}

func unixArchiveStat(parent *os.File, name string) (unix.Stat_t, error) {
	var stat unix.Stat_t
	err := unix.Fstatat(int(parent.Fd()), name, &stat, unix.AT_SYMLINK_NOFOLLOW)
	return stat, archiveUnixError(err)
}

func unixArchiveEntry(stat unix.Stat_t) (ArchiveEntry, error) {
	kind := unixEntryType(uint32(stat.Mode))
	if kind == EntryLink {
		return ArchiveEntry{}, ErrLink
	}
	if kind != EntryFile && kind != EntryDirectory {
		return ArchiveEntry{}, ErrNotRegular
	}
	identity := fmt.Sprintf("unix:%x:%x", uint64(stat.Dev), uint64(stat.Ino))
	version := archiveMetadataVersion(fmt.Sprintf("%s|%s|%d|%d|%d:%d|%d:%d|%d|%d|%d",
		identity, kind, stat.Size, stat.Mode, stat.Mtim.Sec, stat.Mtim.Nsec,
		stat.Ctim.Sec, stat.Ctim.Nsec, stat.Nlink, stat.Uid, stat.Gid))
	return ArchiveEntry{Kind: kind, Size: stat.Size, Identity: identity, Version: version}, nil
}

func unixArchiveSame(stat unix.Stat_t, file *os.File) (bool, error) {
	if file == nil {
		return false, nil
	}
	var other unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &other); err != nil {
		return false, err
	}
	return stat.Dev == other.Dev && stat.Ino == other.Ino, nil
}

func openUnixArchiveDirectory(parent *os.File, name string) (*os.File, error) {
	stat, err := unixArchiveStat(parent, name)
	if err != nil {
		return nil, err
	}
	if unixEntryType(uint32(stat.Mode)) == EntryLink {
		return nil, ErrLink
	}
	if unixEntryType(uint32(stat.Mode)) != EntryDirectory {
		return nil, ErrNotDirectory
	}
	fd, err := unix.Openat(int(parent.Fd()), name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_DIRECTORY, 0)
	if err != nil {
		return nil, archiveUnixError(err)
	}
	return os.NewFile(uintptr(fd), name), nil
}

func openUnixArchiveGuard(root *Root) (*os.File, error) {
	guard, err := openUnixArchiveDirectory(root.file, ArchiveDirectory)
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	return guard, err
}

// Walk parent handles, never reconstructed absolute paths. The forbidden
// identity also catches filesystem aliases of the protected archive directory.
func verifyUnixArchiveLocation(root, parent, forbidden *os.File) error {
	current, err := openDirectoryWithinRoot(&Root{file: parent}, nil)
	if err != nil {
		return err
	}
	defer func() { _ = current.Close() }()
	for depth := 0; depth <= 64; depth++ {
		var stat unix.Stat_t
		if err := unix.Fstat(int(current.Fd()), &stat); err != nil {
			return err
		}
		if same, err := unixArchiveSame(stat, forbidden); err != nil {
			return err
		} else if same {
			return ErrArchiveProtected
		}
		if same, err := unixArchiveSame(stat, root); err != nil {
			return err
		} else if same {
			return nil
		}
		next, err := openDirectoryWithinRoot(&Root{file: current}, []string{".."})
		if err != nil {
			return err
		}
		same, statErr := unixArchiveSame(stat, next)
		if closeErr := current.Close(); closeErr != nil {
			_ = next.Close()
			return closeErr
		}
		current = next
		if statErr != nil {
			return statErr
		}
		if same {
			return ErrOutsideRoot
		}
	}
	return ErrOutsideRoot
}

func verifyUnixArchiveGuard(root *Root, guard *os.File) error {
	stat, err := unixArchiveStat(root.file, ArchiveDirectory)
	if err != nil {
		return err
	}
	if unixEntryType(uint32(stat.Mode)) == EntryLink {
		return ErrLink
	}
	same, err := unixArchiveSame(stat, guard)
	if err != nil {
		return err
	}
	if !same {
		return ErrChanged
	}
	return verifyUnixArchiveLocation(root.file, guard, nil)
}

func checkArchivePublishParent(ctx context.Context, root *Root, parent *os.File) (err error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	guard, err := openUnixArchiveGuard(root)
	if err != nil {
		return err
	}
	if guard != nil {
		defer func() { err = errors.Join(err, guard.Close()) }()
	}
	return verifyUnixArchiveLocation(root.file, parent, guard)
}

func checkArchiveWritePath(ctx context.Context, root *Root, components []string) (err error) {
	result := ArchiveResult{Outcome: PublishUnchanged}
	var handles []*os.File
	defer func() { closeArchiveHandles(handles, &result, &err) }()
	guard, err := openUnixArchiveGuard(root)
	if err != nil {
		return err
	}
	if guard != nil {
		handles = append(handles, guard)
	}
	parent := root.file
	for index, component := range components {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := verifyUnixArchiveLocation(root.file, parent, guard); err != nil {
			return err
		}
		stat, err := unixArchiveStat(parent, component)
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		entry, err := unixArchiveEntry(stat)
		if err != nil {
			return err
		}
		if same, err := unixArchiveSame(stat, guard); err != nil {
			return err
		} else if same {
			return ErrArchiveProtected
		}
		if entry.Kind != EntryDirectory {
			if index != len(components)-1 {
				return ErrNotDirectory
			}
			return nil
		}
		directory, err := openUnixArchiveDirectory(parent, component)
		if err != nil {
			return err
		}
		handles = append(handles, directory)
		parent = directory
	}
	return verifyUnixArchiveLocation(root.file, parent, guard)
}

func inspectArchiveSource(ctx context.Context, root *Root, components []string) (entry ArchiveEntry, err error) {
	result := ArchiveResult{Outcome: PublishUnchanged}
	var handles []*os.File
	defer func() { closeArchiveHandles(handles, &result, &err) }()
	guard, err := openUnixArchiveGuard(root)
	if err != nil {
		return entry, err
	}
	if guard != nil {
		handles = append(handles, guard)
	}
	parent, _, err := openPublishParentWithinRoot(root, components[:len(components)-1], false)
	if err != nil {
		return entry, archiveUnixError(err)
	}
	handles = append(handles, parent)
	return inspectUnixArchiveAt(ctx, root, parent, guard, components[len(components)-1])
}

func inspectUnixArchiveAt(ctx context.Context, root *Root, parent, guard *os.File, name string) (ArchiveEntry, error) {
	if err := ctx.Err(); err != nil {
		return ArchiveEntry{}, err
	}
	if err := verifyUnixArchiveLocation(root.file, parent, guard); err != nil {
		return ArchiveEntry{}, err
	}
	stat, err := unixArchiveStat(parent, name)
	if err != nil {
		return ArchiveEntry{}, err
	}
	for _, protected := range []*os.File{root.file, guard} {
		if same, err := unixArchiveSame(stat, protected); err != nil {
			return ArchiveEntry{}, err
		} else if same {
			return ArchiveEntry{}, ErrArchiveProtected
		}
	}
	return unixArchiveEntry(stat)
}

func archiveWithinRoot(ctx context.Context, root *Root, source, destination []string, expected ArchiveEntry) (result ArchiveResult, err error) {
	result.Outcome = PublishUnchanged
	var handles []*os.File
	defer func() {
		closeArchiveHandles(handles, &result, &err)
		finishArchive(&result, &err)
	}()
	guard, err := openUnixArchiveGuard(root)
	if err != nil {
		return result, err
	}
	if guard != nil {
		handles = append(handles, guard)
	}
	sourceParent, _, err := openPublishParentWithinRoot(root, source[:len(source)-1], false)
	if err != nil {
		return result, archiveSourceChangedError(archiveUnixError(err))
	}
	handles = append(handles, sourceParent)
	sourceName := source[len(source)-1]
	entry, err := inspectUnixArchiveAt(ctx, root, sourceParent, guard, sourceName)
	if err != nil {
		return result, archiveSourceChangedError(err)
	}
	if entry != expected {
		return result, ErrChanged
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if guard == nil {
		mkdirErr := unix.Mkdirat(int(root.file.Fd()), ArchiveDirectory, 0o700)
		if mkdirErr == nil {
			result.DirectoriesCreated = true
		} else if !errors.Is(mkdirErr, unix.EEXIST) {
			return result, archiveUnixError(mkdirErr)
		}
		guard, err = openUnixArchiveDirectory(root.file, ArchiveDirectory)
		if err != nil {
			return result, err
		}
		handles = append(handles, guard)
	}
	parent := guard
	for _, component := range destination[1 : len(destination)-1] {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		if err := verifyUnixArchiveGuard(root, guard); err != nil {
			return result, err
		}
		if err := verifyUnixArchiveLocation(guard, parent, nil); err != nil {
			return result, err
		}
		next, openErr := openUnixArchiveDirectory(parent, component)
		if errors.Is(openErr, ErrNotFound) {
			mkdirErr := unix.Mkdirat(int(parent.Fd()), component, 0o700)
			if mkdirErr == nil {
				result.DirectoriesCreated = true
			} else if !errors.Is(mkdirErr, unix.EEXIST) {
				return result, archiveUnixError(mkdirErr)
			}
			next, openErr = openUnixArchiveDirectory(parent, component)
		}
		if openErr != nil {
			return result, openErr
		}
		handles = append(handles, next)
		parent = next
	}
	if err := verifyUnixArchiveGuard(root, guard); err != nil {
		return result, err
	}
	if err := verifyUnixArchiveLocation(guard, parent, nil); err != nil {
		return result, err
	}
	// A held destination parent must still have the frozen destination spelling,
	// not merely remain somewhere inside the archive tree.
	destinationParent, _, err := openPublishParentWithinRoot(root, destination[:len(destination)-1], false)
	if err != nil {
		return result, errors.Join(ErrChanged, archiveUnixError(err))
	}
	handles = append(handles, destinationParent)
	var destinationStat unix.Stat_t
	if err := unix.Fstat(int(parent.Fd()), &destinationStat); err != nil {
		return result, err
	}
	if same, err := unixArchiveSame(destinationStat, destinationParent); err != nil {
		return result, err
	} else if !same {
		return result, ErrChanged
	}
	// Reopen the original spelling too: ancestry alone would not catch a
	// source parent renamed to a different location inside this workspace.
	reopened, _, err := openPublishParentWithinRoot(root, source[:len(source)-1], false)
	if err != nil {
		return result, errors.Join(ErrChanged, archiveUnixError(err))
	}
	handles = append(handles, reopened)
	var sourceStat unix.Stat_t
	if err := unix.Fstat(int(sourceParent.Fd()), &sourceStat); err != nil {
		return result, err
	}
	if same, err := unixArchiveSame(sourceStat, reopened); err != nil {
		return result, err
	} else if !same {
		return result, ErrChanged
	}
	entry, err = inspectUnixArchiveAt(ctx, root, sourceParent, guard, sourceName)
	if err != nil {
		return result, errors.Join(ErrChanged, err)
	}
	if entry != expected {
		return result, ErrChanged
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	renameErr := archiveRenameUnix(int(sourceParent.Fd()), sourceName, int(parent.Fd()), destination[len(destination)-1])
	if renameErr != nil {
		// EIO/EINTR and unclassified filesystem errors do not establish whether
		// a remote/other filesystem committed the rename. Never undo or retry.
		if !unixArchiveRenameUnchanged(renameErr) {
			result.Outcome = PublishUnknown
		}
		return result, archiveUnixError(renameErr)
	}
	result.Outcome = PublishCompleted
	// A late cancellation cannot undo a completed move. Sync each retained
	// directory, including newly created ancestors, before closing handles.
	for _, directory := range append([]*os.File{root.file}, handles...) {
		if syncErr := archiveSyncUnix(int(directory.Fd())); syncErr != nil {
			result.Outcome = PublishUnknown
			return result, fmt.Errorf("sync secure archive directory: %w", syncErr)
		}
	}
	return result, nil
}

func unixArchiveRenameUnchanged(err error) bool {
	for _, known := range []error{unix.EEXIST, unix.ENOTEMPTY, unix.EXDEV, unix.ENOSYS, unix.ENOTSUP,
		unix.EINVAL, unix.ENOENT, unix.EACCES, unix.EPERM, unix.ENOTDIR, unix.EISDIR,
		unix.ELOOP, unix.ENAMETOOLONG, unix.ENOSPC, unix.EDQUOT, unix.EROFS, unix.EBUSY, ErrArchiveUnsupported} {
		if errors.Is(err, known) {
			return true
		}
	}
	return false
}
