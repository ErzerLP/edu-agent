//go:build linux || darwin

package localexec

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func testManager(t *testing.T, options Options) *Manager {
	t.Helper()
	if options.StopGrace == 0 {
		options.StopGrace = 60 * time.Millisecond
	}
	m := New(options)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
		defer cancel()
		if err := m.Close(ctx); err != nil && errorCode(err) != "cleanup_incomplete" {
			t.Errorf("cleanup: %v", err)
		}
	})
	return m
}

func launch(t *testing.T, m *Manager, owner, id, command string, stdin bool) Snapshot {
	t.Helper()
	s, err := m.Start(context.Background(), owner, id, StartArgs{Command: command, CWD: t.TempDir(), Stdin: stdin})
	if err != nil {
		t.Fatalf("Start: %v (%+v)", err, s)
	}
	return s
}

func finish(t *testing.T, m *Manager, owner, id string) Snapshot {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	s, err := m.Wait(ctx, owner, id, 4*time.Second)
	if err != nil {
		t.Fatalf("Wait: %v (%+v)", err, s)
	}
	if s.Controllable || s.FinishedAt.IsZero() {
		t.Fatalf("not terminal: %+v", s)
	}
	return s
}

func output(t *testing.T, m *Manager, owner, id, stream string, pageSize int) []byte {
	t.Helper()
	var result []byte
	var offset int64
	for pageNumber := 0; pageNumber < 100000; pageNumber++ {
		page, err := m.Read(owner, id, stream, offset, pageSize)
		if err != nil {
			t.Fatal(err)
		}
		if page.Offset != offset || page.NextOffset != offset+int64(len(page.Data)) {
			t.Fatalf("bad contiguous cursor: %+v", page)
		}
		result = append(result, page.Data...)
		if !page.More {
			return result
		}
		if page.NextOffset <= offset {
			t.Fatalf("stalled: %+v", page)
		}
		offset = page.NextOffset
	}
	t.Fatal("unbounded pages")
	return nil
}

func requireCode(t *testing.T, err error, code string) {
	t.Helper()
	if errorCode(err) != code {
		t.Fatalf("error = %v, want code %s", err, code)
	}
}

func TestShellPipelineCWDEnvironmentAndDefaultEOF(t *testing.T) {
	t.Setenv("SHELL", "/bin/sh")
	m := testManager(t, Options{})
	cwd := t.TempDir() // independent of any workspace root
	t.Setenv("LOCALEXEC_DELETE", "must-not-inherit")
	override := "outside workspace"
	s, err := m.Start(context.Background(), "owner", "pipeline", StartArgs{
		CWD:     cwd,
		Command: "printf '%s' \"$LOCALEXEC_VALUE\" | tr '[:lower:]' '[:upper:]' > result; printf '%s:%s:' \"$PWD\" \"${LOCALEXEC_DELETE-unset}\"; cat result; printf 'err' >&2; cat >/dev/null",
		Env:     map[string]*string{"LOCALEXEC_VALUE": &override, "LOCALEXEC_DELETE": nil},
	})
	if err != nil {
		t.Fatal(err)
	}
	if s.Shell != "/bin/sh" {
		t.Fatalf("shell: %+v", s)
	}
	s = finish(t, m, "owner", s.TaskID)
	if s.State != StateExited || s.Reason != "completed" || s.ExitCode == nil || *s.ExitCode != 0 || s.CleanupIncomplete || s.OutputState != "complete" {
		t.Fatalf("snapshot: %+v", s)
	}
	if got := string(output(t, m, "owner", s.TaskID, "stdout", 3)); got != cwd+":unset:OUTSIDE WORKSPACE" {
		t.Fatalf("stdout: %q", got)
	}
	if got := string(output(t, m, "owner", s.TaskID, "stderr", 2)); got != "err" {
		t.Fatalf("stderr: %q", got)
	}
	file, err := os.ReadFile(filepath.Join(cwd, "result"))
	if err != nil || string(file) != "OUTSIDE WORKSPACE" {
		t.Fatalf("redirection: %q, %v", file, err)
	}
	next := launch(t, m, "owner", "independent", "printf '%s' \"${LOCALEXEC_VALUE-unset}\"", false)
	finish(t, m, "owner", next.TaskID)
	if got := string(output(t, m, "owner", next.TaskID, "stdout", 10)); got != "unset" {
		t.Fatalf("env leaked between shells: %q", got)
	}
}

func TestWaitAndCallerCancellationDoNotKillTask(t *testing.T) {
	m := testManager(t, Options{})
	ctx, cancel := context.WithCancel(context.Background())
	s, err := m.Start(ctx, "owner", "persistent", StartArgs{CWD: t.TempDir(), Command: "cat; printf done", Stdin: true})
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	short, err := m.Wait(context.Background(), "owner", s.TaskID, 15*time.Millisecond)
	if err != nil || short.State != StateRunning {
		t.Fatalf("short wait: %+v %v", short, err)
	}
	waitCtx, cancelWait := context.WithCancel(context.Background())
	cancelWait()
	short, err = m.Wait(waitCtx, "owner", s.TaskID, time.Second)
	requireCode(t, err, "wait_canceled")
	if short.State != StateRunning {
		t.Fatalf("canceled wait killed task: %+v", short)
	}
	input, err := m.WriteInput(context.Background(), "owner", s.TaskID, []byte("alive"))
	if err != nil || input.Written != 5 {
		t.Fatalf("input: %+v %v", input, err)
	}
	if err := m.CloseInput("owner", s.TaskID); err != nil {
		t.Fatal(err)
	}
	s = finish(t, m, "owner", s.TaskID)
	if s.State != StateExited || string(output(t, m, "owner", s.TaskID, "stdout", 4)) != "alivedone" {
		t.Fatalf("result: %+v", s)
	}
}

func TestStartIdentityFailuresAndOwnerIsolation(t *testing.T) {
	m := testManager(t, Options{})
	cwd := t.TempDir()
	args := StartArgs{CWD: cwd, Command: "printf x >> count"}
	const callers = 12
	var wg sync.WaitGroup
	ids := make(chan string, callers)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s, err := m.Start(context.Background(), "owner", "same-call", args)
			if err != nil {
				t.Errorf("Start: %v", err)
				return
			}
			ids <- s.TaskID
		}()
	}
	wg.Wait()
	close(ids)
	id := ""
	for got := range ids {
		if id != "" && id != got {
			t.Fatalf("duplicate tasks: %s %s", id, got)
		}
		id = got
	}
	finish(t, m, "owner", id)
	data, err := os.ReadFile(filepath.Join(cwd, "count"))
	if err != nil || string(data) != "x" {
		t.Fatalf("duplicate side effects: %q %v", data, err)
	}
	otherCall, err := m.Start(context.Background(), "owner", "different-call", args)
	if err != nil || otherCall.TaskID == id {
		t.Fatalf("command text deduped: %+v %v", otherCall, err)
	}
	finish(t, m, "owner", otherCall.TaskID)
	data, _ = os.ReadFile(filepath.Join(cwd, "count"))
	if string(data) != "xx" {
		t.Fatalf("distinct calls: %q", data)
	}
	otherOwner := launch(t, m, "other", "same-call", "exit 0", false)
	finish(t, m, "other", otherOwner.TaskID)
	if len(m.List("other")) != 1 || len(m.List("unknown")) != 0 {
		t.Fatal("list owner isolation")
	}
	_, err = m.Status("other", id)
	requireCode(t, err, "task_not_found")
	_, err = m.Read("other", id, "stdout", 0, 1)
	requireCode(t, err, "task_not_found")
	_, err = m.WriteInput(context.Background(), "other", id, nil)
	requireCode(t, err, "task_not_found")
	err = m.CloseInput("other", id)
	requireCode(t, err, "task_not_found")
	_, err = m.Wait(context.Background(), "other", id, 0)
	requireCode(t, err, "task_not_found")
	_, err = m.Stop(context.Background(), "other", id)
	requireCode(t, err, "task_not_found")

	bad, err := m.Start(context.Background(), "owner", "failed", StartArgs{CWD: cwd, Shell: "/private-sensitive-missing-shell", Command: "sensitive-command"})
	requireCode(t, err, "shell_unavailable")
	if bad.TaskID == "" || bad.State != StateFailed || bad.ExitCode != nil {
		t.Fatalf("failed identity: %+v", bad)
	}
	if strings.Contains(err.Error(), "sensitive") {
		t.Fatalf("unsafe error: %v", err)
	}
	retry, err := m.Start(context.Background(), "owner", "failed", args)
	requireCode(t, err, "shell_unavailable")
	if retry.TaskID != bad.TaskID {
		t.Fatalf("failed call relaunched: %+v", retry)
	}
}

func TestStartBoundaryValidationAndNonzeroExit(t *testing.T) {
	m := testManager(t, Options{})
	cwd := t.TempDir()
	nul := "secret\x00value"
	cases := []struct {
		name, code string
		args       StartArgs
	}{
		{"missing cwd", "invalid_cwd", StartArgs{Command: "true"}},
		{"bad cwd", "cwd_unavailable", StartArgs{CWD: filepath.Join(cwd, "missing")}},
		{"command nul", "invalid_argument", StartArgs{CWD: cwd, Command: "private\x00text"}},
		{"env separator", "invalid_env", StartArgs{CWD: cwd, Env: map[string]*string{"bad=name": nil}}},
		{"env empty", "invalid_env", StartArgs{CWD: cwd, Env: map[string]*string{"": nil}}},
		{"env nul", "invalid_env", StartArgs{CWD: cwd, Env: map[string]*string{"NAME": &nul}}},
		{"negative timeout", "invalid_timeout", StartArgs{CWD: cwd, TimeoutMS: -1}},
		{"overflow timeout", "invalid_timeout", StartArgs{CWD: cwd, TimeoutMS: 1<<63 - 1}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, err := m.Start(context.Background(), "owner", tc.name, tc.args)
			requireCode(t, err, tc.code)
			if s.TaskID == "" || s.State != StateFailed || !s.StartedAt.IsZero() {
				t.Fatalf("bad start: %+v", s)
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s, err := m.Start(ctx, "owner", "canceled", StartArgs{CWD: cwd, Command: "touch must-not-exist"})
	requireCode(t, err, "start_canceled")
	if s.State != StateCanceled || !s.StartedAt.IsZero() {
		t.Fatalf("canceled start: %+v", s)
	}
	if _, err := os.Stat(filepath.Join(cwd, "must-not-exist")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("canceled side effect: %v", err)
	}
	s = launch(t, m, "owner", "nonzero", "printf failure >&2; exit 7", false)
	s = finish(t, m, "owner", s.TaskID)
	if s.State != StateExited || s.Reason != "nonzero_exit" || s.ExitCode == nil || *s.ExitCode != 7 {
		t.Fatalf("nonzero: %+v", s)
	}
	*s.ExitCode = 99
	fresh, _ := m.Status("owner", s.TaskID)
	if *fresh.ExitCode != 7 {
		t.Fatal("snapshot pointer aliases manager")
	}
	// Executable files with a missing interpreter fail in Start, not LookPath.
	shell := filepath.Join(cwd, "bad-shell")
	if err := os.WriteFile(shell, []byte("#!/missing-sensitive-interpreter\n"), 0700); err != nil {
		t.Fatal(err)
	}
	s, err = m.Start(context.Background(), "owner", "exec-failed", StartArgs{CWD: cwd, Shell: shell, Command: "secret"})
	requireCode(t, err, "start_failed")
	if s.Shell != shell || s.State != StateFailed {
		t.Fatalf("exec failed: %+v", s)
	}
}

func TestOutputPaginationRawBytesAndQuotas(t *testing.T) {
	m := testManager(t, Options{OutputBytesPerTask: 7, OutputBytesTotal: 9})
	s := launch(t, m, "owner", "raw", "printf '\\377abcde'; printf 'XYZ' >&2", false)
	s = finish(t, m, "owner", s.TaskID)
	if s.StdoutBytes != 6 || s.StderrBytes != 3 || s.StdoutRetained+s.StderrRetained != 7 || s.OutputState != "truncated" {
		t.Fatalf("quota stats: %+v", s)
	}
	for _, stream := range []string{"stdout", "stderr"} {
		var retained []byte
		var offset int64
		for {
			page, err := m.Read("owner", s.TaskID, stream, offset, 2)
			if err != nil {
				t.Fatal(err)
			}
			if page.Offset != offset || page.NextOffset < offset {
				t.Fatalf("cursor: %+v", page)
			}
			retained = append(retained, page.Data...)
			if len(page.Data) == 0 && offset < page.Received && page.NextOffset != page.Received {
				t.Fatalf("gap not advanced: %+v", page)
			}
			offset = page.NextOffset
			if !page.More {
				break
			}
		}
		full := []byte{255, 'a', 'b', 'c', 'd', 'e'}
		if stream == "stderr" {
			full = []byte("XYZ")
		}
		if !bytes.Equal(retained, full[:len(retained)]) {
			t.Fatalf("raw output altered: %v", retained)
		}
	}
	next := launch(t, m, "owner", "global", "printf 12345", false)
	next = finish(t, m, "owner", next.TaskID)
	if next.StdoutBytes != 5 || next.StdoutRetained != 2 {
		t.Fatalf("manager quota: %+v", next)
	}
	blocked, err := m.Start(context.Background(), "owner", "quota-reject", StartArgs{CWD: t.TempDir(), Command: "exit 0"})
	requireCode(t, err, "output_limit")
	if blocked.TaskID == "" || !blocked.StartedAt.IsZero() {
		t.Fatalf("quota side effect: %+v", blocked)
	}
	page, _ := m.Read("owner", next.TaskID, "stdout", 0, 100)
	page.Data[0] = 'x'
	page2, _ := m.Read("owner", next.TaskID, "stdout", 0, 100)
	if string(page2.Data) != "12" {
		t.Fatal("Read aliases retention")
	}
	_, err = m.Read("owner", next.TaskID, "stdout", 6, 1)
	requireCode(t, err, "invalid_offset")
	_, err = m.Read("owner", next.TaskID, "combined", 0, 1)
	requireCode(t, err, "invalid_stream")
}

func TestInputMultipleChunksAndEOF(t *testing.T) {
	m := testManager(t, Options{})
	cwd := t.TempDir()
	s, err := m.Start(context.Background(), "owner", "input", StartArgs{CWD: cwd, Command: "cat > result; wc -c < result", Stdin: true})
	if err != nil {
		t.Fatal(err)
	}
	var wanted []byte
	for i := 0; i < 5; i++ {
		chunk := bytes.Repeat([]byte{byte('a' + i)}, 32<<10)
		written, err := m.WriteInput(context.Background(), "owner", s.TaskID, chunk)
		if err != nil || written.Written != len(chunk) || written.Outcome != "written" {
			t.Fatalf("write %d: %+v %v", i, written, err)
		}
		wanted = append(wanted, chunk...)
	}
	if err := m.CloseInput("owner", s.TaskID); err != nil {
		t.Fatal(err)
	}
	if err := m.CloseInput("owner", s.TaskID); err != nil {
		t.Fatal(err)
	}
	s = finish(t, m, "owner", s.TaskID)
	actual, err := os.ReadFile(filepath.Join(cwd, "result"))
	if err != nil || !bytes.Equal(actual, wanted) {
		t.Fatalf("cumulative stdin: len=%d want=%d err=%v", len(actual), len(wanted), err)
	}
	if got := strings.TrimSpace(string(output(t, m, "owner", s.TaskID, "stdout", 3))); got != "163840" {
		t.Fatalf("wc: %q", got)
	}
	_, err = m.WriteInput(context.Background(), "owner", s.TaskID, []byte("after EOF"))
	requireCode(t, err, "stdin_closed")
}

func errorCode(err error) string {
	var stable *Error
	if errors.As(err, &stable) {
		return stable.Code
	}
	return ""
}

func TestInputCancellationAndConcurrentClose(t *testing.T) {
	m := testManager(t, Options{})
	// Shell builtin loop avoids orphaning a helper when force-stopped.
	s := launch(t, m, "owner", "blocked-input", "while :; do :; done", true)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	start := time.Now()
	// A pipe can hold a complete 64 KiB call (and Darwin can grow pipes), so
	// fill it under ONE bounded deadline rather than assume its capacity.
	var result InputResult
	var err error
	for i := 0; i < 32; i++ {
		result, err = m.WriteInput(ctx, "owner", s.TaskID, bytes.Repeat([]byte("x"), MaxInputBytes))
		if err != nil {
			break
		}
	}
	if err == nil || result.Written >= MaxInputBytes || result.Written < 0 || result.Outcome == "written" {
		t.Fatalf("canceled write: %+v %v", result, err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("input cancellation blocked")
	}
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			_, _ = m.WriteInput(ctx, "owner", s.TaskID, bytes.Repeat([]byte("y"), MaxInputBytes))
		}()
	}
	if err := m.CloseInput("owner", s.TaskID); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	_, err = m.WriteInput(context.Background(), "owner", s.TaskID, []byte("z"))
	requireCode(t, err, "stdin_closed")
}

func TestStopTimeoutAndChildCleanup(t *testing.T) {
	m := testManager(t, Options{})
	s := launch(t, m, "owner", "stop", "trap '' TERM; printf ready; while :; do :; done", false)
	awaitOutput(t, m, "owner", s.TaskID, "ready")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	start := time.Now()
	stopped, err := m.Stop(ctx, "owner", s.TaskID)
	if err != nil || stopped.State != StateCanceled || stopped.Reason != "stopped" || stopped.CleanupIncomplete {
		t.Fatalf("stop: %+v %v", stopped, err)
	}
	if time.Since(start) < m.options.StopGrace {
		t.Fatal("TERM grace not honored")
	}
	again, err := m.Stop(ctx, "owner", s.TaskID)
	if err != nil || again.State != stopped.State {
		t.Fatalf("repeat stop: %+v %v", again, err)
	}
	s, err = m.Start(context.Background(), "owner", "timeout", StartArgs{CWD: t.TempDir(), Command: "while :; do :; done", TimeoutMS: 30})
	if err != nil {
		t.Fatal(err)
	}
	s = finish(t, m, "owner", s.TaskID)
	if s.State != StateTimedOut || s.Reason != "execution_timeout" {
		t.Fatalf("timeout: %+v", s)
	}

	// The shell trap reaps its ordinary child. Group signaling must reach the
	// child too; merely killing the shell leaves this wait blocked.
	s = launch(t, m, "owner", "child", "trap 'wait; exit 0' TERM; sleep 60 & child=$!; printf '%s' \"$child\"; wait", false)
	childPID := awaitPID(t, m, s.TaskID)
	stopped, err = m.Stop(ctx, "owner", s.TaskID)
	if err != nil || stopped.Controllable {
		t.Fatalf("child stop: %+v %v", stopped, err)
	}
	if err := unix.Kill(childPID, 0); !errors.Is(err, unix.ESRCH) {
		t.Fatalf("child survived/reaping incomplete: pid=%d err=%v snapshot=%+v", childPID, err, stopped)
	}
	if stopped.CleanupIncomplete {
		t.Fatalf("fully reaped group reported incomplete: %+v", stopped)
	}
}

func awaitOutput(t *testing.T, m *Manager, owner, id, wanted string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		page, err := m.Read(owner, id, "stdout", 0, MaxReadBytes)
		if err != nil {
			t.Fatal(err)
		}
		if string(page.Data) == wanted {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("missing readiness output")
}

func awaitPID(t *testing.T, m *Manager, id string) int {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		page, err := m.Read("owner", id, "stdout", 0, 100)
		if err != nil {
			t.Fatal(err)
		}
		var pid int
		if _, err := fmt.Sscanf(string(page.Data), "%d", &pid); err == nil && pid > 1 {
			return pid
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("missing child PID")
	return 0
}

func TestMainExitDoesNotWaitForeverForInheritedPipes(t *testing.T) {
	m := testManager(t, Options{})
	s := launch(t, m, "owner", "inherited", "sleep 60 & printf '%s' \"$!\"; exit 3", false)
	s = finish(t, m, "owner", s.TaskID)
	if s.State != StateExited || s.ExitCode == nil || *s.ExitCode != 3 || s.Controllable {
		t.Fatalf("main exit: %+v", s)
	}
	pid := awaitPID(t, m, s.TaskID)
	// Some test containers do not reap orphan zombies. Existence in that case
	// must be reported conservatively; it is not evidence of a live survivor.
	if err := unix.Kill(pid, 0); err == nil && !s.CleanupIncomplete {
		t.Fatalf("unproven group cleanup: %+v", s)
	}
}

func TestQuotaRecordsConcurrencyAndClose(t *testing.T) {
	m := testManager(t, Options{MaxTasks: 3, MaxConcurrent: 1})
	s := launch(t, m, "owner", "live", "cat", true)
	cwd := t.TempDir()
	blocked, err := m.Start(context.Background(), "owner", "blocked", StartArgs{CWD: cwd, Command: "touch side-effect"})
	requireCode(t, err, "concurrency_limit")
	if blocked.TaskID == "" || !blocked.StartedAt.IsZero() {
		t.Fatalf("rejection identity: %+v", blocked)
	}
	if err := m.CloseInput("owner", s.TaskID); err != nil {
		t.Fatal(err)
	}
	finish(t, m, "owner", s.TaskID)
	retry, err := m.Start(context.Background(), "owner", "blocked", StartArgs{CWD: cwd, Command: "touch side-effect"})
	requireCode(t, err, "concurrency_limit")
	if retry.TaskID != blocked.TaskID {
		t.Fatal("rejected call retried execution")
	}
	last := launch(t, m, "owner", "last", "cat", true)
	_, err = m.Start(context.Background(), "owner", "too-many", StartArgs{CWD: cwd})
	requireCode(t, err, "task_limit")
	if _, err := os.Stat(filepath.Join(cwd, "side-effect")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("quota side effect: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	for i := 0; i < 2; i++ {
		if err := m.Close(ctx); err != nil {
			t.Fatal(err)
		}
	}
	_, err = m.Start(context.Background(), "owner", "closed", StartArgs{CWD: cwd})
	requireCode(t, err, "manager_closed")
	ended, _ := m.Status("owner", last.TaskID)
	if ended.Controllable || ended.FinishedAt.IsZero() {
		t.Fatalf("Close did not settle: %+v", ended)
	}
}

func TestConcurrentOutputQueriesInputAndShutdown(t *testing.T) {
	m := testManager(t, Options{MaxTasks: 32, MaxConcurrent: 8})
	s := launch(t, m, "owner", "io", "cat", true)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			for j := 0; j < 20; j++ {
				_, _ = m.WriteInput(ctx, "owner", s.TaskID, []byte(fmt.Sprintf("%d:%d\n", i, j)))
				_, _ = m.Read("owner", s.TaskID, "stdout", 0, 11)
				_, _ = m.Status("owner", s.TaskID)
				_ = m.List("owner")
				_, _ = m.Wait(ctx, "owner", s.TaskID, time.Millisecond)
			}
		}(i)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = m.Close(ctx)
	}()
	wg.Wait()
	ended := finish(t, m, "owner", s.TaskID)
	if ended.Controllable {
		t.Fatalf("still live: %+v", ended)
	}
}
