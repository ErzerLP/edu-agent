//go:build linux || darwin

package securefile

import (
	"context"
	"errors"
	"golang.org/x/sys/unix"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMoveRenameFaultsNoFallbackNoOverwrite(t *testing.T) {
	for _, fault := range []string{"conflict", "cross_device", "unsupported", "unknown_before", "unknown_after", "sync", "close", "late_cancel"} {
		t.Run(fault, func(t *testing.T) {
			dir, root, p := moveFixture(t, false)
			rename, sync, close := moveRenameUnix, moveSyncUnix, moveClose
			defer func() { moveRenameUnix, moveSyncUnix, moveClose = rename, sync, close }()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			calls := 0
			moveRenameUnix = func(a int, n string, b int, d string) error {
				calls++
				switch fault {
				case "conflict":
					if e := os.WriteFile(filepath.Join(dir, "target"), []byte("competitor"), 0600); e != nil {
						t.Fatal(e)
					}
				case "cross_device":
					return unix.EXDEV
				case "unsupported":
					return unix.ENOTSUP
				case "unknown_before":
					return unix.EIO
				}
				e := rename(a, n, b, d)
				if e == nil && fault == "unknown_after" {
					return unix.EIO
				}
				if fault == "late_cancel" {
					cancel()
				}
				return e
			}
			if fault == "sync" {
				moveSyncUnix = func(int) error { return unix.EIO }
			}
			if fault == "close" {
				moveClose = func(f *os.File) error { return errors.Join(close(f), unix.EIO) }
			}
			result, err := root.Move(ctx, p)
			want := PublishUnchanged
			published := fault == "unknown_after" || fault == "sync" || fault == "close" || fault == "late_cancel"
			if fault == "unknown_before" || published {
				want = PublishUnknown
			}
			if fault == "late_cancel" {
				want = PublishCompleted
			}
			if result.Outcome != want || calls != 1 {
				t.Fatal(result, err, calls, want)
			}
			if want == PublishUnknown && !errors.Is(err, ErrOutcomeUnknown) {
				t.Fatal("unknown lost", err)
			}
			if want == PublishCompleted && err != nil {
				t.Fatal("late cancellation undone", err)
			}
			if fault == "cross_device" && !errors.Is(err, ErrCrossDevice) || fault == "unsupported" && !errors.Is(err, ErrArchiveUnsupported) {
				t.Fatal("wrong classification", err)
			}
			src, srcErr := os.ReadFile(filepath.Join(dir, "source"))
			dst, dstErr := os.ReadFile(filepath.Join(dir, "target"))
			if published {
				if !os.IsNotExist(srcErr) || dstErr != nil || string(dst) != "original\x00\xff" {
					t.Fatal("lost renamed entry", srcErr, dstErr)
				}
			} else {
				if srcErr != nil || string(src) != "original\x00\xff" {
					t.Fatal("source changed", srcErr)
				}
				if fault == "conflict" {
					if string(dst) != "competitor" {
						t.Fatal("overwritten")
					}
				} else if !os.IsNotExist(dstErr) {
					t.Fatal("fallback publication", dstErr)
				}
			}
			entries, _ := os.ReadDir(dir)
			for _, entry := range entries {
				if strings.HasPrefix(entry.Name(), ".edu-agent-") {
					t.Fatal("temp or archive created", entry)
				}
			}
		})
	}
}
func TestMoveParentsFrozenAndRelocationDetected(t *testing.T) {
	for _, side := range []string{"src", "dst"} {
		for _, stage := range []string{"before_commit", "held_before", "after_rename"} {
			for _, location := range []string{"inside", "outside", "archive"} {
				t.Run(side+"/"+stage+"/"+location, func(t *testing.T) {
					dir := t.TempDir()
					for _, name := range []string{"src", "dst", ArchiveDirectory} {
						if err := os.Mkdir(filepath.Join(dir, name), 0700); err != nil {
							t.Fatal(err)
						}
					}
					if err := os.WriteFile(filepath.Join(dir, "src/file"), []byte("original"), 0600); err != nil {
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
					p, err := root.PrepareMove(t.Context(), "src/file", "dst/file", stat.Version)
					if err != nil {
						t.Fatal(err)
					}
					relocated := filepath.Join(dir, "relocated")
					if location == "outside" {
						relocated = filepath.Join(t.TempDir(), "relocated")
					}
					if location == "archive" {
						relocated = filepath.Join(dir, ArchiveDirectory, "relocated")
					}
					relocate := func() {
						if e := os.Rename(filepath.Join(dir, side), relocated); e != nil {
							t.Fatal(e)
						}
						if e := os.Mkdir(filepath.Join(dir, side), 0700); e != nil {
							t.Fatal(e)
						}
					}
					if stage == "held_before" {
						s, e := openMoveState(t.Context(), root, p, true)
						if e != nil {
							t.Fatal(e)
						}
						defer s.close()
						relocate()
						if err = s.verify(t.Context(), p); err == nil {
							t.Fatal("held parent relocation accepted")
						}
						return
					}
					rename := moveRenameUnix
					defer func() { moveRenameUnix = rename }()
					calls := 0
					moveRenameUnix = func(a int, n string, b int, d string) error {
						calls++
						e := rename(a, n, b, d)
						if e == nil && stage == "after_rename" {
							relocate()
						}
						return e
					}
					if stage == "before_commit" {
						relocate()
					}
					result, err := root.Move(t.Context(), p)
					if err == nil || result.Outcome == PublishCompleted {
						t.Fatal("relocation accepted", result, err)
					}
					if stage == "before_commit" {
						if calls != 0 || result.Outcome != PublishUnchanged {
							t.Fatal("published replaced parent", result, calls)
						}
					} else {
						if result.Outcome != PublishUnknown {
							t.Fatal("lost published uncertainty", result, err)
						}
					}
				})
			}
		}
	}
}
func TestMoveLinksSpecialEntriesAndHardlinkConflicts(t *testing.T) {
	dir, root, p := moveFixture(t, false)
	if err := os.Mkdir(filepath.Join(dir, ArchiveDirectory), 0700); err != nil {
		t.Fatal(err)
	}
	for _, link := range []struct{ name, target string }{{"link", "source"}, {"archive-alias", ArchiveDirectory}, {"outside", t.TempDir()}} {
		if err := os.Symlink(link.target, filepath.Join(dir, link.name)); err != nil {
			t.Fatal(err)
		}
	}
	if err := unix.Mkfifo(filepath.Join(dir, "fifo"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(filepath.Join(dir, "source"), filepath.Join(dir, "hardlink")); err != nil {
		t.Fatal(err)
	}
	current, err := root.Stat(t.Context(), "source")
	if err != nil {
		t.Fatal(err)
	}
	for _, pair := range [][2]string{{"link", "target"}, {"link", "link"}, {"fifo", "target"}, {"fifo", "fifo"}, {"source", "hardlink"}, {"source", "archive-alias/new"}, {"source", "outside/new"}} {
		if _, err := root.PrepareMove(t.Context(), pair[0], pair[1], current.Version); err == nil {
			t.Fatal("accepted unsafe pair", pair)
		}
	}
	// Replacement with a FIFO between prepare and commit must not block on open.
	if err := os.Rename(filepath.Join(dir, "source"), filepath.Join(dir, "saved")); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(filepath.Join(dir, "source"), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 1000000000)
	defer cancel()
	if result, err := root.Move(ctx, p); err == nil || result.Outcome != PublishUnchanged {
		t.Fatal(result, err)
	}
}
