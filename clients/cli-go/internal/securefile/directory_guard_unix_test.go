//go:build linux || darwin

package securefile

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func directoryGuardTestWatch(t *testing.T, root *Root, relative string) *DirectoryChangeGuard {
	t.Helper()
	guard, err := root.WatchDirectory(t.Context(), relative)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = guard.Close() })
	if err := guard.Check(); err != nil {
		t.Fatalf("fresh guard: %v", err)
	}
	return guard
}

func directoryGuardTestEOF(t *testing.T, scan *DirectoryScan) {
	t.Helper()
	for page := 0; page < 10; page++ {
		_, _, done, err := scan.Next(t.Context(), 1)
		if err != nil {
			t.Fatal(err)
		}
		if done {
			return
		}
	}
	t.Fatal("small fixture did not reach EOF")
}

func TestDirectoryGuardDetachSurvivesScannerAndRootClose(t *testing.T) {
	for _, mutation := range []string{"add", "delete", "transient-add-delete", "rename-entry", "attribute", "rename-directory", "delete-directory"} {
		t.Run(mutation, func(t *testing.T) {
			base := t.TempDir()
			target := filepath.Join(base, "dir")
			if err := os.Mkdir(target, 0700); err != nil {
				t.Fatal(err)
			}
			victim := filepath.Join(target, "victim")
			if err := os.WriteFile(victim, nil, 0600); err != nil {
				t.Fatal(err)
			}
			root := directoryScanTestRoot(t, base)
			scan := directoryScanTestOpen(t, root, "dir")
			directory, anchor := scan.directory, scan.root.file
			if err := root.Close(); err != nil {
				t.Fatal(err)
			}
			directoryGuardTestEOF(t, scan)
			guard, err := scan.DetachChangeGuard()
			if err != nil || guard == nil {
				t.Fatalf("detach: %v, %v", guard, err)
			}
			t.Cleanup(func() { _ = guard.Close() })
			for _, file := range []*os.File{directory, anchor} {
				if _, err := file.Stat(); !errors.Is(err, os.ErrClosed) {
					t.Fatalf("detach retained a scanner handle: %v", err)
				}
			}
			if err := scan.Close(); err != nil {
				t.Fatal(err)
			}
			if err := guard.Check(); err != nil {
				t.Fatalf("scanner Close revoked detached guard: %v", err)
			}
			if _, _, _, err := scan.Next(t.Context(), 1); !errors.Is(err, os.ErrClosed) {
				t.Fatalf("detached scanner allowed Next: %v", err)
			}
			if again, err := scan.DetachChangeGuard(); again != nil || !errors.Is(err, os.ErrClosed) {
				t.Fatalf("repeated detach: %v, %v", again, err)
			}
			must := func(err error) {
				t.Helper()
				if err != nil {
					t.Fatal(err)
				}
			}
			switch mutation {
			case "add":
				must(os.WriteFile(filepath.Join(target, "added"), nil, 0600))
			case "delete":
				must(os.Remove(victim))
			case "transient-add-delete":
				name := filepath.Join(target, "temporary")
				must(os.WriteFile(name, nil, 0600))
				must(os.Remove(name))
			case "rename-entry":
				must(os.Rename(victim, filepath.Join(target, "renamed")))
			case "attribute":
				must(os.Chmod(target, 0750))
			case "rename-directory":
				must(os.Rename(target, filepath.Join(base, "moved")))
			case "delete-directory":
				must(os.RemoveAll(target))
			}
			for repeat := 0; repeat < 2; repeat++ {
				if err := guard.Check(); !errors.Is(err, ErrChanged) {
					t.Fatalf("detached change was not sticky: %v", err)
				}
			}
		})
	}
}

func TestDirectoryGuardDetachRequiresRealEOF(t *testing.T) {
	path := t.TempDir()
	directoryScanTestEntries(t, path, 1)
	scan := directoryScanTestOpen(t, directoryScanTestRoot(t, path), ".")
	for attempt := 0; attempt < 2; attempt++ {
		if guard, err := scan.DetachChangeGuard(); guard != nil || err == nil {
			t.Fatalf("premature detach: %v, %v", guard, err)
		}
		if attempt == 0 {
			entries, _, done, err := scan.Next(t.Context(), 1)
			if err != nil || done || len(entries) != 1 {
				t.Fatalf("full page: %+v, %v, %v", entries, done, err)
			}
		}
	}
	directoryGuardTestEOF(t, scan)
	guard, err := scan.DetachChangeGuard()
	if err != nil {
		t.Fatal(err)
	}
	if err := guard.Close(); err != nil {
		t.Fatal(err)
	}
	for _, closed := range []*DirectoryScan{scan, nil, {}} {
		if guard, err := closed.DetachChangeGuard(); guard != nil || !errors.Is(err, os.ErrClosed) {
			t.Fatalf("closed detach: %v, %v", guard, err)
		}
	}
}

func TestDirectoryGuardDetachRechecksAndClosesOnFailure(t *testing.T) {
	path := t.TempDir()
	scan := directoryScanTestOpen(t, directoryScanTestRoot(t, path), ".")
	directoryGuardTestEOF(t, scan)
	guard, directory, anchor := scan.guard, scan.directory, scan.root.file
	if err := os.WriteFile(filepath.Join(path, "late"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if detached, err := scan.DetachChangeGuard(); detached != nil || !errors.Is(err, ErrChanged) {
		t.Fatalf("changed detach: %v, %v", detached, err)
	}
	if err := guard.Check(); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("failed detach leaked guard: %v", err)
	}
	for _, file := range []*os.File{directory, anchor} {
		if _, err := file.Stat(); !errors.Is(err, os.ErrClosed) {
			t.Fatalf("failed detach leaked handle: %v", err)
		}
	}
}

func TestDirectoryGuardCloseOwnershipAndConcurrency(t *testing.T) {
	path := t.TempDir()
	root := directoryScanTestRoot(t, path)
	scan := directoryScanTestOpen(t, root, ".")
	owned := scan.guard
	if err := scan.Close(); err != nil {
		t.Fatal(err)
	}
	if err := owned.Check(); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("scanner Close did not release guard: %v", err)
	}
	guard := directoryGuardTestWatch(t, root, ".")
	start := make(chan struct{})
	failures := make(chan error, 12)
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(closeGuard bool) {
			defer wg.Done()
			<-start
			if closeGuard {
				failures <- guard.Close()
			} else if err := guard.Check(); err != nil && !errors.Is(err, os.ErrClosed) {
				failures <- err
			}
		}(i%3 == 0)
	}
	close(start)
	wg.Wait()
	close(failures)
	for err := range failures {
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, closed := range []*DirectoryChangeGuard{guard, nil, {}} {
		for repeat := 0; repeat < 2; repeat++ {
			if err := closed.Close(); err != nil {
				t.Fatal(err)
			}
		}
		if err := closed.Check(); !errors.Is(err, os.ErrClosed) {
			t.Fatalf("closed Check: %v", err)
		}
	}
}

func TestDirectoryGuardSharedDirectoryIndependentObservers(t *testing.T) {
	for _, closeFirst := range []bool{false, true} {
		t.Run(map[bool]string{false: "deliver-to-both", true: "close-one"}[closeFirst], func(t *testing.T) {
			path := t.TempDir()
			root := directoryScanTestRoot(t, path)
			first := directoryGuardTestWatch(t, root, ".")
			second := directoryGuardTestWatch(t, root, ".")
			if closeFirst {
				if err := first.Close(); err != nil {
					t.Fatal(err)
				}
			}
			if err := root.Close(); err != nil {
				t.Fatal(err)
			}
			if err := second.Check(); err != nil {
				t.Fatalf("peer/root close invalidated unchanged directory: %v", err)
			}
			if err := os.WriteFile(filepath.Join(path, "new"), nil, 0600); err != nil {
				t.Fatal(err)
			}
			if !closeFirst {
				if err := first.Check(); !errors.Is(err, ErrChanged) {
					t.Fatalf("first observer: %v", err)
				}
			}
			if err := second.Check(); !errors.Is(err, ErrChanged) {
				t.Fatalf("second observer missed consumed event: %v", err)
			}
		})
	}
}

func TestDirectoryGuardWatchRejectsUnsafePaths(t *testing.T) {
	path, outside := t.TempDir(), t.TempDir()
	if err := os.Mkdir(filepath.Join(path, "folder"), 0700); err != nil {
		t.Fatal(err)
	}
	for name, target := range map[string]string{"inside": "folder", "outside": outside, "dangling": "missing"} {
		if err := os.Symlink(target, filepath.Join(path, name)); err != nil {
			t.Fatal(err)
		}
	}
	root := directoryScanTestRoot(t, path)
	for _, relative := range []string{"inside", "outside", "dangling", "inside/child", "outside/child"} {
		if guard, err := root.WatchDirectory(t.Context(), relative); guard != nil || !errors.Is(err, ErrLink) {
			t.Fatalf("link %q accepted: %v, %v", relative, guard, err)
		}
	}
	for _, relative := range []string{"", "..", "../escape", "/absolute", "folder/../folder", "folder//child", `folder\child`} {
		if guard, err := root.WatchDirectory(t.Context(), relative); guard != nil || err == nil {
			t.Fatalf("unsafe %q accepted: %v, %v", relative, guard, err)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if guard, err := root.WatchDirectory(ctx, "."); guard != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled watch: %v, %v", guard, err)
	}
}
