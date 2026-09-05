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

func TestArchiveRejectsLinksAndSpecialEntriesWithoutOpeningContents(t *testing.T) {
	root, path := archiveTestRoot(t)
	outside := t.TempDir()
	archiveTestWrite(t, filepath.Join(outside, "secret"), []byte("outside"))
	if err := os.Symlink(outside, filepath.Join(path, "link")); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(filepath.Join(path, "fifo"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, source := range []string{"link", "link/secret", "fifo"} {
		_, err := root.InspectArchiveSource(context.Background(), source)
		if err == nil {
			t.Fatalf("accepted %q", source)
		}
		if source == "fifo" && !errors.Is(err, ErrNotRegular) {
			t.Fatalf("FIFO error=%v", err)
		}
	}
	if err := root.CheckArchiveWritePath(context.Background(), "link/new/file"); err == nil {
		t.Fatal("write check accepted linked ancestor")
	}
}

func TestArchiveMovesInternalLinksWithoutFollowingThem(t *testing.T) {
	root, path := archiveTestRoot(t)
	outside := filepath.Join(t.TempDir(), "secret")
	archiveTestWrite(t, outside, []byte("outside"))
	archiveTestWrite(t, filepath.Join(path, "source", "child"), []byte("inside"))
	if err := os.Symlink(outside, filepath.Join(path, "source", "link")); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(filepath.Join(path, "source", "fifo"), 0o600); err != nil {
		t.Fatal(err)
	}
	entry := archiveTestInspect(t, root, "source")
	result, err := root.Archive(context.Background(), "source", ArchiveDirectory+"/candidate/source", entry)
	if err != nil || result.Outcome != PublishCompleted {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	link, err := os.Readlink(filepath.Join(path, ArchiveDirectory, "candidate", "source", "link"))
	if err != nil || link != outside {
		t.Fatalf("link=%q err=%v", link, err)
	}
	stat, err := os.Lstat(filepath.Join(path, ArchiveDirectory, "candidate", "source", "fifo"))
	if err != nil || stat.Mode()&os.ModeNamedPipe == 0 {
		t.Fatalf("internal FIFO was not preserved: %v", err)
	}
	got, err := os.ReadFile(outside)
	if err != nil || string(got) != "outside" {
		t.Fatalf("outside=%q err=%v", got, err)
	}
}

func TestArchiveRejectsLinkedDestinationRootAndParents(t *testing.T) {
	for _, linked := range []string{ArchiveDirectory, ArchiveDirectory + "/candidate"} {
		t.Run(filepath.Base(linked), func(t *testing.T) {
			root, path := archiveTestRoot(t)
			archiveTestWrite(t, filepath.Join(path, "source"), []byte("bytes"))
			entry := archiveTestInspect(t, root, "source")
			outside := t.TempDir()
			if err := os.MkdirAll(filepath.Dir(filepath.Join(path, filepath.FromSlash(linked))), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, filepath.Join(path, filepath.FromSlash(linked))); err != nil {
				t.Fatal(err)
			}
			result, err := root.Archive(context.Background(), "source", ArchiveDirectory+"/candidate/source", entry)
			if !errors.Is(err, ErrLink) || result.Outcome != PublishUnchanged || result.DirectoriesCreated {
				t.Fatalf("result=%+v err=%v", result, err)
			}
			entries, err := os.ReadDir(outside)
			if err != nil || len(entries) != 0 {
				t.Fatalf("escaped: %+v %v", entries, err)
			}
			if got, err := root.ReadLimit("source", 1024, false); err != nil || string(got) != "bytes" {
				t.Fatalf("normal reads affected: %q %v", got, err)
			}
		})
	}
}

func TestArchiveHandleAncestryRejectsMovedParents(t *testing.T) {
	for _, move := range []string{"archive", "outside"} {
		t.Run(move, func(t *testing.T) {
			root, path := archiveTestRoot(t)
			archiveTestWrite(t, filepath.Join(path, "parent", "source"), []byte("bytes"))
			if err := os.Mkdir(filepath.Join(path, ArchiveDirectory), 0o700); err != nil {
				t.Fatal(err)
			}
			parent, _, err := openPublishParentWithinRoot(root, []string{"parent"}, false)
			if err != nil {
				t.Fatal(err)
			}
			defer parent.Close()
			destination, want := filepath.Join(path, ArchiveDirectory, "moved"), ErrArchiveProtected
			if move == "outside" {
				destination, want = filepath.Join(t.TempDir(), "moved"), ErrOutsideRoot
			}
			if err := os.Rename(filepath.Join(path, "parent"), destination); err != nil {
				t.Fatal(err)
			}
			if err := checkArchivePublishParent(context.Background(), root, parent); !errors.Is(err, want) {
				t.Fatalf("held parent check=%v, want %v", err, want)
			}
		})
	}
}

func TestArchiveProtectedPublishRechecksOpenedParentAfterVersionValidation(t *testing.T) {
	root, path := archiveTestRoot(t)
	archiveTestWrite(t, filepath.Join(path, "parent", "source"), []byte("old"))
	if err := os.Mkdir(filepath.Join(path, ArchiveDirectory), 0o700); err != nil {
		t.Fatal(err)
	}
	original := snapshotFileIdentityForPlatform
	defer func() { snapshotFileIdentityForPlatform = original }()
	moved := false
	snapshotFileIdentityForPlatform = func(file *os.File, info os.FileInfo) (string, error) {
		identity, err := original(file, info)
		if err != nil {
			return "", err
		}
		if !moved {
			if err := os.Rename(filepath.Join(path, "parent"), filepath.Join(path, ArchiveDirectory, "moved")); err != nil {
				return "", err
			}
			moved = true
		}
		return identity, nil
	}
	result, err := root.Publish(context.Background(), "parent/source", []byte("new"), PublishOptions{
		Mode: PublishReplace, Permission: 0o600, ExpectedHash: snapshotContentHash([]byte("old")), ExpectedLimit: 1024, ProtectArchive: true,
	})
	if !moved || !errors.Is(err, ErrArchiveProtected) || result.Outcome != PublishUnchanged {
		t.Fatalf("moved=%v result=%+v err=%v", moved, result, err)
	}
	got, err := os.ReadFile(filepath.Join(path, ArchiveDirectory, "moved", "source"))
	if err != nil || string(got) != "old" {
		t.Fatalf("archive was overwritten: %q %v", got, err)
	}
}

func TestArchiveRenameErrorClassification(t *testing.T) {
	for _, tc := range []struct{ errno, want error }{{unix.EXDEV, ErrCrossDevice}, {unix.ENOSYS, ErrArchiveUnsupported}, {unix.ENOTSUP, ErrArchiveUnsupported}, {unix.EEXIST, ErrAlreadyExists}} {
		if !errors.Is(archiveUnixError(tc.errno), tc.want) || !unixArchiveRenameUnchanged(tc.errno) {
			t.Fatalf("classification %v", tc.errno)
		}
	}
	if unixArchiveRenameUnchanged(unix.EIO) || unixArchiveRenameUnchanged(unix.EINTR) {
		t.Fatal("ambiguous rename failure claimed unchanged")
	}
}

func TestArchiveRenameFaultsAndLateCancellation(t *testing.T) {
	for _, fault := range []string{"late-cancel", "cross-device", "unsupported", "ambiguous", "sync"} {
		t.Run(fault, func(t *testing.T) {
			root, path := archiveTestRoot(t)
			archiveTestWrite(t, filepath.Join(path, "source"), []byte("bytes"))
			if err := os.MkdirAll(filepath.Join(path, ArchiveDirectory, "candidate"), 0o700); err != nil {
				t.Fatal(err)
			}
			entry := archiveTestInspect(t, root, "source")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			originalRename, originalSync := archiveRenameUnix, archiveSyncUnix
			defer func() { archiveRenameUnix, archiveSyncUnix = originalRename, originalSync }()
			calls := 0
			archiveRenameUnix = func(fromFD int, from string, toFD int, to string) error {
				calls++
				switch fault {
				case "cross-device":
					return unix.EXDEV
				case "unsupported":
					return unix.ENOSYS
				case "ambiguous":
					return unix.EIO
				}
				err := originalRename(fromFD, from, toFD, to)
				if fault == "late-cancel" {
					cancel()
				}
				return err
			}
			if fault == "sync" {
				archiveSyncUnix = func(int) error { return unix.EIO }
			}
			result, err := root.Archive(ctx, "source", ArchiveDirectory+"/candidate/source", entry)
			wantOutcome, wantErr := PublishUnchanged, ErrCrossDevice
			switch fault {
			case "late-cancel":
				wantOutcome, wantErr = PublishCompleted, nil
				if !errors.Is(ctx.Err(), context.Canceled) {
					t.Fatal("late cancellation did not occur")
				}
			case "unsupported":
				wantErr = ErrArchiveUnsupported
			case "ambiguous", "sync":
				wantOutcome, wantErr = PublishUnknown, ErrOutcomeUnknown
			}
			if calls != 1 || !errors.Is(err, wantErr) || result.Outcome != wantOutcome || result.DirectoriesCreated {
				t.Fatalf("calls=%d result=%+v err=%v", calls, result, err)
			}
			preserved := filepath.Join(path, "source")
			if fault == "sync" || fault == "late-cancel" {
				preserved = filepath.Join(path, ArchiveDirectory, "candidate", "source")
				if _, err := os.Lstat(filepath.Join(path, "source")); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("source still present after rename: %v", err)
				}
			}
			if got, err := os.ReadFile(preserved); err != nil || string(got) != "bytes" {
				t.Fatalf("preserved=%q err=%v", got, err)
			}
		})
	}
}

type archiveCancelAfterCreateContext struct {
	context.Context
	path string
}

func (ctx archiveCancelAfterCreateContext) Err() error {
	if _, err := os.Stat(ctx.path); err == nil {
		return context.Canceled
	}
	return nil
}

func TestArchiveCancellationAfterContainerCreationReportsSideEffect(t *testing.T) {
	root, path := archiveTestRoot(t)
	archiveTestWrite(t, filepath.Join(path, "source"), []byte("bytes"))
	entry := archiveTestInspect(t, root, "source")
	ctx := archiveCancelAfterCreateContext{Context: context.Background(), path: filepath.Join(path, ArchiveDirectory, "candidate")}
	result, err := root.Archive(ctx, "source", ArchiveDirectory+"/candidate/source", entry)
	if !errors.Is(err, context.Canceled) || !errors.Is(err, ErrOutcomeUnknown) || result.Outcome != PublishUnknown || !result.DirectoriesCreated {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if got, err := os.ReadFile(filepath.Join(path, "source")); err != nil || string(got) != "bytes" {
		t.Fatalf("source=%q err=%v", got, err)
	}
}
