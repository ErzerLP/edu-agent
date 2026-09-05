//go:build windows

package securefile

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

func TestWindowsArchiveProtectsShortDirectoryAliases(t *testing.T) {
	root, path := archiveTestRoot(t)
	archivePath := filepath.Join(path, ArchiveDirectory)
	if err := os.MkdirAll(filepath.Join(archivePath, "nested"), 0o700); err != nil {
		t.Fatal(err)
	}
	long, err := windows.UTF16PtrFromString(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	buffer := make([]uint16, 32768)
	n, err := windows.GetShortPathName(long, &buffer[0], uint32(len(buffer)))
	if err != nil {
		t.Skipf("8.3 aliases unavailable on native filesystem: %v", err)
	}
	if n >= uint32(len(buffer)) {
		t.Fatal("short path exceeds buffer")
	}
	alias := filepath.Base(windows.UTF16ToString(buffer[:n]))
	if strings.EqualFold(alias, ArchiveDirectory) {
		t.Skip("native filesystem has 8.3 generation disabled")
	}
	for _, relative := range []string{alias, alias + "/nested", alias + "/nested/new/file"} {
		if err := root.CheckArchiveWritePath(context.Background(), relative); !errors.Is(err, ErrArchiveProtected) {
			t.Fatalf("Check(%q)=%v", relative, err)
		}
		if _, err := root.InspectArchiveSource(context.Background(), relative); !errors.Is(err, ErrArchiveProtected) {
			t.Fatalf("Inspect(%q)=%v", relative, err)
		}
		result, err := root.Publish(context.Background(), relative+"/file", []byte("blocked"), PublishOptions{Mode: PublishCreate, Permission: 0o600, ProtectArchive: true})
		if !errors.Is(err, ErrArchiveProtected) || result.Outcome != PublishUnchanged {
			t.Fatalf("Publish(%q)=%+v %v", relative, result, err)
		}
	}
	parent, err := openDirectoryWithinRoot(root, []string{alias, "nested"})
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	if err := checkArchivePublishParent(context.Background(), root, parent); !errors.Is(err, ErrArchiveProtected) {
		t.Fatalf("opened alias parent=%v", err)
	}
}

func TestWindowsArchiveRejectsReparseSourcesAndDestinationRoots(t *testing.T) {
	for _, fixture := range []string{"source-junction", "destination-junction", "source-symlink"} {
		t.Run(fixture, func(t *testing.T) {
			root, path := archiveTestRoot(t)
			outside := t.TempDir()
			archiveTestWrite(t, filepath.Join(path, "source"), []byte("inside"))
			entry := archiveTestInspect(t, root, "source")
			switch fixture {
			case "source-junction":
				createWindowsJunctionFixture(t, filepath.Join(path, "junction"), outside)
				if _, err := root.InspectArchiveSource(context.Background(), "junction"); !errors.Is(err, ErrLink) {
					t.Fatal(err)
				}
			case "source-symlink":
				createWindowsFileSymlinkFixture(t, filepath.Join(path, "link"), filepath.Join(path, "source"))
				if _, err := root.InspectArchiveSource(context.Background(), "link"); !errors.Is(err, ErrLink) {
					t.Fatal(err)
				}
			case "destination-junction":
				createWindowsJunctionFixture(t, filepath.Join(path, ArchiveDirectory), outside)
				result, err := root.Archive(context.Background(), "source", ArchiveDirectory+"/candidate/source", entry)
				if !errors.Is(err, ErrLink) || result.Outcome != PublishUnchanged {
					t.Fatalf("result=%+v err=%v", result, err)
				}
			}
		})
	}
}

func TestWindowsArchiveProtectedPublishPinsAllAncestors(t *testing.T) {
	root, path := archiveTestRoot(t)
	archiveTestWrite(t, filepath.Join(path, "parent", "child", "source"), []byte("inside"))
	if err := os.Mkdir(filepath.Join(path, ArchiveDirectory), 0o700); err != nil {
		t.Fatal(err)
	}
	parent, created, handles, err := openWindowsProtectedPublishParent(context.Background(), root, []string{"parent", "child"}, false)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		for i := len(handles) - 1; i >= 0; i-- {
			_ = handles[i].Close()
		}
	}()
	if created {
		t.Fatal("unexpected parent creation")
	}
	if err := os.Rename(filepath.Join(path, "parent"), filepath.Join(path, ArchiveDirectory, "moved")); err == nil {
		t.Fatal("ancestor rename was not blocked by pinned handle")
	}
	if err := checkArchivePublishParent(context.Background(), root, parent); err != nil {
		t.Fatal(err)
	}
}

func TestWindowsArchiveRejectsStreamsAndDeviceNames(t *testing.T) {
	root, path := archiveTestRoot(t)
	archiveTestWrite(t, filepath.Join(path, "source"), []byte("inside"))
	entry := archiveTestInspect(t, root, "source")
	for _, invalid := range []string{"source:stream", "NUL", "COM1.txt", "source.", "source "} {
		if _, err := root.InspectArchiveSource(context.Background(), invalid); err == nil {
			t.Fatalf("accepted source %q", invalid)
		}
		result, err := root.Archive(context.Background(), "source", ArchiveDirectory+"/candidate/"+invalid, entry)
		if err == nil || result.Outcome != PublishUnchanged || result.DirectoriesCreated {
			t.Fatalf("accepted destination %q: %+v %v", invalid, result, err)
		}
	}
}
