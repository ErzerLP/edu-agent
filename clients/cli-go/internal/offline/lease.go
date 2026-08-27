package offline

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

type LeaseMode uint8

const (
	LeaseShared LeaseMode = iota + 1
	LeaseExclusive
)

type Lease struct {
	file *os.File
	mode LeaseMode
}

func AcquireLease(ctx context.Context, root string, mode LeaseMode, timeout time.Duration) (*Lease, error) {
	if mode != LeaseShared && mode != LeaseExclusive {
		return nil, errors.New("invalid offline lease mode")
	}
	if timeout <= 0 {
		timeout = DefaultLeaseTimeout
	}
	if err := ensureRoot(root); err != nil {
		return nil, err
	}
	path, err := managedPath(root, ".lease", true)
	if err != nil {
		return nil, err
	}
	file, err := openLeaseFile(path)
	if err != nil {
		return nil, fmt.Errorf("open offline profile lease: %w", err)
	}
	deadline := time.Now().Add(timeout)
	for {
		locked, err := tryLockFile(file, mode)
		if err != nil {
			_ = file.Close()
			return nil, fmt.Errorf("acquire offline profile lease: %w", err)
		}
		if locked {
			return &Lease{file: file, mode: mode}, nil
		}
		if time.Now().After(deadline) {
			_ = file.Close()
			return nil, ErrProfileBusy
		}
		select {
		case <-ctx.Done():
			_ = file.Close()
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return nil, ErrProfileBusy
			}
			return nil, ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func (l *Lease) Close() error {
	if l == nil || l.file == nil {
		return nil
	}
	file := l.file
	l.file = nil
	unlockErr := unlockFile(file)
	closeErr := file.Close()
	if unlockErr != nil {
		return fmt.Errorf("release offline profile lease: %w", unlockErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close offline profile lease: %w", closeErr)
	}
	return nil
}

func leasePath(root string) string { return filepath.Join(root, ".lease") }
