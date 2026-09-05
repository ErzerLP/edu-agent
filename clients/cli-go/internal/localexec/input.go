package localexec

import (
	"context"
	"errors"
	"os"
	"time"
)

// Written is the number of bytes synchronously acknowledged by the pipe write,
// NOT a claim that the child processed them. Outcome is written, partial, or
// not_written. We never queue an asynchronous write or retry an ambiguous input;
// the synchronous pollable OS pipe reports the exact count even on cancellation.
type InputResult struct {
	Written int
	Outcome string
}

// WriteInput serializes whole calls in input-gate acquisition order. Callers
// needing a specific order must await each call. Concurrent CloseInput wins
// immediately, unblocks the current writer, and prevents every later write.
// Each call is capped at MaxInputBytes and 30s (or its earlier context deadline).
// No helper goroutine can continue writing after this method returns.
func (m *Manager) WriteInput(ctx context.Context, owner, taskID string, data []byte) (InputResult, error) {
	result := InputResult{Outcome: "not_written"}
	m.mu.Lock()
	t, err := m.taskLocked(owner, taskID)
	m.mu.Unlock()
	if err != nil {
		return result, err
	}
	if len(data) > MaxInputBytes {
		return result, failure("input_limit")
	}
	ctx, cancel := context.WithTimeout(ctx, inputCallLimit)
	defer cancel()
	select {
	case <-ctx.Done():
		return result, failure("input_canceled")
	case <-t.inputGate:
	}
	defer func() { t.inputGate <- struct{}{} }()
	if ctx.Err() != nil {
		return result, failure("input_canceled")
	}
	t.inputMu.Lock()
	file, closed := t.input, t.inputClosed
	t.inputMu.Unlock()
	if closed {
		return result, failure("stdin_closed")
	}
	if file == nil {
		return result, failure("stdin_not_ready")
	}
	deadline, _ := ctx.Deadline()
	if err := file.SetWriteDeadline(deadline); err != nil {
		return result, failure("stdin_closed")
	}
	interrupted := make(chan struct{})
	stopInterrupt := context.AfterFunc(ctx, func() {
		_ = file.SetWriteDeadline(time.Now())
		close(interrupted)
	})
	n, writeErr := file.Write(data)
	// Join a cancellation callback before releasing the gate: a late callback
	// must never apply a stale deadline to the NEXT writer.
	if !stopInterrupt() {
		<-interrupted
	}
	result.Written = n
	if n == len(data) {
		result.Outcome = "written"
		return result, nil
	}
	if n > 0 {
		result.Outcome = "partial"
	}
	if ctx.Err() != nil {
		return result, failure("input_canceled")
	}
	if errors.Is(writeErr, os.ErrDeadlineExceeded) {
		return result, failure("input_timeout")
	}
	if errors.Is(writeErr, os.ErrClosed) {
		return result, failure("stdin_closed")
	}
	return result, failure("input_failed")
}

func (m *Manager) CloseInput(owner, taskID string) error {
	m.mu.Lock()
	t, err := m.taskLocked(owner, taskID)
	m.mu.Unlock()
	if err != nil {
		return err
	}
	closeTaskInput(t)
	return nil
}

func closeTaskInput(t *task) {
	t.inputMu.Lock()
	if !t.inputClosed {
		t.inputClosed = true
		file := t.input
		t.input = nil
		t.inputMu.Unlock()
		closeFile(file)
		return
	}
	t.inputMu.Unlock()
}
