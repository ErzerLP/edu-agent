//go:build linux || darwin

package securefile

import (
	"bytes"
	"context"
	"errors"
	"golang.org/x/sys/unix"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCopyStreamingFaultClassificationAndOwnedCleanup(t *testing.T) {
	for _, fault := range []string{"source_write", "source_replace", "cancel_read", "cancel_published", "target_race", "temp_open", "write", "sync", "parent_sync", "close", "rename_unknown", "rename_unsupported", "temp_replace"} {
		t.Run(fault, func(t *testing.T) {
			originalData := bytes.Repeat([]byte("binary\x00\xff"), 20000)
			dir, root, p := copyFixture(t, originalData)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			read, write, sync, close, rename, parentSync, tempOpen := copyRead, copyWrite, copySync, copyClose, copyRenameUnix, copyParentSyncUnix, copyTempOpenUnix
			defer func() {
				copyRead = read
				copyWrite = write
				copySync = sync
				copyClose = close
				copyRenameUnix = rename
				copyParentSyncUnix = parentSync
				copyTempOpenUnix = tempOpen
			}()
			changed := false
			copyRead = func(f *os.File, b []byte) (int, error) {
				n, err := read(f, b)
				if !changed {
					changed = true
					switch fault {
					case "source_write":
						if e := os.WriteFile(filepath.Join(dir, "source"), []byte("external"), 0600); e != nil {
							t.Fatal(e)
						}
					case "source_replace":
						if e := os.Rename(filepath.Join(dir, "source"), filepath.Join(dir, "old")); e != nil {
							t.Fatal(e)
						}
						if e := os.WriteFile(filepath.Join(dir, "source"), originalData, 0600); e != nil {
							t.Fatal(e)
						}
					case "cancel_read":
						cancel()
					}
				}
				return n, err
			}
			if fault == "temp_open" {
				copyTempOpenUnix = func(int, string, int, uint32) (int, error) { return -1, unix.ENOSPC }
			}
			if fault == "write" {
				copyWrite = func(*os.File, []byte) (int, error) { return 0, unix.ENOSPC }
			}
			if fault == "sync" {
				copySync = func(*os.File) error { return unix.EIO }
			}
			if fault == "parent_sync" {
				copyParentSyncUnix = func(int) error { return unix.EIO }
			}
			if fault == "close" {
				copyClose = func(f *os.File) error {
					e := close(f)
					if strings.HasPrefix(f.Name(), ".edu-agent-") {
						return errors.Join(unix.EIO, e)
					}
					return e
				}
			}
			if fault == "temp_replace" {
				copySync = func(f *os.File) error {
					if e := os.Rename(filepath.Join(dir, f.Name()), filepath.Join(dir, "owned-moved")); e != nil {
						t.Fatal(e)
					}
					if e := os.WriteFile(filepath.Join(dir, f.Name()), []byte("user-owned replacement"), 0600); e != nil {
						t.Fatal(e)
					}
					return sync(f)
				}
			}
			copyRenameUnix = func(a int, name string, b int, target string) error {
				if fault == "target_race" {
					if e := os.WriteFile(filepath.Join(dir, "target"), []byte("competitor"), 0600); e != nil {
						t.Fatal(e)
					}
				}
				if fault == "rename_unsupported" {
					return unix.ENOTSUP
				}
				e := rename(a, name, b, target)
				if fault == "cancel_published" {
					cancel()
				}
				if fault == "rename_unknown" && e == nil {
					return unix.EIO
				}
				return e
			}
			got, err := root.Copy(ctx, p)
			outcome := PublishUnchanged
			if fault == "cancel_published" {
				outcome = PublishCompleted
			}
			if fault == "parent_sync" || fault == "close" || fault == "rename_unknown" || fault == "temp_replace" {
				outcome = PublishUnknown
			}
			if got.Outcome != outcome {
				t.Fatalf("got=%+v err=%v want=%s", got, err, outcome)
			}
			if outcome == PublishUnknown && (!errors.Is(err, ErrOutcomeUnknown) || got.ContentHash != "") {
				t.Fatal("unknown lost", got, err)
			}
			if outcome == PublishCompleted && (err != nil || got.ContentHash == "") {
				t.Fatal("late cancellation lost completion", got, err)
			}
			if outcome == PublishUnchanged && err == nil {
				t.Fatal("fault not detected")
			}
			source, _ := os.ReadFile(filepath.Join(dir, "source"))
			if fault != "source_write" && !bytes.Equal(source, originalData) {
				t.Fatal("copy changed source")
			}
			target, targetErr := os.ReadFile(filepath.Join(dir, "target"))
			if fault == "target_race" {
				if string(target) != "competitor" {
					t.Fatal("overwrote competitor")
				}
			} else if fault == "parent_sync" || fault == "close" || fault == "rename_unknown" || fault == "cancel_published" {
				if targetErr != nil || !bytes.Equal(target, originalData) {
					t.Fatal("published result lost", targetErr)
				}
			} else if !os.IsNotExist(targetErr) {
				t.Fatal("unexpected publication", targetErr)
			}
			entries, _ := os.ReadDir(dir)
			for _, e := range entries {
				if strings.HasPrefix(e.Name(), ".edu-agent-") {
					if fault != "temp_replace" {
						t.Fatal("leaked temp", e.Name())
					}
					data, _ := os.ReadFile(filepath.Join(dir, e.Name()))
					if string(data) != "user-owned replacement" {
						t.Fatal("deleted another entry")
					}
				}
			}
		})
	}
}
func TestCopyParentIdentityRelocationAndAliases(t *testing.T) {
	for _, side := range []string{"source", "destination"} {
		for _, place := range []string{"replace_before", "inside", "outside", "archive"} {
			t.Run(side+"/"+place, func(t *testing.T) {
				dir := t.TempDir()
				for _, name := range []string{"src", "dst", ArchiveDirectory} {
					if err := os.Mkdir(filepath.Join(dir, name), 0700); err != nil {
						t.Fatal(err)
					}
				}
				data := bytes.Repeat([]byte("x"), 70000)
				if err := os.WriteFile(filepath.Join(dir, "src/file"), data, 0600); err != nil {
					t.Fatal(err)
				}
				root, err := OpenRoot(dir)
				if err != nil {
					t.Fatal(err)
				}
				defer root.Close()
				stat, err := root.Stat(t.Context(), "src/file")
				if err != nil {
					t.Fatal(err)
				}
				p, err := root.PrepareCopy(t.Context(), "src/file", "dst/copy", stat.Version)
				if err != nil {
					t.Fatal(err)
				}
				parent := "src"
				if side == "destination" {
					parent = "dst"
				}
				dest := filepath.Join(dir, "moved")
				if place == "outside" {
					dest = filepath.Join(t.TempDir(), "moved")
				}
				if place == "archive" {
					dest = filepath.Join(dir, ArchiveDirectory, "moved")
				}
				move := func() {
					if err := os.Rename(filepath.Join(dir, parent), dest); err != nil {
						t.Fatal(err)
					}
				}
				if place == "replace_before" {
					move()
					if err := os.Mkdir(filepath.Join(dir, parent), 0700); err != nil {
						t.Fatal(err)
					}
				}
				write := copyWrite
				defer func() { copyWrite = write }()
				once := false
				copyWrite = func(f *os.File, b []byte) (int, error) {
					n, e := write(f, b)
					if !once && place != "replace_before" {
						once = true
						move()
					}
					return n, e
				}
				result, err := root.Copy(t.Context(), p)
				if err == nil || result.Outcome == PublishCompleted {
					t.Fatal("relocation accepted", result, err)
				}
				if side == "destination" && place != "replace_before" && result.Outcome != PublishUnknown {
					t.Fatal("unremovable temp not unknown", result, err)
				}
				for _, path := range []string{filepath.Join(dir, "dst/copy"), filepath.Join(dest, "copy")} {
					if _, err := os.Stat(path); !os.IsNotExist(err) {
						t.Fatal("published through moved parent", path, err)
					}
				}
			})
		}
	}
	dir, root, p := copyFixture(t, []byte("safe"))
	for _, name := range []string{"link", "fifo", "directory"} {
		path := filepath.Join(dir, name)
		switch name {
		case "link":
			if err := os.Symlink("source", path); err != nil {
				t.Fatal(err)
			}
		case "fifo":
			if err := unix.Mkfifo(path, 0600); err != nil {
				t.Fatal(err)
			}
		case "directory":
			if err := os.Mkdir(path, 0700); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := root.PrepareCopy(t.Context(), name, "target", p.Version()); err == nil {
			t.Fatal("accepted nonregular", name)
		}
	}
	if err := os.Mkdir(filepath.Join(dir, ArchiveDirectory), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(ArchiveDirectory, filepath.Join(dir, "alias")); err != nil {
		t.Fatal(err)
	}
	if _, err := root.PrepareCopy(t.Context(), "source", "alias/target", p.Version()); err == nil {
		t.Fatal("archive alias accepted")
	}
}
func TestCopyStripsSpecialPermissionBits(t *testing.T) {
	dir, root, _ := copyFixture(t, []byte("mode"))
	if err := os.Chmod(filepath.Join(dir, "source"), os.ModeSetuid|os.ModeSetgid|os.ModeSticky|0751); err != nil {
		t.Fatal(err)
	}
	info, err := root.Stat(t.Context(), "source")
	if err != nil {
		t.Fatal(err)
	}
	p, err := root.PrepareCopy(t.Context(), "source", "target", info.Version)
	if err != nil {
		t.Fatal(err)
	}
	if result, err := root.Copy(t.Context(), p); err != nil || result.Outcome != PublishCompleted {
		t.Fatal(result, err)
	}
	mode, err := os.Stat(filepath.Join(dir, "target"))
	if err != nil {
		t.Fatal(err)
	}
	if mode.Mode().Perm() != 0751 || mode.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
		t.Fatal(mode.Mode())
	}
}
