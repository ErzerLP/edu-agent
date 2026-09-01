//go:build !windows

package securefile

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRootPublishRevalidatesExpectedHashAtPublicationBoundary(t *testing.T) {
	rootPath := t.TempDir()
	target := filepath.Join(rootPath, "notes.txt")
	if err := os.WriteFile(target, []byte("old\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := OpenRoot(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	oldHash := snapshotContentHash([]byte("old\n"))
	if err := os.WriteFile(target, []byte("external\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := root.Publish(context.Background(), "notes.txt", []byte("candidate\n"), PublishOptions{
		Mode: PublishReplace, Permission: 0o600, ExpectedHash: oldHash, ExpectedLimit: 1 << 20,
	})
	if !errors.Is(err, ErrChanged) || result.Outcome != PublishUnchanged {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if data, err := os.ReadFile(target); err != nil || string(data) != "external\n" {
		t.Fatalf("target=%q err=%v", data, err)
	}
	entries, err := os.ReadDir(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if len(entry.Name()) >= len(".edu-agent-") && entry.Name()[:len(".edu-agent-")] == ".edu-agent-" {
			t.Fatalf("temporary file remained after revalidation failure: %s", entry.Name())
		}
	}
}

func TestRootPublishReportsUnknownWhenParentCreationPartiallyChangesDisk(t *testing.T) {
	rootPath := t.TempDir()
	root, err := OpenRoot(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	path := "created/" + strings.Repeat("x", 300) + "/notes.txt"
	result, err := root.Publish(context.Background(), path, []byte("candidate\n"), PublishOptions{
		Mode: PublishCreate, Permission: 0o600,
	})
	if !errors.Is(err, ErrOutcomeUnknown) || result.Outcome != PublishUnknown {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	info, statErr := os.Stat(filepath.Join(rootPath, "created"))
	if statErr != nil || !info.IsDir() {
		t.Fatalf("created parent side effect missing: info=%+v err=%v", info, statErr)
	}
}

func TestVerifyUnixPublishParentRejectsDirectoryMovedOutsideRoot(t *testing.T) {
	rootPath := t.TempDir()
	if err := os.Mkdir(filepath.Join(rootPath, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	root, err := OpenRoot(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	parent, _, err := openPublishParentWithinRoot(root, []string{"sub"}, false)
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	outside := filepath.Join(t.TempDir(), "moved")
	if err := os.Rename(filepath.Join(rootPath, "sub"), outside); err != nil {
		t.Skipf("directory exchange unavailable: %v", err)
	}
	if err := verifyUnixPublishParentWithinRoot(int(root.file.Fd()), int(parent.Fd()), 64); !errors.Is(err, ErrOutsideRoot) {
		t.Fatalf("verify err=%v", err)
	}
}

func TestRootSnapshotIdentityMatchesHardlinkAliases(t *testing.T) {
	rootPath := t.TempDir()
	original := filepath.Join(rootPath, "original.txt")
	alias := filepath.Join(rootPath, "alias.txt")
	if err := os.WriteFile(original, []byte("same\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(original, alias); err != nil {
		t.Skipf("hardlinks unavailable: %v", err)
	}
	root, err := OpenRoot(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	first, err := root.ReadSnapshot("original.txt", 1024, false)
	if err != nil {
		t.Fatal(err)
	}
	second, err := root.ReadSnapshot("alias.txt", 1024, false)
	if err != nil {
		t.Fatal(err)
	}
	if first.Identity == "" || first.Identity != second.Identity {
		t.Fatalf("hardlink identities differ: first=%q second=%q", first.Identity, second.Identity)
	}
}

func TestRootReadDirAndSnapshotDoNotFollowSymlinks(t *testing.T) {
	rootPath := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(outside, []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootPath, "inside.txt"), []byte("inside"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(rootPath, "link.txt")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	root, err := OpenRoot(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	entries, skipped, complete, err := root.ReadDir(".", 10)
	if err != nil || skipped != 0 || !complete {
		t.Fatalf("entries=%+v skipped=%d complete=%t err=%v", entries, skipped, complete, err)
	}
	foundLink := false
	for _, entry := range entries {
		if entry.Name == "link.txt" && entry.Type == EntryLink {
			foundLink = true
		}
	}
	if !foundLink {
		t.Fatalf("link entry not classified: %+v", entries)
	}
	if _, err := root.ReadSnapshot("link.txt", 1024, false); !errors.Is(err, ErrLink) {
		t.Fatalf("link read err=%v", err)
	}
	snapshot, err := root.ReadSnapshot("inside.txt", 1024, false)
	if err != nil || string(snapshot.Data) != "inside" {
		t.Fatalf("snapshot=%+v err=%v", snapshot, err)
	}
}
