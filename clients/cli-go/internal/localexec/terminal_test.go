//go:build linux || darwin

package localexec

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// Helpers run only inside a new task's private PTY, never the user's terminal.
func TestPTYHelper(t *testing.T) {
	mode := os.Getenv("LOCALEXEC_PTY_HELPER")
	if mode == "" {
		return
	}
	if mode == "job-child" {
		signal.Ignore(unix.SIGHUP, unix.SIGTERM)
		fmt.Print("JOBREADY\n")
		time.Sleep(250 * time.Millisecond)
		os.Exit(0)
	}
	for fd := 0; fd < 3; fd++ {
		if _, err := getTerminalAttributes(fd); err != nil {
			fmt.Fprintf(os.Stderr, "NOTTY:%d\n", fd)
			os.Exit(2)
		}
	}
	controlling, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		os.Exit(3)
	}
	_ = controlling.Close()
	attributes, err := getTerminalAttributes(0)
	if err != nil {
		os.Exit(4)
	}
	attributes.Lflag &^= unix.ECHO
	attributes.Lflag |= unix.ICANON | unix.ISIG
	attributes.Cc[unix.VINTR], attributes.Cc[unix.VEOF] = 29, 6
	if mode == "raw" || mode == "blocked" {
		attributes.Lflag &^= unix.ICANON | unix.ISIG
		attributes.Iflag &^= unix.ICRNL | unix.IXON
		attributes.Cc[unix.VMIN], attributes.Cc[unix.VTIME] = 1, 0
	}
	if mode == "disabled" {
		attributes.Cc[unix.VINTR], attributes.Cc[unix.VEOF] = disabledTerminalByte, disabledTerminalByte
	}
	if setPTYTestAttributes(0, attributes) != nil {
		os.Exit(5)
	}
	if mode == "flood" {
		_, _ = os.Stdout.Write(bytes.Repeat([]byte("x"), 512<<10))
		_, _ = os.Stderr.Write([]byte("TAIL"))
		os.Exit(0)
	}
	if mode == "job" {
		fmt.Printf("SESSIONROOT:%d\n", os.Getpid())
		child := exec.Command(os.Args[0], "-test.run=^TestPTYHelper$")
		child.Env = append(os.Environ(), "LOCALEXEC_PTY_HELPER=job-child")
		child.Stdin, child.Stdout, child.Stderr = os.Stdin, os.Stdout, os.Stderr
		child.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if child.Start() != nil {
			os.Exit(6)
		}
		// The test sends an acknowledgement only after the child has installed
		// its HUP handler. This creates a genuinely separate job-control pgrp.
		var acknowledgement [16]byte
		_, _ = os.Stdin.Read(acknowledgement[:])
		os.Exit(0)
	}
	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt, unix.SIGINT)
	go func() {
		for range interrupt {
			fmt.Print("INTERRUPTED\n")
		}
	}()
	printSize := func() {
		size, err := unix.IoctlGetWinsize(0, unix.TIOCGWINSZ)
		if err != nil {
			os.Exit(7)
		}
		fmt.Printf("SIZE:%d:%d\n", size.Row, size.Col)
	}
	printSize()
	fmt.Printf("TTY:0:1:2 TERM:%s READY\n", os.Getenv("TERM"))
	if mode == "blocked" {
		time.Sleep(10 * time.Second)
		os.Exit(0)
	}
	buffer := make([]byte, 512)
	for {
		n, err := os.Stdin.Read(buffer)
		if n > 0 {
			fmt.Printf("READ:%x\n", buffer[:n])
			if string(buffer[:n]) == "quit\n" {
				fmt.Fprint(os.Stderr, "STDERR-TAIL\n")
				os.Exit(0)
			}
			if string(buffer[:n]) == "size\n" {
				printSize()
			}
		}
		if err == io.EOF {
			fmt.Print("TERMINAL-EOF\n")
			continue // VEOF is not a permanent pipe close.
		}
		if err != nil {
			os.Exit(8)
		}
	}
}

func startPTY(t *testing.T, m *Manager, id, mode string, customize func(*StartArgs)) Snapshot {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	args := StartArgs{CWD: t.TempDir(), Shell: "/bin/sh", PTY: true,
		Command: "case $- in *i*) printf UNEXPECTED-INTERACTIVE;; esac; exec '" + strings.ReplaceAll(executable, "'", "'\\''") + "' -test.run='^TestPTYHelper$'",
		Env:     map[string]*string{"LOCALEXEC_PTY_HELPER": &mode}}
	if customize != nil {
		customize(&args)
	}
	snapshot, err := m.Start(t.Context(), "owner", id, args)
	if err != nil {
		t.Fatalf("PTY start: %+v %v (requires an available private /dev/ptmx)", snapshot, err)
	}
	return snapshot
}

func awaitPTY(t *testing.T, m *Manager, id, wanted string) []byte {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	var data []byte
	for time.Now().Before(deadline) {
		page, err := m.Read("owner", id, "stdout", 0, MaxReadBytes)
		if err != nil {
			t.Fatal(err)
		}
		data = page.Data
		if bytes.Contains(data, []byte(wanted)) {
			return data
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("PTY output missing %q: %q", wanted, data)
	return nil
}

func writePTY(t *testing.T, m *Manager, id string, data []byte) {
	t.Helper()
	result, err := m.WriteInput(t.Context(), "owner", id, data)
	if err != nil || result.Written != len(data) || result.Outcome != "written" {
		t.Fatalf("write: %+v %v", result, err)
	}
}

func TestPTYStdioMergedInputAndResize(t *testing.T) {
	t.Setenv("TERM", "inherited-terminal")
	for _, custom := range []bool{false, true} {
		t.Run(strconv.FormatBool(custom), func(t *testing.T) {
			m := testManager(t, Options{})
			s := startPTY(t, m, "stdio", "canonical", func(args *StartArgs) {
				if custom {
					args.Rows, args.Cols = 37, 101
					args.Env["TERM"] = nil
				}
			})
			rows, cols, term := 24, 80, "inherited-terminal"
			if custom {
				rows, cols, term = 37, 101, ""
			}
			if !s.PTY || s.Rows != rows || s.Cols != cols {
				t.Fatalf("mode metadata: %+v", s)
			}
			data := awaitPTY(t, m, s.TaskID, "TTY:0:1:2 TERM:"+term+" READY")
			if !bytes.Contains(data, []byte(fmt.Sprintf("SIZE:%d:%d", rows, cols))) || bytes.Contains(data, []byte("UNEXPECTED-INTERACTIVE")) {
				t.Fatalf("terminal launch: %q", data)
			}
			writePTY(t, m, s.TaskID, []byte("first\n"))
			awaitPTY(t, m, s.TaskID, "READ:66697273740a")
			writePTY(t, m, s.TaskID, []byte("second\n"))
			awaitPTY(t, m, s.TaskID, "READ:7365636f6e640a")
			for i := 0; i < 2; i++ {
				resized, err := m.Resize("owner", s.TaskID, 48, 132)
				if err != nil || resized.Rows != 48 || resized.Cols != 132 {
					t.Fatalf("resize: %+v %v", resized, err)
				}
			}
			writePTY(t, m, s.TaskID, []byte("size\n"))
			awaitPTY(t, m, s.TaskID, "SIZE:48:132")
			_, err := m.Resize("owner", s.TaskID, 0, 1)
			requireCode(t, err, "invalid_terminal_size")
			_, err = m.Read("owner", s.TaskID, "stderr", 0, 1)
			requireCode(t, err, "pty_merged_output")
			_, err = m.Search(t.Context(), "owner", s.TaskID, "stderr", "x", 0, 1)
			requireCode(t, err, "pty_merged_output")
			requireCode(t, m.CloseInput("owner", s.TaskID), "pty_use_send_eof")
			writePTY(t, m, s.TaskID, []byte("quit\n"))
			s = finish(t, m, "owner", s.TaskID)
			if s.State != StateExited || s.ExitCode == nil || *s.ExitCode != 0 || s.CleanupIncomplete || s.OutputState != "complete" || s.StderrBytes != 0 {
				t.Fatalf("terminal result: %+v", s)
			}
			awaitPTY(t, m, s.TaskID, "STDERR-TAIL")
			_, err = m.Interrupt(t.Context(), "owner", s.TaskID)
			requireCode(t, err, "pty_not_active")
			_, err = m.SendEOF(t.Context(), "owner", s.TaskID)
			requireCode(t, err, "pty_not_active")
			_, err = m.Resize("owner", s.TaskID, 1, 1)
			requireCode(t, err, "pty_not_active")
		})
	}
}

func TestPTYCurrentVINTRAndCanonicalVEOF(t *testing.T) {
	m := testManager(t, Options{})
	s := startPTY(t, m, "controls", "canonical", nil)
	awaitPTY(t, m, s.TaskID, "READY")
	result, err := m.Interrupt(t.Context(), "owner", s.TaskID)
	if err != nil || result.Written != 1 || result.Outcome != "written" {
		t.Fatalf("VINTR: %+v %v", result, err)
	}
	awaitPTY(t, m, s.TaskID, "INTERRUPTED")
	writePTY(t, m, s.TaskID, []byte("pending"))
	result, err = m.SendEOF(t.Context(), "owner", s.TaskID)
	if err != nil || result.Written != 1 {
		t.Fatalf("VEOF: %+v %v", result, err)
	}
	awaitPTY(t, m, s.TaskID, "READ:70656e64696e67")
	result, err = m.SendEOF(t.Context(), "owner", s.TaskID)
	if err != nil || result.Written != 1 {
		t.Fatalf("empty VEOF: %+v %v", result, err)
	}
	awaitPTY(t, m, s.TaskID, "TERMINAL-EOF")
	writePTY(t, m, s.TaskID, []byte("after\n"))
	awaitPTY(t, m, s.TaskID, "READ:61667465720a")
	writePTY(t, m, s.TaskID, []byte("quit\n"))
	finish(t, m, "owner", s.TaskID)
}

func TestPTYRawAndDisabledControls(t *testing.T) {
	for _, mode := range []string{"raw", "disabled"} {
		t.Run(mode, func(t *testing.T) {
			m := testManager(t, Options{})
			s := startPTY(t, m, mode, mode, nil)
			awaitPTY(t, m, s.TaskID, "READY")
			for _, control := range []struct {
				call    func(context.Context, string, string) (InputResult, error)
				byteHex string
			}{{m.Interrupt, "1d"}, {m.SendEOF, "06"}} {
				result, err := control.call(t.Context(), "owner", s.TaskID)
				if mode == "disabled" {
					requireCode(t, err, "pty_control_disabled")
					if result.Written != 0 || result.Outcome != "not_written" {
						t.Fatalf("disabled control wrote: %+v", result)
					}
				} else {
					if err != nil || result.Written != 1 {
						t.Fatalf("raw control: %+v %v", result, err)
					}
					data := awaitPTY(t, m, s.TaskID, "READ:"+control.byteHex)
					if bytes.Contains(data, []byte("INTERRUPTED")) || bytes.Contains(data, []byte("TERMINAL-EOF")) {
						t.Fatalf("raw mode changed byte semantics: %q", data)
					}
				}
			}
			writePTY(t, m, s.TaskID, []byte("quit\n"))
			finish(t, m, "owner", s.TaskID)
		})
	}
}

func TestPTYOwnerPipeAndFailedStartValidation(t *testing.T) {
	m := testManager(t, Options{})
	pipe := launch(t, m, "owner", "pipe", "cat", true)
	for _, owner := range []string{"wrong", "owner"} {
		code := "pty_required"
		if owner == "wrong" {
			code = "task_not_found"
		}
		_, err := m.Interrupt(t.Context(), owner, pipe.TaskID)
		requireCode(t, err, code)
		_, err = m.SendEOF(t.Context(), owner, pipe.TaskID)
		requireCode(t, err, code)
		_, err = m.Resize(owner, pipe.TaskID, 1, 1)
		requireCode(t, err, code)
	}
	writePTY(t, m, pipe.TaskID, []byte("pipe-unmodified"))
	if err := m.CloseInput("owner", pipe.TaskID); err != nil {
		t.Fatal(err)
	}
	pipe = finish(t, m, "owner", pipe.TaskID)
	if pipe.PTY || pipe.Rows != 0 || pipe.Cols != 0 || string(output(t, m, "owner", pipe.TaskID, "stdout", 32)) != "pipe-unmodified" {
		t.Fatalf("pipe regression: %+v", pipe)
	}
	store := newByteArtifactStore()
	bindOutput(t, m, "owner", store)
	for i, dimensions := range [][2]int{{-1, 80}, {24, 4097}} {
		s, err := m.Start(t.Context(), "owner", fmt.Sprintf("bad-size-%d", i), StartArgs{PTY: true, CWD: t.TempDir(), Rows: dimensions[0], Cols: dimensions[1]})
		requireCode(t, err, "invalid_terminal_size")
		if !s.PTY || !validTerminalSize(s.Rows, s.Cols) || !s.StartedAt.IsZero() {
			t.Fatalf("invalid failed metadata: %+v", s)
		}
	}
	restored := testManager(t, Options{})
	bindOutput(t, restored, "owner", store)
	if len(restored.List("owner")) != 2 {
		t.Fatal("failed starts did not restore")
	}
}

func TestPTYStopTimeoutAndClose(t *testing.T) {
	for _, operation := range []string{"stop", "timeout", "close"} {
		t.Run(operation, func(t *testing.T) {
			m := testManager(t, Options{})
			s := startPTY(t, m, operation, "blocked", func(args *StartArgs) {
				if operation == "timeout" {
					args.TimeoutMS = 120
				}
			})
			awaitPTY(t, m, s.TaskID, "READY")
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			if operation == "stop" {
				var err error
				s, err = m.Stop(ctx, "owner", s.TaskID)
				if err != nil {
					t.Fatal(err)
				}
			} else if operation == "close" {
				if err := m.Close(ctx); err != nil {
					t.Fatal(err)
				}
			}
			s = finish(t, m, "owner", s.TaskID)
			want := StateCanceled
			if operation == "timeout" {
				want = StateTimedOut
			}
			if s.State != want || s.CleanupIncomplete || s.Controllable {
				t.Fatalf("cleanup: %+v", s)
			}
		})
	}
	m := testManager(t, Options{})
	s, err := m.Start(t.Context(), "owner", "ordinary-child", StartArgs{PTY: true, CWD: t.TempDir(), Shell: "/bin/sh",
		Command: "trap 'wait; exit 0' TERM; sleep 60 & child=$!; printf '%s' \"$child\"; wait"})
	if err != nil {
		t.Fatal(err)
	}
	child := awaitPID(t, m, s.TaskID)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	s, err = m.Stop(ctx, "owner", s.TaskID)
	if err != nil || s.CleanupIncomplete || unix.Kill(child, 0) != unix.ESRCH {
		t.Fatalf("ordinary child not reaped: %+v %v", s, err)
	}
}

func TestPTYJobControlSessionCleanupConservative(t *testing.T) {
	m := testManager(t, Options{})
	s := startPTY(t, m, "job", "job", nil)
	data := awaitPTY(t, m, s.TaskID, "JOBREADY")
	var root int
	if _, err := fmt.Sscanf(string(data), "SESSIONROOT:%d", &root); err != nil || root < 1 {
		t.Fatalf("missing anchored session identity: %q", data)
	}
	groupLive, groupErr := groupHasLiveMembers(root)
	sessionLive, sessionErr := sessionHasMembers(root, true)
	if groupErr != nil || groupLive || sessionErr != nil || !sessionLive {
		t.Fatalf("job-control group incorrectly treated as entire session: group=%t/%v session=%t/%v", groupLive, groupErr, sessionLive, sessionErr)
	}
	writePTY(t, m, s.TaskID, []byte("exit\n"))
	s = finish(t, m, "owner", s.TaskID)
	// The bounded child self-exits; zombies may remain with init in containers.
	// We must account for them before releasing the pinned session leader.
	// On hosts that reap them promptly cleanup may legitimately be complete.
	if s.Controllable || s.State != StateExited {
		t.Fatalf("job control did not settle: %+v", s)
	}
}

func TestPTYInputCancellationResizeAndCloseRace(t *testing.T) {
	m := testManager(t, Options{})
	s := startPTY(t, m, "blocked-io", "blocked", nil)
	awaitPTY(t, m, s.TaskID, "READY")
	ctx, cancel := context.WithTimeout(t.Context(), 40*time.Millisecond)
	var result InputResult
	var err error
	for i := 0; i < 32; i++ {
		result, err = m.WriteInput(ctx, "owner", s.TaskID, bytes.Repeat([]byte("x"), MaxInputBytes))
		if err != nil {
			break
		}
	}
	cancel()
	if err == nil || result.Written >= MaxInputBytes || result.Outcome == "written" || result.Written < 0 {
		t.Fatalf("PTY write not cancelable: %+v %v", result, err)
	}
	ctx, cancel = context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 8; j++ {
				_, _ = m.Resize("owner", s.TaskID, 24+i, 80+j)
				_, _ = m.WriteInput(ctx, "owner", s.TaskID, bytes.Repeat([]byte("y"), MaxInputBytes))
				_, _ = m.Interrupt(ctx, "owner", s.TaskID)
			}
		}(i)
	}
	if err := m.Close(ctx); err != nil {
		t.Fatal(err)
	}
	wg.Wait() // Every writer/control callback has joined; no asynchronous writes.
	if ctx.Err() != nil {
		t.Fatal("PTY writers/resize failed to join within the close budget")
	}
	_, err = m.Resize("owner", s.TaskID, 24, 80)
	requireCode(t, err, "pty_not_active")
}

func TestPTYArchiveMergedRoundTripAndDrainFailures(t *testing.T) {
	for _, mode := range []string{"saved", "quota", "save-failed"} {
		t.Run(mode, func(t *testing.T) {
			store := newByteArtifactStore()
			options := Options{OutputBytesPerTask: 17}
			if mode == "quota" {
				options.SavedOutputBytesPerTask = 100
			}
			if mode == "save-failed" {
				store.beforeWrite = func(_ context.Context, name string, _ []byte) error {
					if strings.HasPrefix(name, "out_") {
						return failure("output_save_failed")
					}
					return nil
				}
			}
			m := testManager(t, options)
			bindOutput(t, m, "owner", store)
			s := startPTY(t, m, "flood", "flood", nil)
			s = finish(t, m, "owner", s.TaskID)
			if s.StdoutBytes != 512<<10+4 || s.StderrBytes != 0 || s.OutputState == "incomplete" || s.State != StateExited || s.CleanupIncomplete {
				t.Fatalf("PTY did not drain: %+v", s)
			}
			if mode == "saved" && (s.StdoutSaved != s.StdoutBytes || s.Persistence != "saved") {
				t.Fatalf("not saved: %+v", s)
			}
			if mode != "saved" && s.PersistenceError == "" {
				t.Fatalf("hidden save failure: %+v", s)
			}
			reopened := testManager(t, Options{})
			bindOutput(t, reopened, "owner", store.copy())
			history, err := reopened.Status("owner", s.TaskID)
			if err != nil || !history.PTY || !history.Restored || history.Controllable || history.Rows != 24 || history.Cols != 80 {
				t.Fatalf("history mode: %+v %v", history, err)
			}
			if mode == "saved" {
				page, err := reopened.Read("owner", s.TaskID, "stdout", 512<<10, 4)
				if err != nil || string(page.Data) != "TAIL" || !page.Historical || page.Availability != "saved" {
					t.Fatalf("PTY tail lost: %+v %v", page, err)
				}
			}
			_, err = reopened.Read("owner", s.TaskID, "stderr", 0, 1)
			requireCode(t, err, "pty_merged_output")
			_, err = reopened.Interrupt(t.Context(), "owner", s.TaskID)
			requireCode(t, err, "pty_not_active")
			_, err = reopened.SendEOF(t.Context(), "owner", s.TaskID)
			requireCode(t, err, "pty_not_active")
			_, err = reopened.Resize("owner", s.TaskID, 1, 1)
			requireCode(t, err, "pty_not_active")
		})
	}
}

func TestPTYArchiveStrictVersionsAndResizeMetadata(t *testing.T) {
	store := newByteArtifactStore()
	m := testManager(t, Options{})
	bindOutput(t, m, "owner", store)
	s := startPTY(t, m, "metadata", "canonical", nil)
	awaitPTY(t, m, s.TaskID, "READY")
	if _, err := m.Resize("owner", s.TaskID, 4096, 1); err != nil {
		t.Fatal(err)
	}
	crash := testManager(t, Options{})
	bindOutput(t, crash, "owner", store.copy())
	unknown, _ := crash.Status("owner", s.TaskID)
	if unknown.State != StateUnknown || !unknown.PTY || unknown.Rows != 4096 || unknown.Cols != 1 || unknown.Controllable {
		t.Fatalf("live resized metadata not saved: %+v", unknown)
	}
	writePTY(t, m, s.TaskID, []byte("quit\n"))
	finish(t, m, "owner", s.TaskID)
	for _, mutation := range []string{"v1", "v1-new-key", "v1-new-error", "future", "null-pty", "null-rows", "null-cols", "bad-size", "pipe-size", "stderr", "missing"} {
		t.Run(mutation, func(t *testing.T) {
			data, err := store.ReadArtifact(t.Context(), metadataName(s.TaskID))
			if err != nil {
				t.Fatal(err)
			}
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(data, &fields); err != nil {
				t.Fatal(err)
			}
			code := "output_corrupt"
			switch mutation {
			case "v1", "v1-new-error", "v1-new-key":
				fields["version"] = json.RawMessage("1")
				delete(fields, "pty")
				delete(fields, "rows")
				delete(fields, "cols")
				if mutation == "v1" {
					code = ""
				}
				if mutation == "v1-new-key" {
					fields["pty"] = json.RawMessage("false")
				}
				if mutation == "v1-new-error" {
					fields["start_error"] = json.RawMessage(`"pty_failed"`)
				}
			case "future":
				fields["version"] = json.RawMessage("3")
				code = "output_version_unsupported"
			case "null-pty", "null-rows", "null-cols":
				fields[strings.TrimPrefix(mutation, "null-")] = json.RawMessage("null")
			case "bad-size":
				fields["rows"] = json.RawMessage("4097")
			case "pipe-size":
				fields["pty"] = json.RawMessage("false")
			case "stderr":
				fields["stderr_received"] = json.RawMessage("1")
			case "missing":
				delete(fields, "cols")
			}
			data, err = json.Marshal(fields)
			if err != nil {
				t.Fatal(err)
			}
			decoded, err := decodeMetadata(data, s.TaskID)
			requireCode(t, err, code)
			if mutation == "v1" {
				if decoded.PTY || decoded.Rows != 0 || decoded.Cols != 0 {
					t.Fatalf("v1 was not pipe-upcast: %+v", decoded)
				}
				legacy := store.copy()
				legacy.blobs[metadataName(s.TaskID)] = data
				restored := testManager(t, Options{})
				bindOutput(t, restored, "owner", legacy)
				history, _ := restored.Status("owner", s.TaskID)
				if history.PTY || !history.Restored {
					t.Fatalf("v1 not restored: %+v", history)
				}
			}
		})
	}
}
