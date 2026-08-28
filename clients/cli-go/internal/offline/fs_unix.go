//go:build !windows

package offline

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

func openReadNoFollow(path string) (*os.File, os.FileInfo, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		_ = unix.Close(fd)
		return nil, nil, errors.New("create offline file handle")
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, nil, err
	}
	return file, info, nil
}

func replaceFile(from, to string, replace bool) error {
	if !replace {
		if err := unix.Link(from, to); err != nil {
			return err
		}
		return os.Remove(from)
	}
	return os.Rename(from, to)
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

func isReparsePoint(string, os.FileInfo) bool { return false }

func enforcePrivateDirectory(_ string, info os.FileInfo) error {
	if info.Mode().Perm()&0o077 != 0 {
		return ErrUnsafePath
	}
	return nil
}

func enforcePrivateFile(_ string, info os.FileInfo) error {
	if info.Mode().Perm()&0o077 != 0 {
		return ErrUnsafePath
	}
	return nil
}

func openLeaseFile(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_RDWR|unix.O_CREAT|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		_ = unix.Close(fd)
		return nil, errors.New("create offline lease handle")
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		_ = file.Close()
		return nil, ErrUnsafePath
	}
	return file, nil
}
