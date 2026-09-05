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

func TestStatMetadataAndArchiveVersionCompatibility(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "folder"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, ArchiveDirectory), 0700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"file", ArchiveDirectory + "/file"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte{0, 255, 42}, 0600); err != nil {
			t.Fatal(err)
		}
	}
	root, err := OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	for name, kind := range map[string]EntryType{".": EntryDirectory, "folder": EntryDirectory, "file": EntryFile, ArchiveDirectory: EntryDirectory, ArchiveDirectory + "/file": EntryFile} {
		entry, err := root.Stat(t.Context(), name)
		if err != nil || entry.Kind != kind || entry.Identity == "" || !validArchiveVersion(entry.Version) || entry.ModTime.IsZero() {
			t.Fatalf("%s: %+v %v", name, entry, err)
		}
		if name == "file" || name == "folder" {
			archive, err := root.InspectArchiveSource(t.Context(), name)
			if err != nil || archive.Kind != entry.Kind || archive.Size != entry.Size || archive.Version != entry.Version || archive.Identity != entry.Identity {
				t.Fatalf("incompatible archive version: %+v %+v %v", archive, entry, err)
			}
		}
	}
	for _, name := range []string{"missing", "missing/child"} {
		if _, err := root.Stat(t.Context(), name); !errors.Is(err, ErrNotFound) {
			t.Fatalf("missing %s: %v", name, err)
		}
	}
	if _, err := root.Stat(t.Context(), "file/child"); !errors.Is(err, ErrNotDirectory) {
		t.Fatalf("not-directory misclassified: %v", err)
	}
	for _, name := range []string{"../escape", "/absolute", "folder/../file", "folder//file", `folder\file`} {
		if _, err := root.Stat(t.Context(), name); err == nil || errors.Is(err, ErrNotFound) {
			t.Fatalf("unsafe %q: %v", name, err)
		}
	}
	for _, name := range []string{".", ArchiveDirectory, ArchiveDirectory + "/file"} {
		if _, err := root.InspectArchiveSource(t.Context(), name); !errors.Is(err, ErrArchiveProtected) {
			t.Fatalf("archive policy changed: %s %v", name, err)
		}
	}
	before, _ := root.Stat(t.Context(), "file")
	mtime := time.Now().Add(2 * time.Hour)
	if err := os.Chtimes(filepath.Join(dir, "file"), mtime, mtime); err != nil {
		t.Fatal(err)
	}
	after, err := root.Stat(t.Context(), "file")
	if err != nil || after.Version == before.Version {
		t.Fatalf("touch did not change entry version: %+v %v", after, err)
	}
}

func TestStatHashRawBoundedAndChanged(t *testing.T) {
	dir := t.TempDir()
	data := []byte{0, 0xff, '\r', '\n', 0xef, 0xbb, 0xbf}
	if err := os.WriteFile(filepath.Join(dir, "data"), data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "large"), bytes.Repeat([]byte{0xff}, (1<<20)+1), 0600); err != nil {
		t.Fatal(err)
	}
	root, err := OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	entry, err := root.Stat(t.Context(), "data")
	if err != nil {
		t.Fatal(err)
	}
	hash, err := root.HashEntry(t.Context(), "data", entry, 1<<20)
	if err != nil || hash != snapshotContentHash(data) {
		t.Fatalf("raw hash: %s %v", hash, err)
	}
	large, err := root.Stat(t.Context(), "large")
	if err != nil || large.Size != (1<<20)+1 {
		t.Fatalf("large metadata: %+v %v", large, err)
	}
	if _, err := root.HashEntry(t.Context(), "large", large, 1<<20); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("large hash: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "data"), []byte("changed"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := root.HashEntry(t.Context(), "data", entry, 1<<20); !errors.Is(err, ErrChanged) {
		t.Fatalf("stale hash version accepted: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := root.Stat(ctx, "."); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := root.HashEntry(ctx, "data", entry, 1<<20); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := readEntryHash(ctx, strings.NewReader("x"), EntryInfo{Size: 1}, 10); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
