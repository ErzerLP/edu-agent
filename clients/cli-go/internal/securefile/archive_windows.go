//go:build windows

package securefile

import (
	"context"
	"errors"
	"os"
	"strings"

	"golang.org/x/sys/windows"
)

func archiveWindowsError(err error) error {
	if isWindowsNotFound(err) {
		return ErrNotFound
	}
	err = normalizeWindowsNTError(err)
	switch {
	case errors.Is(err, windows.ERROR_NOT_SAME_DEVICE):
		return ErrCrossDevice
	case errors.Is(err, windows.ERROR_ALREADY_EXISTS), errors.Is(err, windows.ERROR_FILE_EXISTS):
		return ErrAlreadyExists
	case errors.Is(err, windows.ERROR_NOT_SUPPORTED), errors.Is(err, windows.ERROR_INVALID_FUNCTION),
		errors.Is(err, windows.ERROR_CALL_NOT_IMPLEMENTED), errors.Is(err, windows.ERROR_INVALID_PARAMETER):
		return ErrArchiveUnsupported
	default:
		return classifyWindowsOpenError(err)
	}
}

// No FILE_NON_DIRECTORY_FILE restriction: both regular files and directories
// are accepted. No data access is requested and no descendants are enumerated.
func openWindowsArchiveEntry(parent *os.File, name string, access, share uint32) (*os.File, error) {
	handle, err := ntCreateWindowsRelative(windows.Handle(parent.Fd()), name,
		access|windows.FILE_READ_ATTRIBUTES|windows.SYNCHRONIZE, windows.FILE_OPEN,
		windows.FILE_SYNCHRONOUS_IO_NONALERT|windows.FILE_OPEN_REPARSE_POINT|windows.FILE_OPEN_FOR_BACKUP_INTENT,
		windows.FILE_ATTRIBUTE_NORMAL, share)
	if err != nil {
		return nil, archiveWindowsError(err)
	}
	file, info, err := windowsFileFromHandle(handle, name)
	if err != nil {
		return nil, archiveWindowsError(err)
	}
	if windowsEntryType(info) != EntryFile && windowsEntryType(info) != EntryDirectory {
		_ = file.Close()
		return nil, ErrNotRegular
	}
	return file, nil
}

func windowsArchiveEntry(file *os.File) (ArchiveEntry, error) {
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(windows.Handle(file.Fd()), &info); err != nil {
		return ArchiveEntry{}, archiveWindowsError(err)
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return ArchiveEntry{}, ErrLink
	}
	entry := windowsEntryInfo(info)
	return ArchiveEntry{Kind: entry.Kind, Size: entry.Size, Identity: entry.Identity, Version: entry.Version}, nil
}

func windowsArchiveSame(a, b *os.File) (bool, error) {
	if b == nil {
		return false, nil
	}
	first, err := windowsArchiveEntry(a)
	if err != nil {
		return false, err
	}
	second, err := windowsArchiveEntry(b)
	if err != nil {
		return false, err
	}
	return first.Identity == second.Identity, nil
}

func openWindowsArchiveGuard(root *Root) (*os.File, error) {
	guard, err := openWindowsDirectoryRelative(windows.Handle(root.file.Fd()), ArchiveDirectory)
	if isWindowsNotFound(err) {
		return nil, nil
	}
	return guard, archiveWindowsError(err)
}

func rejectWindowsArchiveIdentity(file *os.File, protected ...*os.File) error {
	for _, other := range protected {
		if same, err := windowsArchiveSame(file, other); err != nil {
			return err
		} else if same {
			return ErrArchiveProtected
		}
	}
	return nil
}

// Every ancestor remains open without FILE_SHARE_DELETE until the operation
// ends. Comparing handle identities (not spellings) catches case and 8.3 aliases.
func openWindowsArchiveParents(ctx context.Context, root *Root, components []string, guard *os.File, handles *[]*os.File) (*os.File, error) {
	parent := root.file
	for _, component := range components {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		next, err := openWindowsDirectoryRelative(windows.Handle(parent.Fd()), component)
		if err != nil {
			return nil, archiveWindowsError(err)
		}
		*handles = append(*handles, next)
		if err := rejectWindowsArchiveIdentity(next, guard, root.file); err != nil {
			return nil, err
		}
		parent = next
	}
	return parent, nil
}

// Check the *opened* publication parent, not the caller's path spelling. The
// kernel's normalized handle paths expand 8.3 names; identity also covers the
// archive directory itself. Protected publication pins every ancestor as well.
func checkArchivePublishParent(ctx context.Context, root *Root, parent *os.File) (err error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	rootPath, err := resolvedHandlePath(windows.Handle(root.file.Fd()))
	if err != nil {
		return err
	}
	parentPath, err := resolvedHandlePath(windows.Handle(parent.Fd()))
	if err != nil {
		return err
	}
	if !windowsArchivePathWithin(rootPath, parentPath) {
		return ErrOutsideRoot
	}
	guard, err := openWindowsArchiveGuard(root)
	if err != nil {
		return err
	}
	if guard == nil {
		return nil
	}
	defer func() { err = errors.Join(err, guard.Close()) }()
	if err := rejectWindowsArchiveIdentity(parent, guard); err != nil {
		return err
	}
	guardPath, err := resolvedHandlePath(windows.Handle(guard.Fd()))
	if err != nil {
		return err
	}
	if windowsArchivePathWithin(guardPath, parentPath) {
		return ErrArchiveProtected
	}
	return nil
}

func windowsArchivePathWithin(root, path string) bool {
	prefix := strings.TrimRight(root, `\`) + `\`
	return strings.EqualFold(root, path) || len(path) >= len(prefix) && strings.EqualFold(prefix, path[:len(prefix)])
}

func openWindowsProtectedPublishParent(ctx context.Context, root *Root, components []string, create bool) (parent *os.File, created bool, handles []*os.File, err error) {
	parent = root.file
	guard, err := openWindowsArchiveGuard(root)
	if err != nil {
		return nil, false, handles, err
	}
	if guard != nil {
		handles = append(handles, guard)
	}
	result := ArchiveResult{Outcome: PublishUnchanged}
	for _, component := range components {
		if err := ctx.Err(); err != nil {
			return nil, result.DirectoriesCreated, handles, err
		}
		next, openErr := openWindowsDirectoryRelative(windows.Handle(parent.Fd()), component)
		if isWindowsNotFound(openErr) && create {
			next, openErr = createWindowsArchiveDirectory(parent, component, &result)
			if errors.Is(openErr, ErrAlreadyExists) {
				next, openErr = openWindowsDirectoryRelative(windows.Handle(parent.Fd()), component)
			}
		}
		if openErr != nil {
			return nil, result.DirectoriesCreated, handles, archiveWindowsError(openErr)
		}
		handles = append(handles, next)
		if err := rejectWindowsArchiveIdentity(next, guard); err != nil {
			return nil, result.DirectoriesCreated, handles, err
		}
		parent = next
	}
	return parent, result.DirectoriesCreated, handles, nil
}

func checkArchiveWritePath(ctx context.Context, root *Root, components []string) (err error) {
	if err := validateWindowsRelativeComponents(components); err != nil {
		return err
	}
	result := ArchiveResult{Outcome: PublishUnchanged}
	var handles []*os.File
	defer func() { closeArchiveHandles(handles, &result, &err) }()
	guard, err := openWindowsArchiveGuard(root)
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
		next, err := openWindowsArchiveEntry(parent, component, 0, windowsReadShare)
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		handles = append(handles, next)
		if err := rejectWindowsArchiveIdentity(next, guard, root.file); err != nil {
			return err
		}
		entry, err := windowsArchiveEntry(next)
		if err != nil {
			return err
		}
		if index != len(components)-1 {
			if entry.Kind != EntryDirectory {
				return ErrNotDirectory
			}
			directory, err := openWindowsDirectoryRelative(windows.Handle(parent.Fd()), component)
			if err != nil {
				return archiveWindowsError(err)
			}
			handles = append(handles, directory)
			parent = directory
		}
	}
	return nil
}

func inspectArchiveSource(ctx context.Context, root *Root, components []string) (entry ArchiveEntry, err error) {
	if err := validateWindowsRelativeComponents(components); err != nil {
		return entry, err
	}
	result := ArchiveResult{Outcome: PublishUnchanged}
	var handles []*os.File
	defer func() { closeArchiveHandles(handles, &result, &err) }()
	guard, err := openWindowsArchiveGuard(root)
	if err != nil {
		return entry, err
	}
	if guard != nil {
		handles = append(handles, guard)
	}
	parent, err := openWindowsArchiveParents(ctx, root, components[:len(components)-1], guard, &handles)
	if err != nil {
		return entry, err
	}
	if err := ctx.Err(); err != nil {
		return entry, err
	}
	file, err := openWindowsArchiveEntry(parent, components[len(components)-1], 0, windowsReadShare)
	if err != nil {
		return entry, err
	}
	handles = append(handles, file)
	if err := rejectWindowsArchiveIdentity(file, guard, root.file); err != nil {
		return entry, err
	}
	return windowsArchiveEntry(file)
}

func createWindowsArchiveDirectory(parent *os.File, name string, result *ArchiveResult) (*os.File, error) {
	handle, err := ntCreateWindowsRelative(windows.Handle(parent.Fd()), name,
		windowsDirectoryAccess, windows.FILE_CREATE,
		windows.FILE_DIRECTORY_FILE|windows.FILE_SYNCHRONOUS_IO_NONALERT|windows.FILE_OPEN_REPARSE_POINT|windows.FILE_OPEN_FOR_BACKUP_INTENT,
		windows.FILE_ATTRIBUTE_NORMAL, windowsReadShare)
	if err != nil {
		return nil, archiveWindowsError(err)
	}
	// Record creation before handle conversion/inspection, which can fail.
	result.DirectoriesCreated = true
	file, _, err := windowsFileFromHandle(handle, name)
	return file, archiveWindowsError(err)
}

func archiveWithinRoot(ctx context.Context, root *Root, source, destination []string, expected ArchiveEntry) (result ArchiveResult, err error) {
	result.Outcome = PublishUnchanged
	if err := validateWindowsRelativeComponents(source); err != nil {
		return result, err
	}
	if err := validateWindowsRelativeComponents(destination); err != nil {
		return result, err
	}
	if err := checkArchiveWritePath(ctx, root, source); err != nil {
		return result, archiveSourceChangedError(err)
	}
	var handles []*os.File
	defer func() {
		closeArchiveHandles(handles, &result, &err)
		finishArchive(&result, &err)
	}()
	guard, err := openWindowsArchiveGuard(root)
	if err != nil {
		return result, err
	}
	if guard != nil {
		handles = append(handles, guard)
	}
	sourceParent, err := openWindowsArchiveParents(ctx, root, source[:len(source)-1], guard, &handles)
	if err != nil {
		return result, archiveSourceChangedError(err)
	}
	sourceName := source[len(source)-1]
	file, err := openWindowsArchiveEntry(sourceParent, sourceName, windows.DELETE, windowsReadShare)
	if err != nil {
		return result, archiveSourceChangedError(err)
	}
	handles = append(handles, file)
	if err := rejectWindowsArchiveIdentity(file, root.file, guard); err != nil {
		return result, err
	}
	entry, err := windowsArchiveEntry(file)
	if err != nil {
		return result, err
	}
	if entry != expected {
		return result, ErrChanged
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if guard == nil {
		guard, err = createWindowsArchiveDirectory(root.file, ArchiveDirectory, &result)
		if errors.Is(err, ErrAlreadyExists) {
			guard, err = openWindowsDirectoryRelative(windows.Handle(root.file.Fd()), ArchiveDirectory)
		}
		if err != nil {
			return result, archiveWindowsError(err)
		}
		handles = append(handles, guard)
	}
	parent := guard
	for _, component := range destination[1 : len(destination)-1] {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		next, openErr := openWindowsDirectoryRelative(windows.Handle(parent.Fd()), component)
		if isWindowsNotFound(openErr) {
			next, openErr = createWindowsArchiveDirectory(parent, component, &result)
			if errors.Is(openErr, ErrAlreadyExists) {
				next, openErr = openWindowsDirectoryRelative(windows.Handle(parent.Fd()), component)
			}
		}
		if openErr != nil {
			return result, archiveWindowsError(openErr)
		}
		handles = append(handles, next)
		parent = next
	}
	// All ancestors are pinned; verify the original entry name through its
	// parent too. Share-delete is needed because our source handle owns DELETE.
	reopened, err := openWindowsArchiveEntry(sourceParent, sourceName, 0, windowsAllShare)
	if err != nil {
		return result, errors.Join(ErrChanged, err)
	}
	handles = append(handles, reopened)
	entry, err = windowsArchiveEntry(reopened)
	if err != nil {
		return result, err
	}
	if entry != expected {
		return result, ErrChanged
	}
	if err := rejectWindowsArchiveIdentity(reopened, root.file, guard); err != nil {
		return result, err
	}
	var fromInfo, toInfo windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(windows.Handle(file.Fd()), &fromInfo); err != nil {
		return result, archiveWindowsError(err)
	}
	if err := windows.GetFileInformationByHandle(windows.Handle(parent.Fd()), &toInfo); err != nil {
		return result, archiveWindowsError(err)
	}
	if fromInfo.VolumeSerialNumber != toInfo.VolumeSerialNumber {
		return result, ErrCrossDevice
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	renameErr := renameWindowsFileHandle(windows.Handle(file.Fd()), windows.Handle(parent.Fd()), destination[len(destination)-1], false)
	if renameErr != nil {
		mapped := archiveWindowsError(renameErr)
		if errors.Is(mapped, ErrCrossDevice) || errors.Is(mapped, ErrArchiveUnsupported) {
			return result, mapped
		}
		mapped, unchanged := classifyWindowsRenameError(renameErr, PublishCreate)
		if !unchanged {
			result.Outcome = PublishUnknown
		}
		return result, mapped
	}
	// Windows has no supported directory-fsync equivalent here. Successful
	// handle rename and closure establish completion, as in Root.Publish.
	// Never reinterpret a cancellation arriving after this point as unchanged.
	result.Outcome = PublishCompleted
	return result, nil
}
