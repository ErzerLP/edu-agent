//go:build !windows

package filelock

import (
	"os"
	"path/filepath"
	"testing"
)

func TestFileLockUnixRejectsSymlinkAndUsesPrivateMode(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "private.lock")
	lock, err := Acquire(t.Context(), path, Exclusive, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		t.Fatalf("lock mode = %v, want regular 0600", info.Mode())
	}
	target := filepath.Join(root, "target")
	if err := os.WriteFile(target, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link.lock")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("create required symlink fixture: %v", err)
	}
	if linked, err := Acquire(t.Context(), link, Exclusive, 0); err == nil {
		_ = linked.Close()
		t.Fatal("symlink lock target was accepted")
	}
}
