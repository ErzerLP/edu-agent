package nocturne

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/memory"
	"github.com/edu-agent/edu-agent/server/internal/privacy"
	"github.com/google/uuid"
)

func TestManagedBackupRoundTripChunkBoundariesAndDestroyedRestore(t *testing.T) {
	for _, size := range []int{0, 1, 7, 8, 9, 16, 17, 31} {
		t.Run(fmt.Sprintf("size-%d", size), func(t *testing.T) {
			plaintext := bytes.Repeat([]byte("private-dump-body|"), 3)
			plaintext = plaintext[:min(size, len(plaintext))]
			repository := newFixtureBackupRepository()
			dump := &fixtureDump{content: plaintext, writeStep: 3}
			controller, root := fixtureBackupController(t, repository, dump, 8, time.Date(2026, 9, 3, 10, 11, 12, 123456000, time.UTC))
			artifact, err := controller.Produce(context.Background(), 3)
			if err != nil {
				t.Fatal(err)
			}
			ciphertext, err := os.ReadFile(filepath.Join(root, artifact.Path))
			if err != nil {
				t.Fatal(err)
			}
			if len(ciphertext) < backupHeaderSize+4+16 || bytes.Equal(ciphertext, plaintext) || len(plaintext) >= 8 && bytes.Contains(ciphertext, plaintext) {
				t.Fatalf("artifact was not encrypted: plaintext=%d ciphertext=%d", len(plaintext), len(ciphertext))
			}
			if !bytes.Equal(ciphertext[:8], backupMagic[:]) || artifact.Size != int64(len(ciphertext)) {
				t.Fatalf("format header or size mismatch artifact=%+v", artifact)
			}
			digest := sha256.Sum256(ciphertext)
			if artifact.SHA256 != hex.EncodeToString(digest[:]) {
				t.Fatalf("artifact digest=%q", artifact.SHA256)
			}
			var restored bytes.Buffer
			if err := controller.RestoreVerify(context.Background(), artifact, &restored); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(restored.Bytes(), plaintext) {
				t.Fatalf("restore mismatch got=%q want=%q", restored.Bytes(), plaintext)
			}
			repository.destroy()
			restored.Reset()
			if err := controller.RestoreVerify(context.Background(), artifact, &restored); !errors.Is(err, privacy.ErrGenerationKeyDestroyed) {
				t.Fatalf("destroyed key restore err=%v", err)
			}
			if _, err := os.Stat(filepath.Join(root, artifact.Path)); err != nil {
				t.Fatalf("destroy verification relied on deleting artifact: %v", err)
			}
		})
	}
}

func TestManagedBackupProducerFailureLeavesAtomicInventoryAndStableLock(t *testing.T) {
	repository := newFixtureBackupRepository()
	dump := &fixtureDump{content: []byte("first private dump"), writeStep: 2}
	controller, root := fixtureBackupController(t, repository, dump, 8, time.Date(2026, 9, 3, 11, 0, 0, 0, time.UTC))
	artifact, err := controller.Produce(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	manifestBefore, err := os.ReadFile(filepath.Join(root, backupInventoryName))
	if err != nil {
		t.Fatal(err)
	}
	artifactBefore, err := os.ReadFile(filepath.Join(root, artifact.Path))
	if err != nil {
		t.Fatal(err)
	}
	lockBefore, err := os.Stat(filepath.Join(root, backupLockName))
	if err != nil {
		t.Fatal(err)
	}

	dump.content = []byte("plaintext that must never persist: command-output-secret")
	dump.fail = true
	dump.failure = errors.New("raw command output: password=command-output-secret")
	_, err = controller.Produce(context.Background(), 1)
	if !errors.Is(err, ErrBackupDumpFailed) || strings.Contains(err.Error(), "password") || strings.Contains(err.Error(), "command-output-secret") {
		t.Fatalf("producer error leaked dump output: %v", err)
	}
	manifestAfter, _ := os.ReadFile(filepath.Join(root, backupInventoryName))
	artifactAfter, _ := os.ReadFile(filepath.Join(root, artifact.Path))
	lockAfter, err := os.Stat(filepath.Join(root, backupLockName))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(manifestBefore, manifestAfter) || !bytes.Equal(artifactBefore, artifactAfter) {
		t.Fatal("failed producer changed a published artifact or inventory")
	}
	if !os.SameFile(lockBefore, lockAfter) {
		t.Fatal("producer replaced the named lock inode")
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Fatalf("partial entry remained after failure: %v", entryNames(entries))
	}
	for _, entry := range entries {
		if strings.Contains(entry.Name(), ".tmp") {
			t.Fatalf("temporary entry remained: %s", entry.Name())
		}
		if entry.Name() == backupLockName || entry.Name() == backupInventoryName {
			continue
		}
		payload, readErr := os.ReadFile(filepath.Join(root, entry.Name()))
		if readErr != nil {
			t.Fatal(readErr)
		}
		if bytes.Contains(payload, dump.content) {
			t.Fatal("plaintext dump persisted in backup root")
		}
	}
	inventory, err := controller.Inventory(context.Background())
	if err != nil || len(inventory) != 1 || !sameArtifact(inventory[0], artifact) {
		t.Fatalf("inventory=%+v err=%v", inventory, err)
	}
}

func TestManagedBackupPublicationJournalRecoversRecordFailureAndUnknown(t *testing.T) {
	for _, test := range []struct {
		name        string
		afterCommit bool
		tempOnly    bool
	}{
		{name: "before-commit"},
		{name: "after-commit-unknown", afterCommit: true},
		{name: "temp-only-crash-recovery", tempOnly: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			repository := newFixtureBackupRepository()
			repository.recordFailures = []fixtureRecordFailure{{err: errors.New("database outcome unavailable"), afterCommit: test.afterCommit}}
			dump := &fixtureDump{content: []byte("journal private dump body"), writeStep: 3}
			controller, root := fixtureBackupController(t, repository, dump, 8, time.Date(2026, 9, 4, 1, 0, 0, 0, time.UTC))
			_, err := controller.Produce(context.Background(), 1)
			if err == nil {
				t.Fatal("record failure unexpectedly returned success")
			}
			journalPath := filepath.Join(controller.journalParent, controller.journalName)
			journalPayload, err := os.ReadFile(journalPath)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Contains(journalPayload, []byte(publicationJournalFormat)) || bytes.Contains(journalPayload, dump.content) || bytes.Contains(journalPayload, repository.key[:]) {
				t.Fatalf("journal leaked protected content or omitted format: %s", journalPayload)
			}
			if test.tempOnly {
				if err := os.Rename(journalPath, filepath.Join(controller.journalParent, controller.journalTempName)); err != nil {
					t.Fatal(err)
				}
			}
			lockBefore, err := os.Stat(filepath.Join(root, backupLockName))
			if err != nil {
				t.Fatal(err)
			}
			inventory, err := controller.Inventory(context.Background())
			if err != nil || len(inventory) != 1 || len(repository.records) != 1 || !repository.recordInsideFence {
				t.Fatalf("recovered inventory=%+v records=%+v fenced=%v err=%v", inventory, repository.records, repository.recordInsideFence, err)
			}
			if _, err := os.Lstat(journalPath); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("journal remained after recovery: %v", err)
			}
			if _, err := os.Lstat(filepath.Join(controller.journalParent, controller.journalTempName)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("journal temp remained after recovery: %v", err)
			}
			lockAfter, err := os.Stat(filepath.Join(root, backupLockName))
			if err != nil || !os.SameFile(lockBefore, lockAfter) {
				t.Fatalf("journal recovery replaced lock inode: %v", err)
			}
		})
	}
}

func TestManagedBackupPublicationJournalConvergesAfterKeyDestruction(t *testing.T) {
	repository := newFixtureBackupRepository()
	repository.recordFailures = []fixtureRecordFailure{{err: errors.New("record outcome unknown"), afterCommit: true}}
	controller, root := fixtureBackupController(t, repository, &fixtureDump{content: []byte("destroyed journal dump")}, 8, time.Date(2026, 9, 4, 2, 0, 0, 0, time.UTC))
	if _, err := controller.Produce(context.Background(), 1); err == nil {
		t.Fatal("record failure unexpectedly returned success")
	}
	manifestPayload, err := os.ReadFile(filepath.Join(root, backupInventoryName))
	if err != nil {
		t.Fatal(err)
	}
	published, err := decodeManifest(manifestPayload)
	if err != nil || len(published) != 1 {
		t.Fatalf("published=%+v err=%v", published, err)
	}
	repository.destroy()
	inventory, err := controller.Inventory(context.Background())
	if err != nil || len(inventory) != 0 {
		t.Fatalf("destroyed recovery inventory=%+v err=%v", inventory, err)
	}
	if _, err := os.Lstat(filepath.Join(root, published[0].Path)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("destroyed artifact remained: %v", err)
	}
	if len(repository.discarded) != 1 || repository.discarded[0] != published[0].Path || len(repository.records) != 0 {
		t.Fatalf("discarded=%v records=%v", repository.discarded, repository.records)
	}
	if _, err := os.Lstat(filepath.Join(controller.journalParent, controller.journalName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("destroyed publication journal remained: %v", err)
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 2 {
		t.Fatalf("root entries=%v err=%v", entryNames(entries), err)
	}
}

func TestManagedBackupPublicationJournalPathProtection(t *testing.T) {
	t.Run("symlink", func(t *testing.T) {
		repository := newFixtureBackupRepository()
		controller, _ := fixtureBackupController(t, repository, &fixtureDump{}, 8, time.Now().UTC())
		external := filepath.Join(t.TempDir(), "external")
		if err := os.WriteFile(external, []byte("outside"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(external, filepath.Join(controller.journalParent, controller.journalName)); err != nil {
			t.Fatal(err)
		}
		if _, err := controller.Inventory(context.Background()); !errors.Is(err, ErrBackupJournal) {
			t.Fatalf("journal symlink err=%v", err)
		}
	})
	t.Run("hardlink", func(t *testing.T) {
		repository := newFixtureBackupRepository()
		controller, _ := fixtureBackupController(t, repository, &fixtureDump{}, 8, time.Now().UTC())
		source := filepath.Join(controller.journalParent, controller.journalName+".source")
		if err := os.WriteFile(source, []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Link(source, filepath.Join(controller.journalParent, controller.journalName)); err != nil {
			t.Fatal(err)
		}
		if _, err := controller.Inventory(context.Background()); !errors.Is(err, ErrBackupJournal) {
			t.Fatalf("journal hardlink err=%v", err)
		}
	})
	t.Run("temp-symlink", func(t *testing.T) {
		repository := newFixtureBackupRepository()
		controller, _ := fixtureBackupController(t, repository, &fixtureDump{}, 8, time.Now().UTC())
		external := filepath.Join(t.TempDir(), "external")
		if err := os.WriteFile(external, []byte("outside"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(external, filepath.Join(controller.journalParent, controller.journalTempName)); err != nil {
			t.Fatal(err)
		}
		if _, err := controller.Inventory(context.Background()); !errors.Is(err, ErrBackupJournal) {
			t.Fatalf("journal temp symlink err=%v", err)
		}
	})
	t.Run("parent-replaced", func(t *testing.T) {
		base := t.TempDir()
		parent := filepath.Join(base, "parent")
		root := filepath.Join(parent, "backups")
		if err := os.MkdirAll(root, 0o700); err != nil {
			t.Fatal(err)
		}
		repository := newFixtureBackupRepository()
		controller, err := NewBackupController(BackupControllerOptions{Root: root, DumpSource: &fixtureDump{}, Keys: repository, Inventory: repository})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(parent, parent+"-old"); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(root, 0o700); err != nil {
			t.Fatal(err)
		}
		if _, err := controller.Inventory(context.Background()); !errors.Is(err, ErrBackupFilesystem) {
			t.Fatalf("replaced journal parent err=%v", err)
		}
	})
}

func TestManagedBackupRestoreToFileIsAtomic(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		repository := newFixtureBackupRepository()
		plaintext := []byte("verified persistent restore")
		controller, _ := fixtureBackupController(t, repository, &fixtureDump{content: plaintext}, 8, time.Date(2026, 9, 4, 3, 0, 0, 0, time.UTC))
		artifact, err := controller.Produce(context.Background(), 1)
		if err != nil {
			t.Fatal(err)
		}
		destination := filepath.Join(t.TempDir(), "restore.sql")
		if err := controller.RestoreToFile(context.Background(), artifact, destination); err != nil {
			t.Fatal(err)
		}
		payload, err := os.ReadFile(destination)
		if err != nil || !bytes.Equal(payload, plaintext) {
			t.Fatalf("restore=%q err=%v", payload, err)
		}
		info, err := os.Stat(destination)
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("restore mode=%v err=%v", info.Mode(), err)
		}
	})
	t.Run("authentication-failure", func(t *testing.T) {
		repository := newFixtureBackupRepository()
		plaintext := bytes.Repeat([]byte("late authentication failure|"), 4)
		controller, root := fixtureBackupController(t, repository, &fixtureDump{content: plaintext}, 8, time.Date(2026, 9, 4, 4, 0, 0, 0, time.UTC))
		artifact, err := controller.Produce(context.Background(), 1)
		if err != nil {
			t.Fatal(err)
		}
		artifactPath := filepath.Join(root, artifact.Path)
		ciphertext, err := os.ReadFile(artifactPath)
		if err != nil {
			t.Fatal(err)
		}
		ciphertext[len(ciphertext)-1] ^= 0x40
		if err := os.WriteFile(artifactPath, ciphertext, 0o600); err != nil {
			t.Fatal(err)
		}
		digest := sha256.Sum256(ciphertext)
		artifact.SHA256 = hex.EncodeToString(digest[:])
		manifest, err := encodeManifest([]privacy.ManagedBackupArtifact{artifact})
		if err != nil || os.WriteFile(filepath.Join(root, backupInventoryName), manifest, 0o600) != nil {
			t.Fatalf("rewrite tampered fixture manifest: %v", err)
		}
		destinationDirectory := t.TempDir()
		destination := filepath.Join(destinationDirectory, "restore.sql")
		if err := controller.RestoreToFile(context.Background(), artifact, destination); !errors.Is(err, privacy.ErrManagedBackupIntegrity) {
			t.Fatalf("tampered restore err=%v", err)
		}
		if _, err := os.Lstat(destination); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("partial plaintext target remained: %v", err)
		}
		entries, err := os.ReadDir(destinationDirectory)
		if err != nil || len(entries) != 0 {
			t.Fatalf("restore temp remained entries=%v err=%v", entryNames(entries), err)
		}
	})
}

func TestManagedBackupManifestPathSafetyAndStrictShapeFailClosed(t *testing.T) {
	repository := newFixtureBackupRepository()
	dump := &fixtureDump{content: []byte("private")}
	root := t.TempDir()
	manifest := `{"format":"edu-agent-managed-backup-inventory-v1","artifacts":[{"path":"../escape","created_at":"2026-09-03T00:00:00Z","size":1,"sha256":"` + strings.Repeat("0", 64) + `","learner_generation":1,"wrapped_key_id":"` + repository.id + `"}]}`
	if err := os.WriteFile(filepath.Join(root, backupInventoryName), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	controller, err := NewBackupController(BackupControllerOptions{Root: root, DumpSource: dump, Keys: repository, Inventory: repository})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := controller.Produce(context.Background(), 1); !errors.Is(err, ErrBackupInventory) {
		t.Fatalf("unsafe path inventory err=%v", err)
	}

	strictRoot := t.TempDir()
	duplicate := `{"format":"edu-agent-managed-backup-inventory-v1","format":"edu-agent-managed-backup-inventory-v1","artifacts":[]}`
	if err := os.WriteFile(filepath.Join(strictRoot, backupInventoryName), []byte(duplicate), 0o600); err != nil {
		t.Fatal(err)
	}
	strictController, err := NewBackupController(BackupControllerOptions{Root: strictRoot, DumpSource: dump, Keys: repository, Inventory: repository})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := strictController.Produce(context.Background(), 1); !errors.Is(err, ErrBackupInventory) {
		t.Fatalf("duplicate manifest field err=%v", err)
	}

	realRoot := t.TempDir()
	symlink := filepath.Join(t.TempDir(), "backup-link")
	if err := os.Symlink(realRoot, symlink); err != nil {
		t.Fatal(err)
	}
	if _, err := NewBackupController(BackupControllerOptions{Root: symlink, DumpSource: dump, Keys: repository, Inventory: repository}); !errors.Is(err, ErrBackupFilesystem) {
		t.Fatalf("symlink backup root err=%v", err)
	}
}

func TestManagedBackupDecryptFixtureRejectsTruncationTrailingAndWrongKey(t *testing.T) {
	key := bytes.Repeat([]byte{0x31}, 32)
	plaintext := []byte("chunk-zero|chunk-one|chunk-two")
	var encrypted bytes.Buffer
	writer, err := newChunkEncryptWriter(&encrypted, key, 8, bytes.NewReader(bytes.Repeat([]byte{0x72}, 16)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write(plaintext); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	var restored bytes.Buffer
	if err := DecryptManagedBackup(&restored, bytes.NewReader(encrypted.Bytes()), key); err != nil || !bytes.Equal(restored.Bytes(), plaintext) {
		t.Fatalf("fixture restore=%q err=%v", restored.Bytes(), err)
	}
	corrupted := append([]byte(nil), encrypted.Bytes()...)
	corrupted[backupHeaderSize+4] ^= 0x80
	fixtures := map[string]struct {
		payload []byte
		key     []byte
	}{
		"truncated": {payload: append([]byte(nil), encrypted.Bytes()[:encrypted.Len()-1]...), key: key},
		"trailing":  {payload: append(append([]byte(nil), encrypted.Bytes()...), 0), key: key},
		"tampered":  {payload: corrupted, key: key},
		"wrong-key": {payload: append([]byte(nil), encrypted.Bytes()...), key: bytes.Repeat([]byte{0x32}, 32)},
	}
	for name, fixture := range fixtures {
		t.Run(name, func(t *testing.T) {
			if err := DecryptManagedBackup(io.Discard, bytes.NewReader(fixture.payload), fixture.key); !errors.Is(err, privacy.ErrManagedBackupIntegrity) {
				t.Fatalf("fixture accepted err=%v", err)
			}
		})
	}
}

func TestManagedBackupPrecisePruneSuccess(t *testing.T) {
	fixture := newManagedPruneFixture(t)
	remoteInventory, err := fixture.maintenance.Backups(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	retention, err := fixture.controller.RetentionInventory(context.Background(), fixture.cutoff, remoteInventory)
	if err != nil || len(retention.Prune) != 2 || len(retention.Retained) != 1 || retention.ManifestSHA256 != remoteInventory.ManifestSHA256 {
		t.Fatalf("retention=%+v err=%v", retention, err)
	}
	badDigest := remoteInventory
	badDigest.ManifestSHA256 = strings.Repeat("f", 64)
	if _, err := fixture.controller.RetentionInventory(context.Background(), fixture.cutoff, badDigest); !errors.Is(err, ErrBackupMaintenance) {
		t.Fatalf("digest mismatch err=%v", err)
	}
	if err := fixture.controller.Prune(context.Background(), fixture.cutoff, fixture.maintenance); err != nil {
		remoteAfter, remoteErr := fixture.maintenance.Backups(context.Background())
		localAfter, localErr := fixture.controller.localBackupSnapshot(context.Background())
		t.Fatalf("prune err=%v remote=%+v remoteErr=%v local=%+v localErr=%v requests=%+v", err, remoteAfter, remoteErr, localAfter, localErr, fixture.maintenance.requests)
	}
	if len(fixture.maintenance.requests) != 1 {
		t.Fatalf("requests=%+v", fixture.maintenance.requests)
	}
	request := fixture.maintenance.requests[0]
	expectedPaths := []string{fixture.artifacts[0].Path, fixture.artifacts[1].Path}
	sort.Strings(expectedPaths)
	if !canonicalUUID(request.OperationID) || !request.Cutoff.Equal(fixture.cutoff) || request.ExpectedManifestSHA256 != remoteInventory.ManifestSHA256 ||
		!equalStrings(request.Paths, expectedPaths) || !equalStrings(fixture.repository.pruned, expectedPaths) {
		t.Fatalf("request=%+v pruned=%v want=%v", request, fixture.repository.pruned, expectedPaths)
	}
	local, err := fixture.controller.Inventory(context.Background())
	if err != nil || len(local) != 1 || !sameArtifact(local[0], fixture.artifacts[2]) || len(fixture.repository.records) != 1 {
		t.Fatalf("local=%+v db=%+v err=%v", local, fixture.repository.records, err)
	}
	if err := fixture.controller.Prune(context.Background(), fixture.cutoff.In(time.FixedZone("offset", 3600)), fixture.maintenance); !errors.Is(err, ErrBackupMaintenance) {
		t.Fatalf("non-UTC cutoff err=%v", err)
	}
}

func TestManagedBackupPruneLostResponseReconcilesExactState(t *testing.T) {
	fixture := newManagedPruneFixture(t)
	fixture.maintenance.pruneErr = &Error{category: CategoryTransport, operation: "prune_backups", mutationDispatched: true}
	if err := fixture.controller.Prune(context.Background(), fixture.cutoff, fixture.maintenance); err != nil {
		t.Fatal(err)
	}
	if len(fixture.maintenance.requests) != 1 || fixture.maintenance.backupCalls < 2 || len(fixture.repository.pruned) != 2 || len(fixture.repository.records) != 1 {
		t.Fatalf("requests=%d gets=%d pruned=%v db=%+v", len(fixture.maintenance.requests), fixture.maintenance.backupCalls, fixture.repository.pruned, fixture.repository.records)
	}
}

func TestManagedBackupPruneDBMarkFailureResumesByThreeWayReconciliation(t *testing.T) {
	fixture := newManagedPruneFixture(t)
	fixture.maintenance.afterApply = func(memory.BackupPruneRequest) {
		fixture.repository.markFailures = []error{errors.New("database unavailable")}
	}
	firstErr := fixture.controller.Prune(context.Background(), fixture.cutoff, fixture.maintenance)
	if firstErr == nil {
		t.Fatal("database mark failure returned success")
	}
	if len(fixture.maintenance.requests) != 1 || len(fixture.repository.records) != 3 || len(fixture.repository.pruned) != 0 || len(fixture.repository.markFailures) != 0 {
		t.Fatalf("first err=%v requests=%d db=%+v pruned=%v failures=%v", firstErr, len(fixture.maintenance.requests), fixture.repository.records, fixture.repository.pruned, fixture.repository.markFailures)
	}
	if err := fixture.controller.Prune(context.Background(), fixture.cutoff, fixture.maintenance); err != nil {
		t.Fatal(err)
	}
	if len(fixture.maintenance.requests) != 1 || len(fixture.repository.records) != 1 || len(fixture.repository.pruned) != 2 {
		t.Fatalf("resume requests=%d db=%+v pruned=%v", len(fixture.maintenance.requests), fixture.repository.records, fixture.repository.pruned)
	}
}

func TestManagedBackupPruneFailsClosedOnDriftNewArtifactAndResponseMismatch(t *testing.T) {
	t.Run("digest-drift", func(t *testing.T) {
		fixture := newManagedPruneFixture(t)
		fixture.maintenance.beforePrune = func(memory.BackupPruneRequest) {
			fixture.current = fixture.cutoff.Add(24 * time.Hour)
			if _, err := fixture.controller.Produce(context.Background(), 2); err != nil {
				t.Fatal(err)
			}
		}
		if err := fixture.controller.Prune(context.Background(), fixture.cutoff, fixture.maintenance); err == nil {
			t.Fatal("digest drift returned success")
		}
		if len(fixture.repository.pruned) != 0 || len(fixture.repository.records) != 4 {
			t.Fatalf("pruned=%v db=%+v", fixture.repository.pruned, fixture.repository.records)
		}
	})
	t.Run("new-eligible-after-lost-response", func(t *testing.T) {
		fixture := newManagedPruneFixture(t)
		fixture.maintenance.afterApply = func(memory.BackupPruneRequest) {
			fixture.current = fixture.cutoff.Add(-time.Hour)
			if _, err := fixture.controller.Produce(context.Background(), 2); err != nil {
				t.Fatal(err)
			}
		}
		fixture.maintenance.pruneErr = &Error{category: CategoryTransport, operation: "prune_backups", mutationDispatched: true}
		if err := fixture.controller.Prune(context.Background(), fixture.cutoff, fixture.maintenance); err == nil {
			t.Fatal("ambiguous lost response returned success")
		}
		if len(fixture.repository.pruned) != 0 || len(fixture.repository.records) != 4 {
			t.Fatalf("pruned=%v db=%+v", fixture.repository.pruned, fixture.repository.records)
		}
	})
	t.Run("response-path-mismatch", func(t *testing.T) {
		fixture := newManagedPruneFixture(t)
		fixture.maintenance.responsePathMismatch = true
		if err := fixture.controller.Prune(context.Background(), fixture.cutoff, fixture.maintenance); !errors.Is(err, ErrBackupMaintenance) {
			t.Fatalf("response mismatch err=%v", err)
		}
		if len(fixture.repository.pruned) != 0 || len(fixture.repository.records) != 3 {
			t.Fatalf("pruned=%v db=%+v", fixture.repository.pruned, fixture.repository.records)
		}
	})
}

func TestManagedBackupErasureVerificationNoArtifactIsNotApplicable(t *testing.T) {
	repository := newFixtureBackupRepository()
	now := time.Date(2026, 9, 5, 1, 2, 3, 0, time.UTC)
	controller, root := fixtureBackupController(t, repository, &fixtureDump{}, 8, now)
	manifest, err := encodeManifest(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, backupInventoryName), manifest, 0o600); err != nil {
		t.Fatal(err)
	}
	controller.maintenance = &fixtureMaintenance{root: root}
	repository.barrierState = privacy.ManagedBackupBarrierState{VerifiedUnrecoverableAt: now.Add(-time.Minute), DestroyedOldKeyCount: 0}
	result, err := controller.VerifyManagedBackups(context.Background(), privacy.ManagedBackupVerificationRequest{
		ErasureID: uuid.NewString(), LearnerGeneration: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != privacy.StepNotApplicable || result.StableReason != "no_pre_barrier_managed_backup_artifacts" ||
		!validBackupDigest(result.EvidenceDigest) || !result.CompletedAt.Equal(now) {
		t.Fatalf("verification=%+v", result)
	}
}

func TestManagedBackupErasureVerificationDestroyedArtifactSucceedsAndLiveKeyFails(t *testing.T) {
	for _, test := range []struct {
		name       string
		destroyKey bool
		status     privacy.StepStatus
		reason     string
	}{
		{name: "destroyed", destroyKey: true, status: privacy.StepSucceeded, reason: "pre_barrier_managed_backups_unrecoverable_by_destroyed_keys"},
		{name: "live", status: privacy.StepFailed, reason: "pre_barrier_managed_backup_key_not_destroyed"},
	} {
		t.Run(test.name, func(t *testing.T) {
			repository := newFixtureBackupRepository()
			now := time.Date(2026, 9, 5, 2, 0, 0, 0, time.UTC)
			controller, root := fixtureBackupController(t, repository, &fixtureDump{content: []byte("verification fixture")}, 8, now)
			artifact, err := controller.Produce(context.Background(), 1)
			if err != nil {
				t.Fatal(err)
			}
			controller.maintenance = &fixtureMaintenance{root: root}
			repository.barrierState = privacy.ManagedBackupBarrierState{VerifiedUnrecoverableAt: now.Add(time.Minute), DestroyedOldKeyCount: 1}
			if test.destroyKey {
				repository.destroy()
			}
			result, err := controller.VerifyManagedBackups(context.Background(), privacy.ManagedBackupVerificationRequest{
				ErasureID: uuid.NewString(), LearnerGeneration: 2,
			})
			if err != nil {
				t.Fatal(err)
			}
			if result.Status != test.status || result.StableReason != test.reason || !validBackupDigest(result.EvidenceDigest) {
				t.Fatalf("verification=%+v", result)
			}
			if _, err := os.Stat(filepath.Join(root, artifact.Path)); err != nil {
				t.Fatalf("verification required artifact deletion: %v", err)
			}
		})
	}
}

func TestManagedBackupErasureVerificationInventoryUnavailableAndMismatch(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*fixtureMaintenance)
		status privacy.StepStatus
		reason string
	}{
		{
			name: "unavailable",
			mutate: func(maintenance *fixtureMaintenance) {
				maintenance.backupErr = errors.New("maintenance unavailable")
			},
			status: privacy.StepUnknown,
			reason: "managed_backup_remote_inventory_unavailable",
		},
		{
			name: "mismatch",
			mutate: func(maintenance *fixtureMaintenance) {
				inventory, err := fixtureInventoryFromRoot(maintenance.root)
				if err != nil {
					t.Fatal(err)
				}
				inventory.ManifestSHA256 = strings.Repeat("f", 64)
				maintenance.backupOverride = &inventory
			},
			status: privacy.StepPartial,
			reason: "managed_backup_inventory_mismatch",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			repository := newFixtureBackupRepository()
			now := time.Date(2026, 9, 5, 3, 0, 0, 0, time.UTC)
			controller, root := fixtureBackupController(t, repository, &fixtureDump{}, 8, now)
			manifest, err := encodeManifest(nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, backupInventoryName), manifest, 0o600); err != nil {
				t.Fatal(err)
			}
			maintenance := &fixtureMaintenance{root: root}
			test.mutate(maintenance)
			controller.maintenance = maintenance
			repository.barrierState = privacy.ManagedBackupBarrierState{VerifiedUnrecoverableAt: now.Add(-time.Minute)}
			result, err := controller.VerifyManagedBackups(context.Background(), privacy.ManagedBackupVerificationRequest{
				ErasureID: uuid.NewString(), LearnerGeneration: 2,
			})
			if err != nil {
				t.Fatal(err)
			}
			if result.Status != test.status || result.StableReason != test.reason || !validBackupDigest(result.EvidenceDigest) {
				t.Fatalf("verification=%+v", result)
			}
		})
	}
}

type fixtureDump struct {
	content   []byte
	writeStep int
	fail      bool
	failure   error
}

func (d *fixtureDump) Dump(_ context.Context, destination io.Writer) error {
	step := d.writeStep
	if step <= 0 {
		step = len(d.content)
		if step == 0 {
			step = 1
		}
	}
	for offset := 0; offset < len(d.content); offset += step {
		end := min(offset+step, len(d.content))
		if _, err := destination.Write(d.content[offset:end]); err != nil {
			return err
		}
	}
	if d.fail {
		return d.failure
	}
	return nil
}

type fixtureRecordFailure struct {
	err         error
	afterCommit bool
}

type fixtureBackupRepository struct {
	mu                sync.Mutex
	id                string
	generation        int64
	key               [32]byte
	destroyed         bool
	barrierState      privacy.ManagedBackupBarrierState
	barrierErr        error
	insideFence       bool
	recordInsideFence bool
	recordFailures    []fixtureRecordFailure
	records           []privacy.ManagedBackupArtifact
	discarded         []string
	markFailures      []error
	pruned            []string
}

func newFixtureBackupRepository() *fixtureBackupRepository {
	repository := &fixtureBackupRepository{id: uuid.NewString()}
	for index := range repository.key {
		repository.key[index] = byte(index + 1)
	}
	return repository
}

func (r *fixtureBackupRepository) WithGenerationKey(_ context.Context, generation int64, callback func(privacy.GenerationKeyLease) error) error {
	r.mu.Lock()
	if r.destroyed {
		r.mu.Unlock()
		return privacy.ErrGenerationKeyDestroyed
	}
	if r.generation == 0 {
		r.generation = generation
	}
	if r.generation != generation {
		r.mu.Unlock()
		return privacy.ErrGenerationKeyUnavailable
	}
	key := append([]byte(nil), r.key[:]...)
	r.insideFence = true
	lease := &fixtureKeyLease{
		id: r.id, generation: generation, key: key, active: true,
		record: func(artifact privacy.ManagedBackupArtifact) error { return r.recordFromFence(artifact) },
	}
	r.mu.Unlock()
	defer func() {
		lease.invalidate()
		r.mu.Lock()
		r.insideFence = false
		r.mu.Unlock()
	}()
	err := callback(lease)
	if err == nil && !lease.recorded {
		return privacy.ErrManagedBackupConflict
	}
	return err
}

func (r *fixtureBackupRepository) WithExistingGenerationKey(_ context.Context, generation int64, id string, callback func(privacy.GenerationKeyLease) error) error {
	r.mu.Lock()
	if r.destroyed {
		r.mu.Unlock()
		return privacy.ErrGenerationKeyDestroyed
	}
	if r.generation != generation || r.id != id {
		r.mu.Unlock()
		return privacy.ErrGenerationKeyUnavailable
	}
	key := append([]byte(nil), r.key[:]...)
	lease := &fixtureKeyLease{id: r.id, generation: generation, key: key, active: true}
	r.mu.Unlock()
	defer lease.invalidate()
	return callback(lease)
}

func (r *fixtureBackupRepository) VerifyGenerationKeyDestroyed(_ context.Context, generation int64, id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.destroyed && r.generation == generation && r.id == id {
		return nil
	}
	return privacy.ErrGenerationKeyUnavailable
}

func (r *fixtureBackupRepository) VerifyManagedBackupBarrier(_ context.Context, _ string, _ int64) (privacy.ManagedBackupBarrierState, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.barrierErr != nil {
		return privacy.ManagedBackupBarrierState{}, r.barrierErr
	}
	return r.barrierState, nil
}

func (r *fixtureBackupRepository) RecordManagedBackup(_ context.Context, artifact privacy.ManagedBackupArtifact) error {
	r.mu.Lock()
	r.insideFence = true
	r.mu.Unlock()
	err := r.recordFromFence(artifact)
	r.mu.Lock()
	r.insideFence = false
	r.mu.Unlock()
	return err
}

func (r *fixtureBackupRepository) recordFromFence(artifact privacy.ManagedBackupArtifact) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.insideFence || r.destroyed || artifact.WrappedKeyID != r.id || artifact.LearnerGeneration != r.generation {
		return privacy.ErrGenerationKeyUnavailable
	}
	r.recordInsideFence = true
	var failure fixtureRecordFailure
	if len(r.recordFailures) > 0 {
		failure = r.recordFailures[0]
		r.recordFailures = r.recordFailures[1:]
		if !failure.afterCommit {
			return failure.err
		}
	}
	found := false
	for _, existing := range r.records {
		found = found || sameArtifact(existing, artifact)
	}
	if !found {
		r.records = append(r.records, artifact)
	}
	return failure.err
}

func (r *fixtureBackupRepository) DiscardManagedBackupPublication(_ context.Context, artifact privacy.ManagedBackupArtifact, _ time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.discarded = append(r.discarded, artifact.Path)
	retained := r.records[:0]
	for _, existing := range r.records {
		if !sameArtifact(existing, artifact) {
			retained = append(retained, existing)
		}
	}
	r.records = retained
	return nil
}

func (r *fixtureBackupRepository) ManagedBackupInventory(context.Context) ([]privacy.ManagedBackupArtifact, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]privacy.ManagedBackupArtifact(nil), r.records...), nil
}

func (r *fixtureBackupRepository) MarkManagedBackupsPruned(_ context.Context, paths []string, _ time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.markFailures) > 0 {
		err := r.markFailures[0]
		r.markFailures = r.markFailures[1:]
		return err
	}
	pathSet := make(map[string]struct{}, len(paths))
	for _, path := range paths {
		pathSet[path] = struct{}{}
	}
	retained := r.records[:0]
	for _, artifact := range r.records {
		if _, pruned := pathSet[artifact.Path]; !pruned {
			retained = append(retained, artifact)
		}
	}
	r.records = retained
	r.pruned = append(r.pruned, paths...)
	return nil
}

func (r *fixtureBackupRepository) destroy() {
	r.mu.Lock()
	r.destroyed = true
	for index := range r.key {
		r.key[index] = 0
	}
	r.mu.Unlock()
}

type fixtureKeyLease struct {
	id         string
	generation int64
	key        []byte
	active     bool
	recorded   bool
	record     func(privacy.ManagedBackupArtifact) error
}

func (l *fixtureKeyLease) WrappedKeyID() string { return l.id }
func (l *fixtureKeyLease) Generation() int64    { return l.generation }
func (l *fixtureKeyLease) Use(callback func([]byte) error) error {
	if !l.active {
		return privacy.ErrGenerationKeyUnavailable
	}
	return callback(l.key)
}
func (l *fixtureKeyLease) RecordManagedBackup(_ context.Context, artifact privacy.ManagedBackupArtifact) error {
	if !l.active || l.record == nil || l.recorded {
		return privacy.ErrManagedBackupConflict
	}
	if err := l.record(artifact); err != nil {
		return err
	}
	l.recorded = true
	return nil
}
func (l *fixtureKeyLease) invalidate() {
	for index := range l.key {
		l.key[index] = 0
	}
	l.key = nil
	l.active = false
}

type fixtureMaintenance struct {
	root                 string
	requests             []memory.BackupPruneRequest
	backupCalls          int
	backupErr            error
	backupOverride       *memory.BackupInventory
	beforePrune          func(memory.BackupPruneRequest)
	afterApply           func(memory.BackupPruneRequest)
	pruneErr             error
	responsePathMismatch bool
}

func (m *fixtureMaintenance) Backups(context.Context) (memory.BackupInventory, error) {
	m.backupCalls++
	if m.backupErr != nil {
		return memory.BackupInventory{}, m.backupErr
	}
	if m.backupOverride != nil {
		result := *m.backupOverride
		result.Artifacts = append([]memory.ManagedBackup(nil), m.backupOverride.Artifacts...)
		return result, nil
	}
	return fixtureInventoryFromRoot(m.root)
}

func (m *fixtureMaintenance) PruneBackups(_ context.Context, request memory.BackupPruneRequest) (memory.BackupPruneResult, error) {
	m.requests = append(m.requests, request)
	if m.beforePrune != nil {
		m.beforePrune(request)
	}
	postMutation, err := applyFixturePrune(m.root, request)
	if err != nil {
		return memory.BackupPruneResult{}, err
	}
	if m.afterApply != nil {
		m.afterApply(request)
	}
	if m.pruneErr != nil {
		return memory.BackupPruneResult{}, m.pruneErr
	}
	deletedPaths := append([]string(nil), request.Paths...)
	if m.responsePathMismatch {
		if len(deletedPaths) > 1 {
			deletedPaths = deletedPaths[:1]
		} else {
			deletedPaths = []string{deletedPaths[0] + ".mismatch"}
		}
	}
	return memory.BackupPruneResult{
		OperationID: request.OperationID, DeletedPaths: deletedPaths, ManifestSHA256: postMutation.ManifestSHA256,
	}, nil
}

type managedPruneFixture struct {
	repository  *fixtureBackupRepository
	controller  *BackupController
	maintenance *fixtureMaintenance
	root        string
	artifacts   []privacy.ManagedBackupArtifact
	cutoff      time.Time
	current     time.Time
}

func newManagedPruneFixture(t *testing.T) *managedPruneFixture {
	t.Helper()
	fixture := &managedPruneFixture{
		repository: newFixtureBackupRepository(), cutoff: time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC),
		current: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC),
	}
	fixture.root = t.TempDir()
	controller, err := NewBackupController(BackupControllerOptions{
		Root: fixture.root, DumpSource: &fixtureDump{content: []byte("retention dump"), writeStep: 4},
		Keys: fixture.repository, Inventory: fixture.repository, ChunkSize: 8, Now: func() time.Time { return fixture.current },
	})
	if err != nil {
		t.Fatal(err)
	}
	fixture.controller = controller
	for _, value := range []time.Time{
		time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 9, 3, 0, 0, 0, 0, time.UTC),
	} {
		fixture.current = value
		artifact, err := controller.Produce(context.Background(), 2)
		if err != nil {
			t.Fatal(err)
		}
		fixture.artifacts = append(fixture.artifacts, artifact)
	}
	fixture.maintenance = &fixtureMaintenance{root: fixture.root}
	return fixture
}

func fixtureInventoryFromRoot(root string) (memory.BackupInventory, error) {
	payload, err := os.ReadFile(filepath.Join(root, backupInventoryName))
	if err != nil {
		return memory.BackupInventory{}, err
	}
	artifacts, err := decodeManifest(payload)
	if err != nil {
		return memory.BackupInventory{}, err
	}
	manifestDigest, err := managedBackupManifestDigest(artifacts)
	if err != nil {
		return memory.BackupInventory{}, err
	}
	inventory := memory.BackupInventory{Validated: true, ManifestSHA256: manifestDigest}
	for _, artifact := range artifacts {
		inventory.Artifacts = append(inventory.Artifacts, memory.ManagedBackup{
			Path: artifact.Path, CreatedAt: artifact.CreatedAt, Size: artifact.Size, SHA256: artifact.SHA256,
			LearnerGeneration: artifact.LearnerGeneration, WrappedKeyID: artifact.WrappedKeyID,
		})
	}
	return inventory, nil
}

func applyFixturePrune(root string, request memory.BackupPruneRequest) (memory.BackupInventory, error) {
	current, err := fixtureInventoryFromRoot(root)
	if err != nil {
		return memory.BackupInventory{}, err
	}
	if current.ManifestSHA256 != request.ExpectedManifestSHA256 {
		return memory.BackupInventory{}, &Error{category: CategoryActive, operation: "prune_backups", mutationDispatched: true}
	}
	requested := make(map[string]struct{}, len(request.Paths))
	for _, path := range request.Paths {
		requested[path] = struct{}{}
	}
	retained := make([]privacy.ManagedBackupArtifact, 0, len(current.Artifacts))
	found := make(map[string]struct{}, len(request.Paths))
	for _, item := range current.Artifacts {
		artifact := privacy.ManagedBackupArtifact{
			Path: item.Path, CreatedAt: item.CreatedAt, Size: item.Size, SHA256: item.SHA256,
			LearnerGeneration: item.LearnerGeneration, WrappedKeyID: item.WrappedKeyID,
		}
		if _, prune := requested[artifact.Path]; !prune {
			retained = append(retained, artifact)
			continue
		}
		if artifact.CreatedAt.After(request.Cutoff) {
			return memory.BackupInventory{}, &Error{category: CategoryValidation, operation: "prune_backups", mutationDispatched: true}
		}
		found[artifact.Path] = struct{}{}
	}
	if len(found) != len(requested) {
		return memory.BackupInventory{}, &Error{category: CategoryActive, operation: "prune_backups", mutationDispatched: true}
	}
	for path := range requested {
		if err := os.Remove(filepath.Join(root, path)); err != nil {
			return memory.BackupInventory{}, err
		}
	}
	manifest, err := encodeManifest(retained)
	if err != nil {
		return memory.BackupInventory{}, err
	}
	tempManifest := filepath.Join(root, ".managed-inventory.fixture.tmp")
	if err := os.WriteFile(tempManifest, manifest, 0o600); err != nil {
		return memory.BackupInventory{}, err
	}
	if err := os.Rename(tempManifest, filepath.Join(root, backupInventoryName)); err != nil {
		return memory.BackupInventory{}, err
	}
	return fixtureInventoryFromRoot(root)
}

func fixtureBackupController(t *testing.T, repository *fixtureBackupRepository, dump DumpSource, chunkSize int, now time.Time) (*BackupController, string) {
	t.Helper()
	return fixtureBackupControllerWithClock(t, repository, dump, chunkSize, func() time.Time { return now })
}

func fixtureBackupControllerWithClock(t *testing.T, repository *fixtureBackupRepository, dump DumpSource, chunkSize int, now func() time.Time) (*BackupController, string) {
	t.Helper()
	root := t.TempDir()
	controller, err := NewBackupController(BackupControllerOptions{
		Root: root, DumpSource: dump, Keys: repository, Inventory: repository, ChunkSize: chunkSize, Now: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	return controller, root
}

func entryNames(entries []os.DirEntry) []string {
	result := make([]string, len(entries))
	for index, entry := range entries {
		result[index] = entry.Name()
	}
	return result
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

// Keep the manifest fixture tied to standard JSON parsing as well as the
// controller's stricter duplicate-key validation.
func TestManagedBackupManifestMatchesSchemaFieldNames(t *testing.T) {
	repository := newFixtureBackupRepository()
	controller, root := fixtureBackupController(t, repository, &fixtureDump{content: []byte("schema fixture")}, 8, time.Date(2026, 9, 3, 0, 0, 0, 0, time.UTC))
	if _, err := controller.Produce(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	payload, err := os.ReadFile(filepath.Join(root, backupInventoryName))
	if err != nil {
		t.Fatal(err)
	}
	var manifest map[string]json.RawMessage
	if err := json.Unmarshal(payload, &manifest); err != nil {
		t.Fatal(err)
	}
	if len(manifest) != 2 || manifest["format"] == nil || manifest["artifacts"] == nil {
		t.Fatalf("manifest fields=%v", manifest)
	}
	var artifacts []map[string]json.RawMessage
	if err := json.Unmarshal(manifest["artifacts"], &artifacts); err != nil || len(artifacts) != 1 {
		t.Fatalf("artifacts=%v err=%v", artifacts, err)
	}
	required := []string{"path", "created_at", "size", "sha256", "learner_generation", "wrapped_key_id"}
	for _, field := range required {
		if artifacts[0][field] == nil {
			t.Fatalf("missing schema field %q", field)
		}
	}
	if len(artifacts[0]) != len(required) {
		t.Fatalf("unexpected artifact fields=%v", artifacts[0])
	}
	var schemaWire backupWire
	wirePayload := `{"path":"fixture.enc","created_at":"2026-09-03T00:00:00Z","size":9,"sha256":"` + strings.Repeat("a", 64) + `","learner_generation":1,"wrapped_key_id":"` + repository.id + `"}`
	if err := json.Unmarshal([]byte(wirePayload), &schemaWire); err != nil || schemaWire.Size == nil || *schemaWire.Size != 9 {
		t.Fatalf("schema size wire=%+v err=%v", schemaWire, err)
	}
	legacySize := strings.Replace(wirePayload, `"size":9`, `"size_bytes":9`, 1)
	if err := json.Unmarshal([]byte(legacySize), &schemaWire); err != nil || schemaWire.Size == nil || *schemaWire.Size != 9 {
		t.Fatalf("legacy size wire=%+v err=%v", schemaWire, err)
	}
	bothSizes := strings.Replace(wirePayload, `"size":9`, `"size":9,"size_bytes":9`, 1)
	if err := json.Unmarshal([]byte(bothSizes), &schemaWire); !errors.Is(err, ErrBackupInventory) {
		t.Fatalf("ambiguous size fields err=%v", err)
	}
}
