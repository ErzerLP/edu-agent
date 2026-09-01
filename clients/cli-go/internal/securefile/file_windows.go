//go:build windows

package securefile

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	windowsDirectoryAccess = windows.FILE_GENERIC_READ
	windowsReadShare       = windows.FILE_SHARE_READ | windows.FILE_SHARE_WRITE
	windowsPinnedReadShare = windows.FILE_SHARE_READ
	windowsAllShare        = windows.FILE_SHARE_READ | windows.FILE_SHARE_WRITE | windows.FILE_SHARE_DELETE
	windowsTempAttempts    = 8
)

func ensureDirectory(path string, _ bool) error {
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
	return nil
}

func checkPrivateFile(os.FileInfo) error { return nil }

func OpenRoot(path string) (*Root, error) {
	root, err := openRoot(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("open secure root without following links: %w", classifyWindowsOpenError(err))
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
	if err := validateWindowsRelativeComponents(components); err != nil {
		return Snapshot{}, err
	}
	file, info, err := openReadWithinRoot(r, components)
	if errors.Is(err, os.ErrNotExist) {
		return Snapshot{}, ErrNotFound
	}
	if err != nil {
		return Snapshot{}, fmt.Errorf("open secure file within root: %w", classifyWindowsOpenError(err))
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
	if err := validateWindowsRelativeComponents(components); err != nil {
		return nil, 0, false, err
	}
	directory, err := openDirectoryWithinRoot(r, components)
	if errors.Is(err, os.ErrNotExist) {
		return nil, 0, false, ErrNotFound
	}
	if err != nil {
		return nil, 0, false, fmt.Errorf("open secure directory within root: %w", classifyWindowsOpenError(err))
	}
	defer directory.Close()

	// os.File.ReadDir enumerates from the already-open directory handle. It does
	// not reconstruct the workspace path, and DirEntry.Info uses the attributes
	// returned by that handle-relative enumeration.
	raw, readErr := directory.ReadDir(limit + 1)
	if readErr != nil && !errors.Is(readErr, io.EOF) {
		return nil, 0, false, fmt.Errorf("read secure directory: %w", classifyWindowsOpenError(readErr))
	}
	complete := len(raw) <= limit
	if !complete {
		raw = raw[:limit]
	}
	entries := make([]DirEntry, 0, len(raw))
	skipped := 0
	for _, entry := range raw {
		info, err := entry.Info()
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				skipped++
				continue
			}
			return nil, skipped, false, fmt.Errorf("inspect secure directory entry: %w", classifyWindowsOpenError(err))
		}
		entries = append(entries, DirEntry{Name: entry.Name(), Type: windowsEntryType(info)})
	}
	return entries, skipped, complete, nil
}

func openRoot(path string) (*Root, error) {
	absolutePath, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	file, info, err := openDirectoryNoFollow(absolutePath)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		_ = file.Close()
		return nil, ErrNotDirectory
	}
	resolvedPath, err := resolvedHandlePath(windows.Handle(file.Fd()))
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	return &Root{file: file, path: absolutePath, resolvedPath: resolvedPath}, nil
}

func openDirectoryWithinRoot(root *Root, components []string) (*os.File, error) {
	if err := validateWindowsRelativeComponents(components); err != nil {
		return nil, err
	}
	parent := windows.Handle(root.file.Fd())
	ownedParent := (*os.File)(nil)
	if len(components) == 0 {
		return duplicateWindowsDirectoryHandle(parent, ".")
	}
	for _, component := range components {
		directory, err := openWindowsDirectoryRelative(parent, component)
		if ownedParent != nil {
			_ = ownedParent.Close()
		}
		if err != nil {
			return nil, err
		}
		ownedParent = directory
		parent = windows.Handle(directory.Fd())
	}
	return ownedParent, nil
}

func openReadWithinRoot(root *Root, components []string) (*os.File, os.FileInfo, error) {
	if err := validateWindowsRelativeComponents(components); err != nil {
		return nil, nil, err
	}
	parent := windows.Handle(root.file.Fd())
	ownedParent := (*os.File)(nil)
	for index, component := range components {
		if index == len(components)-1 {
			file, info, err := openWindowsFileRelative(parent, component, windowsPinnedReadShare)
			if ownedParent != nil {
				_ = ownedParent.Close()
			}
			return file, info, err
		}
		directory, err := openWindowsDirectoryRelative(parent, component)
		if ownedParent != nil {
			_ = ownedParent.Close()
		}
		if err != nil {
			return nil, nil, err
		}
		ownedParent = directory
		parent = windows.Handle(directory.Fd())
	}
	return nil, nil, errors.New("secure file path is empty")
}

func openDirectoryNoFollow(path string) (*os.File, os.FileInfo, error) {
	return openWindowsPathNoFollow(path, windows.FILE_FLAG_BACKUP_SEMANTICS)
}

func openReadNoFollow(path string) (*os.File, os.FileInfo, error) {
	return openWindowsPathNoFollow(path, 0)
}

func openWindowsPathNoFollow(path string, extraFlags uint32) (*os.File, os.FileInfo, error) {
	pathPtr, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, nil, err
	}
	handle, err := windows.CreateFile(
		pathPtr,
		windows.GENERIC_READ,
		windowsReadShare,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT|extraFlags,
		0,
	)
	if err != nil {
		return nil, nil, err
	}
	file, info, err := windowsFileFromHandle(handle, path)
	if err != nil {
		return nil, nil, err
	}
	return file, info, nil
}

func duplicateWindowsDirectoryHandle(source windows.Handle, name string) (*os.File, error) {
	process := windows.CurrentProcess()
	var duplicate windows.Handle
	if err := windows.DuplicateHandle(process, source, process, &duplicate, 0, false, windows.DUPLICATE_SAME_ACCESS); err != nil {
		return nil, err
	}
	file, info, err := windowsFileFromHandle(duplicate, name)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		if closeErr := file.Close(); closeErr != nil {
			return nil, fmt.Errorf("%w: close unexpected duplicate: %v", ErrNotDirectory, closeErr)
		}
		return nil, ErrNotDirectory
	}
	return file, nil
}

func openWindowsDirectoryRelative(parent windows.Handle, name string) (*os.File, error) {
	handle, err := ntCreateWindowsRelative(
		parent,
		name,
		windowsDirectoryAccess,
		windows.FILE_OPEN,
		windows.FILE_DIRECTORY_FILE|windows.FILE_SYNCHRONOUS_IO_NONALERT|windows.FILE_OPEN_REPARSE_POINT|windows.FILE_OPEN_FOR_BACKUP_INTENT,
		windows.FILE_ATTRIBUTE_NORMAL,
		windowsReadShare,
	)
	if err != nil {
		return nil, err
	}
	file, info, err := windowsFileFromHandle(handle, name)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		_ = file.Close()
		return nil, ErrNotDirectory
	}
	return file, nil
}

func openWindowsFileRelative(parent windows.Handle, name string, share uint32) (*os.File, os.FileInfo, error) {
	handle, err := ntCreateWindowsRelative(
		parent,
		name,
		windows.FILE_GENERIC_READ,
		windows.FILE_OPEN,
		windows.FILE_NON_DIRECTORY_FILE|windows.FILE_SYNCHRONOUS_IO_NONALERT|windows.FILE_OPEN_REPARSE_POINT|windows.FILE_OPEN_FOR_BACKUP_INTENT,
		windows.FILE_ATTRIBUTE_NORMAL,
		share,
	)
	if err != nil {
		return nil, nil, err
	}
	file, info, err := windowsFileFromHandle(handle, name)
	if err != nil {
		return nil, nil, err
	}
	if !info.Mode().IsRegular() {
		_ = file.Close()
		return nil, nil, ErrNotRegular
	}
	return file, info, nil
}

func ntCreateWindowsRelative(parent windows.Handle, name string, access, disposition, options, attributes, share uint32) (windows.Handle, error) {
	objectName, err := windows.NewNTUnicodeString(name)
	if err != nil {
		return windows.InvalidHandle, err
	}
	objectAttributes := windows.OBJECT_ATTRIBUTES{
		Length:        uint32(unsafe.Sizeof(windows.OBJECT_ATTRIBUTES{})),
		RootDirectory: parent,
		ObjectName:    objectName,
		Attributes:    windows.OBJ_CASE_INSENSITIVE | windows.OBJ_DONT_REPARSE,
	}
	var handle windows.Handle
	err = windows.NtCreateFile(
		&handle,
		access,
		&objectAttributes,
		&windows.IO_STATUS_BLOCK{},
		nil,
		attributes,
		share,
		disposition,
		options,
		0,
		0,
	)
	runtime.KeepAlive(objectName)
	if err != nil {
		return windows.InvalidHandle, normalizeWindowsNTError(err)
	}
	return handle, nil
}

func windowsFileFromHandle(handle windows.Handle, name string) (*os.File, os.FileInfo, error) {
	var handleInfo windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &handleInfo); err != nil {
		_ = windows.CloseHandle(handle)
		return nil, nil, err
	}
	if handleInfo.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		_ = windows.CloseHandle(handle)
		return nil, nil, ErrLink
	}
	file := os.NewFile(uintptr(handle), name)
	if file == nil {
		_ = windows.CloseHandle(handle)
		return nil, nil, errors.New("create secure file handle")
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, nil, err
	}
	return file, info, nil
}

func resolvedHandlePath(handle windows.Handle) (string, error) {
	size := uint32(256)
	for {
		buffer := make([]uint16, size)
		length, err := windows.GetFinalPathNameByHandle(handle, &buffer[0], uint32(len(buffer)), 0)
		if err != nil {
			return "", err
		}
		if length < uint32(len(buffer)) {
			return filepath.Clean(windows.UTF16ToString(buffer[:length])), nil
		}
		size = length + 1
	}
}

func windowsEntryType(info os.FileInfo) EntryType {
	if data, ok := info.Sys().(*syscall.Win32FileAttributeData); ok && data.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return EntryLink
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return EntryLink
	}
	if info.IsDir() {
		return EntryDirectory
	}
	if info.Mode().IsRegular() {
		return EntryFile
	}
	return EntryOther
}

func normalizeWindowsNTError(err error) error {
	status, ok := err.(windows.NTStatus)
	if !ok {
		return err
	}
	switch status {
	case windows.STATUS_REPARSE_POINT_ENCOUNTERED,
		windows.STATUS_STOPPED_ON_SYMLINK,
		windows.STATUS_REPARSE_POINT_NOT_RESOLVED:
		return ErrLink
	case windows.STATUS_NOT_A_DIRECTORY:
		return ErrNotDirectory
	case windows.STATUS_FILE_IS_A_DIRECTORY:
		return ErrNotRegular
	case windows.STATUS_OBJECT_NAME_COLLISION:
		return windows.ERROR_ALREADY_EXISTS
	default:
		return status.Errno()
	}
}

func classifyWindowsOpenError(err error) error {
	err = normalizeWindowsNTError(err)
	switch {
	case errors.Is(err, ErrLink), errors.Is(err, syscall.ELOOP):
		return ErrLink
	case errors.Is(err, windows.ERROR_ACCESS_DENIED), errors.Is(err, windows.ERROR_SHARING_VIOLATION):
		return ErrPermission
	case errors.Is(err, windows.ERROR_DIRECTORY), errors.Is(err, syscall.ENOTDIR), errors.Is(err, ErrNotDirectory):
		return ErrNotDirectory
	default:
		return err
	}
}

func publishWithinRootOptions(ctx context.Context, root *Root, components []string, data []byte, options PublishOptions) (result PublishResult, err error) {
	result.Outcome = PublishUnchanged
	if err := validateWindowsRelativeComponents(components); err != nil {
		return result, err
	}
	mode := options.Mode
	parent, parentsCreated, err := openWindowsPublishParent(root, components[:len(components)-1], mode == PublishCreate)
	if err != nil {
		if parentsCreated {
			return PublishResult{Outcome: PublishUnknown}, fmt.Errorf("%w: %v", ErrOutcomeUnknown, classifyWindowsOpenError(err))
		}
		return result, classifyWindowsOpenError(err)
	}
	defer parent.Close()
	defer func() {
		if err != nil && result.Outcome == PublishUnchanged && parentsCreated {
			result.Outcome = PublishUnknown
			err = fmt.Errorf("%w: %v", ErrOutcomeUnknown, err)
		}
	}()

	target := components[len(components)-1]
	if err := inspectWindowsPublishTarget(windows.Handle(parent.Fd()), target, mode); err != nil {
		return result, err
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}

	temp, err := createWindowsTemporaryFile(windows.Handle(parent.Fd()))
	if err != nil {
		return result, classifyWindowsOpenError(err)
	}
	cleanupTemp := true
	defer func() {
		if temp == nil {
			return
		}
		if cleanupTemp {
			cleanupErr := deleteWindowsFileHandle(temp)
			temp = nil
			if cleanupErr != nil {
				result.Outcome = PublishUnknown
				if err == nil {
					err = fmt.Errorf("%w: temporary cleanup failed: %v", ErrOutcomeUnknown, cleanupErr)
				} else {
					err = fmt.Errorf("%w: %v; temporary cleanup failed: %v", ErrOutcomeUnknown, err, cleanupErr)
				}
			}
			return
		}
		closeErr := temp.Close()
		temp = nil
		if closeErr != nil {
			result.Outcome = PublishUnknown
			if err == nil {
				err = fmt.Errorf("%w: close publication handle: %v", ErrOutcomeUnknown, closeErr)
			} else if !errors.Is(err, ErrOutcomeUnknown) {
				err = fmt.Errorf("%w: %v; close publication handle: %v", ErrOutcomeUnknown, err, closeErr)
			}
		}
	}()

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
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if mode == PublishReplace {
		if err := revalidateWindowsPublishTargetAndCopyDACL(
			windows.Handle(parent.Fd()),
			target,
			temp,
			options.ExpectedHash,
			options.ExpectedLimit,
		); err != nil {
			return result, err
		}
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}

	renameErr := renameWindowsFileHandle(
		windows.Handle(temp.Fd()),
		windows.Handle(parent.Fd()),
		target,
		mode == PublishReplace,
	)
	if renameErr != nil {
		mapped, unchanged := classifyWindowsRenameError(renameErr, mode)
		if unchanged {
			return result, mapped
		}
		// A status outside the explicitly understood no-publication set is not
		// safe to clean up: the handle might already name the published target.
		// Close it without issuing a delete and report the state as unknown.
		cleanupTemp = false
		result.Outcome = PublishUnknown
		return result, fmt.Errorf("%w: %v", ErrOutcomeUnknown, mapped)
	}

	cleanupTemp = false
	result.Outcome = PublishCompleted
	return result, nil
}

var snapshotFileIdentityForPlatform = func(file *os.File, _ os.FileInfo) (string, error) {
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(windows.Handle(file.Fd()), &info); err != nil {
		return "", err
	}
	index := uint64(info.FileIndexHigh)<<32 | uint64(info.FileIndexLow)
	return fmt.Sprintf("windows:%x:%x", info.VolumeSerialNumber, index), nil
}

func revalidateWindowsPublishTargetAndCopyDACL(parent windows.Handle, target string, temp *os.File, expectedHash string, limit int64) error {
	file, info, err := openWindowsFileRelative(parent, target, windowsPinnedReadShare)
	if errors.Is(err, os.ErrNotExist) {
		return ErrNotFound
	}
	if err != nil {
		return classifyWindowsOpenError(err)
	}
	closed := false
	defer func() {
		if !closed {
			_ = file.Close()
		}
	}()

	snapshot, err := readWindowsOpenFileSnapshot(file, info, limit)
	if err != nil {
		return err
	}
	if snapshotContentHash(snapshot.Data) != expectedHash {
		return ErrChanged
	}

	securityDescriptor, err := windows.GetSecurityInfo(
		windows.Handle(file.Fd()),
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION,
	)
	if err != nil {
		return classifyWindowsOpenError(err)
	}
	dacl, _, err := securityDescriptor.DACL()
	if err != nil {
		return classifyWindowsOpenError(err)
	}
	control, _, err := securityDescriptor.Control()
	if err != nil {
		return classifyWindowsOpenError(err)
	}
	securityInformation := windows.SECURITY_INFORMATION(windows.DACL_SECURITY_INFORMATION)
	if control&windows.SE_DACL_PROTECTED != 0 {
		securityInformation |= windows.PROTECTED_DACL_SECURITY_INFORMATION
	} else {
		securityInformation |= windows.UNPROTECTED_DACL_SECURITY_INFORMATION
	}
	if err := windows.SetSecurityInfo(
		windows.Handle(temp.Fd()),
		windows.SE_FILE_OBJECT,
		securityInformation,
		nil,
		nil,
		dacl,
		nil,
	); err != nil {
		return classifyWindowsOpenError(err)
	}
	runtime.KeepAlive(securityDescriptor)
	if err := temp.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	closed = true
	return nil
}

func readWindowsOpenFileSnapshot(file *os.File, info os.FileInfo, limit int64) (Snapshot, error) {
	if limit < 0 {
		return Snapshot{}, errors.New("secure file size limit is invalid")
	}
	if !info.Mode().IsRegular() {
		return Snapshot{}, ErrNotRegular
	}
	if info.Size() > limit {
		return Snapshot{}, ErrTooLarge
	}
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return Snapshot{}, fmt.Errorf("read secure file: %w", err)
	}
	if int64(len(data)) > limit {
		return Snapshot{}, ErrTooLarge
	}
	after, err := file.Stat()
	if err != nil {
		return Snapshot{}, fmt.Errorf("reinspect secure file: %w", err)
	}
	if !after.Mode().IsRegular() || !os.SameFile(info, after) || after.Size() != info.Size() || !after.ModTime().Equal(info.ModTime()) || int64(len(data)) != after.Size() {
		return Snapshot{}, ErrChanged
	}
	identity, err := snapshotFileIdentityForPlatform(file, after)
	if err != nil {
		return Snapshot{}, fmt.Errorf("identify secure file: %w", err)
	}
	return Snapshot{Data: data, Size: after.Size(), ModTime: after.ModTime(), Mode: after.Mode(), Identity: identity}, nil
}

func openWindowsPublishParent(root *Root, components []string, create bool) (*os.File, bool, error) {
	if err := validateWindowsRelativeComponents(components); err != nil {
		return nil, false, err
	}
	parent := windows.Handle(root.file.Fd())
	ownedParent := (*os.File)(nil)
	created := false
	if len(components) == 0 {
		file, err := duplicateWindowsDirectoryHandle(parent, ".")
		return file, false, err
	}
	for _, component := range components {
		directory, err := openWindowsDirectoryRelative(parent, component)
		if isWindowsNotFound(err) && create {
			directory, err = createWindowsDirectoryRelative(parent, component)
			if errors.Is(err, windows.ERROR_ALREADY_EXISTS) || errors.Is(err, windows.ERROR_FILE_EXISTS) {
				directory, err = openWindowsDirectoryRelative(parent, component)
			} else if err == nil {
				created = true
			}
		}
		if ownedParent != nil {
			_ = ownedParent.Close()
		}
		if err != nil {
			return nil, created, err
		}
		ownedParent = directory
		parent = windows.Handle(directory.Fd())
	}
	return ownedParent, created, nil
}

func createWindowsDirectoryRelative(parent windows.Handle, name string) (*os.File, error) {
	handle, err := ntCreateWindowsRelative(
		parent,
		name,
		windowsDirectoryAccess,
		windows.FILE_CREATE,
		windows.FILE_DIRECTORY_FILE|windows.FILE_SYNCHRONOUS_IO_NONALERT|windows.FILE_OPEN_REPARSE_POINT|windows.FILE_OPEN_FOR_BACKUP_INTENT,
		windows.FILE_ATTRIBUTE_NORMAL,
		windowsReadShare,
	)
	if err != nil {
		return nil, err
	}
	file, info, err := windowsFileFromHandle(handle, name)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		_ = file.Close()
		return nil, ErrNotDirectory
	}
	return file, nil
}

func inspectWindowsPublishTarget(parent windows.Handle, target string, mode PublishMode) error {
	file, info, err := openWindowsFileRelative(parent, target, windowsReadShare)
	if isWindowsNotFound(err) {
		if mode == PublishReplace {
			return ErrNotFound
		}
		return nil
	}
	if err != nil {
		return classifyWindowsOpenError(err)
	}
	defer file.Close()
	if mode == PublishCreate {
		return ErrAlreadyExists
	}
	if !info.Mode().IsRegular() {
		return ErrNotRegular
	}
	return nil
}

func createWindowsTemporaryFile(parent windows.Handle) (*os.File, error) {
	for attempt := 0; attempt < windowsTempAttempts; attempt++ {
		name, err := secureWindowsTempName()
		if err != nil {
			return nil, err
		}
		handle, err := ntCreateWindowsRelative(
			parent,
			name,
			windows.FILE_GENERIC_READ|windows.FILE_GENERIC_WRITE|windows.DELETE|windows.WRITE_DAC,
			windows.FILE_CREATE,
			windows.FILE_NON_DIRECTORY_FILE|windows.FILE_SYNCHRONOUS_IO_NONALERT|windows.FILE_OPEN_REPARSE_POINT|windows.FILE_OPEN_FOR_BACKUP_INTENT|windows.FILE_WRITE_THROUGH,
			windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_ATTRIBUTE_TEMPORARY,
			0,
		)
		if errors.Is(err, windows.ERROR_ALREADY_EXISTS) || errors.Is(err, windows.ERROR_FILE_EXISTS) {
			continue
		}
		if err != nil {
			return nil, err
		}
		file, info, err := windowsFileFromHandle(handle, name)
		if err != nil {
			return nil, err
		}
		if !info.Mode().IsRegular() {
			_ = deleteWindowsFileHandle(file)
			return nil, ErrNotRegular
		}
		return file, nil
	}
	return nil, windows.ERROR_ALREADY_EXISTS
}

func secureWindowsTempName() (string, error) {
	var value [12]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return ".edu-agent-" + hex.EncodeToString(value[:]), nil
}

type windowsFileRenameInformation struct {
	ReplaceIfExists uint32
	RootDirectory   windows.Handle
	FileNameLength  uint32
	FileName        [1]uint16
}

func renameWindowsFileHandle(source, parent windows.Handle, target string, replace bool) error {
	targetUTF16, err := windows.UTF16FromString(target)
	if err != nil {
		return err
	}
	targetUTF16 = targetUTF16[:len(targetUTF16)-1]
	var header windowsFileRenameInformation
	bufferSize := int(unsafe.Offsetof(header.FileName)) + len(targetUTF16)*2
	buffer := make([]byte, bufferSize)
	information := (*windowsFileRenameInformation)(unsafe.Pointer(&buffer[0]))
	if replace {
		information.ReplaceIfExists = 1
	}
	information.RootDirectory = parent
	information.FileNameLength = uint32(len(targetUTF16) * 2)
	copy(unsafe.Slice(&information.FileName[0], len(targetUTF16)), targetUTF16)
	err = windows.NtSetInformationFile(
		source,
		&windows.IO_STATUS_BLOCK{},
		&buffer[0],
		uint32(len(buffer)),
		windows.FileRenameInformation,
	)
	runtime.KeepAlive(buffer)
	return err
}

func classifyWindowsRenameError(err error, mode PublishMode) (error, bool) {
	status, ok := err.(windows.NTStatus)
	if ok {
		switch status {
		case windows.STATUS_OBJECT_NAME_COLLISION:
			if mode == PublishCreate {
				return ErrAlreadyExists, true
			}
			return windows.ERROR_ALREADY_EXISTS, true
		case windows.STATUS_OBJECT_NAME_NOT_FOUND, windows.STATUS_OBJECT_PATH_NOT_FOUND:
			return ErrNotFound, true
		case windows.STATUS_REPARSE_POINT_ENCOUNTERED,
			windows.STATUS_STOPPED_ON_SYMLINK,
			windows.STATUS_REPARSE_POINT_NOT_RESOLVED:
			return ErrLink, true
		case windows.STATUS_NOT_A_DIRECTORY:
			return ErrNotDirectory, true
		case windows.STATUS_FILE_IS_A_DIRECTORY:
			return ErrNotRegular, true
		case windows.STATUS_ACCESS_DENIED,
			windows.STATUS_SHARING_VIOLATION,
			windows.STATUS_CANNOT_DELETE,
			windows.STATUS_DELETE_PENDING:
			return ErrPermission, true
		default:
			return status.Errno(), false
		}
	}
	mapped := classifyWindowsOpenError(err)
	switch {
	case mode == PublishCreate && (errors.Is(mapped, windows.ERROR_ALREADY_EXISTS) || errors.Is(mapped, windows.ERROR_FILE_EXISTS)):
		return ErrAlreadyExists, true
	case errors.Is(mapped, ErrLink), errors.Is(mapped, ErrNotDirectory), errors.Is(mapped, ErrNotRegular), errors.Is(mapped, ErrPermission), isWindowsNotFound(mapped):
		return mapped, true
	default:
		return mapped, false
	}
}

type windowsFileDispositionInformationEx struct {
	Flags uint32
}

type windowsFileDispositionInformation struct {
	DeleteFile bool
}

func deleteWindowsFileHandle(file *os.File) error {
	if file == nil {
		return nil
	}
	handle := windows.Handle(file.Fd())
	information := windowsFileDispositionInformationEx{
		Flags: windows.FILE_DISPOSITION_DELETE |
			windows.FILE_DISPOSITION_FORCE_IMAGE_SECTION_CHECK |
			windows.FILE_DISPOSITION_POSIX_SEMANTICS |
			windows.FILE_DISPOSITION_IGNORE_READONLY_ATTRIBUTE,
	}
	err := windows.NtSetInformationFile(
		handle,
		&windows.IO_STATUS_BLOCK{},
		(*byte)(unsafe.Pointer(&information)),
		uint32(unsafe.Sizeof(information)),
		windows.FileDispositionInformationEx,
	)
	if status, ok := err.(windows.NTStatus); ok && (status == windows.STATUS_INVALID_INFO_CLASS || status == windows.STATUS_INVALID_PARAMETER || status == windows.STATUS_NOT_SUPPORTED) {
		fallback := windowsFileDispositionInformation{DeleteFile: true}
		err = windows.NtSetInformationFile(
			handle,
			&windows.IO_STATUS_BLOCK{},
			(*byte)(unsafe.Pointer(&fallback)),
			uint32(unsafe.Sizeof(fallback)),
			windows.FileDispositionInformation,
		)
	}
	if err != nil {
		_ = file.Close()
		return normalizeWindowsNTError(err)
	}
	return file.Close()
}

func isWindowsNotFound(err error) bool {
	err = normalizeWindowsNTError(err)
	return errors.Is(err, os.ErrNotExist) ||
		errors.Is(err, windows.ERROR_FILE_NOT_FOUND) ||
		errors.Is(err, windows.ERROR_PATH_NOT_FOUND)
}

func validateWindowsRelativeComponents(components []string) error {
	for _, component := range components {
		if err := validateWindowsRelativeComponent(component); err != nil {
			return err
		}
	}
	return nil
}

func validateWindowsRelativeComponent(component string) error {
	if component == "" || component == "." || component == ".." {
		return errors.New("secure relative path contains an invalid Windows component")
	}
	if strings.ContainsAny(component, `<>:"|?*`) || strings.HasSuffix(component, ".") || strings.HasSuffix(component, " ") {
		return errors.New("secure relative path contains a Windows stream, device, or invalid name")
	}
	for _, value := range component {
		if value < 0x20 {
			return errors.New("secure relative path contains a Windows control character")
		}
	}
	deviceBase := component
	if index := strings.IndexByte(deviceBase, '.'); index >= 0 {
		deviceBase = deviceBase[:index]
	}
	deviceBase = strings.ToUpper(deviceBase)
	switch deviceBase {
	case "CON", "PRN", "AUX", "NUL", "CLOCK$":
		return errors.New("secure relative path contains a reserved Windows device name")
	}
	if len(deviceBase) == 4 && (strings.HasPrefix(deviceBase, "COM") || strings.HasPrefix(deviceBase, "LPT")) && deviceBase[3] >= '1' && deviceBase[3] <= '9' {
		return errors.New("secure relative path contains a reserved Windows device name")
	}
	return nil
}

func replaceFile(from, to string) error {
	fromPtr, err := windows.UTF16PtrFromString(from)
	if err != nil {
		return err
	}
	toPtr, err := windows.UTF16PtrFromString(to)
	if err != nil {
		return err
	}
	return windows.MoveFileEx(fromPtr, toPtr, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH)
}

func syncDirectory(string) error { return nil }
