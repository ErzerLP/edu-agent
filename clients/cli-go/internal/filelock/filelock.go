package filelock

import (
	"context"
	"errors"
	"os"
	"sync"
	"time"
)

var ErrBusy = errors.New("文件锁正在使用")

type Mode uint8

const (
	Shared Mode = iota + 1
	Exclusive
)

// Lock owns both the platform lock and its file handle.
type Lock struct {
	file *os.File
	once sync.Once
	err  error
}

// Acquire opens a regular, non-link lock file and waits for a platform lock up
// to timeout. A non-positive timeout performs one immediate attempt.
func Acquire(ctx context.Context, path string, mode Mode, timeout time.Duration) (*Lock, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if mode != Shared && mode != Exclusive {
		return nil, errors.New("文件锁模式无效")
	}
	file, err := openLockFile(path)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(timeout)
	for {
		if err := ctx.Err(); err != nil {
			_ = file.Close()
			return nil, err
		}
		locked, lockErr := tryPlatformLock(file, mode)
		if lockErr != nil {
			_ = file.Close()
			return nil, lockErr
		}
		if locked {
			return &Lock{file: file}, nil
		}
		if timeout <= 0 || !time.Now().Before(deadline) {
			_ = file.Close()
			return nil, ErrBusy
		}
		wait := 25 * time.Millisecond
		if remaining := time.Until(deadline); remaining < wait {
			wait = remaining
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			_ = file.Close()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

func (l *Lock) Close() error {
	if l == nil {
		return nil
	}
	l.once.Do(func() {
		if l.file == nil {
			return
		}
		file := l.file
		l.file = nil
		unlockErr := platformUnlock(file)
		closeErr := file.Close()
		if unlockErr != nil {
			l.err = unlockErr
		} else {
			l.err = closeErr
		}
	})
	return l.err
}
