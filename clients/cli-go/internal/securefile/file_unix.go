//go:build !windows

package securefile

import (
	"errors"
	"fmt"
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

func checkPrivateFile(info os.FileInfo) error {
	if info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("secure file permissions are too broad: %04o", info.Mode().Perm())
	}
	return nil
}

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

func replaceFile(from, to string) error { return os.Rename(from, to) }

func syncDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
