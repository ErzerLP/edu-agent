//go:build windows

package securefile

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
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
		return nil, fmt.Errorf("open secure root without following links: %w", err)
	}
	return root, nil
}

func (r *Root) ReadLimit(relative string, limit int64, private bool) ([]byte, error) {
	if r == nil || r.file == nil {
		return nil, errors.New("secure root is closed")
	}
	components, err := relativeComponents(relative)
	if err != nil {
		return nil, err
	}
	file, info, err := openReadWithinRoot(r, components)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("open secure file within root: %w", err)
	}
	return readOpenFile(file, info, limit, private)
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
		return nil, errors.New("secure root is not a directory")
	}
	resolvedPath, err := resolvedHandlePath(windows.Handle(file.Fd()))
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	return &Root{file: file, path: absolutePath, resolvedPath: resolvedPath}, nil
}

func openReadWithinRoot(root *Root, components []string) (*os.File, os.FileInfo, error) {
	currentPath := root.path
	for index, component := range components {
		if strings.Contains(component, ":") {
			return nil, nil, errors.New("secure relative path contains a Windows stream or volume separator")
		}
		currentPath = filepath.Join(currentPath, component)
		if index == len(components)-1 {
			break
		}
		directory, _, err := openDirectoryNoFollow(currentPath)
		if err != nil {
			return nil, nil, err
		}
		resolvedPath, err := resolvedHandlePath(windows.Handle(directory.Fd()))
		_ = directory.Close()
		if err != nil {
			return nil, nil, err
		}
		if !resolvedPathWithinRoot(root.resolvedPath, resolvedPath) {
			return nil, nil, errors.New("secure directory resolves outside its root")
		}
	}
	file, info, err := openReadNoFollow(currentPath)
	if err != nil {
		return nil, nil, err
	}
	resolvedPath, err := resolvedHandlePath(windows.Handle(file.Fd()))
	if err != nil {
		_ = file.Close()
		return nil, nil, err
	}
	if !resolvedPathWithinRoot(root.resolvedPath, resolvedPath) {
		_ = file.Close()
		return nil, nil, errors.New("secure file resolves outside its root")
	}
	return file, info, nil
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
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT|extraFlags,
		0,
	)
	if err != nil {
		return nil, nil, err
	}
	var handleInfo windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &handleInfo); err != nil {
		_ = windows.CloseHandle(handle)
		return nil, nil, err
	}
	if handleInfo.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		_ = windows.CloseHandle(handle)
		return nil, nil, errors.New("secure file is a reparse point")
	}
	file := os.NewFile(uintptr(handle), path)
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

func resolvedPathWithinRoot(rootPath, candidatePath string) bool {
	rootPath = strings.TrimRight(filepath.Clean(rootPath), `\\/`)
	candidatePath = filepath.Clean(candidatePath)
	if strings.EqualFold(rootPath, candidatePath) {
		return false
	}
	return strings.HasPrefix(strings.ToLower(candidatePath), strings.ToLower(rootPath+string(filepath.Separator)))
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
