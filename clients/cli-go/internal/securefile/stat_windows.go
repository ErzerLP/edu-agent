//go:build windows

package securefile

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"golang.org/x/sys/windows"
)

func windowsEntryInfo(info windows.ByHandleFileInformation) EntryInfo {
	kind := EntryFile
	switch {
	case info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0:
		kind = EntryLink
	case info.FileAttributes&windows.FILE_ATTRIBUTE_DEVICE != 0:
		kind = EntryOther
	case info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0:
		kind = EntryDirectory
	}
	identity := fmt.Sprintf("windows:%x:%x", info.VolumeSerialNumber, uint64(info.FileIndexHigh)<<32|uint64(info.FileIndexLow))
	size := int64(uint64(info.FileSizeHigh)<<32 | uint64(info.FileSizeLow))
	version := archiveMetadataVersion(fmt.Sprintf("%s|%s|%d|%d|%d:%d|%d:%d|%d", identity, kind, size,
		info.FileAttributes, info.LastWriteTime.HighDateTime, info.LastWriteTime.LowDateTime,
		info.CreationTime.HighDateTime, info.CreationTime.LowDateTime, info.NumberOfLinks))
	return EntryInfo{Kind: kind, Size: size, ModTime: time.Unix(0, info.LastWriteTime.Nanoseconds()).UTC(), Identity: identity, Version: version}
}

func windowsStatHandle(file *os.File) (EntryInfo, error) {
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(windows.Handle(file.Fd()), &info); err != nil {
		return EntryInfo{}, archiveWindowsError(err)
	}
	return windowsEntryInfo(info), nil
}

// All parents remain open without share-delete, pinning their ancestry. No
// path is reopened by absolute name and reparse parents are rejected.
func openWindowsStatParents(ctx context.Context, root *Root, components []string, handles *[]*os.File) (*os.File, error) {
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
		parent = next
	}
	return parent, nil
}

func closeStatHandles(handles []*os.File, err *error) {
	for i := len(handles) - 1; i >= 0; i-- {
		// Do not allow a close failure to be classified as absence.
		if closeErr := handles[i].Close(); closeErr != nil {
			*err = closeErr
		}
	}
}

func statWithinRoot(ctx context.Context, root *Root, components []string) (entry EntryInfo, err error) {
	if err := validateWindowsRelativeComponents(components); err != nil {
		return entry, err
	}
	var handles []*os.File
	defer func() { closeStatHandles(handles, &err) }()
	if len(components) == 0 {
		return windowsStatHandle(root.file)
	}
	parent, err := openWindowsStatParents(ctx, root, components[:len(components)-1], &handles)
	if err != nil {
		return entry, err
	}
	if err := ctx.Err(); err != nil {
		return entry, err
	}
	name := components[len(components)-1]
	// Attributes only: even a final reparse point is not dereferenced. Do not
	// use windowsFileFromHandle, which deliberately rejects links for readers.
	handle, err := ntCreateWindowsRelative(windows.Handle(parent.Fd()), name,
		windows.FILE_READ_ATTRIBUTES|windows.SYNCHRONIZE, windows.FILE_OPEN,
		windows.FILE_SYNCHRONOUS_IO_NONALERT|windows.FILE_OPEN_REPARSE_POINT|windows.FILE_OPEN_FOR_BACKUP_INTENT,
		windows.FILE_ATTRIBUTE_NORMAL, windowsReadShare)
	if err != nil {
		return entry, archiveWindowsError(err)
	}
	file := os.NewFile(uintptr(handle), name)
	if file == nil {
		_ = windows.CloseHandle(handle)
		return entry, errors.New("create secure stat handle")
	}
	handles = append(handles, file)
	entry, err = windowsStatHandle(file)
	if err == nil {
		err = ctx.Err()
	}
	return entry, err
}

func hashEntryWithinRoot(ctx context.Context, root *Root, components []string, expected EntryInfo, limit int64) (hash string, err error) {
	if err := validateWindowsRelativeComponents(components); err != nil {
		return "", err
	}
	var handles []*os.File
	defer func() { closeStatHandles(handles, &err) }()
	parent, err := openWindowsStatParents(ctx, root, components[:len(components)-1], &handles)
	if err != nil {
		return "", err
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	file, _, err := openWindowsFileRelative(windows.Handle(parent.Fd()), components[len(components)-1], windowsPinnedReadShare)
	if err != nil {
		return "", archiveWindowsError(err)
	}
	handles = append(handles, file)
	before, err := windowsStatHandle(file)
	if err != nil {
		return "", err
	}
	if before != expected {
		return "", ErrChanged
	}
	hash, err = readEntryHash(ctx, file, expected, limit)
	if err != nil {
		return "", err
	}
	after, err := windowsStatHandle(file)
	if err != nil {
		return "", err
	}
	if after != expected {
		return "", ErrChanged
	}
	return hash, ctx.Err()
}
