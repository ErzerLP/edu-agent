package filelock

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestLockCloseIsConcurrentAndIdempotent(t *testing.T) {
	lock, err := Acquire(t.Context(), filepath.Join(t.TempDir(), "close.lock"), Exclusive, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	var group sync.WaitGroup
	errorsSeen := make(chan error, 16)
	for range 16 {
		group.Add(1)
		go func() {
			defer group.Done()
			errorsSeen <- lock.Close()
		}()
	}
	group.Wait()
	close(errorsSeen)
	for closeErr := range errorsSeen {
		if closeErr != nil {
			t.Fatalf("close error=%v", closeErr)
		}
	}
}

func TestExclusiveLockRejectsSecondWriter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.lock")
	first, err := Acquire(t.Context(), path, Exclusive, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Acquire(t.Context(), path, Exclusive, 10*time.Millisecond); !errors.Is(err, ErrBusy) {
		t.Fatalf("second lock error=%v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := Acquire(t.Context(), path, Exclusive, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestFileLockProcessExclusion(t *testing.T) {
	if os.Getenv("EDU_AGENT_FILELOCK_HELPER") == "1" {
		if _, err := Acquire(context.Background(), os.Getenv("EDU_AGENT_FILELOCK_PATH"), Exclusive, 50*time.Millisecond); !errors.Is(err, ErrBusy) {
			t.Fatalf("expected cross-process ErrBusy, got %v", err)
		}
		t.Log("FILELOCK_PROCESS_HELPER_SUCCESS")
		return
	}
	path := filepath.Join(t.TempDir(), "process.lock")
	lock, err := Acquire(t.Context(), path, Exclusive, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	command := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestFileLockProcessExclusion$", "-test.v")
	command.Env = append(os.Environ(), "EDU_AGENT_FILELOCK_HELPER=1", "EDU_AGENT_FILELOCK_PATH="+path)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("process helper failed: %v\n%s", err, output)
	}
	if !bytes.Contains(output, []byte("FILELOCK_PROCESS_HELPER_SUCCESS")) {
		t.Fatalf("process helper success marker missing:\n%s", output)
	}
	t.Log(string(output))
}
