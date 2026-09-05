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

func TestStatNoFollowNoFIFOReadAndPinnedRoot(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("outside secret"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(filepath.Join(dir, "fifo"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "data"), []byte("body"), 0000); err != nil {
		t.Fatal(err)
	}
	root, err := OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	for name, kind := range map[string]EntryType{"link": EntryLink, "fifo": EntryOther, "data": EntryFile} {
		entry, err := root.Stat(t.Context(), name)
		if err != nil || entry.Kind != kind {
			t.Fatalf("%s: %+v %v", name, entry, err)
		}
		if kind != EntryFile {
			if _, err := root.HashEntry(t.Context(), name, entry, 1<<20); !errors.Is(err, ErrNotRegular) {
				t.Fatalf("non-file hash: %v", err)
			}
		}
	}
	if _, err := root.Stat(t.Context(), "link/secret"); !errors.Is(err, ErrLink) {
		t.Fatalf("link parent accepted: %v", err)
	}
	if err := os.Chmod(filepath.Join(dir, "data"), 0600); err != nil {
		t.Fatal(err)
	}
	expected, err := root.Stat(t.Context(), "data")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "data")); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(filepath.Join(dir, "data"), 0600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := root.HashEntry(t.Context(), "data", expected, 1<<20); done <- err }()
	select {
	case err := <-done:
		if !errors.Is(err, ErrNotRegular) {
			t.Fatalf("FIFO replacement: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("hash blocked opening a substituted FIFO")
	}
	// The root handle, not its former absolute spelling, remains authoritative.
	moved := dir + "-moved"
	if err := os.Rename(dir, moved); err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(moved)
	if err := os.Symlink(outside, dir); err != nil {
		t.Fatal(err)
	}
	if entry, err := root.Stat(t.Context(), "fifo"); err != nil || entry.Kind != EntryOther {
		t.Fatalf("lost pinned root: %+v %v", entry, err)
	}
}

func TestStatPermissionIsNotMissing(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permission denial requires an unprivileged user")
	}
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "denied"), 0700); err != nil {
		t.Fatal(err)
	}
	root, err := OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := os.Chmod(filepath.Join(dir, "denied"), 0000); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(filepath.Join(dir, "denied"), 0700)
	if _, err := root.Stat(t.Context(), "denied/missing"); !errors.Is(err, ErrPermission) || errors.Is(err, ErrNotFound) {
		t.Fatalf("permission disguised as missing: %v", err)
	}
}

func TestStatRelocatedParentFailsConfinement(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "parent"), 0700); err != nil {
		t.Fatal(err)
	}
	root, err := OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	parent, err := openUnixStatParent(t.Context(), root, []string{"parent"})
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	if err := os.Rename(filepath.Join(dir, "parent"), filepath.Join(outside, "moved")); err != nil {
		t.Fatal(err)
	}
	if err := verifyUnixArchiveLocation(root.file, parent, nil); !errors.Is(err, ErrOutsideRoot) {
		t.Fatalf("relocated parent accepted: %v", err)
	}
}
