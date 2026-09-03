package offline

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"time"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/filelock"
)

type LeaseMode uint8

const (
	LeaseShared LeaseMode = iota + 1
	LeaseExclusive
)

type Lease struct {
	lock *filelock.Lock
	once sync.Once
	err  error
}

func AcquireLease(ctx context.Context, root string, mode LeaseMode, timeout time.Duration) (*Lease, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if timeout <= 0 {
		timeout = DefaultLeaseTimeout
	}
	if err := ensureRoot(root); err != nil {
		return nil, err
	}
	path, err := managedPath(root, ".lease", false)
	if err != nil {
		return nil, err
	}
	lockMode := filelock.Shared
	if mode == LeaseExclusive {
		lockMode = filelock.Exclusive
	} else if mode != LeaseShared {
		return nil, errors.New("offline lease mode is invalid")
	}
	lock, err := filelock.Acquire(ctx, filepath.Clean(path), lockMode, timeout)
	if errors.Is(err, filelock.ErrBusy) || errors.Is(err, context.DeadlineExceeded) {
		return nil, ErrProfileBusy
	}
	if err != nil {
		return nil, err
	}
	return &Lease{lock: lock}, nil
}

func (l *Lease) Close() error {
	if l == nil {
		return nil
	}
	l.once.Do(func() {
		if l.lock != nil {
			l.err = l.lock.Close()
			l.lock = nil
		}
	})
	return l.err
}
