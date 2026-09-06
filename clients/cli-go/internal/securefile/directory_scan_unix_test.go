//go:build linux || darwin

package securefile

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"golang.org/x/sys/unix"
)

func directoryScanTestEntries(t *testing.T, path string, count int) {
	t.Helper()
	for i := 0; i < count; i++ {
		if err := os.WriteFile(filepath.Join(path, fmt.Sprintf("entry-%04d", i)), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
}

func directoryScanTestRoot(t *testing.T, path string) *Root {
	t.Helper()
	root, err := OpenRoot(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	return root
}

func directoryScanTestOpen(t *testing.T, root *Root, relative string) *DirectoryScan {
	t.Helper()
	scan, err := root.OpenDirectoryScan(t.Context(), relative)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = scan.Close() })
	return scan
}

func TestDirectoryScanContinuesBeyond2000(t *testing.T) {
	path := t.TempDir()
	if err := os.Mkdir(filepath.Join(path, "many"), 0700); err != nil {
		t.Fatal(err)
	}
	directoryScanTestEntries(t, filepath.Join(path, "many"), 2501)
	root := directoryScanTestRoot(t, path)
	scan := directoryScanTestOpen(t, root, "many")
	frozen, err := root.Stat(t.Context(), "many")
	if err != nil || scan.Entry() != frozen || frozen.Kind != EntryDirectory || frozen.Version == "" {
		t.Fatalf("frozen metadata: %+v, %+v, %v", scan.Entry(), frozen, err)
	}
	seen := make(map[string]bool)
	for page := 0; page < 100; page++ {
		entries, skipped, complete, err := scan.Next(t.Context(), 73)
		if err != nil || skipped != 0 || len(entries) > 73 {
			t.Fatalf("page %d: %d entries, skipped=%d, complete=%v, err=%v", page, len(entries), skipped, complete, err)
		}
		for _, entry := range entries {
			if seen[entry.Name] || entry.Type != EntryFile {
				t.Fatalf("duplicate or wrong classification: %+v", entry)
			}
			seen[entry.Name] = true
		}
		if complete {
			if len(seen) != 2501 {
				t.Fatalf("false EOF: got %d entries", len(seen))
			}
			for i := 0; i < 2501; i++ {
				if !seen[fmt.Sprintf("entry-%04d", i)] {
					t.Fatalf("missing entry %d", i)
				}
			}
			return
		}
	}
	t.Fatal("scan never reached EOF")
}

func TestDirectoryScanRootEOFAndEmpty(t *testing.T) {
	for _, count := range []int{0, 4} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			path := t.TempDir()
			directoryScanTestEntries(t, path, count)
			scan := directoryScanTestOpen(t, directoryScanTestRoot(t, path), ".")
			for page := 0; page < count/2; page++ {
				entries, skipped, complete, err := scan.Next(t.Context(), 2)
				if err != nil || len(entries) != 2 || skipped != 0 || complete {
					t.Fatalf("full page is not EOF: %+v, %d, %v, %v", entries, skipped, complete, err)
				}
			}
			for repeat := 0; repeat < 2; repeat++ {
				entries, skipped, complete, err := scan.Next(t.Context(), 2)
				if err != nil || len(entries) != 0 || skipped != 0 || !complete {
					t.Fatalf("real EOF: %+v, %d, %v, %v", entries, skipped, complete, err)
				}
			}
		})
	}
}

func TestDirectoryScanPreCanceledAndInvalidLimitDoNotConsume(t *testing.T) {
	path := t.TempDir()
	directoryScanTestEntries(t, path, 3)
	root := directoryScanTestRoot(t, path)
	scan := directoryScanTestOpen(t, root, ".")
	first, _, _, err := scan.Next(t.Context(), 1)
	if err != nil || len(first) != 1 {
		t.Fatalf("first page: %+v, %v", first, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if opened, err := root.OpenDirectoryScan(ctx, "."); !errors.Is(err, context.Canceled) || opened != nil {
		t.Fatalf("canceled open: %v, %v", opened, err)
	}
	if entries, skipped, done, err := scan.Next(ctx, 1); !errors.Is(err, context.Canceled) || len(entries) != 0 || skipped != 0 || done {
		t.Fatalf("canceled page: %+v, %d, %v, %v", entries, skipped, done, err)
	}
	for _, limit := range []int{-1, 0, 65537} {
		if entries, skipped, done, err := scan.Next(t.Context(), limit); err == nil || len(entries) != 0 || skipped != 0 || done {
			t.Fatalf("invalid limit %d accepted: %+v, %d, %v, %v", limit, entries, skipped, done, err)
		}
	}
	rest, skipped, _, err := scan.Next(t.Context(), 65536)
	if err != nil || skipped != 0 || len(rest) != 2 || rest[0].Name == first[0].Name || rest[1].Name == first[0].Name || rest[0].Name == rest[1].Name {
		t.Fatalf("position changed: first=%+v, rest=%+v, skipped=%d, err=%v", first, rest, skipped, err)
	}
}

func TestDirectoryScanCloseAndIndependentRootAnchor(t *testing.T) {
	path := t.TempDir()
	directoryScanTestEntries(t, path, 3)
	root := directoryScanTestRoot(t, path)
	scan := directoryScanTestOpen(t, root, ".")
	entry, directory, anchor := scan.Entry(), scan.directory, scan.root.file
	if err := root.Close(); err != nil {
		t.Fatal(err)
	}
	if entries, _, _, err := scan.Next(t.Context(), 1); err != nil || len(entries) != 1 {
		t.Fatalf("scan depended on original Root: %+v, %v", entries, err)
	}
	for repeat := 0; repeat < 2; repeat++ {
		if err := scan.Close(); err != nil {
			t.Fatal(err)
		}
	}
	for _, file := range []*os.File{directory, anchor} {
		if _, err := file.Stat(); !errors.Is(err, os.ErrClosed) {
			t.Fatalf("owned descriptor not closed: %v", err)
		}
	}
	if scan.Entry() != entry {
		t.Fatal("Close changed frozen metadata")
	}
	for _, closed := range []*DirectoryScan{scan, nil, {}} {
		if entries, skipped, done, err := closed.Next(t.Context(), 1); !errors.Is(err, os.ErrClosed) || len(entries) != 0 || skipped != 0 || done {
			t.Fatalf("closed scan: %+v, %d, %v, %v", entries, skipped, done, err)
		}
		if err := closed.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestDirectoryScanNextCloseConcurrent(t *testing.T) {
	path := t.TempDir()
	directoryScanTestEntries(t, path, 32)
	root := directoryScanTestRoot(t, path)
	for round := 0; round < 12; round++ {
		scan := directoryScanTestOpen(t, root, ".")
		entry := scan.Entry()
		start := make(chan struct{})
		failures := make(chan error, 12)
		var wg sync.WaitGroup
		for i := 0; i < 12; i++ {
			wg.Add(1)
			go func(closeScan bool) {
				defer wg.Done()
				<-start
				if closeScan {
					failures <- scan.Close()
					return
				}
				entries, skipped, complete, err := scan.Next(t.Context(), 1)
				if err != nil && !errors.Is(err, os.ErrClosed) || skipped != 0 || complete || err != nil && len(entries) != 0 || scan.Entry() != entry {
					failures <- fmt.Errorf("concurrent page: %+v, %d, %v, %v", entries, skipped, complete, err)
				}
			}(i%4 == 0)
		}
		close(start)
		wg.Wait()
		close(failures)
		for err := range failures {
			if err != nil {
				t.Fatal(err)
			}
		}
		if _, _, _, err := scan.Next(t.Context(), 1); !errors.Is(err, os.ErrClosed) {
			t.Fatalf("scan remained open: %v", err)
		}
	}
}

func TestDirectoryScanRejectsUnsafePathsAndLinks(t *testing.T) {
	path, outside := t.TempDir(), t.TempDir()
	if err := os.Mkdir(filepath.Join(path, "folder"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "file"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	for name, target := range map[string]string{"inside-link": "folder", "outside-link": outside, "dangling-link": "missing"} {
		if err := os.Symlink(target, filepath.Join(path, name)); err != nil {
			t.Fatal(err)
		}
	}
	root := directoryScanTestRoot(t, path)
	for _, name := range []string{"", "..", "../escape", "/absolute", "folder/../file", "folder//file", "folder/.", "folder/", `folder\file`, "folder/\x00bad"} {
		if scan, err := root.OpenDirectoryScan(t.Context(), name); err == nil || scan != nil {
			t.Fatalf("unsafe path %q: %v, %v", name, scan, err)
		}
	}
	for name, want := range map[string]error{
		"inside-link": ErrLink, "outside-link": ErrLink, "dangling-link": ErrLink,
		"inside-link/child": ErrLink, "outside-link/child": ErrLink,
		"file": ErrNotDirectory, "file/child": ErrNotDirectory,
		"missing": ErrNotFound, "missing/child": ErrNotFound,
	} {
		scan, err := root.OpenDirectoryScan(t.Context(), name)
		if scan != nil || !errors.Is(err, want) {
			t.Fatalf("path %q: %v, %v; want %v", name, scan, err, want)
		}
		if strings.Contains(err.Error(), path) || strings.Contains(err.Error(), outside) {
			t.Fatalf("OS absolute path exposed: %v", err)
		}
	}
	for _, closed := range []*Root{nil, {}} {
		if scan, err := closed.OpenDirectoryScan(t.Context(), "."); err == nil || scan != nil {
			t.Fatalf("closed root accepted: %v, %v", scan, err)
		}
	}
}

func TestDirectoryScanClassifiesChildrenWithoutFollowing(t *testing.T) {
	path, outside := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("must not be read"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{"folder", ArchiveDirectory} {
		if err := os.Mkdir(filepath.Join(path, dir), 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(path, "file"), nil, 0000); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(path, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("missing", filepath.Join(path, "dangling")); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(filepath.Join(path, "fifo"), 0600); err != nil {
		t.Fatal(err)
	}
	root := directoryScanTestRoot(t, path)
	scan := directoryScanTestOpen(t, root, ".")
	want := map[string]EntryType{"folder": EntryDirectory, ArchiveDirectory: EntryDirectory, "file": EntryFile, "link": EntryLink, "dangling": EntryLink, "fifo": EntryOther}
	entries, skipped, _, err := scan.Next(t.Context(), 100)
	if err != nil || skipped != 0 || len(entries) != len(want) {
		t.Fatalf("children: %+v, %d, %v", entries, skipped, err)
	}
	for _, entry := range entries {
		if entry.Type != want[entry.Name] {
			t.Fatalf("wrong no-follow classification: %+v", entry)
		}
		delete(want, entry.Name)
	}
	if len(want) != 0 {
		t.Fatalf("missing children: %+v", want)
	}
	archive := directoryScanTestOpen(t, root, ArchiveDirectory)
	if entries, _, done, err := archive.Next(t.Context(), 1); err != nil || len(entries) != 0 || !done {
		t.Fatalf("explicit archive directory blocked: %+v, %v, %v", entries, done, err)
	}
}
