// Package localexec manages local shells and bounded output, optionally in a PTY.
// Optional opaque archives provide persistence without exposing keys or paths.
// It deliberately provides no workspace sandbox or credentials.
package localexec

import (
	"context"
	"crypto/rand"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	DefaultMaxTasks                      = 256
	DefaultMaxConcurrent                 = 16
	DefaultOutputBytesPerTask            = 8 << 20
	DefaultOutputBytesTotal              = 64 << 20
	DefaultSavedOutputBytesPerTask int64 = 128 << 20
	DefaultStopGrace                     = 2 * time.Second
	MaxInputBytes                        = 64 << 10
	MaxReadBytes                         = 64 << 10

	StateStarting  = "starting"
	StateRunning   = "running"
	StateStopping  = "stopping"
	StateFinishing = "finishing"
	StateExited    = "exited"
	StateFailed    = "failed"
	StateCanceled  = "canceled"
	StateTimedOut  = "timed_out"
	StateUnknown   = "unknown"
)

const (
	inputCallLimit   = 30 * time.Second
	killSettleLimit  = time.Second
	outputDrainLimit = 250 * time.Millisecond
	pollInterval     = 20 * time.Millisecond
)

// Error contains only a stable code, never an underlying OS error or input.
// Use errors.As to extract Code. Start may return both a task snapshot and Error.
type Error struct{ Code string }

func (e *Error) Error() string  { return "localexec: " + e.Code }
func failure(code string) error { return &Error{Code: code} }

// Nonpositive option values select the documented defaults.
type Options struct {
	MaxTasks                int
	MaxConcurrent           int
	OutputBytesPerTask      int
	OutputBytesTotal        int
	SavedOutputBytesPerTask int64
	StopGrace               time.Duration
}

type StartArgs struct {
	Command   string
	CWD       string
	Shell     string
	Env       map[string]*string
	Stdin     bool
	PTY       bool
	Rows      int
	Cols      int
	TimeoutMS int64
}

// Snapshot is safe to project as task metadata: no command, env, stdin, or raw errors.
// A terminal State can have CleanupIncomplete set. Controllable becomes false when
// the supervisor retires; it never sends signals using retired task identities.
type Snapshot struct {
	TaskID            string
	State             string
	Reason            string
	ExitCode          *int
	Shell             string
	StartedAt         time.Time
	FinishedAt        time.Time
	Controllable      bool
	CleanupIncomplete bool
	StdoutBytes       int64
	StderrBytes       int64
	StdoutRetained    int64
	StderrRetained    int64
	OutputState       string
	StdoutSaved       int64
	StderrSaved       int64
	Persistence       string
	PersistenceError  string
	Restored          bool
	PTY               bool
	Rows              int
	Cols              int
}

type callKey struct{ owner, id string }

type task struct {
	owner             string
	snapshot          Snapshot
	startError        string
	active            bool
	reaped            bool
	terminal          bool
	settling          bool
	stopReason        string
	stop              chan struct{}
	done              chan struct{}
	stdout            outputStream
	stderr            outputStream
	outputIncomplete  bool
	callDigest        string
	binding           *archiveBinding
	outputGuarded     bool // output collected under a persistent privacy authority
	journal           bool
	archiveStopped    bool
	archiveUnreadable bool
	persistenceError  string
	drainStarted      time.Time

	// archiveMu serializes segment+metadata transactions, never process control.
	// Its lock order is archiveMu -> binding.mu -> Manager.mu. Tails are guarded
	// by archiveMu; all other task/output bookkeeping uses Manager.mu.
	archiveMu              sync.Mutex
	stdoutTail, stderrTail []byte

	// Input is independently synchronized; never hold Manager.mu across pipe I/O.
	inputMu     sync.Mutex
	input       *os.File
	inputClosed bool
	inputGate   chan struct{}
}

// Manager must be constructed with New and must not be copied. All output and
// execution bookkeeping is retained until the Manager itself is discarded.
type Manager struct {
	mu        sync.Mutex
	options   Options
	prefix    string
	sequence  uint64
	tasks     map[string]*task
	calls     map[callKey]*task
	bindings  map[string]*archiveBinding
	active    int
	retained  int64
	closed    bool
	closeDone chan struct{}
}

func New(options Options) *Manager {
	if options.MaxTasks <= 0 {
		options.MaxTasks = DefaultMaxTasks
	}
	if options.MaxConcurrent <= 0 {
		options.MaxConcurrent = DefaultMaxConcurrent
	}
	if options.OutputBytesPerTask <= 0 {
		options.OutputBytesPerTask = DefaultOutputBytesPerTask
	}
	if options.OutputBytesTotal <= 0 {
		options.OutputBytesTotal = DefaultOutputBytesTotal
	}
	if options.SavedOutputBytesPerTask <= 0 {
		options.SavedOutputBytesPerTask = DefaultSavedOutputBytesPerTask
	}
	if options.StopGrace <= 0 {
		options.StopGrace = DefaultStopGrace
	}
	return &Manager{options: options, prefix: rand.Text(), tasks: make(map[string]*task), calls: make(map[callKey]*task), bindings: make(map[string]*archiveBinding), closeDone: make(chan struct{})}
}

// Start uses ctx only until launch. Once launched, only TimeoutMS, Stop, or Close
// ends execution. Deduplication is by owner+callID, including failed starts (not
// by argument equality). Invalid identities and exhausted record capacity cannot
// allocate a task record. Other launch rejections retain an inspectable identity.
func (m *Manager) Start(ctx context.Context, owner, callID string, args StartArgs) (Snapshot, error) {
	if owner == "" || callID == "" {
		return Snapshot{}, failure("invalid_identity")
	}
	m.mu.Lock()
	key := callKey{owner, digestCall(callID)}
	if existing := m.calls[key]; existing != nil {
		snapshot := m.snapshotLocked(existing)
		code := existing.startError
		m.mu.Unlock()
		if code != "" {
			return snapshot, failure(code)
		}
		return snapshot, nil
	}
	if m.closed {
		m.mu.Unlock()
		return Snapshot{}, failure("manager_closed")
	}
	if len(m.tasks) >= m.options.MaxTasks {
		m.mu.Unlock()
		return Snapshot{}, failure("task_limit")
	}
	m.sequence++
	t := &task{owner: owner, callDigest: key.id, binding: m.bindingLocked(owner), snapshot: Snapshot{TaskID: m.prefix + "-" + strconv.FormatUint(m.sequence, 10), State: StateStarting}, stop: make(chan struct{}), done: make(chan struct{}), inputGate: make(chan struct{}, 1)}
	t.outputGuarded = t.binding.available
	// Always publish valid mode metadata, even for rejected dimensions. Invalid
	// requested sizes are not execution inputs retained in the journal.
	rows, cols, sizeCode := terminalSize(args)
	t.snapshot.PTY, t.snapshot.Rows, t.snapshot.Cols = args.PTY, rows, cols
	t.inputClosed = !args.Stdin && !args.PTY
	t.inputGate <- struct{}{}
	m.tasks[t.snapshot.TaskID], m.calls[key] = t, t
	code := ""
	switch {
	case !platformSupported():
		code = "unsupported_platform"
	case sizeCode != "":
		code = sizeCode
	case ctx.Err() != nil:
		code = "start_canceled"
	case m.active >= m.options.MaxConcurrent:
		code = "concurrency_limit"
	case m.retained >= int64(m.options.OutputBytesTotal) && !t.binding.available:
		code = "output_limit"
	}
	if code == "" {
		t.active = true
		m.active++
	}
	m.mu.Unlock()

	// Publish a non-executable record before any launch-side OS effects. A
	// storage failure does not block execution while memory remains available.
	m.initializeArchive(t)
	if code != "" {
		return m.startFailed(t, code)
	}
	m.mu.Lock()
	noOutputRoom := m.retained >= int64(m.options.OutputBytesTotal) && (!t.journal || t.archiveStopped || !t.binding.available)
	m.mu.Unlock()
	if noOutputRoom {
		return m.startFailed(t, "output_limit")
	}

	cmd, code := prepare(args)
	if code != "" {
		return m.startFailed(t, code)
	}
	m.mu.Lock()
	t.snapshot.Shell = cmd.Path
	m.mu.Unlock()
	var pipes *processPipes
	var err error
	if args.PTY {
		pipes, err = openPTY(cmd, rows, cols)
	} else {
		pipes, err = openPipes(cmd, args.Stdin)
	}
	if err != nil {
		if args.PTY {
			return m.startFailed(t, "pty_failed")
		}
		return m.startFailed(t, "pipe_failed")
	}
	// Serialize the actual launch with shutdown's transition. No published task
	// can slip past Close's launch barrier; reservations and validation occur first.
	m.mu.Lock()
	if ctx.Err() != nil || t.stopReason != "" || m.closed {
		code = "start_canceled"
		if m.closed {
			code = "manager_closed"
		}
		m.mu.Unlock()
		pipes.closeAll()
		return m.startFailed(t, code)
	}
	err = cmd.Start() // NOT CommandContext: caller cancellation must not kill tasks.
	if err != nil {
		m.mu.Unlock()
		pipes.closeAll()
		return m.startFailed(t, "start_failed")
	}
	t.snapshot.State = StateRunning
	t.snapshot.StartedAt = time.Now()
	t.snapshot.Controllable = true
	t.inputMu.Lock()
	if t.inputClosed {
		closeFile(pipes.stdinWriter)
	} else {
		t.input = pipes.stdinWriter
	}
	t.inputMu.Unlock()
	m.mu.Unlock()
	pipes.closeChildEnds()
	// exec needs these only during Start, and we do not retain sensitive arguments.
	cmd.Args, cmd.Env, cmd.Dir = nil, nil, ""
	go m.supervise(t, cmd, pipes, time.Duration(args.TimeoutMS)*time.Millisecond)
	return m.Status(owner, t.snapshot.TaskID)
}

func prepare(args StartArgs) (*exec.Cmd, string) {
	if args.CWD == "" || strings.ContainsRune(args.CWD, 0) {
		return nil, "invalid_cwd"
	}
	if strings.ContainsRune(args.Command, 0) || strings.ContainsRune(args.Shell, 0) {
		return nil, "invalid_argument"
	}
	if args.TimeoutMS < 0 || args.TimeoutMS > int64((1<<63-1)/int64(time.Millisecond)) {
		return nil, "invalid_timeout"
	}
	for key, value := range args.Env {
		// Match the OS environment boundary: names cannot be empty, contain NUL,
		// or contain the '=' separator. Do not impose a shell-variable whitelist.
		if key == "" || strings.ContainsAny(key, "=\x00") || (value != nil && strings.ContainsRune(*value, 0)) {
			return nil, "invalid_env"
		}
	}
	cwd, err := filepath.Abs(args.CWD)
	if err != nil {
		return nil, "cwd_unavailable"
	}
	info, err := os.Stat(cwd)
	if err != nil || !info.IsDir() {
		return nil, "cwd_unavailable"
	}
	// Empty Shell prefers the launcher's available user shell, then /bin/sh.
	// Overrides in args.Env affect the child, not how we choose that shell.
	resolved, err := resolveShell(args.Shell, cwd)
	if err != nil {
		return nil, "shell_unavailable"
	}
	cmd := exec.Command(resolved, "-c", args.Command)
	cmd.Dir = cwd
	cmd.Env = mergedEnvironment(args.Env)
	configureProcessGroup(cmd)
	return cmd, ""
}

func resolveShell(shell, cwd string) (string, error) {
	lookup := func(candidate string) (string, error) {
		// Explicit relative paths are relative to cwd; bare names use the
		// launcher's PATH. No fallback for an explicitly supplied shell.
		if strings.ContainsRune(candidate, os.PathSeparator) && !filepath.IsAbs(candidate) {
			candidate = filepath.Join(cwd, candidate)
		}
		resolved, err := exec.LookPath(candidate)
		if err != nil {
			return "", err
		}
		return filepath.Abs(resolved)
	}
	if shell != "" {
		return lookup(shell)
	}
	if preferred := os.Getenv("SHELL"); preferred != "" {
		if resolved, err := lookup(preferred); err == nil {
			return resolved, nil
		}
	}
	return lookup("/bin/sh")
}

func mergedEnvironment(overrides map[string]*string) []string {
	env := make(map[string]string)
	for _, entry := range os.Environ() {
		if key, value, ok := strings.Cut(entry, "="); ok {
			env[key] = value
		}
	}
	for key, value := range overrides {
		if value == nil {
			delete(env, key)
		} else {
			env[key] = *value
		}
	}
	keys := make([]string, 0, len(env))
	for key := range env {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := make([]string, 0, len(keys))
	for _, key := range keys {
		result = append(result, key+"="+env[key])
	}
	return result
}

func (m *Manager) startFailed(t *task, code string) (Snapshot, error) {
	m.mu.Lock()
	t.startError = code
	t.settling = true
	final := t.snapshot
	final.State, final.Reason = StateFailed, code
	if code == "start_canceled" || code == "manager_closed" {
		final.State = StateCanceled
	}
	final.FinishedAt = time.Now()
	m.mu.Unlock()
	m.finalizeArchive(t, final)
	m.mu.Lock()
	defer m.mu.Unlock()
	applyFinalLocked(t, final)
	t.terminal = true
	m.releaseActiveLocked(t)
	close(t.done)
	return m.snapshotLocked(t), failure(code)
}

// Preserve immutable task identity fields, which also name journal blobs.
func applyFinalLocked(t *task, final Snapshot) {
	t.snapshot.State, t.snapshot.Reason, t.snapshot.ExitCode = final.State, final.Reason, final.ExitCode
	t.snapshot.FinishedAt, t.snapshot.CleanupIncomplete = final.FinishedAt, final.CleanupIncomplete
	t.snapshot.Controllable = false
}

func (m *Manager) releaseActiveLocked(t *task) {
	if t.active {
		t.active = false
		m.active--
	}
}

func (m *Manager) taskLocked(owner, taskID string) (*task, error) {
	t := m.tasks[taskID]
	if t == nil || t.owner != owner {
		return nil, failure("task_not_found")
	}
	return t, nil
}

func (m *Manager) snapshotLocked(t *task) Snapshot {
	s := t.snapshot
	if s.ExitCode != nil {
		exitCode := *s.ExitCode
		s.ExitCode = &exitCode
	}
	s.StdoutBytes, s.StderrBytes = t.stdout.received, t.stderr.received
	s.StdoutRetained, s.StderrRetained = m.readableLocked(t, &t.stdout), m.readableLocked(t, &t.stderr)
	s.StdoutSaved, s.StderrSaved = t.stdout.saved, t.stderr.saved
	s.Persistence, s.PersistenceError = m.persistenceLocked(t), m.persistenceErrorLocked(t)
	s.OutputState = "collecting"
	if t.terminal {
		s.OutputState = "complete"
	}
	if s.StdoutRetained < s.StdoutBytes || s.StderrRetained < s.StderrBytes {
		s.OutputState = "truncated"
	}
	if t.outputIncomplete {
		s.OutputState = "incomplete"
	}
	return s
}

func (m *Manager) Status(owner, taskID string) (Snapshot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, err := m.taskLocked(owner, taskID)
	if err != nil {
		return Snapshot{}, err
	}
	return m.snapshotLocked(t), nil
}

func (m *Manager) List(owner string) []Snapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	result := make([]Snapshot, 0)
	for _, t := range m.tasks {
		if t.owner == owner {
			result = append(result, m.snapshotLocked(t))
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].TaskID < result[j].TaskID })
	return result
}

// Wait's duration is a wait budget, never an execution timeout. Nonpositive
// durations are a status poll. A canceled wait returns current state plus a code.
func (m *Manager) Wait(ctx context.Context, owner, taskID string, duration time.Duration) (Snapshot, error) {
	m.mu.Lock()
	t, err := m.taskLocked(owner, taskID)
	if err != nil {
		m.mu.Unlock()
		return Snapshot{}, err
	}
	if t.terminal || duration <= 0 {
		s := m.snapshotLocked(t)
		m.mu.Unlock()
		return s, nil
	}
	m.mu.Unlock()
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-t.done:
	case <-timer.C:
	case <-ctx.Done():
		s, _ := m.Status(owner, taskID)
		return s, failure("wait_canceled")
	}
	return m.Status(owner, taskID)
}

func (m *Manager) requestStopLocked(t *task, reason string) {
	if t.terminal || t.settling || t.stopReason != "" {
		return
	}
	t.stopReason = reason
	if t.snapshot.State != StateStarting {
		t.snapshot.State = StateStopping
	}
	close(t.stop)
}

// Stop's request is durable in this Manager even if ctx ends the caller's wait.
// Background cleanup remains bounded and records any unprovable result.
func (m *Manager) Stop(ctx context.Context, owner, taskID string) (Snapshot, error) {
	m.mu.Lock()
	t, err := m.taskLocked(owner, taskID)
	if err != nil {
		m.mu.Unlock()
		return Snapshot{}, err
	}
	m.requestStopLocked(t, "stopped")
	m.mu.Unlock()
	select {
	case <-t.done:
		return m.Status(owner, taskID)
	case <-ctx.Done():
		s, _ := m.Status(owner, taskID)
		return s, failure("stop_wait_canceled")
	}
}

// Close is idempotent. Its first call bars new launches and requests all stops
// together. Caller cancellation ends only the wait; later Close can await it.
// Cleanup uncertainty is reported both in snapshots and as cleanup_incomplete.
func (m *Manager) Close(ctx context.Context) error {
	m.mu.Lock()
	if !m.closed {
		m.closed = true
		pending := make([]*task, 0, len(m.tasks))
		for _, t := range m.tasks {
			m.requestStopLocked(t, "manager_closed")
			pending = append(pending, t)
		}
		go func() {
			for _, t := range pending {
				<-t.done
			}
			close(m.closeDone)
		}()
	}
	m.mu.Unlock()
	select {
	case <-m.closeDone:
		m.mu.Lock()
		defer m.mu.Unlock()
		for _, t := range m.tasks {
			if t.snapshot.CleanupIncomplete {
				return failure("cleanup_incomplete")
			}
		}
		return nil
	case <-ctx.Done():
		return failure("close_wait_canceled")
	}
}
