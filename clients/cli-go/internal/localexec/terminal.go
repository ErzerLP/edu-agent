package localexec

import (
	"context"
	"os"
)

const (
	DefaultRows          = 24
	DefaultCols          = 80
	MaxTerminalDimension = 4096
)

func validTerminalSize(rows, cols int) bool {
	return rows >= 1 && rows <= MaxTerminalDimension && cols >= 1 && cols <= MaxTerminalDimension
}

func terminalSize(args StartArgs) (int, int, string) {
	if !args.PTY {
		if args.Rows != 0 || args.Cols != 0 {
			return 0, 0, "invalid_terminal_size"
		}
		return 0, 0, ""
	}
	rows, cols := args.Rows, args.Cols
	if rows == 0 {
		rows = DefaultRows
	}
	if cols == 0 {
		cols = DefaultCols
	}
	if !validTerminalSize(rows, cols) {
		return DefaultRows, DefaultCols, "invalid_terminal_size"
	}
	return rows, cols, ""
}

// Interrupt writes the current VINTR byte through the terminal line discipline.
// Written means accepted bytes, not that ISIG is enabled or a program exited.
func (m *Manager) Interrupt(ctx context.Context, owner, taskID string) (InputResult, error) {
	return m.writeInput(ctx, owner, taskID, nil, "interrupt")
}

// SendEOF writes the current VEOF byte. In canonical mode it makes pending input
// readable (or produces one zero-length read); it never closes the terminal.
// In raw mode the same byte can be ordinary input. Disabled controls are errors.
func (m *Manager) SendEOF(ctx context.Context, owner, taskID string) (InputResult, error) {
	return m.writeInput(ctx, owner, taskID, nil, "eof")
}

// terminalInputLocked requires inputMu. A restored mode flag is never a control
// capability: only the live input descriptor installed after Start grants one.
func terminalInputLocked(t *task) (*os.File, error) {
	if !t.snapshot.PTY {
		return nil, failure("pty_required")
	}
	if t.inputClosed || t.input == nil {
		return nil, failure("pty_not_active")
	}
	return t.input, nil
}

// Resize applies a real TIOCSWINSZ. Kernel terminal job control delivers SIGWINCH;
// no PID/PGID lookup or numeric signal is used. Repeated sizes remain real calls.
func (m *Manager) Resize(owner, taskID string, rows, cols int) (Snapshot, error) {
	m.mu.Lock()
	t, err := m.taskLocked(owner, taskID)
	m.mu.Unlock()
	if err != nil {
		return Snapshot{}, err
	}
	t.inputMu.Lock()
	file, err := terminalInputLocked(t)
	if err == nil && !validTerminalSize(rows, cols) {
		err = failure("invalid_terminal_size")
	}
	if err == nil {
		if resizeTerminal(file, rows, cols) != nil {
			err = failure("pty_resize_failed")
		} else {
			// No terminal syscall spans Manager.mu. A successful resize must be
			// reflected before the supervisor revokes input and finalizes metadata.
			// Start is the only inverse lock order: it cannot encounter a nonnil
			// input here until it has released inputMu for the last time.
			m.mu.Lock()
			t.snapshot.Rows, t.snapshot.Cols = rows, cols
			m.mu.Unlock()
		}
	}
	t.inputMu.Unlock()
	if err == nil {
		m.saveTerminalMetadata(t)
	}
	snapshot, _ := m.Status(owner, taskID)
	return snapshot, err
}

func (m *Manager) saveTerminalMetadata(t *task) {
	t.archiveMu.Lock()
	defer t.archiveMu.Unlock()
	t.binding.mu.RLock()
	defer t.binding.mu.RUnlock()
	m.mu.Lock()
	if !t.journal || t.archiveStopped || t.binding.backend == nil || t.settling || t.terminal {
		m.mu.Unlock()
		return
	}
	meta := m.metadataLocked(t, false)
	m.mu.Unlock()
	if err := writeMetadata(t.binding.backend, meta); err != nil {
		m.mu.Lock()
		m.archiveFailedLocked(t, archiveError(err))
		m.mu.Unlock()
	}
}
