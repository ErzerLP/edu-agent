//go:build linux || darwin || windows

package securefile

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func archiveTestRoot(t *testing.T) (*Root, string) {
	t.Helper()
	path := t.TempDir()
	root, err := OpenRoot(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	return root, path
}

func archiveTestWrite(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func archiveTestInspect(t *testing.T, root *Root, path string) ArchiveEntry {
	t.Helper()
	entry, err := root.InspectArchiveSource(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if entry.Identity == "" || !validArchiveVersion(entry.Version) {
		t.Fatalf("invalid entry: %+v", entry)
	}
	return entry
}

func TestArchiveBinaryAndNonemptyDirectory(t *testing.T) {
	for _, directory := range []bool{false, true} {
		name := "binary"
		if directory {
			name = "directory"
		}
		t.Run(name, func(t *testing.T) {
			root, path := archiveTestRoot(t)
			source := "nested/source"
			payload := source
			kind := EntryFile
			if directory {
				payload += "/child/deep/data.bin"
				kind = EntryDirectory
			}
			data := []byte{0, 255, 13, 10, 0, 128, 1, 2, 3}
			archiveTestWrite(t, filepath.Join(path, filepath.FromSlash(payload)), data)
			before, err := os.Stat(filepath.Join(path, filepath.FromSlash(source)))
			if err != nil {
				t.Fatal(err)
			}
			entry := archiveTestInspect(t, root, source)
			if entry.Kind != kind {
				t.Fatalf("entry=%+v", entry)
			}
			if _, err := os.Lstat(filepath.Join(path, ArchiveDirectory)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("inspection created archive: %v", err)
			}
			destination := ArchiveDirectory + "/frozen-candidate/" + source
			result, err := root.Archive(context.Background(), source, destination, entry)
			if err != nil || result.Outcome != PublishCompleted || !result.DirectoriesCreated {
				t.Fatalf("result=%+v err=%v", result, err)
			}
			if _, err := os.Lstat(filepath.Join(path, filepath.FromSlash(source))); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("source remains: %v", err)
			}
			after, err := os.Stat(filepath.Join(path, filepath.FromSlash(destination)))
			if err != nil || !os.SameFile(before, after) {
				t.Fatalf("not the same renamed entry: %v", err)
			}
			got, err := os.ReadFile(filepath.Join(path, ArchiveDirectory, "frozen-candidate", filepath.FromSlash(payload)))
			if err != nil || !bytes.Equal(data, got) {
				t.Fatalf("data=%v err=%v", got, err)
			}
			if err := root.CheckArchiveWritePath(context.Background(), destination); !errors.Is(err, ErrArchiveProtected) {
				t.Fatalf("write into archive: %v", err)
			}
		})
	}
}

func TestArchiveNoReplaceForFilesAndDirectories(t *testing.T) {
	for _, directory := range []bool{false, true} {
		t.Run(map[bool]string{false: "file", true: "directory"}[directory], func(t *testing.T) {
			root, path := archiveTestRoot(t)
			source, destination := "source", ArchiveDirectory+"/existing/source"
			payload := ""
			if directory {
				payload = "/child.bin"
			}
			archiveTestWrite(t, filepath.Join(path, filepath.FromSlash(source+payload)), []byte("source bytes"))
			archiveTestWrite(t, filepath.Join(path, filepath.FromSlash(destination+payload)), []byte("archive bytes"))
			entry := archiveTestInspect(t, root, source)
			result, err := root.Archive(context.Background(), source, destination, entry)
			if !errors.Is(err, ErrAlreadyExists) || result.Outcome != PublishUnchanged || result.DirectoriesCreated {
				t.Fatalf("result=%+v err=%v", result, err)
			}
			for relative, want := range map[string]string{source + payload: "source bytes", destination + payload: "archive bytes"} {
				got, err := os.ReadFile(filepath.Join(path, filepath.FromSlash(relative)))
				if err != nil || string(got) != want {
					t.Fatalf("%s=%q err=%v", relative, got, err)
				}
			}
		})
	}
}

func TestArchiveEntryMetadataPreconditions(t *testing.T) {
	for _, change := range []string{"replace", "size", "mtime", "expected-kind", "expected-size", "expected-identity", "expected-version"} {
		t.Run(change, func(t *testing.T) {
			root, path := archiveTestRoot(t)
			file := filepath.Join(path, "source")
			archiveTestWrite(t, file, []byte("initial"))
			entry := archiveTestInspect(t, root, "source")
			switch change {
			case "replace":
				if err := os.Rename(file, file+".old"); err != nil {
					t.Fatal(err)
				}
				archiveTestWrite(t, file, []byte("initial"))
			case "size":
				archiveTestWrite(t, file, []byte("changed-size"))
			case "mtime":
				mtime := time.Unix(123456789, 0)
				if err := os.Chtimes(file, mtime, mtime); err != nil {
					t.Fatal(err)
				}
			case "expected-kind":
				entry.Kind = EntryDirectory
			case "expected-size":
				entry.Size++
			case "expected-identity":
				entry.Identity += "other"
			case "expected-version":
				entry.Version = archiveMetadataVersion("different")
			}
			result, err := root.Archive(context.Background(), "source", ArchiveDirectory+"/candidate/source", entry)
			if !errors.Is(err, ErrChanged) || result.Outcome != PublishUnchanged || result.DirectoriesCreated {
				t.Fatalf("result=%+v err=%v", result, err)
			}
			if _, err := os.Stat(file); err != nil {
				t.Fatalf("source lost: %v", err)
			}
			if _, err := os.Lstat(filepath.Join(path, ArchiveDirectory)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("stale candidate created archive: %v", err)
			}
		})
	}
}

func TestArchiveProtectsSourceAndValidatesDestination(t *testing.T) {
	root, path := archiveTestRoot(t)
	archiveTestWrite(t, filepath.Join(path, "source"), []byte("bytes"))
	entry := archiveTestInspect(t, root, "source")
	for _, source := range []string{".", ArchiveDirectory, ArchiveDirectory + "/x", strings.ToUpper(ArchiveDirectory) + "/x"} {
		if _, err := root.InspectArchiveSource(context.Background(), source); !errors.Is(err, ErrArchiveProtected) {
			t.Fatalf("Inspect(%q): %v", source, err)
		}
		if err := root.CheckArchiveWritePath(context.Background(), source); !errors.Is(err, ErrArchiveProtected) {
			t.Fatalf("Check(%q): %v", source, err)
		}
		result, err := root.Archive(context.Background(), source, ArchiveDirectory+"/x/source", entry)
		if !errors.Is(err, ErrArchiveProtected) || result.Outcome != PublishUnchanged {
			t.Fatalf("Archive(%q): %+v %v", source, result, err)
		}
	}
	for _, destination := range []string{"other/source", ArchiveDirectory, ArchiveDirectory + "/source", ArchiveDirectory + "/../source", ArchiveDirectory + "/candidate/../../source", "/absolute/source", ArchiveDirectory + "/candidate//source", ArchiveDirectory + `/candidate\source`, strings.ToUpper(ArchiveDirectory) + "/candidate/source"} {
		result, err := root.Archive(context.Background(), "source", destination, entry)
		if err == nil || result.Outcome != PublishUnchanged || result.DirectoriesCreated {
			t.Fatalf("destination %q: %+v %v", destination, result, err)
		}
	}
	if err := root.CheckArchiveWritePath(context.Background(), "not-yet/created/file"); err != nil {
		t.Fatalf("normal create path: %v", err)
	}
	if err := root.CheckArchiveWritePath(context.Background(), "source"); err != nil {
		t.Fatalf("normal existing file: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(path, "not-yet")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("check created parent: %v", err)
	}
}

func TestArchiveCancellationAndInvalidExpectedAreSideEffectFree(t *testing.T) {
	root, path := archiveTestRoot(t)
	archiveTestWrite(t, filepath.Join(path, "source"), []byte("bytes"))
	entry := archiveTestInspect(t, root, "source")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result, err := root.Archive(ctx, "source", ArchiveDirectory+"/candidate/source", entry)
	if !errors.Is(err, context.Canceled) || result.Outcome != PublishUnchanged || result.DirectoriesCreated {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if _, err := root.InspectArchiveSource(ctx, "source"); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	for _, bad := range []ArchiveEntry{{}, {Kind: EntryOther, Size: 1, Identity: "x", Version: entry.Version}, {Kind: EntryFile, Size: -1, Identity: entry.Identity, Version: entry.Version}, {Kind: EntryFile, Identity: entry.Identity, Version: "content-hash"}} {
		result, err := root.Archive(context.Background(), "source", ArchiveDirectory+"/candidate/source", bad)
		if err == nil || result.Outcome != PublishUnchanged || result.DirectoriesCreated {
			t.Fatalf("bad=%+v result=%+v err=%v", bad, result, err)
		}
	}
	if _, err := os.Lstat(filepath.Join(path, ArchiveDirectory)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("created archive: %v", err)
	}
}

func TestArchiveReportsContainerSideEffectsWithoutCleanup(t *testing.T) {
	root, path := archiveTestRoot(t)
	archiveTestWrite(t, filepath.Join(path, "source"), []byte("bytes"))
	entry := archiveTestInspect(t, root, "source")
	destination := ArchiveDirectory + "/created/" + strings.Repeat("x", 300) + "/source"
	result, err := root.Archive(context.Background(), "source", destination, entry)
	if !errors.Is(err, ErrOutcomeUnknown) || result.Outcome != PublishUnknown || !result.DirectoriesCreated {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if _, err := os.Stat(filepath.Join(path, ArchiveDirectory, "created")); err != nil {
		t.Fatalf("container side effect missing: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(path, "source")); err != nil || string(got) != "bytes" {
		t.Fatalf("source=%q err=%v", got, err)
	}
}

func TestArchiveProtectedPublishIsOptInAndAllowsNormalFiles(t *testing.T) {
	root, path := archiveTestRoot(t)
	options := PublishOptions{Mode: PublishCreate, Permission: 0o600, ProtectArchive: true}
	for _, relative := range []string{ArchiveDirectory + "/candidate/file", strings.ToUpper(ArchiveDirectory) + "/candidate/file"} {
		result, err := root.Publish(context.Background(), relative, []byte("blocked"), options)
		if !errors.Is(err, ErrArchiveProtected) || result.Outcome != PublishUnchanged {
			t.Fatalf("protected %q: %+v %v", relative, result, err)
		}
	}
	result, err := root.Publish(context.Background(), "normal/new/file", []byte("created"), options)
	if err != nil || result.Outcome != PublishCompleted {
		t.Fatalf("normal create: %+v %v", result, err)
	}
	options.Mode, options.ExpectedHash, options.ExpectedLimit = PublishReplace, snapshotContentHash([]byte("created")), 1024
	result, err = root.Publish(context.Background(), "normal/new/file", []byte("updated"), options)
	if err != nil || result.Outcome != PublishCompleted {
		t.Fatalf("normal replace: %+v %v", result, err)
	}
	// Legacy/internal publication is unchanged when the option is absent.
	result, err = root.Publish(context.Background(), ArchiveDirectory+"/internal/file", []byte("internal"), PublishOptions{Mode: PublishCreate, Permission: 0o600})
	if err != nil || result.Outcome != PublishCompleted {
		t.Fatalf("unprotected compatibility: %+v %v", result, err)
	}
	if got, err := os.ReadFile(filepath.Join(path, "normal", "new", "file")); err != nil || string(got) != "updated" {
		t.Fatalf("normal=%q %v", got, err)
	}
}

func TestArchiveDirectoryVersionDoesNotSnapshotDescendantContents(t *testing.T) {
	root, path := archiveTestRoot(t)
	archiveTestWrite(t, filepath.Join(path, "source", "child"), []byte("initial"))
	entry := archiveTestInspect(t, root, "source")
	archiveTestWrite(t, filepath.Join(path, "source", "child"), []byte("updated descendant"))
	if after := archiveTestInspect(t, root, "source"); after != entry {
		t.Fatalf("descendant data changed entry version: before=%+v after=%+v", entry, after)
	}
	result, err := root.Archive(context.Background(), "source", ArchiveDirectory+"/candidate/source", entry)
	if err != nil || result.Outcome != PublishCompleted {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	got, err := os.ReadFile(filepath.Join(path, ArchiveDirectory, "candidate", "source", "child"))
	if err != nil || string(got) != "updated descendant" {
		t.Fatalf("descendant=%q err=%v", got, err)
	}
}
