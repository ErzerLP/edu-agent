//go:build linux || darwin

package securefile

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestMkdirPlanCreatesOnlyFrozenChain(t *testing.T) {
	rootPath := t.TempDir()
	root, err := OpenRoot(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if _, err = root.PrepareMkdir(t.Context(), "a/b", false); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing parent: %v", err)
	}
	plan, err := root.PrepareMkdir(t.Context(), "a/b", true)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Count() != 2 || plan.Anchor() != "." {
		t.Fatalf("plan=%+v", plan)
	}
	if _, err = os.Stat(filepath.Join(rootPath, "a")); !os.IsNotExist(err) {
		t.Fatalf("prepare wrote: %v", err)
	}
	result, err := root.Mkdir(t.Context(), plan)
	if err != nil || result.Outcome != PublishCompleted || result.Created != 2 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	info, err := os.Stat(filepath.Join(rootPath, "a/b"))
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
		t.Fatalf("directory=%v err=%v", info, err)
	}
	if _, err = root.Mkdir(t.Context(), plan); !errors.Is(err, ErrChanged) {
		t.Fatalf("reused: %v", err)
	}
	existing, err := root.PrepareMkdir(t.Context(), "a/b", false)
	if err != nil || existing.Count() != 0 {
		t.Fatalf("existing=%+v %v", existing, err)
	}
}
func TestMkdirProtectedAndConflictingEntries(t *testing.T) {
	dir := t.TempDir()
	root, err := OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err = os.Mkdir(filepath.Join(dir, ArchiveDirectory), 0o700); err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink(ArchiveDirectory, filepath.Join(dir, "alias")); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dir, "file"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{".", "../outside", "/absolute", ArchiveDirectory + "/child", ".EDU-AGENT-ARCHIVE/x", "alias/child", "file", "file/child"} {
		if _, err = root.PrepareMkdir(t.Context(), p, true); err == nil {
			t.Fatalf("accepted %s", p)
		}
	}
	plan, err := root.PrepareMkdir(t.Context(), "collision/child", true)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Mkdir(filepath.Join(dir, "collision"), 0o700); err != nil {
		t.Fatal(err)
	}
	got, err := root.Mkdir(t.Context(), plan)
	if !errors.Is(err, ErrAlreadyExists) || got.Created != 0 || got.Outcome != PublishUnchanged {
		t.Fatalf("collision=%+v %v", got, err)
	}
}
func TestMkdirParentReplacementAndRelocation(t *testing.T) {
	for _, target := range []string{"replace", "outside", "archive"} {
		t.Run(target, func(t *testing.T) {
			dir := t.TempDir()
			root, err := OpenRoot(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			if err = os.Mkdir(filepath.Join(dir, "parent"), 0o700); err != nil {
				t.Fatal(err)
			}
			plan, err := root.PrepareMkdir(t.Context(), "parent/a/b", true)
			if err != nil {
				t.Fatal(err)
			}
			if target == "replace" {
				if err = os.Rename(filepath.Join(dir, "parent"), filepath.Join(dir, "old")); err != nil {
					t.Fatal(err)
				}
				if err = os.Mkdir(filepath.Join(dir, "parent"), 0o700); err != nil {
					t.Fatal(err)
				}
				got, err := root.Mkdir(t.Context(), plan)
				if !errors.Is(err, ErrChanged) || got.Created != 0 {
					t.Fatalf("replace=%+v %v", got, err)
				}
				return
			}
			destination := filepath.Join(t.TempDir(), "parent")
			if target == "archive" {
				if err = os.Mkdir(filepath.Join(dir, ArchiveDirectory), 0o700); err != nil {
					t.Fatal(err)
				}
				destination = filepath.Join(dir, ArchiveDirectory, "parent")
			}
			original := mkdirAtUnix
			defer func() { mkdirAtUnix = original }()
			calls := 0
			mkdirAtUnix = func(fd int, name string, mode uint32) error {
				calls++
				err := original(fd, name, mode)
				if err == nil && calls == 1 {
					err = os.Rename(filepath.Join(dir, "parent"), destination)
				}
				return err
			}
			got, err := root.Mkdir(t.Context(), plan)
			if got.Outcome != PublishUnknown || got.Created != 1 || !errors.Is(err, ErrOutcomeUnknown) || calls != 1 {
				t.Fatalf("relocate=%+v %v calls=%d", got, err, calls)
			}
			if _, err = os.Stat(filepath.Join(destination, "a")); err != nil {
				t.Fatalf("created dir rolled back: %v", err)
			}
			if _, err = os.Stat(filepath.Join(destination, "a/b")); !os.IsNotExist(err) {
				t.Fatalf("escaped second creation: %v", err)
			}
		})
	}
}
func TestMkdirPartialFailureCancellationAndSync(t *testing.T) {
	for _, fault := range []string{"cancel_before", "cancel_after", "second_create", "sync"} {
		t.Run(fault, func(t *testing.T) {
			dir := t.TempDir()
			root, err := OpenRoot(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			plan, err := root.PrepareMkdir(t.Context(), "a/b", true)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			original, sync := mkdirAtUnix, mkdirSyncUnix
			defer func() { mkdirAtUnix = original; mkdirSyncUnix = sync }()
			calls := 0
			mkdirAtUnix = func(fd int, name string, mode uint32) error {
				calls++
				if fault == "second_create" && calls == 2 {
					return unix.ENOSPC
				}
				err := original(fd, name, mode)
				if fault == "cancel_after" && err == nil {
					cancel()
				}
				return err
			}
			if fault == "sync" {
				mkdirSyncUnix = func(int) error { return unix.EIO }
			}
			if fault == "cancel_before" {
				cancel()
			}
			got, err := root.Mkdir(ctx, plan)
			if fault == "cancel_before" {
				if got.Outcome != PublishUnchanged || got.Created != 0 || !errors.Is(err, context.Canceled) {
					t.Fatalf("before=%+v %v", got, err)
				}
				return
			}
			if got.Outcome != PublishUnknown || got.Created != 1 || !errors.Is(err, ErrOutcomeUnknown) {
				t.Fatalf("partial=%+v %v", got, err)
			}
			if _, err = os.Stat(filepath.Join(dir, "a")); err != nil {
				t.Fatalf("lost known creation: %v", err)
			}
		})
	}
}
