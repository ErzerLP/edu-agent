//go:build linux || darwin

package securefile

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestRootReadSnapshotFIFOReplacementDoesNotBlock(t *testing.T) {
	rootPath := t.TempDir()
	path := filepath.Join(rootPath, ".gitignore")
	if err := os.WriteFile(path, []byte("*.txt\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := OpenRoot(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	info, err := root.Stat(t.Context(), ".gitignore")
	if err != nil || info.Kind != EntryFile {
		t.Fatalf("stat=%+v err=%v", info, err)
	}
	// Deterministically exercise the TOCTOU interval: a successful regular-file
	// preflight cannot prevent replacement before the actual leaf open.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := root.ReadSnapshot(".gitignore", 64<<10, false); done <- err }()
	select {
	case err := <-done:
		if !errors.Is(err, ErrNotRegular) {
			t.Fatalf("fifo error=%v", err)
		}
	case <-time.After(time.Second):
		// Unblock a regressed O_RDONLY opener so the test itself cannot hang.
		fd, _ := unix.Open(path, unix.O_RDWR|unix.O_NONBLOCK, 0)
		if fd >= 0 {
			defer unix.Close(fd)
		}
		select {
		case <-done:
		case <-time.After(time.Second):
		}
		t.Fatal("ReadSnapshot blocked on FIFO after successful regular-file stat")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("*.txt\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	snapshot, err := root.ReadSnapshot(".gitignore", 64<<10, false)
	if err != nil || string(snapshot.Data) != "*.txt\n" {
		t.Fatalf("regular file changed: %+v %v", snapshot, err)
	}
}
