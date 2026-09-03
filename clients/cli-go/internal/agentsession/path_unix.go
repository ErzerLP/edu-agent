//go:build !windows

package agentsession

import "os"

func sessionPathIsReparse(string, os.FileInfo) bool { return false }

func enforcePrivateSessionDirectory(path string, info os.FileInfo) error {
	if info.Mode().Perm()&0o077 == 0 {
		return nil
	}
	if err := os.Chmod(path, 0o700); err != nil {
		return err
	}
	updated, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if updated.Mode()&os.ModeSymlink != 0 || !updated.IsDir() || updated.Mode().Perm()&0o077 != 0 {
		return ErrInvalid
	}
	return nil
}

func enforcePrivateSessionFile(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return ErrInvalid
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return err
	}
	updated, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if updated.Mode()&os.ModeSymlink != 0 || !updated.Mode().IsRegular() || updated.Mode().Perm()&0o077 != 0 {
		return ErrInvalid
	}
	return nil
}

func validatePrivateSessionLock(path string) error {
	return enforcePrivateSessionFile(path)
}
