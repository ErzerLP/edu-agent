package modelclient

import (
	"context"
	"io"
	"sync"
	"time"
)

// inactivityTimeout cancels one request when the configured interval passes
// without any response-body bytes. A generation guards against an expired
// timer callback racing with a concurrent response notification.
type inactivityTimeout struct {
	mu         sync.Mutex
	timeout    time.Duration
	cancel     context.CancelCauseFunc
	timer      *time.Timer
	generation uint64
	stopped    bool
}

func newInactivityTimeout(parent context.Context, timeout time.Duration) (context.Context, *inactivityTimeout) {
	ctx, cancel := context.WithCancelCause(parent)
	monitor := &inactivityTimeout{timeout: timeout, cancel: cancel}
	monitor.mu.Lock()
	monitor.armLocked()
	monitor.mu.Unlock()
	return ctx, monitor
}

func (m *inactivityTimeout) Touch() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.stopped {
		return
	}
	m.generation++
	if m.timer != nil {
		m.timer.Stop()
	}
	m.armLocked()
}

func (m *inactivityTimeout) Stop() {
	m.mu.Lock()
	if m.stopped {
		m.mu.Unlock()
		return
	}
	m.stopped = true
	m.generation++
	if m.timer != nil {
		m.timer.Stop()
	}
	m.cancel(nil)
	m.mu.Unlock()
}

func (m *inactivityTimeout) armLocked() {
	generation := m.generation
	m.timer = time.AfterFunc(m.timeout, func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		if m.stopped || m.generation != generation {
			return
		}
		m.stopped = true
		m.cancel(context.DeadlineExceeded)
	})
}

type activityReader struct {
	reader io.Reader
	touch  func()
}

func (r activityReader) Read(buffer []byte) (int, error) {
	count, err := r.reader.Read(buffer)
	if count > 0 {
		r.touch()
	}
	return count, err
}

func requestContextError(parent, request context.Context) error {
	if err := parent.Err(); err != nil {
		return err
	}
	if cause := context.Cause(request); cause != nil {
		return cause
	}
	return request.Err()
}
