//go:build !windows

package securefile

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"

	"golang.org/x/sys/unix"
)

func ensureDirectory(path string, private bool) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return fmt.Errorf("create secure directory: %w", err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("inspect secure directory: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return errors.New("secure directory is not a directory")
	}
	if private && info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("secure directory permissions are too broad: %04o", info.Mode().Perm())
	}
	return nil
}

func OpenRoot(path string) (*Root, error) {
	root, err := openRoot(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("open secure root without following links: %w", classifyUnixOpenError(err))
	}
	return root, nil
}

func (r *Root) ReadLimit(relative string, limit int64, private bool) ([]byte, error) {
	snapshot, err := r.ReadSnapshot(relative, limit, private)
	if err != nil {
		return nil, err
	}
	return snapshot.Data, nil
}

func (r *Root) ReadSnapshot(relative string, limit int64, private bool) (Snapshot, error) {
	if r == nil || r.file == nil {
		return Snapshot{}, errors.New("secure root is closed")
	}
	components, err := relativeComponents(relative)
	if err != nil {
		return Snapshot{}, err
	}
	file, info, err := openReadWithinRoot(r, components)
	if errors.Is(err, os.ErrNotExist) {
		return Snapshot{}, ErrNotFound
	}
	if err != nil {
		return Snapshot{}, fmt.Errorf("open secure file within root: %w", classifyUnixOpenError(err))
	}
	return readOpenFileSnapshot(file, info, limit, private)
}

func (r *Root) ReadDir(relative string, limit int) ([]DirEntry, int, bool, error) {
	if r == nil || r.file == nil {
		return nil, 0, false, errors.New("secure root is closed")
	}
	if limit < 1 {
		return nil, 0, false, errors.New("secure directory entry limit is invalid")
	}
	components, err := directoryComponents(relative)
	if err != nil {
		return nil, 0, false, err
	}
	directory, err := openDirectoryWithinRoot(r, components)
	if errors.Is(err, os.ErrNotExist) {
		return nil, 0, false, ErrNotFound
	}
	if err != nil {
		return nil, 0, false, fmt.Errorf("open secure directory within root: %w", classifyUnixOpenError(err))
	}
	defer directory.Close()
	names, readErr := directory.Readdirnames(limit + 1)
	if readErr != nil && !errors.Is(readErr, io.EOF) {
		return nil, 0, false, fmt.Errorf("read secure directory: %w", readErr)
	}
	complete := len(names) <= limit
	if !complete {
		names = names[:limit]
	}
	entries := make([]DirEntry, 0, len(names))
	skipped := 0
	for _, name := range names {
		var stat unix.Stat_t
		if err := unix.Fstatat(int(directory.Fd()), name, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			if errors.Is(err, unix.ENOENT) {
				skipped++
				continue
			}
			return nil, skipped, false, fmt.Errorf("inspect secure directory entry: %w", classifyUnixOpenError(err))
		}
		entries = append(entries, DirEntry{Name: name, Type: unixEntryType(uint32(stat.Mode))})
	}
	return entries, skipped, complete, nil
}

// Delete removes a root-confined regular file without following links. Missing
// files are treated as already deleted.
func (r *Root) Delete(relative string) error {
	if r == nil || r.file == nil {
		return errors.New("secure root is closed")
	}
	components, err := relativeComponents(relative)
	if err != nil {
		return err
	}
	return deleteWithinRoot(r, components)
}

func deleteWithinRoot(root *Root, components []string) error {
	parent, _, err := openPublishParentWithinRoot(root, components[:len(components)-1], false)
	if errors.Is(err, unix.ENOENT) {
		return nil
	}
	if err != nil {
		return classifyUnixOpenError(err)
	}
	defer parent.Close()
	target := components[len(components)-1]
	var stat unix.Stat_t
	if err := unix.Fstatat(int(parent.Fd()), target, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		if errors.Is(err, unix.ENOENT) {
			return nil
		}
		return classifyUnixOpenError(err)
	}
	switch uint32(stat.Mode) & unix.S_IFMT {
	case unix.S_IFLNK:
		return ErrLink
	case unix.S_IFREG:
	default:
		return ErrNotRegular
	}
	if err := verifyUnixPublishParentWithinRoot(int(root.file.Fd()), int(parent.Fd()), 64); err != nil {
		return err
	}
	if err := unix.Unlinkat(int(parent.Fd()), target, 0); err != nil {
		if errors.Is(err, unix.ENOENT) {
			return nil
		}
		return classifyUnixOpenError(err)
	}
	if err := syncDirectoryHandle(int(parent.Fd())); err != nil {
		return fmt.Errorf("sync secure directory after delete: %w", err)
	}
	return nil
}

func openRoot(path string) (*Root, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_DIRECTORY, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		_ = unix.Close(fd)
		return nil, errors.New("create secure root handle")
	}
	return &Root{file: file, path: path}, nil
}

func openDirectoryWithinRoot(root *Root, components []string) (*os.File, error) {
	if len(components) == 0 {
		fd, err := unix.Openat(int(root.file.Fd()), ".", unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_DIRECTORY, 0)
		if err != nil {
			return nil, err
		}
		file := os.NewFile(uintptr(fd), ".")
		if file == nil {
			_ = unix.Close(fd)
			return nil, errors.New("reopen secure root handle")
		}
		return file, nil
	}
	parentFD := int(root.file.Fd())
	ownedParent := false
	for _, component := range components {
		fd, err := unix.Openat(parentFD, component, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_DIRECTORY, 0)
		if ownedParent {
			_ = unix.Close(parentFD)
		}
		if err != nil {
			return nil, err
		}
		parentFD = fd
		ownedParent = true
	}
	file := os.NewFile(uintptr(parentFD), components[len(components)-1])
	if file == nil {
		_ = unix.Close(parentFD)
		return nil, errors.New("create secure directory handle")
	}
	return file, nil
}

func openReadWithinRoot(root *Root, components []string) (*os.File, os.FileInfo, error) {
	parentFD := int(root.file.Fd())
	ownedParent := false
	for index, component := range components {
		flags := unix.O_RDONLY | unix.O_CLOEXEC | unix.O_NOFOLLOW
		if index < len(components)-1 {
			flags |= unix.O_DIRECTORY
		}
		fd, err := unix.Openat(parentFD, component, flags, 0)
		if ownedParent {
			_ = unix.Close(parentFD)
		}
		if err != nil {
			return nil, nil, err
		}
		parentFD = fd
		ownedParent = true
	}
	file := os.NewFile(uintptr(parentFD), components[len(components)-1])
	if file == nil {
		_ = unix.Close(parentFD)
		return nil, nil, errors.New("create secure file handle")
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, nil, err
	}
	return file, info, nil
}

func openReadNoFollow(path string) (*os.File, os.FileInfo, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		_ = unix.Close(fd)
		return nil, nil, errors.New("create secure file handle")
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, nil, err
	}
	return file, info, nil
}

func classifyUnixOpenError(err error) error {
	switch {
	case errors.Is(err, unix.ELOOP):
		return ErrLink
	case errors.Is(err, unix.ENOTDIR):
		return ErrNotDirectory
	case errors.Is(err, unix.EACCES), errors.Is(err, unix.EPERM):
		return ErrPermission
	default:
		return err
	}
}

func unixEntryType(mode uint32) EntryType {
	switch mode & unix.S_IFMT {
	case unix.S_IFREG:
		return EntryFile
	case unix.S_IFDIR:
		return EntryDirectory
	case unix.S_IFLNK:
		return EntryLink
	default:
		return EntryOther
	}
}

func publishWithinRootOptions(ctx context.Context, root *Root, components []string, data []byte, options PublishOptions) (result PublishResult, err error) {
	result.Outcome = PublishUnchanged
	mode, permission := options.Mode, options.Permission.Perm()
	parent, parentsCreated, err := openPublishParentWithinRoot(root, components[:len(components)-1], mode == PublishCreate)
	if err != nil {
		if parentsCreated {
			return PublishResult{Outcome: PublishUnknown}, fmt.Errorf("%w: %v", ErrOutcomeUnknown, classifyUnixOpenError(err))
		}
		return result, classifyUnixOpenError(err)
	}
	defer parent.Close()
	defer func() {
		if err != nil && result.Outcome == PublishUnchanged && parentsCreated {
			result.Outcome = PublishUnknown
			err = fmt.Errorf("%w: %v", ErrOutcomeUnknown, err)
		}
	}()
	if options.ProtectArchive {
		if err := checkArchivePublishParent(ctx, root, parent); err != nil {
			return result, err
		}
	}
	target := components[len(components)-1]
	var targetStat unix.Stat_t
	targetErr := unix.Fstatat(int(parent.Fd()), target, &targetStat, unix.AT_SYMLINK_NOFOLLOW)
	switch mode {
	case PublishCreate:
		if targetErr == nil {
			if targetStat.Mode&unix.S_IFMT == unix.S_IFLNK {
				return result, ErrLink
			}
			return result, ErrAlreadyExists
		}
		if !errors.Is(targetErr, unix.ENOENT) {
			return result, classifyUnixOpenError(targetErr)
		}
	case PublishReplace:
		if errors.Is(targetErr, unix.ENOENT) {
			return result, ErrNotFound
		}
		if targetErr != nil {
			return result, classifyUnixOpenError(targetErr)
		}
		if targetStat.Mode&unix.S_IFMT == unix.S_IFLNK {
			return result, ErrLink
		}
		if targetStat.Mode&unix.S_IFMT != unix.S_IFREG {
			return result, ErrNotRegular
		}
	}

	tempName, err := secureTempName()
	if err != nil {
		return result, err
	}
	tempFD, err := unix.Openat(int(parent.Fd()), tempName, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW, uint32(permission.Perm()))
	if err != nil {
		return result, classifyUnixOpenError(err)
	}
	temp := os.NewFile(uintptr(tempFD), tempName)
	if temp == nil {
		_ = unix.Close(tempFD)
		_ = unix.Unlinkat(int(parent.Fd()), tempName, 0)
		return result, errors.New("create secure temporary file handle")
	}
	published := false
	defer func() {
		_ = temp.Close()
		if published {
			return
		}
		if cleanupErr := unix.Unlinkat(int(parent.Fd()), tempName, 0); cleanupErr != nil && !errors.Is(cleanupErr, unix.ENOENT) {
			result.Outcome = PublishUnknown
			if err == nil {
				err = ErrOutcomeUnknown
			} else {
				err = fmt.Errorf("%w: cleanup after %v: %v", ErrOutcomeUnknown, err, cleanupErr)
			}
		}
	}()
	if err := unix.Fchmod(tempFD, uint32(permission.Perm())); err != nil {
		return result, classifyUnixOpenError(err)
	}
	for written := 0; written < len(data); {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		count, writeErr := temp.Write(data[written:])
		if writeErr != nil {
			return result, writeErr
		}
		if count == 0 {
			return result, io.ErrShortWrite
		}
		written += count
	}
	if err := temp.Sync(); err != nil {
		return result, err
	}
	if err := temp.Close(); err != nil {
		return result, err
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if err := verifyUnixPublishParentWithinRoot(int(root.file.Fd()), int(parent.Fd()), 64); err != nil {
		return result, err
	}
	if mode == PublishReplace {
		if err := revalidateUnixPublishTarget(int(parent.Fd()), target, options.ExpectedHash, options.ExpectedLimit); err != nil {
			return result, err
		}
	}

	if options.ProtectArchive {
		if err := checkArchivePublishParent(ctx, root, parent); err != nil {
			return result, err
		}
	}
	switch mode {
	case PublishCreate:
		if err := unix.Linkat(int(parent.Fd()), tempName, int(parent.Fd()), target, 0); err != nil {
			if errors.Is(err, unix.EEXIST) {
				return result, ErrAlreadyExists
			}
			return result, classifyUnixOpenError(err)
		}
		published = true
		if cleanupErr := unix.Unlinkat(int(parent.Fd()), tempName, 0); cleanupErr != nil {
			result.Outcome = PublishUnknown
			return result, fmt.Errorf("%w: temporary hardlink cleanup failed: %v", ErrOutcomeUnknown, cleanupErr)
		}
		result.Outcome = PublishCompleted
	case PublishReplace:
		if err := unix.Renameat(int(parent.Fd()), tempName, int(parent.Fd()), target); err != nil {
			return result, classifyUnixOpenError(err)
		}
		published = true
		result.Outcome = PublishCompleted
	}
	if err := syncDirectoryHandle(int(parent.Fd())); err != nil {
		result.Outcome = PublishUnknown
		return result, ErrOutcomeUnknown
	}
	return result, nil
}

func verifyUnixPublishParentWithinRoot(rootFD, parentFD, maxDepth int) error {
	var rootStat unix.Stat_t
	if err := unix.Fstat(rootFD, &rootStat); err != nil {
		return err
	}
	currentFD, err := unix.Openat(parentFD, ".", unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_DIRECTORY, 0)
	if err != nil {
		return err
	}
	defer func() { _ = unix.Close(currentFD) }()
	for depth := 0; depth <= maxDepth; depth++ {
		var currentStat unix.Stat_t
		if err := unix.Fstat(currentFD, &currentStat); err != nil {
			return err
		}
		if currentStat.Dev == rootStat.Dev && currentStat.Ino == rootStat.Ino {
			return nil
		}
		nextFD, err := unix.Openat(currentFD, "..", unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_DIRECTORY, 0)
		if err != nil {
			return err
		}
		var nextStat unix.Stat_t
		if err := unix.Fstat(nextFD, &nextStat); err != nil {
			_ = unix.Close(nextFD)
			return err
		}
		if nextStat.Dev == currentStat.Dev && nextStat.Ino == currentStat.Ino {
			_ = unix.Close(nextFD)
			return ErrOutsideRoot
		}
		_ = unix.Close(currentFD)
		currentFD = nextFD
	}
	return ErrOutsideRoot
}

var snapshotFileIdentityForPlatform = func(file *os.File, _ os.FileInfo) (string, error) {
	var stat unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &stat); err != nil {
		return "", err
	}
	return fmt.Sprintf("unix:%x:%x", uint64(stat.Dev), uint64(stat.Ino)), nil
}

func revalidateUnixPublishTarget(parentFD int, target, expectedHash string, limit int64) error {
	fd, err := unix.Openat(parentFD, target, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return classifyUnixOpenError(err)
	}
	file := os.NewFile(uintptr(fd), target)
	if file == nil {
		_ = unix.Close(fd)
		return errors.New("create secure revalidation handle")
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return err
	}
	snapshot, err := readOpenFileSnapshot(file, info, limit, false)
	if err != nil {
		return err
	}
	if snapshotContentHash(snapshot.Data) != expectedHash {
		return ErrChanged
	}
	return nil
}

func openPublishParentWithinRoot(root *Root, components []string, create bool) (*os.File, bool, error) {
	parentFD := int(root.file.Fd())
	ownedParent := false
	created := false
	if len(components) == 0 {
		file, err := openDirectoryWithinRoot(root, nil)
		return file, false, err
	}
	for _, component := range components {
		fd, err := unix.Openat(parentFD, component, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_DIRECTORY, 0)
		if errors.Is(err, unix.ENOENT) && create {
			mkdirErr := unix.Mkdirat(parentFD, component, 0o755)
			if mkdirErr != nil && !errors.Is(mkdirErr, unix.EEXIST) {
				if ownedParent {
					_ = unix.Close(parentFD)
				}
				return nil, created, mkdirErr
			}
			if mkdirErr == nil {
				created = true
			}
			fd, err = unix.Openat(parentFD, component, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_DIRECTORY, 0)
		}
		if ownedParent {
			_ = unix.Close(parentFD)
		}
		if err != nil {
			return nil, created, err
		}
		parentFD = fd
		ownedParent = true
	}
	file := os.NewFile(uintptr(parentFD), components[len(components)-1])
	if file == nil {
		_ = unix.Close(parentFD)
		return nil, created, errors.New("create secure publish parent handle")
	}
	return file, created, nil
}

func secureTempName() (string, error) {
	var value [12]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return ".edu-agent-" + hex.EncodeToString(value[:]), nil
}

func syncDirectoryHandle(fd int) error {
	err := unix.Fsync(fd)
	if errors.Is(err, unix.EINVAL) || errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.EROFS) {
		return nil
	}
	return err
}

func replaceFile(from, to string) error { return os.Rename(from, to) }

func syncDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
