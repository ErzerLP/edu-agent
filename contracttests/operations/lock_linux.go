//go:build linux

package operations

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
)

type HostLock struct {
	file *os.File
}

func AcquireHostLock(path string) (*HostLock, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = file.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, fmt.Errorf("host qualification lock is held: %s", path)
		}
		return nil, err
	}
	return &HostLock{file: file}, nil
}

func (lock *HostLock) ConfigureChild(command *exec.Cmd) (int, error) {
	if lock == nil || lock.file == nil {
		return 0, errors.New("host qualification lock is unavailable")
	}
	command.ExtraFiles = append(command.ExtraFiles, lock.file)
	return 3 + len(command.ExtraFiles) - 1, nil
}

func (lock *HostLock) Close() error {
	if lock == nil || lock.file == nil {
		return nil
	}
	unlockErr := syscall.Flock(int(lock.file.Fd()), syscall.LOCK_UN)
	closeErr := lock.file.Close()
	if unlockErr != nil {
		return unlockErr
	}
	return closeErr
}
