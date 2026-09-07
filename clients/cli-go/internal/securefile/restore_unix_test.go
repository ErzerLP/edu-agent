//go:build linux || darwin

package securefile

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestArchiveRestoreLinksSpecialEntriesAndConflicts(t *testing.T) {
	for _, fault := range []string{"source-link", "source-fifo", "source-parent-link", "archive-link", "target-parent-link", "target-archive-alias", "target-outside-link", "target-link", "target-file", "target-directory", "target-hardlink"} {
		t.Run(fault, func(t *testing.T) {
			dir, root, p := restoreTestFixture(t, false)
			src, dst := filepath.Join(dir, p.Source()), filepath.Join(dir, p.Destination())
			outside := t.TempDir()
			archiveTestWrite(t, filepath.Join(outside, "sentinel"), []byte("untouched"))
			want := ErrLink
			switch fault {
			case "source-link", "source-fifo":
				if err := os.Rename(src, src+"-saved"); err != nil {
					t.Fatal(err)
				}
				if fault == "source-link" {
					if err := os.Symlink(filepath.Join(outside, "sentinel"), src); err != nil {
						t.Fatal(err)
					}
				} else {
					want = ErrNotRegular
					if err := unix.Mkfifo(src, 0600); err != nil {
						t.Fatal(err)
					}
				}
			case "source-parent-link", "archive-link", "target-parent-link":
				path := filepath.Dir(src)
				if fault == "archive-link" {
					path = filepath.Join(dir, ArchiveDirectory)
				}
				if fault == "target-parent-link" {
					path = filepath.Dir(dst)
				}
				if err := os.Rename(path, path+"-saved"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(path+"-saved", path); err != nil {
					t.Fatal(err)
				}
			case "target-archive-alias", "target-outside-link":
				target := filepath.Join(dir, ArchiveDirectory)
				if fault == "target-outside-link" {
					target = outside
				}
				if err := os.Symlink(target, filepath.Join(dir, "alias")); err != nil {
					t.Fatal(err)
				}
				dst = filepath.Join(dir, "alias/new-entry")
			case "target-link":
				want = ErrAlreadyExists
				if err := os.Symlink(filepath.Join(outside, "sentinel"), dst); err != nil {
					t.Fatal(err)
				}
			case "target-file":
				want = ErrAlreadyExists
				archiveTestWrite(t, dst, []byte("competitor"))
			case "target-directory":
				want = ErrAlreadyExists
				archiveTestWrite(t, filepath.Join(dst, "child"), []byte("competitor"))
			case "target-hardlink":
				want = ErrAlreadyExists
				if err := os.Link(src, dst); err != nil {
					t.Fatal(err)
				}
			}
			relative, err := filepath.Rel(dir, dst)
			if err != nil {
				t.Fatal(err)
			}
			// Hard-link creation changes Nlink/version; use the actual new version
			// so the conflict check, not a stale version, is exercised.
			version := p.Version()
			if fault == "target-hardlink" {
				entry, err := root.Stat(t.Context(), p.Source())
				if err != nil {
					t.Fatal(err)
				}
				version = entry.Version
			}
			plan, err := root.PrepareRestore(t.Context(), p.Source(), filepath.ToSlash(relative), version)
			if plan != nil || !errors.Is(err, want) {
				t.Fatal("unsafe preparation", plan, err, "want", want)
			}
			// The previously authorized plan must reject changed entries without
			// blocking when a FIFO has replaced a regular source.
			if fault != "target-archive-alias" && fault != "target-outside-link" {
				ctx, cancel := context.WithTimeout(t.Context(), time.Second)
				defer cancel()
				result, err := root.Restore(ctx, p)
				if err == nil || result.Outcome != PublishUnchanged {
					t.Fatal(result, err)
				}
			}
			restoreTestBytes(t, filepath.Join(outside, "sentinel"), []byte("untouched"))
			if fault == "target-file" {
				restoreTestBytes(t, dst, []byte("competitor"))
			}
			if fault == "target-directory" {
				restoreTestBytes(t, filepath.Join(dst, "child"), []byte("competitor"))
			}
		})
	}
}

func TestArchiveRestoreSourceMetadataChanges(t *testing.T) {
	for _, change := range []string{"inode", "size", "mtime", "mode", "directory-entry"} {
		t.Run(change, func(t *testing.T) {
			dir, root, p := restoreTestFixture(t, change == "directory-entry")
			source := filepath.Join(dir, p.Source())
			switch change {
			case "inode":
				if err := os.Rename(source, source+"-saved"); err != nil {
					t.Fatal(err)
				}
				archiveTestWrite(t, source, []byte("original\x00\xff"))
			case "size":
				archiveTestWrite(t, source, []byte("changed with a different size"))
			case "mode":
				if err := os.Chmod(source, 0400); err != nil {
					t.Fatal(err)
				}
			case "mtime", "directory-entry":
				if change == "directory-entry" {
					archiveTestWrite(t, filepath.Join(source, "new-child"), []byte("new"))
				}
				fixed := time.Unix(123456789, 0)
				if err := os.Chtimes(source, fixed, fixed); err != nil {
					t.Fatal(err)
				}
			}
			if plan, err := root.PrepareRestore(t.Context(), p.Source(), p.Destination(), p.Version()); plan != nil || !errors.Is(err, ErrChanged) {
				t.Fatal("stale preparation", plan, err)
			}
			if result, err := root.Restore(t.Context(), p); !errors.Is(err, ErrChanged) || result.Outcome != PublishUnchanged {
				t.Fatal(result, err)
			}
			if _, err := os.Lstat(source); err != nil {
				t.Fatal("source disappeared", err)
			}
			restoreTestAbsent(t, filepath.Join(dir, p.Destination()))
		})
	}
}

func TestArchiveRestoreParentsAndArchiveFrozen(t *testing.T) {
	for _, tc := range []struct{ side, location string }{
		{"archive", "workspace"}, {"archive", "outside"},
		{"source", "archive"}, {"source", "workspace"}, {"source", "outside"},
		{"destination", "archive"}, {"destination", "workspace"}, {"destination", "outside"},
	} {
		for _, stage := range []string{"before-commit", "held-before", "after-rename"} {
			t.Run(tc.side+"/"+tc.location+"/"+stage, func(t *testing.T) {
				dir, root, p := restoreTestFixture(t, false)
				old := filepath.Dir(filepath.Join(dir, p.Source()))
				if tc.side == "archive" {
					old = filepath.Join(dir, ArchiveDirectory)
				}
				if tc.side == "destination" {
					old = filepath.Dir(filepath.Join(dir, p.Destination()))
				}
				relocated := filepath.Join(dir, "relocated")
				if tc.location == "archive" {
					relocated = filepath.Join(dir, ArchiveDirectory, "relocated")
				}
				if tc.location == "outside" {
					relocated = filepath.Join(t.TempDir(), "relocated")
				}
				relocate := func() {
					if err := os.Rename(old, relocated); err != nil {
						t.Fatal(err)
					}
					if err := os.Mkdir(old, 0700); err != nil {
						t.Fatal(err)
					}
					if tc.side != "destination" {
						archiveTestWrite(t, filepath.Join(dir, p.Source()), []byte("replacement"))
					} else {
						archiveTestWrite(t, filepath.Join(old, "decoy"), []byte("untouched"))
					}
				}
				if stage == "held-before" {
					s, err := openRestoreState(t.Context(), root, p)
					if err != nil {
						t.Fatal(err)
					}
					defer s.close()
					relocate()
					if err := s.verify(t.Context(), p); err == nil {
						t.Fatal("relocated held parent accepted")
					}
				} else {
					rename := restoreRenameUnix
					defer func() { restoreRenameUnix = rename }()
					calls := 0
					restoreRenameUnix = func(a int, n string, b int, d string) error {
						calls++
						err := rename(a, n, b, d)
						if err == nil && stage == "after-rename" {
							relocate()
						}
						return err
					}
					if stage == "before-commit" {
						relocate()
					}
					result, err := root.Restore(t.Context(), p)
					if stage == "before-commit" {
						if err == nil || result.Outcome != PublishUnchanged || calls != 0 {
							t.Fatal("published through replaced parent", result, err, calls)
						}
					} else if !errors.Is(err, ErrOutcomeUnknown) || result.Outcome != PublishUnknown || calls != 1 {
						t.Fatal("lost publication uncertainty", result, err, calls)
					}
				}
				original := filepath.Join(dir, p.Source())
				if stage == "after-rename" {
					original = filepath.Join(dir, p.Destination())
					if tc.side == "destination" {
						original = filepath.Join(relocated, "restored")
					}
				} else if tc.side == "archive" {
					original = filepath.Join(relocated, "old-container/nested/entry")
				} else if tc.side == "source" {
					original = filepath.Join(relocated, "entry")
				}
				restoreTestBytes(t, original, []byte("original\x00\xff"))
				if tc.side != "destination" {
					restoreTestBytes(t, filepath.Join(dir, p.Source()), []byte("replacement"))
				} else {
					restoreTestBytes(t, filepath.Join(old, "decoy"), []byte("untouched"))
				}
				if stage != "after-rename" || tc.side == "destination" {
					restoreTestAbsent(t, filepath.Join(dir, p.Destination()))
				}
			})
		}
	}
}

func TestArchiveRestoreRenameFaultsAndLateCancellation(t *testing.T) {
	for _, fault := range []string{"conflict-file", "conflict-directory", "cross-device", "unsupported", "unknown-before", "unknown-after", "sync-source", "sync-destination", "close", "late-cancel", "lost-target", "source-recreated"} {
		t.Run(fault, func(t *testing.T) {
			dir, root, p := restoreTestFixture(t, fault == "conflict-directory")
			rename, sync, close := restoreRenameUnix, restoreSyncUnix, restoreClose
			defer func() { restoreRenameUnix, restoreSyncUnix, restoreClose = rename, sync, close }()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			calls, syncs := 0, 0
			published := false
			restoreRenameUnix = func(a int, n string, b int, d string) error {
				calls++
				switch fault {
				case "conflict-file":
					archiveTestWrite(t, filepath.Join(dir, p.Destination()), []byte("competitor"))
				case "conflict-directory":
					archiveTestWrite(t, filepath.Join(dir, p.Destination(), "child/file"), []byte("competitor"))
				case "cross-device":
					return unix.EXDEV
				case "unsupported":
					return unix.ENOTSUP
				case "unknown-before":
					return unix.EIO
				}
				err := rename(a, n, b, d)
				published = err == nil
				if err == nil {
					switch fault {
					case "unknown-after":
						return unix.EINTR
					case "late-cancel":
						cancel()
					case "lost-target":
						if err := os.Rename(filepath.Join(dir, p.Destination()), filepath.Join(dir, "relocated-target")); err != nil {
							t.Fatal(err)
						}
						archiveTestWrite(t, filepath.Join(dir, p.Destination()), []byte("replacement"))
					case "source-recreated":
						archiveTestWrite(t, filepath.Join(dir, p.Source()), []byte("replacement"))
					}
				}
				return err
			}
			restoreSyncUnix = func(fd int) error {
				syncs++
				if fault == "sync-source" && syncs == 1 || fault == "sync-destination" && syncs == 2 {
					return unix.EIO
				}
				return sync(fd)
			}
			// Fail the final held-handle close, not the no-side-effect parent
			// reopening check that happens before rename.
			restoreClose = func(f *os.File) error {
				err := close(f)
				if published && fault == "close" && f.Name() == p.Source() {
					return errors.Join(err, unix.EIO)
				}
				return err
			}
			result, err := root.Restore(ctx, p)
			want := PublishUnchanged
			if published || fault == "unknown-before" {
				want = PublishUnknown
			}
			if fault == "late-cancel" {
				want = PublishCompleted
			}
			if calls != 1 || result.Outcome != want {
				t.Fatal(result, err, "calls", calls, "want", want)
			}
			if want == PublishUnknown && !errors.Is(err, ErrOutcomeUnknown) || want == PublishCompleted && err != nil || want == PublishUnchanged && err == nil {
				t.Fatal("wrong outcome/error", result, err)
			}
			if fault == "cross-device" && !errors.Is(err, ErrCrossDevice) || fault == "unsupported" && !errors.Is(err, ErrArchiveUnsupported) || (fault == "conflict-file" || fault == "conflict-directory") && !errors.Is(err, ErrAlreadyExists) {
				t.Fatal("wrong stable classification", err)
			}
			if published {
				target := filepath.Join(dir, p.Destination())
				if fault == "lost-target" {
					target = filepath.Join(dir, "relocated-target")
					restoreTestBytes(t, filepath.Join(dir, p.Destination()), []byte("replacement"))
				}
				restoreTestBytes(t, target, []byte("original\x00\xff"))
				if fault == "source-recreated" {
					restoreTestBytes(t, filepath.Join(dir, p.Source()), []byte("replacement"))
				} else {
					restoreTestAbsent(t, filepath.Join(dir, p.Source()))
				}
			} else {
				payload := filepath.Join(dir, p.Source())
				if p.Kind() == EntryDirectory {
					payload = filepath.Join(payload, "child/file")
				}
				restoreTestBytes(t, payload, []byte("original\x00\xff"))
				if fault == "conflict-file" {
					restoreTestBytes(t, filepath.Join(dir, p.Destination()), []byte("competitor"))
				} else if fault == "conflict-directory" {
					restoreTestBytes(t, filepath.Join(dir, p.Destination(), "child/file"), []byte("competitor"))
				} else {
					restoreTestAbsent(t, filepath.Join(dir, p.Destination()))
				}
			}
			if _, err := os.Stat(filepath.Dir(filepath.Join(dir, p.Source()))); err != nil {
				t.Fatal("cleaned archive parent", err)
			}
			if result, err := root.Restore(t.Context(), p); !errors.Is(err, ErrChanged) || result.Outcome != PublishUnchanged || calls != 1 {
				t.Fatal("retried consumed plan", result, err, calls)
			}
		})
	}
}

func TestArchiveRestorePrepareClosesHandles(t *testing.T) {
	dir, root, p := restoreTestFixture(t, false)
	for _, fault := range []string{"none", "missing-parent", "close"} {
		t.Run(fault, func(t *testing.T) {
			close := restoreClose
			defer func() { restoreClose = close }()
			var closed []*os.File
			restoreClose = func(f *os.File) error {
				closed = append(closed, f)
				err := close(f)
				if fault == "close" {
					return errors.Join(err, unix.EIO)
				}
				return err
			}
			destination := p.Destination()
			if fault == "missing-parent" {
				destination = "missing/file"
			}
			plan, err := root.PrepareRestore(t.Context(), p.Source(), destination, p.Version())
			if fault == "none" && (err != nil || plan == nil) || fault != "none" && (err == nil || plan != nil) {
				t.Fatal(plan, err)
			}
			if len(closed) < 2 || len(closed) > 6 {
				t.Fatal("unbounded or unclosed state", len(closed))
			}
			for _, f := range closed {
				if _, err := f.Stat(); !errors.Is(err, os.ErrClosed) {
					t.Fatal("retained live handle", f.Name(), err)
				}
			}
			restoreTestBytes(t, filepath.Join(dir, p.Source()), []byte("original\x00\xff"))
			restoreTestAbsent(t, filepath.Join(dir, p.Destination()))
			restoreTestAbsent(t, filepath.Join(dir, "missing"))
		})
	}
}
