//go:build !windows

package securefile

import (
	"fmt"
	"os"
)

// EnsurePrivateDirectory tightens an existing directory to user-only access.
func EnsurePrivateDirectory(path string) error {
	return ensurePrivatePath(path, true)
}

// EnsurePrivateFile tightens an existing regular file to user-only access.
func EnsurePrivateFile(path string) error {
	return ensurePrivatePath(path, false)
}

// CheckPrivateDirectory verifies that an existing directory is user-only.
func CheckPrivateDirectory(path string) error {
	return checkPrivatePath(path, true)
}

// CheckPrivateFile verifies that an existing regular file is user-only.
func CheckPrivateFile(path string) error {
	return checkPrivatePath(path, false)
}

func ensurePrivatePath(path string, directory bool) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return ErrLink
	}
	if directory && !info.IsDir() {
		return ErrNotDirectory
	}
	if !directory && !info.Mode().IsRegular() {
		return ErrNotRegular
	}
	mode := os.FileMode(0o600)
	if directory {
		mode = 0o700
	}
	if err := os.Chmod(path, mode); err != nil {
		return fmt.Errorf("protect private path: %w", err)
	}
	return checkPrivatePath(path, directory)
}

func checkPrivatePath(path string, directory bool) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return ErrLink
	}
	if directory && !info.IsDir() {
		return ErrNotDirectory
	}
	if !directory && !info.Mode().IsRegular() {
		return ErrNotRegular
	}
	if info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("%w: private path permissions are too broad: %04o", ErrPermission, info.Mode().Perm())
	}
	return nil
}

func checkPrivateOpenFile(_ *os.File, info os.FileInfo) error {
	if info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("%w: secure file permissions are too broad: %04o", ErrPermission, info.Mode().Perm())
	}
	return nil
}

func protectPrivateOpenFile(file *os.File, directory bool) error {
	mode := os.FileMode(0o600)
	if directory {
		mode = 0o700
	}
	if err := file.Chmod(mode); err != nil {
		return err
	}
	info, err := file.Stat()
	if err != nil {
		return err
	}
	return checkPrivateOpenFile(file, info)
}
