//go:build linux || darwin

package securefile

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func directoryScanTestRejectedPage(t *testing.T, scan *DirectoryScan, ctx context.Context, want error) {
	t.Helper()
	directory, anchor, frozen := scan.directory, scan.root.file, scan.Entry()
	entries, skipped, done, err := scan.Next(ctx, 2)
	if !errors.Is(err, want) || len(entries) != 0 || skipped != 0 || done {
		t.Fatalf("invalidated page leaked or misclassified: %+v, %d, %v, %v; want %v", entries, skipped, done, err, want)
	}
	if _, _, _, err := scan.Next(t.Context(), 1); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("invalidated scan was reusable: %v", err)
	}
	for _, file := range []*os.File{directory, anchor} {
		if _, err := file.Stat(); !errors.Is(err, os.ErrClosed) {
			t.Fatalf("invalidated scan leaked an owned handle: %v", err)
		}
	}
	if scan.Entry() != frozen {
		t.Fatal("error changed frozen Entry")
	}
	if err := scan.Close(); err != nil {
		t.Fatalf("Close after failure: %v", err)
	}
}

func TestDirectoryScanRejectsDirectoryChangesBetweenPages(t *testing.T) {
	for _, test := range []struct {
		name string
		want error
	}{
		{"add", ErrChanged},
		{"delete-entry", ErrChanged},
		{"replace-directory", ErrChanged},
		{"remove-path", ErrChanged},
		{"delete-directory", ErrChanged},
		{"touch", ErrChanged},
		{"move-parent-outside", ErrOutsideRoot},
		{"move-parent-inside", ErrChanged},
		{"replace-leaf-with-link", ErrLink},
		{"replace-parent-with-link", ErrLink},
	} {
		t.Run(test.name, func(t *testing.T) {
			base := t.TempDir()
			rootPath, outside := filepath.Join(base, "root"), filepath.Join(base, "outside")
			parent := filepath.Join(rootPath, "parent")
			target := filepath.Join(parent, "dir")
			if err := os.MkdirAll(target, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(outside, 0700); err != nil {
				t.Fatal(err)
			}
			directoryScanTestEntries(t, target, 3)
			root := directoryScanTestRoot(t, rootPath)
			scan := directoryScanTestOpen(t, root, "parent/dir")
			if entries, _, _, err := scan.Next(t.Context(), 1); err != nil || len(entries) != 1 {
				t.Fatalf("initial page: %+v, %v", entries, err)
			}
			must := func(err error) {
				t.Helper()
				if err != nil {
					t.Fatal(err)
				}
			}
			switch test.name {
			case "add":
				must(os.WriteFile(filepath.Join(target, "new"), nil, 0600))
			case "delete-entry":
				must(os.Remove(filepath.Join(target, "entry-0000")))
			case "replace-directory":
				must(os.Rename(target, filepath.Join(parent, "old")))
				must(os.Mkdir(target, 0700))
				directoryScanTestEntries(t, target, 3)
			case "remove-path":
				must(os.Rename(target, filepath.Join(parent, "old")))
			case "delete-directory":
				must(os.RemoveAll(target))
			case "touch":
				changed := scan.Entry().ModTime.Add(time.Hour)
				must(os.Chtimes(target, changed, changed))
			case "move-parent-outside":
				must(os.Rename(parent, filepath.Join(outside, "moved")))
				// An innocent-looking replacement must not hide the held
				// original directory's relocation outside the selected root.
				must(os.MkdirAll(target, 0700))
				directoryScanTestEntries(t, target, 3)
			case "move-parent-inside":
				must(os.Rename(parent, filepath.Join(rootPath, "moved")))
				must(os.MkdirAll(target, 0700))
				directoryScanTestEntries(t, target, 3)
			case "replace-leaf-with-link":
				must(os.Rename(target, filepath.Join(parent, "old")))
				must(os.Symlink(outside, target))
			case "replace-parent-with-link":
				must(os.Rename(parent, filepath.Join(rootPath, "old")))
				must(os.Symlink(outside, parent))
			}
			directoryScanTestRejectedPage(t, scan, t.Context(), test.want)
		})
	}
}

// Observing the descriptor's current offset does not seek away from it or
// discard os.File's buffered directory names. It gives deterministic fault
// timing after Readdirnames has consumed OS state, without production hooks or
// assumptions about the number of context checks in the implementation.
type directoryScanAfterReadContext struct {
	context.Context
	file      *os.File
	initial   int64
	afterRead func()
}

func (c *directoryScanAfterReadContext) Err() error {
	if c.afterRead != nil {
		position, err := unix.Seek(int(c.file.Fd()), 0, io.SeekCurrent)
		if err != nil {
			return err
		}
		if position != c.initial {
			afterRead := c.afterRead
			c.afterRead = nil
			afterRead()
		}
	}
	return c.Context.Err()
}

func TestDirectoryScanDiscardsPageAfterConsumedPositionError(t *testing.T) {
	for _, name := range []string{"add", "vanished-entry", "cancel", "move-parent-outside"} {
		t.Run(name, func(t *testing.T) {
			base := t.TempDir()
			rootPath := filepath.Join(base, "root")
			parent, outside := filepath.Join(rootPath, "parent"), filepath.Join(base, "outside")
			target := filepath.Join(parent, "dir")
			if err := os.MkdirAll(target, 0700); err != nil {
				t.Fatal(err)
			}
			directoryScanTestEntries(t, target, 3)
			scan := directoryScanTestOpen(t, directoryScanTestRoot(t, rootPath), "parent/dir")
			initial, err := unix.Seek(int(scan.directory.Fd()), 0, io.SeekCurrent)
			if err != nil {
				t.Skipf("deterministic consumed-position fault requires observable directory offsets: %v", err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			fired := false
			fault := &directoryScanAfterReadContext{Context: ctx, file: scan.directory, initial: initial}
			fault.afterRead = func() {
				fired = true
				var err error
				switch name {
				case "add":
					err = os.WriteFile(filepath.Join(target, "added-during-page"), nil, 0600)
				case "vanished-entry":
					// Delete every possible first name; none of this page may
					// be returned as a partial success or unexplained skip.
					err = os.RemoveAll(target)
				case "cancel":
					cancel()
				case "move-parent-outside":
					err = os.Rename(parent, outside)
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			want := ErrChanged
			if name == "cancel" {
				want = context.Canceled
			} else if name == "move-parent-outside" {
				want = ErrOutsideRoot
			}
			directoryScanTestRejectedPage(t, scan, fault, want)
			if !fired {
				t.Fatal("fault did not occur after the OS position advanced")
			}
		})
	}
}
