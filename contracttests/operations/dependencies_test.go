package operations

import (
	"strings"
	"testing"
)

func noteSyncTestLock() DependencyLock {
	var lock DependencyLock
	lock.NoteSync.ServiceVersion = "3.6.1"
	lock.NoteSync.ServiceCommit = "7a6c78792c631f999c8a5f725bba5dd7235d6688"
	lock.NoteSync.PluginVersion = "2.4.0"
	lock.NoteSync.PluginCommit = "f2b15c09d34e621d2d97ad526fdee03460bac151"
	return lock
}

func TestNoteSyncAuthorityMatchesPromotedContract(t *testing.T) {
	content := []byte("The supported contract is Fast Note Sync Service `3.6.1` at commit `7a6c78792c631f999c8a5f725bba5dd7235d6688` with Obsidian plugin `2.4.0` at commit `f2b15c09d34e621d2d97ad526fdee03460bac151`; production uses real routes.\n")
	if err := validateNoteSyncAuthorityDocument(content, noteSyncTestLock()); err != nil {
		t.Fatal(err)
	}
}

func TestNoteSyncAuthorityRejectsContractDrift(t *testing.T) {
	content := []byte("The supported contract is Fast Note Sync Service `3.6.2` at commit `7a6c78792c631f999c8a5f725bba5dd7235d6688` with Obsidian plugin `2.4.0` at commit `f2b15c09d34e621d2d97ad526fdee03460bac151`.\n")
	if err := validateNoteSyncAuthorityDocument(content, noteSyncTestLock()); err == nil {
		t.Fatal("dependency lock differing from the promoted NoteSync spec was accepted")
	}
}

func TestNoteSyncLaneBindsAuthoritySpecHash(t *testing.T) {
	root, err := FindRepositoryRoot(".")
	if err != nil {
		t.Fatal(err)
	}
	dependencies, digest, err := LoadDependencyLock(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(dependencies.NoteSyncAuthoritySpecSHA256) != 64 {
		t.Fatalf("authority spec hash=%q", dependencies.NoteSyncAuthoritySpecSHA256)
	}
	lanes, err := buildLaneDefinitions(root, dependencies, digest, RunOptions{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	for _, lane := range lanes {
		if lane.Name != "notesync-real" {
			continue
		}
		if lane.PinnedInputs["authority_spec_sha256"] != dependencies.NoteSyncAuthoritySpecSHA256 {
			t.Fatal("NoteSync lane key does not bind the promoted authority spec hash")
		}
		if !strings.Contains(lane.OutputAssertions[1], dependencies.NoteSync.ServiceVersion) {
			t.Fatal("NoteSync runner assertion does not bind the real service version")
		}
		return
	}
	t.Fatal("NoteSync lane is missing")
}
