package app

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/integrations/nocturne"
	"github.com/edu-agent/edu-agent/server/internal/privacy"
	"github.com/google/uuid"
)

func TestRestoreNocturneBackupArtifactUsesEncryptedInventoryAndRejectsDestroyedKey(t *testing.T) {
	repository := newOperatorBackupRepository()
	root := t.TempDir()
	plaintext := []byte("real encrypted rollback fixture")
	controller, err := nocturne.NewBackupController(nocturne.BackupControllerOptions{
		Root: root, DumpSource: operatorDumpSource{content: plaintext}, Keys: repository,
		Inventory: repository, ChunkSize: 8,
		Now:    func() time.Time { return time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC) },
		Random: bytes.NewReader(bytes.Repeat([]byte{0x71}, 64)),
	})
	if err != nil {
		t.Fatal(err)
	}
	artifact, err := controller.Produce(context.Background(), 4)
	if err != nil {
		t.Fatal(err)
	}
	ciphertext, err := os.ReadFile(filepath.Join(root, artifact.Path))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(ciphertext, plaintext) || bytes.Equal(ciphertext, plaintext) {
		t.Fatal("fixture artifact is not encrypted")
	}

	destination := filepath.Join(t.TempDir(), "rollback.dump")
	if err := restoreNocturneBackupArtifact(context.Background(), artifact.Path, destination, repository, controller); err != nil {
		t.Fatal(err)
	}
	restored, err := os.ReadFile(destination)
	if err != nil || !bytes.Equal(restored, plaintext) {
		t.Fatalf("restored=%q err=%v", restored, err)
	}
	info, err := os.Stat(destination)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("restore mode=%v err=%v", info.Mode(), err)
	}

	repository.destroyed = true
	err = restoreNocturneBackupArtifact(context.Background(), artifact.Path, filepath.Join(t.TempDir(), "destroyed.dump"), repository, controller)
	if !errors.Is(err, privacy.ErrGenerationKeyDestroyed) {
		t.Fatalf("destroyed key restore err=%v", err)
	}
}

func TestRestoreNocturneBackupArtifactRejectsUnsafeRequestedPath(t *testing.T) {
	for _, value := range []string{"", "../artifact", "/absolute", "nested/artifact", "managed-inventory.json", ".edu-agent-backup.lock"} {
		if err := restoreNocturneBackupArtifact(context.Background(), value, "/unused", newOperatorBackupRepository(), nil); !errors.Is(err, privacy.ErrManagedBackupInvalid) {
			t.Fatalf("path %q err=%v", value, err)
		}
	}
}

func TestRequireTmpfsRestoreDestination(t *testing.T) {
	var rootFilesystem syscall.Statfs_t
	if err := syscall.Statfs("/", &rootFilesystem); err == nil && uint64(rootFilesystem.Type) != linuxTmpfsMagic {
		if err := requireTmpfsRestoreDestination("/edu-agent-restore-test.dump"); err == nil {
			t.Fatal("non-tmpfs root filesystem was accepted")
		}
	}
	directory, err := os.MkdirTemp("/dev/shm", "edu-agent-restore-test-")
	if err != nil {
		t.Skipf("tmpfs fixture unavailable: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	output := filepath.Join(directory, "restore.dump")
	if err := requireTmpfsRestoreDestination(output); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(directory, "elsewhere"), output); err != nil {
		t.Fatal(err)
	}
	if err := requireTmpfsRestoreDestination(output); err == nil {
		t.Fatal("existing symlink destination was accepted")
	}
}

type operatorDumpSource struct{ content []byte }

func (s operatorDumpSource) Dump(_ context.Context, destination io.Writer) error {
	_, err := destination.Write(s.content)
	return err
}

type operatorBackupRepository struct {
	id         string
	key        []byte
	generation int64
	destroyed  bool
	artifacts  []privacy.ManagedBackupArtifact
}

func newOperatorBackupRepository() *operatorBackupRepository {
	return &operatorBackupRepository{id: uuid.NewString(), key: bytes.Repeat([]byte{0x42}, 32)}
}

func (r *operatorBackupRepository) WithGenerationKey(_ context.Context, generation int64, callback func(privacy.GenerationKeyLease) error) error {
	if r.destroyed {
		return privacy.ErrGenerationKeyDestroyed
	}
	if r.generation == 0 {
		r.generation = generation
	}
	if r.generation != generation {
		return privacy.ErrGenerationKeyUnavailable
	}
	return callback(&operatorGenerationKeyLease{repository: r, generation: generation})
}

func (r *operatorBackupRepository) WithExistingGenerationKey(_ context.Context, generation int64, keyID string, callback func(privacy.GenerationKeyLease) error) error {
	if r.destroyed {
		return privacy.ErrGenerationKeyDestroyed
	}
	if generation != r.generation || keyID != r.id {
		return privacy.ErrGenerationKeyUnavailable
	}
	return callback(&operatorGenerationKeyLease{repository: r, generation: generation})
}

func (r *operatorBackupRepository) VerifyGenerationKeyDestroyed(context.Context, int64, string) error {
	if r.destroyed {
		return nil
	}
	return privacy.ErrGenerationKeyUnavailable
}

func (r *operatorBackupRepository) RecordManagedBackup(_ context.Context, artifact privacy.ManagedBackupArtifact) error {
	r.artifacts = append(r.artifacts, artifact)
	return nil
}

func (r *operatorBackupRepository) DiscardManagedBackupPublication(context.Context, privacy.ManagedBackupArtifact, time.Time) error {
	return nil
}

func (r *operatorBackupRepository) ManagedBackupInventory(context.Context) ([]privacy.ManagedBackupArtifact, error) {
	return append([]privacy.ManagedBackupArtifact(nil), r.artifacts...), nil
}

func (r *operatorBackupRepository) MarkManagedBackupsPruned(context.Context, []string, time.Time) error {
	return nil
}

type operatorGenerationKeyLease struct {
	repository *operatorBackupRepository
	generation int64
}

func (l *operatorGenerationKeyLease) WrappedKeyID() string { return l.repository.id }
func (l *operatorGenerationKeyLease) Generation() int64    { return l.generation }
func (l *operatorGenerationKeyLease) Use(callback func([]byte) error) error {
	return callback(append([]byte(nil), l.repository.key...))
}
func (l *operatorGenerationKeyLease) RecordManagedBackup(_ context.Context, artifact privacy.ManagedBackupArtifact) error {
	l.repository.artifacts = append(l.repository.artifacts, artifact)
	return nil
}
