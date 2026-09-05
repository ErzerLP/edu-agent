package localexec

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	archiveSegmentBytes  = 256 << 10
	archiveMetadataBytes = 32 << 10
	archiveVersion       = 1
	archiveCallLimit     = 2 * time.Second
	archiveBindLimit     = 5 * time.Second
	archiveSettleLimit   = 4 * archiveCallLimit
)

// ArtifactStore owns encryption, atomic replacement, quotas and generation
// checks. Names are opaque ASCII identifiers; no path or key reaches localexec.
// Implementations must honor context deadlines and must not call back into the
// Manager or its controller. Each blob is at most 256 KiB (metadata is smaller).
// WriteArtifact errors may have an unknown publication outcome. The journal
// never advances its confirmed prefix on error or retries an uncertain write.
type ArtifactStore interface {
	ReadArtifact(context.Context, string) ([]byte, error)
	WriteArtifact(context.Context, string, []byte) error
	ListArtifacts(context.Context, string) ([]string, error)
}

type archiveBinding struct {
	mu        sync.RWMutex
	backend   ArtifactStore // binding.mu
	available bool          // Manager.mu; false on explicit detach
}

// Metadata is deliberately separate from execution inputs and Snapshot. No
// command, environment, stdin, PID, or executable process identity is stored.
// The call key is a one-way digest, only used to prevent accidental replay.
type archiveMetadata struct {
	Version           int       `json:"version"`
	TaskID            string    `json:"task_id"`
	CallDigest        string    `json:"call_digest"`
	State             string    `json:"state"`
	Reason            string    `json:"reason"`
	ExitCode          *int      `json:"exit_code"`
	Shell             string    `json:"shell"`
	StartedAt         time.Time `json:"started_at"`
	FinishedAt        time.Time `json:"finished_at"`
	Final             bool      `json:"final"`
	CleanupIncomplete bool      `json:"cleanup_incomplete"`
	StdoutReceived    int64     `json:"stdout_received"`
	StderrReceived    int64     `json:"stderr_received"`
	StdoutSaved       int64     `json:"stdout_saved"`
	StderrSaved       int64     `json:"stderr_saved"`
	Incomplete        bool      `json:"incomplete"`
	PersistenceError  string    `json:"persistence_error"`
	StartError        string    `json:"start_error"`
}

func digestCall(id string) string {
	sum := sha256.Sum256([]byte(id))
	return hex.EncodeToString(sum[:])
}

func (m *Manager) bindingLocked(owner string) *archiveBinding {
	if m.bindings[owner] == nil {
		m.bindings[owner] = &archiveBinding{}
	}
	return m.bindings[owner]
}

func archiveError(err error) string {
	var stable *Error
	if errors.As(err, &stable) {
		switch stable.Code {
		case "output_store_full", "output_unavailable", "output_corrupt", "output_version_unsupported":
			return stable.Code
		}
	}
	return "output_save_failed"
}

func validPersistenceError(code string) bool {
	switch code {
	case "", "output_store_full", "output_unavailable", "output_corrupt", "output_version_unsupported", "output_save_failed":
		return true
	}
	return false
}

func metadataName(id string) string { return "task_" + id }
func segmentName(id, stream string, index int64) string {
	return "out_" + id + "_" + stream + "_" + strconv.FormatInt(index, 10)
}
func validArtifactID(id string) bool {
	if len(id) == 0 || len(id) > 100 {
		return false
	}
	for _, c := range []byte(id) {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

// BindArchive atomically validates and loads an owner's journal before swapping
// its handle. A failed bind leaves the old binding and task collection intact.
// Existing in-process tasks are never replaced by historical records. Passing
// nil detaches storage and permanently stops current journals (no later islands);
// a subsequent bind can make their committed prefixes readable again.
func (m *Manager) BindArchive(owner string, backend ArtifactStore) error {
	if owner == "" {
		return failure("invalid_identity")
	}
	m.mu.Lock()
	binding := m.bindingLocked(owner)
	m.mu.Unlock()
	binding.mu.Lock()
	defer binding.mu.Unlock()
	loaded := make(map[string]archiveMetadata)
	if backend != nil {
		ctx, cancel := context.WithTimeout(context.Background(), archiveBindLimit)
		defer cancel()
		callCtx, callCancel := context.WithTimeout(ctx, archiveCallLimit)
		names, err := backend.ListArtifacts(callCtx, "task_")
		callCancel()
		if err != nil {
			return failure(archiveError(err))
		}
		if len(names) > m.options.MaxTasks {
			return failure("task_limit")
		}
		for _, name := range names {
			id, ok := strings.CutPrefix(name, "task_")
			if !ok || !validArtifactID(id) {
				return failure("output_corrupt")
			}
			if _, exists := loaded[id]; exists {
				return failure("output_corrupt")
			}
			callCtx, callCancel = context.WithTimeout(ctx, archiveCallLimit)
			data, err := backend.ReadArtifact(callCtx, name)
			callCancel()
			if err != nil {
				return failure(archiveError(err))
			}
			meta, err := decodeMetadata(data, id)
			if err != nil {
				return err
			}
			loaded[id] = meta
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	newCount := 0
	seenCalls := make(map[string]string)
	for id, meta := range loaded {
		if other, ok := seenCalls[meta.CallDigest]; ok && other != id {
			return failure("output_corrupt")
		}
		seenCalls[meta.CallDigest] = id
		if existing := m.tasks[id]; existing != nil {
			if existing.owner != owner || existing.callDigest != meta.CallDigest {
				return failure("output_corrupt")
			}
		} else {
			newCount++
		}
		if existing := m.calls[callKey{owner, meta.CallDigest}]; existing != nil && existing.snapshot.TaskID != id {
			return failure("output_corrupt")
		}
	}
	if len(m.tasks)+newCount > m.options.MaxTasks {
		return failure("task_limit")
	}
	if backend != nil {
		// Do not bind a different/older archive over bytes already committed by
		// this manager. Segment contents remain authenticated by the backend.
		for id, t := range m.tasks {
			if t.owner != owner || !t.journal {
				continue
			}
			meta, exists := loaded[id]
			if !exists || meta.StdoutSaved < t.stdout.saved || meta.StderrSaved < t.stderr.saved {
				return failure("output_unavailable")
			}
		}
	}
	binding.backend, binding.available = backend, backend != nil
	for _, t := range m.tasks {
		if t.owner != owner {
			continue
		}
		if backend == nil && t.journal && !t.terminal {
			t.archiveStopped = true
			t.persistenceError = "output_unavailable"
		}
		if backend != nil {
			t.archiveUnreadable = false
		}
	}
	for id, meta := range loaded {
		if m.tasks[id] != nil {
			continue
		}
		t := restoredTask(owner, binding, meta)
		m.tasks[id], m.calls[callKey{owner, meta.CallDigest}] = t, t
	}
	return nil
}

func decodeMetadata(data []byte, id string) (archiveMetadata, error) {
	var meta archiveMetadata
	if len(data) > archiveMetadataBytes {
		return meta, failure("output_corrupt")
	}
	// Inspect the version first so future schemas produce one stable error,
	// even when they introduce fields that this decoder does not recognize.
	var version struct {
		Version int `json:"version"`
	}
	if json.Unmarshal(data, &version) != nil {
		return meta, failure("output_corrupt")
	}
	if version.Version > archiveVersion {
		return meta, failure("output_version_unsupported")
	}
	if version.Version != archiveVersion {
		return meta, failure("output_corrupt")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&meta) != nil || decoder.Decode(new(any)) != io.EOF {
		return meta, failure("output_corrupt")
	}
	// Flat objects must have exactly the schema's keys, with no duplicates.
	keys := json.NewDecoder(bytes.NewReader(data))
	token, err := keys.Token()
	if err != nil || token != json.Delim('{') {
		return meta, failure("output_corrupt")
	}
	seen := make(map[string]bool)
	for keys.More() {
		token, err = keys.Token()
		key, ok := token.(string)
		if err != nil || !ok || seen[key] {
			return meta, failure("output_corrupt")
		}
		seen[key] = true
		switch key {
		case "version", "task_id", "call_digest", "state", "reason", "exit_code", "shell", "started_at", "finished_at", "final", "cleanup_incomplete", "stdout_received", "stderr_received", "stdout_saved", "stderr_saved", "incomplete", "persistence_error", "start_error":
		default:
			return meta, failure("output_corrupt")
		}
		var value json.RawMessage
		if keys.Decode(&value) != nil || key != "exit_code" && bytes.Equal(value, []byte("null")) {
			return meta, failure("output_corrupt")
		}
	}
	if len(seen) != 18 || meta.TaskID != id || !validArtifactID(id) || len(meta.CallDigest) != 64 {
		return meta, failure("output_corrupt")
	}
	if digest, err := hex.DecodeString(meta.CallDigest); err != nil || len(digest) != sha256.Size || strings.ToLower(meta.CallDigest) != meta.CallDigest {
		return meta, failure("output_corrupt")
	}
	if meta.StdoutSaved < 0 || meta.StderrSaved < 0 || meta.StdoutReceived < meta.StdoutSaved || meta.StderrReceived < meta.StderrSaved || meta.StdoutSaved > (1<<63-1)-meta.StderrSaved || !validPersistenceError(meta.PersistenceError) || len(meta.Shell) > 4096 {
		return meta, failure("output_corrupt")
	}
	if !validStartError(meta.StartError) || !validReason(meta.Reason) {
		return meta, failure("output_corrupt")
	}
	switch meta.State {
	case StateStarting, StateRunning, StateStopping, StateFinishing:
		if meta.Final || meta.ExitCode != nil || !meta.FinishedAt.IsZero() {
			return meta, failure("output_corrupt")
		}
	case StateExited, StateFailed, StateCanceled, StateTimedOut, StateUnknown:
		if !meta.Final || meta.FinishedAt.IsZero() {
			return meta, failure("output_corrupt")
		}
	default:
		return meta, failure("output_corrupt")
	}
	if meta.ExitCode != nil && (*meta.ExitCode < 0 || *meta.ExitCode > 255) {
		return meta, failure("output_corrupt")
	}
	return meta, nil
}

func validStartError(code string) bool {
	switch code {
	case "", "unsupported_platform", "start_canceled", "concurrency_limit", "output_limit", "manager_closed", "invalid_cwd", "cwd_unavailable", "invalid_argument", "invalid_timeout", "invalid_env", "shell_unavailable", "pipe_failed", "start_failed":
		return true
	}
	return false
}
func validReason(code string) bool {
	if validStartError(code) {
		return true
	}
	switch code {
	case "completed", "nonzero_exit", "signaled", "execution_timeout", "stopped", "exit_unknown":
		return true
	}
	return false
}

func restoredTask(owner string, binding *archiveBinding, meta archiveMetadata) *task {
	t := &task{
		owner: owner, binding: binding, callDigest: meta.CallDigest, journal: true,
		archiveStopped: true, persistenceError: meta.PersistenceError, startError: meta.StartError,
		terminal: true, stop: make(chan struct{}), done: make(chan struct{}),
		inputClosed: true, inputGate: make(chan struct{}, 1),
		outputIncomplete: !meta.Final || meta.Incomplete,
		snapshot: Snapshot{TaskID: meta.TaskID, State: meta.State, Reason: meta.Reason,
			ExitCode: meta.ExitCode, Shell: meta.Shell, StartedAt: meta.StartedAt,
			FinishedAt: meta.FinishedAt, CleanupIncomplete: meta.CleanupIncomplete, Restored: true},
	}
	t.stdout.received, t.stdout.saved = meta.StdoutReceived, meta.StdoutSaved
	t.stderr.received, t.stderr.saved = meta.StderrReceived, meta.StderrSaved
	if !meta.Final {
		t.snapshot.State, t.snapshot.Reason = StateUnknown, "exit_unknown"
		t.snapshot.ExitCode, t.snapshot.FinishedAt = nil, time.Time{}
	}
	t.inputGate <- struct{}{}
	close(t.done)
	return t
}

func (m *Manager) readableLocked(t *task, stream *outputStream) int64 {
	if t.binding.available && !t.archiveUnreadable {
		return max(stream.retained, stream.saved)
	}
	return stream.retained
}
func (m *Manager) persistenceErrorLocked(t *task) string {
	if t.journal && !t.binding.available {
		return "output_unavailable"
	}
	return t.persistenceError
}

func (m *Manager) persistenceLocked(t *task) string {
	if t.archiveUnreadable || t.journal && !t.binding.available {
		return "unavailable"
	}
	if t.persistenceError != "" {
		if t.stdout.saved+t.stderr.saved > 0 {
			return "partial"
		}
		return "failed"
	}
	if !t.journal {
		return "memory_only"
	}
	if !t.terminal {
		return "collecting"
	}
	if t.outputIncomplete || t.stdout.saved < t.stdout.received || t.stderr.saved < t.stderr.received {
		return "partial"
	}
	return "saved"
}

func (m *Manager) metadataLocked(t *task, final bool) archiveMetadata {
	return archiveMetadata{Version: archiveVersion, TaskID: t.snapshot.TaskID,
		CallDigest: t.callDigest, State: t.snapshot.State, Reason: t.snapshot.Reason,
		ExitCode: t.snapshot.ExitCode, Shell: t.snapshot.Shell, StartedAt: t.snapshot.StartedAt,
		FinishedAt: t.snapshot.FinishedAt, Final: final, CleanupIncomplete: t.snapshot.CleanupIncomplete,
		StdoutReceived: t.stdout.received, StderrReceived: t.stderr.received,
		StdoutSaved: t.stdout.saved, StderrSaved: t.stderr.saved, Incomplete: t.outputIncomplete,
		PersistenceError: t.persistenceError, StartError: t.startError}
}

func writeMetadata(backend ArtifactStore, meta archiveMetadata) error {
	data, err := json.Marshal(meta)
	if err != nil || len(data) > archiveMetadataBytes {
		return failure("output_save_failed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), archiveCallLimit)
	defer cancel()
	return backend.WriteArtifact(ctx, metadataName(meta.TaskID), data)
}

func (m *Manager) archiveFailedLocked(t *task, code string) {
	t.archiveStopped = true
	if t.persistenceError == "" {
		t.persistenceError = code
	}
}

func (m *Manager) initializeArchive(t *task) {
	t.archiveMu.Lock()
	defer t.archiveMu.Unlock()
	t.binding.mu.RLock()
	defer t.binding.mu.RUnlock()
	if t.binding.backend == nil {
		return
	}
	m.mu.Lock()
	meta := m.metadataLocked(t, false)
	m.mu.Unlock()
	err := writeMetadata(t.binding.backend, meta)
	m.mu.Lock()
	defer m.mu.Unlock()
	if err != nil {
		m.archiveFailedLocked(t, archiveError(err))
		return
	}
	t.journal = true
}

// A data transaction publishes the segment before its committed prefix. Failed
// metadata writes never advance saved; orphaned bytes are never discovered by
// scanning segments. Replacing the current segment preserves its old prefix.
func (m *Manager) saveOutput(t *task, stream *outputStream, name string, offset int64, data []byte) {
	m.mu.Lock()
	enabled := t.journal && !t.archiveStopped
	m.mu.Unlock()
	if !enabled {
		return
	}
	t.archiveMu.Lock()
	defer t.archiveMu.Unlock()
	t.binding.mu.RLock()
	defer t.binding.mu.RUnlock()
	m.mu.Lock()
	if !t.journal || t.archiveStopped || t.binding.backend == nil {
		m.mu.Unlock()
		t.stdoutTail, t.stderrTail = nil, nil
		return
	}
	if stream.saved != offset {
		m.archiveFailedLocked(t, "output_unavailable")
		m.mu.Unlock()
		return
	}
	room := m.options.SavedOutputBytesPerTask - t.stdout.saved - t.stderr.saved
	keep := min(int64(len(data)), room)
	m.mu.Unlock()
	tail := &t.stdoutTail
	if name == "stderr" {
		tail = &t.stderrTail
	}
	remaining := data[:keep]
	for len(remaining) > 0 {
		m.mu.Lock()
		saved := stream.saved
		settleAt := t.drainStarted
		m.mu.Unlock()
		count := min(len(remaining), archiveSegmentBytes-len(*tail))
		*tail = append(*tail, remaining[:count]...)
		ctx, cancel := context.WithTimeout(context.Background(), archiveCallLimit)
		if !settleAt.IsZero() {
			cancel()
			ctx, cancel = context.WithDeadline(context.Background(), minTime(time.Now().Add(archiveCallLimit), settleAt.Add(archiveSettleLimit)))
		}
		err := t.binding.backend.WriteArtifact(ctx, segmentName(t.snapshot.TaskID, name, saved/archiveSegmentBytes), *tail)
		cancel()
		if err == nil {
			m.mu.Lock()
			meta := m.metadataLocked(t, false)
			if name == "stdout" {
				meta.StdoutSaved += int64(count)
			} else {
				meta.StderrSaved += int64(count)
			}
			m.mu.Unlock()
			err = writeMetadata(t.binding.backend, meta)
		}
		m.mu.Lock()
		if err != nil {
			m.archiveFailedLocked(t, archiveError(err))
			m.mu.Unlock()
			t.stdoutTail, t.stderrTail = nil, nil
			return
		}
		stream.saved += int64(count)
		m.mu.Unlock()
		remaining = remaining[count:]
		if len(*tail) == archiveSegmentBytes {
			*tail = nil
		}
	}
	if keep < int64(len(data)) {
		m.mu.Lock()
		m.archiveFailedLocked(t, "output_store_full")
		m.mu.Unlock()
		t.stdoutTail, t.stderrTail = nil, nil
	}
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

// Final execution metadata is an independent, single settlement attempt even
// after a data/quota failure. The supervisor retains its lease until it returns.
func (m *Manager) finalizeArchive(t *task, final Snapshot) {
	t.archiveMu.Lock()
	defer t.archiveMu.Unlock()
	defer func() { t.stdoutTail, t.stderrTail = nil, nil }()
	t.binding.mu.RLock()
	defer t.binding.mu.RUnlock()
	m.mu.Lock()
	if !t.journal || t.binding.backend == nil {
		m.mu.Unlock()
		return
	}
	meta := m.metadataLocked(t, true)
	meta.State, meta.Reason, meta.ExitCode = final.State, final.Reason, final.ExitCode
	meta.FinishedAt, meta.CleanupIncomplete = final.FinishedAt, final.CleanupIncomplete
	m.mu.Unlock()
	if err := writeMetadata(t.binding.backend, meta); err != nil {
		m.mu.Lock()
		m.archiveFailedLocked(t, archiveError(err))
		m.mu.Unlock()
	}
}
