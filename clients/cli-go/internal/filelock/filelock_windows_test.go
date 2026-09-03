//go:build windows

package filelock

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"golang.org/x/sys/windows"
)

func TestFileLockWindowsRejectsFileAndDirectoryReparse(t *testing.T) {
	root := t.TempDir()
	fileTarget := filepath.Join(root, "target.lock")
	if err := os.WriteFile(fileTarget, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	fileLink := filepath.Join(root, "file-link.lock")
	createFileLockWindowsSymlink(t, fileLink, fileTarget)
	if lock, err := Acquire(t.Context(), fileLink, Exclusive, 0); err == nil {
		_ = lock.Close()
		t.Fatal("file reparse lock target was accepted")
	}

	directoryTarget := filepath.Join(root, "target-directory")
	if err := os.Mkdir(directoryTarget, 0o700); err != nil {
		t.Fatal(err)
	}
	directoryLink := filepath.Join(root, "directory-reparse")
	createFileLockWindowsJunction(t, directoryLink, directoryTarget)
	if lock, err := Acquire(t.Context(), directoryLink, Exclusive, 0); err == nil {
		_ = lock.Close()
		t.Fatal("directory reparse lock target was accepted")
	}
}

func createFileLockWindowsSymlink(t *testing.T, linkPath, targetPath string) {
	t.Helper()
	linkPointer, err := windows.UTF16PtrFromString(linkPath)
	if err != nil {
		t.Fatal(err)
	}
	targetPointer, err := windows.UTF16PtrFromString(targetPath)
	if err != nil {
		t.Fatal(err)
	}
	const allowUnprivilegedCreate = 0x2
	if err := windows.CreateSymbolicLink(linkPointer, targetPointer, allowUnprivilegedCreate); err != nil {
		t.Fatalf("create required Windows file reparse fixture: %s", safeFileLockWindowsError(err))
	}
}

func createFileLockWindowsJunction(t *testing.T, junctionPath, targetPath string) {
	t.Helper()
	if err := os.Mkdir(junctionPath, 0o700); err != nil {
		t.Fatal(err)
	}
	pointer, err := windows.UTF16PtrFromString(junctionPath)
	if err != nil {
		t.Fatal(err)
	}
	handle, err := windows.CreateFile(pointer, windows.GENERIC_WRITE, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		t.Fatalf("open Windows junction fixture: %s", safeFileLockWindowsError(err))
	}
	buffer, err := fileLockWindowsMountPointBuffer(targetPath)
	if err != nil {
		_ = windows.CloseHandle(handle)
		t.Fatal(err)
	}
	var returned uint32
	setErr := windows.DeviceIoControl(handle, windows.FSCTL_SET_REPARSE_POINT, &buffer[0], uint32(len(buffer)), nil, 0, &returned, nil)
	closeErr := windows.CloseHandle(handle)
	if setErr != nil {
		t.Fatalf("set Windows junction fixture: %s", safeFileLockWindowsError(setErr))
	}
	if closeErr != nil {
		t.Fatalf("close Windows junction fixture: %s", safeFileLockWindowsError(closeErr))
	}
}

func fileLockWindowsMountPointBuffer(targetPath string) ([]byte, error) {
	cleanTarget := filepath.Clean(targetPath)
	substitute, err := windows.UTF16FromString(`\??\` + cleanTarget)
	if err != nil {
		return nil, err
	}
	printName, err := windows.UTF16FromString(cleanTarget)
	if err != nil {
		return nil, err
	}
	substituteBytes := len(substitute) * 2
	printBytes := len(printName) * 2
	reparseLength := 8 + substituteBytes + printBytes
	if reparseLength > int(^uint16(0)) {
		return nil, errors.New("junction target is too long")
	}
	buffer := make([]byte, 8+reparseLength)
	binary.LittleEndian.PutUint32(buffer[0:4], windows.IO_REPARSE_TAG_MOUNT_POINT)
	binary.LittleEndian.PutUint16(buffer[4:6], uint16(reparseLength))
	binary.LittleEndian.PutUint16(buffer[8:10], 0)
	binary.LittleEndian.PutUint16(buffer[10:12], uint16(substituteBytes-2))
	binary.LittleEndian.PutUint16(buffer[12:14], uint16(substituteBytes))
	binary.LittleEndian.PutUint16(buffer[14:16], uint16(printBytes-2))
	offset := 16
	for _, value := range substitute {
		binary.LittleEndian.PutUint16(buffer[offset:offset+2], value)
		offset += 2
	}
	for _, value := range printName {
		binary.LittleEndian.PutUint16(buffer[offset:offset+2], value)
		offset += 2
	}
	return buffer, nil
}

func safeFileLockWindowsError(err error) string {
	var status windows.NTStatus
	if errors.As(err, &status) {
		return fmt.Sprintf("ntstatus=0x%08x", uint32(status))
	}
	var errno syscall.Errno
	if errors.As(err, &errno) {
		return fmt.Sprintf("win32=%d", uint32(errno))
	}
	return fmt.Sprintf("error_type=%T", err)
}
