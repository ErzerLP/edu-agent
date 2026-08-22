package nocturne

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/memory"
	"github.com/edu-agent/edu-agent/server/internal/privacy"
	"github.com/google/uuid"
)

const (
	backupInventoryName      = "managed-inventory.json"
	backupLockName           = ".edu-agent-backup.lock"
	publicationJournalFormat = "edu-agent-managed-backup-publication-v1"
	publicationJournalSuffix = ".edu-agent-managed-backup-publication.json"
	publicationJournalLimit  = 1 << 20
	backupManifestLimit      = 16 << 20
	backupDefaultChunkSize   = 1 << 20
	backupMaximumChunkSize   = 16 << 20
	backupEncryptionVersion  = uint16(1)
	backupHeaderSize         = 32
)

var (
	backupMagic          = [8]byte{'E', 'D', 'U', 'M', 'B', 'K', 'U', 'P'}
	ErrBackupDumpFailed  = errors.New("managed backup dump failed")
	ErrBackupFilesystem  = errors.New("managed backup filesystem validation failed")
	ErrBackupInventory   = errors.New("managed backup inventory validation failed")
	ErrBackupJournal     = errors.New("managed backup publication journal validation failed")
	ErrBackupRestore     = errors.New("managed backup restore failed")
	ErrBackupMaintenance = errors.New("managed backup maintenance validation failed")
)

// DumpSource writes dump plaintext only to the supplied stream. Implementations
// must not create persistent plaintext files.
type DumpSource interface {
	Dump(context.Context, io.Writer) error
}

// CommandDumpSource runs pg_dump directly, with stdout connected to the
// controller's encrypting writer. Stderr is bounded and discarded; errors expose
// only a fixed category and never raw command output.
type CommandDumpSource struct {
	Args []string
	Env  []string
}

func (s CommandDumpSource) Dump(ctx context.Context, destination io.Writer) error {
	command := exec.CommandContext(ctx, "pg_dump", s.Args...)
	command.Stdout = destination
	command.Stderr = boundedDiscard{limit: 4096}
	if s.Env != nil {
		command.Env = append([]string(nil), s.Env...)
	}
	if err := command.Run(); err != nil {
		if ctx.Err() != nil {
			return errors.New("pg_dump canceled")
		}
		var exitError *exec.ExitError
		if errors.As(err, &exitError) {
			return errors.New("pg_dump exited unsuccessfully")
		}
		return errors.New("pg_dump could not be started")
	}
	return nil
}

// UnmarshalJSON keeps the existing maintenance client compatible with the
// schema's size field while accepting its historical size_bytes fixture. Both
// names together and all unknown fields are rejected.
func (w *backupWire) UnmarshalJSON(payload []byte) error {
	var value struct {
		Path              *string `json:"path"`
		CreatedAt         *string `json:"created_at"`
		SHA256            *string `json:"sha256"`
		WrappedKeyID      *string `json:"wrapped_key_id"`
		Size              *int64  `json:"size"`
		LegacySize        *int64  `json:"size_bytes"`
		LearnerGeneration *int64  `json:"learner_generation"`
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		return err
	}
	if err := requireJSONEOF(decoder); err != nil || value.Size != nil && value.LegacySize != nil {
		return ErrBackupInventory
	}
	*w = backupWire{
		Path: value.Path, CreatedAt: value.CreatedAt, SHA256: value.SHA256,
		WrappedKeyID: value.WrappedKeyID, Size: value.Size, LearnerGeneration: value.LearnerGeneration,
	}
	if w.Size == nil {
		w.Size = value.LegacySize
	}
	return nil
}

type boundedDiscard struct{ limit int64 }

func (w boundedDiscard) Write(value []byte) (int, error) {
	if w.limit > 0 && int64(len(value)) > w.limit {
		_ = value[:w.limit]
	}
	return len(value), nil
}

type BackupInventoryReader interface {
	Backups(context.Context) (memory.BackupInventory, error)
}

type BackupControllerOptions struct {
	Root        string
	DumpSource  DumpSource
	Keys        privacy.GenerationKeyRepository
	Inventory   privacy.ManagedBackupInventoryRepository
	Maintenance BackupInventoryReader
	ChunkSize   int
	Now         func() time.Time
	Random      io.Reader
}

type BackupController struct {
	root            string
	rootChain       []fileIdentity
	journalParent   string
	journalName     string
	journalTempName string
	dump            DumpSource
	keys            privacy.GenerationKeyRepository
	inventory       privacy.ManagedBackupInventoryRepository
	maintenance     BackupInventoryReader
	chunkSize       int
	now             func() time.Time
	random          io.Reader
}

func NewBackupController(options BackupControllerOptions) (*BackupController, error) {
	if options.Root == "" || options.Root == string(filepath.Separator) || !filepath.IsAbs(options.Root) || filepath.Clean(options.Root) != options.Root ||
		options.DumpSource == nil || options.Keys == nil || options.Inventory == nil {
		return nil, errors.New("managed backup controller requires a fixed absolute root, dump source, key repository, and inventory repository")
	}
	chunkSize := options.ChunkSize
	if chunkSize == 0 {
		chunkSize = backupDefaultChunkSize
	}
	if chunkSize < 1 || chunkSize > backupMaximumChunkSize {
		return nil, errors.New("managed backup chunk size is invalid")
	}
	chain, err := directoryChain(options.Root)
	if err != nil {
		return nil, ErrBackupFilesystem
	}
	now := options.Now
	if now == nil {
		now = time.Now
	}
	randomSource := options.Random
	if randomSource == nil {
		randomSource = rand.Reader
	}
	journalParent := filepath.Dir(options.Root)
	journalName := "." + filepath.Base(options.Root) + publicationJournalSuffix
	return &BackupController{
		root: options.Root, rootChain: chain, journalParent: journalParent,
		journalName: journalName, journalTempName: journalName + ".tmp",
		dump: options.DumpSource, keys: options.Keys, inventory: options.Inventory,
		maintenance: options.Maintenance, chunkSize: chunkSize, now: now, random: randomSource,
	}, nil
}

// Produce encrypts the dump stream directly into a same-directory temporary
// artifact. Only ciphertext is fsynced and atomically published.
func (c *BackupController) Produce(ctx context.Context, generation int64) (privacy.ManagedBackupArtifact, error) {
	if generation < 1 {
		return privacy.ManagedBackupArtifact{}, privacy.ErrManagedBackupInvalid
	}
	root, err := c.lockRoot(true)
	if err != nil {
		return privacy.ManagedBackupArtifact{}, err
	}
	defer root.close()
	if err := root.recoverPublication(ctx, c.keys, c.inventory, c.now().UTC().Truncate(time.Microsecond)); err != nil {
		return privacy.ManagedBackupArtifact{}, err
	}
	artifacts, _, manifestSnapshot, err := root.validatedInventory()
	if err != nil {
		return privacy.ManagedBackupArtifact{}, err
	}
	createdAt := c.now().UTC().Truncate(time.Microsecond)
	filename := fmt.Sprintf("managed-g%020d-%s.backup.enc", generation, uuid.NewString())
	var produced privacy.ManagedBackupArtifact
	err = c.keys.WithGenerationKey(ctx, generation, func(lease privacy.GenerationKeyLease) error {
		if lease.Generation() != generation || uuid.Validate(lease.WrappedKeyID()) != nil {
			return privacy.ErrGenerationKeyUnavailable
		}
		return lease.Use(func(key []byte) error {
			artifact, journalSnapshot, produceErr := c.produceWithKey(ctx, root, artifacts, manifestSnapshot, filename, createdAt, lease.WrappedKeyID(), generation, key)
			if produceErr != nil {
				return produceErr
			}
			if err := lease.RecordManagedBackup(ctx, artifact); err != nil {
				return fmt.Errorf("persist managed backup metadata: %w", err)
			}
			if err := root.clearPublicationJournal(journalSnapshot); err != nil {
				return err
			}
			produced = artifact
			return nil
		})
	})
	if err != nil {
		return privacy.ManagedBackupArtifact{}, err
	}
	return produced, nil
}

func (c *BackupController) produceWithKey(
	ctx context.Context,
	root *lockedBackupRoot,
	artifacts []privacy.ManagedBackupArtifact,
	manifestSnapshot *fileSnapshot,
	filename string,
	createdAt time.Time,
	keyID string,
	generation int64,
	key []byte,
) (privacy.ManagedBackupArtifact, *fileSnapshot, error) {
	tempName := "." + filename + "." + uuid.NewString() + ".tmp"
	fd, err := syscall.Openat(root.fd, tempName, syscall.O_RDWR|syscall.O_CREAT|syscall.O_EXCL|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return privacy.ManagedBackupArtifact{}, nil, ErrBackupFilesystem
	}
	temporary := os.NewFile(uintptr(fd), tempName)
	tempPresent := true
	defer func() {
		_ = temporary.Close()
		if tempPresent {
			_ = syscall.Unlinkat(root.fd, tempName)
		}
	}()

	encryptor, err := newChunkEncryptWriter(temporary, key, c.chunkSize, c.random)
	if err != nil {
		return privacy.ManagedBackupArtifact{}, nil, privacy.ErrManagedBackupIntegrity
	}
	defer encryptor.abort()
	if err := c.dump.Dump(ctx, encryptor); err != nil {
		return privacy.ManagedBackupArtifact{}, nil, ErrBackupDumpFailed
	}
	if err := encryptor.Close(); err != nil || temporary.Sync() != nil {
		return privacy.ManagedBackupArtifact{}, nil, ErrBackupFilesystem
	}
	snapshot, err := snapshotFile(temporary)
	if err != nil || temporary.Close() != nil {
		return privacy.ManagedBackupArtifact{}, nil, ErrBackupFilesystem
	}
	artifact := privacy.ManagedBackupArtifact{
		Path: filename, CreatedAt: createdAt, Size: snapshot.size, SHA256: snapshot.sha256,
		LearnerGeneration: generation, WrappedKeyID: keyID,
	}
	if err := artifact.Validate(); err != nil {
		return privacy.ManagedBackupArtifact{}, nil, err
	}
	rootIdentity, err := descriptorIdentity(root.fd)
	if err != nil {
		return privacy.ManagedBackupArtifact{}, nil, ErrBackupFilesystem
	}
	journal := publicationJournal{
		Format: publicationJournalFormat, State: "prepared", RootDevice: rootIdentity.device,
		RootInode: rootIdentity.inode, TempPath: tempName, Artifact: artifact,
	}
	journalSnapshot, err := root.writePublicationJournal(journal, nil)
	if err != nil {
		return privacy.ManagedBackupArtifact{}, nil, err
	}
	if err := root.assertStable(); err != nil {
		return privacy.ManagedBackupArtifact{}, nil, err
	}
	reservationFD, err := syscall.Openat(root.fd, filename, syscall.O_RDWR|syscall.O_CREAT|syscall.O_EXCL|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return privacy.ManagedBackupArtifact{}, nil, ErrBackupFilesystem
	}
	reservationPresent := true
	defer func() {
		_ = syscall.Close(reservationFD)
		if reservationPresent {
			_ = syscall.Unlinkat(root.fd, filename)
		}
	}()
	reservationIdentity, err := descriptorIdentity(reservationFD)
	if err != nil {
		return privacy.ManagedBackupArtifact{}, nil, ErrBackupFilesystem
	}
	namedReservationFD, err := syscall.Openat(root.fd, filename, syscall.O_RDONLY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return privacy.ManagedBackupArtifact{}, nil, ErrBackupFilesystem
	}
	namedReservationIdentity, namedReservationErr := descriptorIdentity(namedReservationFD)
	_ = syscall.Close(namedReservationFD)
	if namedReservationErr != nil || namedReservationIdentity != reservationIdentity {
		return privacy.ManagedBackupArtifact{}, nil, ErrBackupFilesystem
	}
	if err := syscall.Renameat(root.fd, tempName, root.fd, filename); err != nil {
		return privacy.ManagedBackupArtifact{}, nil, ErrBackupFilesystem
	}
	tempPresent = false
	reservationPresent = false
	_ = syscall.Close(reservationFD)
	reservationFD = -1
	if err := syscall.Fsync(root.fd); err != nil {
		return privacy.ManagedBackupArtifact{}, nil, ErrBackupFilesystem
	}
	journal.State = "published"
	journalSnapshot, err = root.writePublicationJournal(journal, journalSnapshot)
	if err != nil {
		return privacy.ManagedBackupArtifact{}, nil, err
	}
	updated := append(append([]privacy.ManagedBackupArtifact(nil), artifacts...), artifact)
	sortArtifacts(updated)
	if _, err := root.publishManifest(updated, manifestSnapshot); err != nil {
		return privacy.ManagedBackupArtifact{}, nil, err
	}
	return artifact, journalSnapshot, nil
}

// Inventory returns only an inventory whose manifest, root entries, and
// artifact hashes all agree.
func (c *BackupController) Inventory(ctx context.Context) ([]privacy.ManagedBackupArtifact, error) {
	root, err := c.lockRoot(true)
	if err != nil {
		return nil, err
	}
	defer root.close()
	if err := root.recoverPublication(ctx, c.keys, c.inventory, c.now().UTC().Truncate(time.Microsecond)); err != nil {
		return nil, err
	}
	artifacts, _, _, err := root.validatedInventory()
	return artifacts, err
}

// RestoreVerify requires both a valid ciphertext artifact and a live matching
// generation key. It is intended for in-memory fixtures and internal streaming;
// application code restoring persistent plaintext must use RestoreToFile.
func (c *BackupController) RestoreVerify(ctx context.Context, artifact privacy.ManagedBackupArtifact, destination io.Writer) error {
	if destination == nil || artifact.Validate() != nil {
		return privacy.ErrManagedBackupInvalid
	}
	root, err := c.lockRoot(false)
	if err != nil {
		return err
	}
	defer root.close()
	artifacts, snapshots, _, err := root.validatedInventory()
	if err != nil {
		return err
	}
	found := false
	for _, candidate := range artifacts {
		if candidate.Path == artifact.Path && sameArtifact(candidate, artifact) {
			found = true
			break
		}
	}
	if !found {
		return ErrBackupInventory
	}
	file, err := root.openRegular(artifact.Path)
	if err != nil {
		return ErrBackupFilesystem
	}
	defer file.Close()
	before, err := snapshotFile(file)
	if err != nil || before != snapshots[artifact.Path] || before.sha256 != artifact.SHA256 {
		return privacy.ErrManagedBackupIntegrity
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return ErrBackupRestore
	}
	err = c.keys.WithExistingGenerationKey(ctx, artifact.LearnerGeneration, artifact.WrappedKeyID, func(lease privacy.GenerationKeyLease) error {
		if lease.Generation() != artifact.LearnerGeneration || lease.WrappedKeyID() != artifact.WrappedKeyID {
			return privacy.ErrGenerationKeyUnavailable
		}
		return lease.Use(func(key []byte) error {
			if err := DecryptManagedBackup(destination, file, key); err != nil {
				return err
			}
			return nil
		})
	})
	if err != nil {
		return err
	}
	after, err := snapshotFile(file)
	if err != nil || after != before {
		return privacy.ErrManagedBackupIntegrity
	}
	return nil
}

// RestoreToFile publishes plaintext only after full ciphertext hash, AEAD,
// terminator, and EOF verification. The destination must not already exist.
func (c *BackupController) RestoreToFile(ctx context.Context, artifact privacy.ManagedBackupArtifact, destination string) (resultErr error) {
	if artifact.Validate() != nil || destination == "" || !filepath.IsAbs(destination) || filepath.Clean(destination) != destination {
		return privacy.ErrManagedBackupInvalid
	}
	directory := filepath.Dir(destination)
	name := filepath.Base(destination)
	if directory == c.root || !validFlatName(name) || name == backupInventoryName || name == backupLockName {
		return privacy.ErrManagedBackupInvalid
	}
	chain, err := directoryChain(directory)
	if err != nil {
		return ErrBackupFilesystem
	}
	directoryFD, err := syscall.Open(directory, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return ErrBackupFilesystem
	}
	defer syscall.Close(directoryFD)
	assertDirectory := func() error {
		current, err := directoryChain(directory)
		identity, identityErr := descriptorIdentity(directoryFD)
		if err != nil || identityErr != nil || !sameIdentityChain(current, chain) || identity != chain[len(chain)-1] {
			return ErrBackupFilesystem
		}
		return nil
	}
	if err := assertDirectory(); err != nil {
		return err
	}
	tempName := "." + name + "." + uuid.NewString() + ".tmp"
	tempFD, err := syscall.Openat(directoryFD, tempName, syscall.O_WRONLY|syscall.O_CREAT|syscall.O_EXCL|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return ErrBackupFilesystem
	}
	temporary := os.NewFile(uintptr(tempFD), tempName)
	tempPresent := true
	reservationFD := -1
	reservationPresent := false
	targetPublished := false
	defer func() {
		_ = temporary.Close()
		if reservationFD >= 0 {
			_ = syscall.Close(reservationFD)
		}
		if resultErr != nil {
			if tempPresent {
				_ = syscall.Unlinkat(directoryFD, tempName)
			}
			if reservationPresent || targetPublished {
				_ = syscall.Unlinkat(directoryFD, name)
			}
			_ = syscall.Fsync(directoryFD)
		}
	}()
	if err := c.RestoreVerify(ctx, artifact, temporary); err != nil {
		return err
	}
	if err := temporary.Chmod(0o600); err != nil || temporary.Sync() != nil || temporary.Close() != nil {
		return ErrBackupFilesystem
	}
	if err := assertDirectory(); err != nil {
		return err
	}
	reservationFD, err = syscall.Openat(directoryFD, name, syscall.O_RDWR|syscall.O_CREAT|syscall.O_EXCL|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return ErrBackupFilesystem
	}
	reservationPresent = true
	reservationIdentity, err := descriptorIdentity(reservationFD)
	if err != nil {
		return ErrBackupFilesystem
	}
	namedFD, err := syscall.Openat(directoryFD, name, syscall.O_RDONLY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return ErrBackupFilesystem
	}
	namedIdentity, namedErr := descriptorIdentity(namedFD)
	_ = syscall.Close(namedFD)
	if namedErr != nil || namedIdentity != reservationIdentity || assertDirectory() != nil {
		return ErrBackupFilesystem
	}
	if err := syscall.Renameat(directoryFD, tempName, directoryFD, name); err != nil {
		return ErrBackupFilesystem
	}
	tempPresent = false
	reservationPresent = false
	targetPublished = true
	_ = syscall.Close(reservationFD)
	reservationFD = -1
	if err := syscall.Fsync(directoryFD); err != nil || assertDirectory() != nil {
		return ErrBackupFilesystem
	}
	targetPublished = false
	return nil
}

// DecryptManagedBackup is the format fixture used by restore tests and offline
// recovery tooling. The stream format is header-v1 followed by authenticated
// length-prefixed chunks and an authenticated zero-length terminator.
func DecryptManagedBackup(destination io.Writer, source io.Reader, key []byte) error {
	if destination == nil || source == nil || len(key) != 32 {
		return privacy.ErrManagedBackupIntegrity
	}
	header := make([]byte, backupHeaderSize)
	if _, err := io.ReadFull(source, header); err != nil || !bytes.Equal(header[:8], backupMagic[:]) ||
		binary.BigEndian.Uint16(header[8:10]) != backupEncryptionVersion || binary.BigEndian.Uint16(header[10:12]) != 0 {
		return privacy.ErrManagedBackupIntegrity
	}
	chunkSize := binary.BigEndian.Uint32(header[12:16])
	if chunkSize == 0 || chunkSize > backupMaximumChunkSize {
		return privacy.ErrManagedBackupIntegrity
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return privacy.ErrManagedBackupIntegrity
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return privacy.ErrManagedBackupIntegrity
	}
	seed := header[16:32]
	var index uint64
	for {
		var lengthBytes [4]byte
		if _, err := io.ReadFull(source, lengthBytes[:]); err != nil {
			return privacy.ErrManagedBackupIntegrity
		}
		plaintextLength := binary.BigEndian.Uint32(lengthBytes[:])
		if plaintextLength > chunkSize {
			return privacy.ErrManagedBackupIntegrity
		}
		ciphertext := make([]byte, int(plaintextLength)+gcm.Overhead())
		if _, err := io.ReadFull(source, ciphertext); err != nil {
			return privacy.ErrManagedBackupIntegrity
		}
		nonce := backupChunkNonce(key, seed, index)
		plaintext, err := gcm.Open(nil, nonce, ciphertext, backupChunkAAD(header, index, plaintextLength))
		if err != nil {
			erasePlaintext(plaintext)
			return privacy.ErrManagedBackupIntegrity
		}
		if plaintextLength == 0 {
			erasePlaintext(plaintext)
			var trailing [1]byte
			if _, trailingErr := io.ReadFull(source, trailing[:]); !errors.Is(trailingErr, io.EOF) {
				return privacy.ErrManagedBackupIntegrity
			}
			return nil
		}
		if uint32(len(plaintext)) != plaintextLength {
			erasePlaintext(plaintext)
			return privacy.ErrManagedBackupIntegrity
		}
		if err := writeAll(destination, plaintext); err != nil {
			erasePlaintext(plaintext)
			return ErrBackupRestore
		}
		erasePlaintext(plaintext)
		index++
	}
}

type BackupRetentionInventory struct {
	Cutoff         time.Time
	ManifestSHA256 string
	Retained       []privacy.ManagedBackupArtifact
	Prune          []privacy.ManagedBackupArtifact
}

type managedBackupSnapshot struct {
	artifacts      []privacy.ManagedBackupArtifact
	manifestSHA256 string
}

func (c *BackupController) localBackupSnapshot(ctx context.Context) (managedBackupSnapshot, error) {
	root, err := c.lockRoot(true)
	if err != nil {
		return managedBackupSnapshot{}, err
	}
	defer root.close()
	if err := root.recoverPublication(ctx, c.keys, c.inventory, c.now().UTC().Truncate(time.Microsecond)); err != nil {
		return managedBackupSnapshot{}, err
	}
	artifacts, _, manifestSnapshot, err := root.validatedInventory()
	if err != nil {
		return managedBackupSnapshot{}, err
	}
	if manifestSnapshot == nil || !validBackupDigest(manifestSnapshot.sha256) {
		return managedBackupSnapshot{}, ErrBackupMaintenance
	}
	manifestDigest, err := managedBackupManifestDigest(artifacts)
	if err != nil {
		return managedBackupSnapshot{}, err
	}
	return managedBackupSnapshot{artifacts: artifacts, manifestSHA256: manifestDigest}, nil
}

func remoteBackupSnapshot(inventory memory.BackupInventory) (managedBackupSnapshot, error) {
	if !inventory.Validated || !validBackupDigest(inventory.ManifestSHA256) {
		return managedBackupSnapshot{}, ErrBackupMaintenance
	}
	artifacts := make([]privacy.ManagedBackupArtifact, 0, len(inventory.Artifacts))
	seen := make(map[string]struct{}, len(inventory.Artifacts))
	for _, item := range inventory.Artifacts {
		artifact := privacy.ManagedBackupArtifact{
			Path: item.Path, CreatedAt: item.CreatedAt, Size: item.Size, SHA256: item.SHA256,
			LearnerGeneration: item.LearnerGeneration, WrappedKeyID: item.WrappedKeyID,
		}
		if artifact.Validate() != nil {
			return managedBackupSnapshot{}, ErrBackupMaintenance
		}
		if _, duplicate := seen[artifact.Path]; duplicate {
			return managedBackupSnapshot{}, ErrBackupMaintenance
		}
		seen[artifact.Path] = struct{}{}
		artifacts = append(artifacts, artifact)
	}
	sortArtifacts(artifacts)
	return managedBackupSnapshot{artifacts: artifacts, manifestSHA256: inventory.ManifestSHA256}, nil
}

func databaseBackupArtifacts(artifacts []privacy.ManagedBackupArtifact) ([]privacy.ManagedBackupArtifact, error) {
	result := append([]privacy.ManagedBackupArtifact(nil), artifacts...)
	seen := make(map[string]struct{}, len(result))
	for _, artifact := range result {
		if artifact.Validate() != nil {
			return nil, ErrBackupMaintenance
		}
		if _, duplicate := seen[artifact.Path]; duplicate {
			return nil, ErrBackupMaintenance
		}
		seen[artifact.Path] = struct{}{}
	}
	sortArtifacts(result)
	return result, nil
}

func (c *BackupController) VerifyManagedBackups(ctx context.Context, request privacy.ManagedBackupVerificationRequest) (privacy.ManagedBackupVerificationResult, error) {
	if err := request.Validate(); err != nil {
		return privacy.ManagedBackupVerificationResult{}, err
	}
	completedAt := c.now().UTC().Truncate(time.Microsecond)
	barrierRepository, ok := c.keys.(privacy.ManagedBackupBarrierRepository)
	if !ok {
		return managedBackupVerificationResult(request, privacy.ManagedBackupBarrierState{}, completedAt,
			privacy.StepUnknown, "managed_backup_barrier_verification_unavailable", "", "", "", 0), nil
	}
	barrier, err := barrierRepository.VerifyManagedBackupBarrier(ctx, request.ErasureID, request.LearnerGeneration)
	if err != nil {
		status := privacy.StepUnknown
		reason := "managed_backup_barrier_verification_unavailable"
		if errors.Is(err, privacy.ErrManagedBackupLiveOldKey) {
			status = privacy.StepFailed
			reason = "pre_barrier_managed_backup_key_still_live"
		} else if errors.Is(err, privacy.ErrManagedBackupBarrierUnproven) {
			reason = "managed_backup_barrier_destruction_unproven"
		}
		return managedBackupVerificationResult(request, privacy.ManagedBackupBarrierState{}, completedAt,
			status, reason, "", "", "", 0), nil
	}

	local, err := c.localBackupSnapshot(ctx)
	if err != nil {
		return managedBackupVerificationResult(request, barrier, completedAt,
			privacy.StepUnknown, "managed_backup_local_inventory_unavailable", "", "", "", 0), nil
	}
	databaseInventory, err := c.inventory.ManagedBackupInventory(ctx)
	if err != nil {
		return managedBackupVerificationResult(request, barrier, completedAt,
			privacy.StepUnknown, "managed_backup_database_inventory_unavailable", local.manifestSHA256, "", "", 0), nil
	}
	database, err := databaseBackupArtifacts(databaseInventory)
	if err != nil {
		return managedBackupVerificationResult(request, barrier, completedAt,
			privacy.StepUnknown, "managed_backup_database_inventory_invalid", local.manifestSHA256, "", "", 0), nil
	}
	databaseDigest := managedBackupArtifactSetDigest(database)
	if c.maintenance == nil {
		return managedBackupVerificationResult(request, barrier, completedAt,
			privacy.StepUnknown, "managed_backup_remote_inventory_unavailable", local.manifestSHA256, databaseDigest, "", 0), nil
	}
	remoteInventory, err := c.maintenance.Backups(ctx)
	if err != nil {
		return managedBackupVerificationResult(request, barrier, completedAt,
			privacy.StepUnknown, "managed_backup_remote_inventory_unavailable", local.manifestSHA256, databaseDigest, "", 0), nil
	}
	remote, err := remoteBackupSnapshot(remoteInventory)
	if err != nil {
		return managedBackupVerificationResult(request, barrier, completedAt,
			privacy.StepUnknown, "managed_backup_remote_inventory_invalid", local.manifestSHA256, databaseDigest, "", 0), nil
	}
	if local.manifestSHA256 != remote.manifestSHA256 || !sameBackupArtifacts(local.artifacts, database) || !sameBackupArtifacts(local.artifacts, remote.artifacts) {
		return managedBackupVerificationResult(request, barrier, completedAt,
			privacy.StepPartial, "managed_backup_inventory_mismatch", local.manifestSHA256, databaseDigest, remote.manifestSHA256, 0), nil
	}

	oldArtifactCount := 0
	for _, artifact := range local.artifacts {
		if artifact.LearnerGeneration >= request.LearnerGeneration {
			continue
		}
		oldArtifactCount++
		if err := c.keys.VerifyGenerationKeyDestroyed(ctx, artifact.LearnerGeneration, artifact.WrappedKeyID); err != nil {
			status := privacy.StepUnknown
			reason := "managed_backup_key_destruction_verification_unavailable"
			if errors.Is(err, privacy.ErrGenerationKeyUnavailable) || errors.Is(err, privacy.ErrManagedBackupLiveOldKey) {
				status = privacy.StepFailed
				reason = "pre_barrier_managed_backup_key_not_destroyed"
			}
			return managedBackupVerificationResult(request, barrier, completedAt,
				status, reason, local.manifestSHA256, databaseDigest, remote.manifestSHA256, oldArtifactCount), nil
		}
		restoreErr := c.RestoreVerify(ctx, artifact, io.Discard)
		switch {
		case errors.Is(restoreErr, privacy.ErrGenerationKeyDestroyed):
			continue
		case restoreErr == nil:
			return managedBackupVerificationResult(request, barrier, completedAt,
				privacy.StepFailed, "pre_barrier_managed_backup_restore_succeeded", local.manifestSHA256, databaseDigest, remote.manifestSHA256, oldArtifactCount), nil
		case errors.Is(restoreErr, privacy.ErrManagedBackupIntegrity), errors.Is(restoreErr, ErrBackupInventory),
			errors.Is(restoreErr, ErrBackupFilesystem), errors.Is(restoreErr, ErrBackupRestore):
			return managedBackupVerificationResult(request, barrier, completedAt,
				privacy.StepPartial, "managed_backup_restore_failure_not_key_destruction", local.manifestSHA256, databaseDigest, remote.manifestSHA256, oldArtifactCount), nil
		default:
			return managedBackupVerificationResult(request, barrier, completedAt,
				privacy.StepUnknown, "managed_backup_restore_verification_unavailable", local.manifestSHA256, databaseDigest, remote.manifestSHA256, oldArtifactCount), nil
		}
	}
	if oldArtifactCount == 0 {
		return managedBackupVerificationResult(request, barrier, completedAt,
			privacy.StepNotApplicable, "no_pre_barrier_managed_backup_artifacts", local.manifestSHA256, databaseDigest, remote.manifestSHA256, 0), nil
	}
	return managedBackupVerificationResult(request, barrier, completedAt,
		privacy.StepSucceeded, "pre_barrier_managed_backups_unrecoverable_by_destroyed_keys", local.manifestSHA256, databaseDigest, remote.manifestSHA256, oldArtifactCount), nil
}

func managedBackupVerificationResult(
	request privacy.ManagedBackupVerificationRequest,
	barrier privacy.ManagedBackupBarrierState,
	completedAt time.Time,
	status privacy.StepStatus,
	reason, localManifest, databaseDigest, remoteManifest string,
	oldArtifactCount int,
) privacy.ManagedBackupVerificationResult {
	evidence := sha256.New()
	_, _ = fmt.Fprintf(evidence, "edu-agent-managed-backup-erasure-verification-v1\n%s\n%d\n%s\n%d\n%s\n%s\n%s\n%d\n%s\n%s\n",
		request.ErasureID, request.LearnerGeneration, barrier.VerifiedUnrecoverableAt.UTC().Format(time.RFC3339Nano),
		barrier.DestroyedOldKeyCount, localManifest, databaseDigest, remoteManifest, oldArtifactCount, status, reason)
	return privacy.ManagedBackupVerificationResult{
		Status: status, StableReason: reason, EvidenceDigest: hex.EncodeToString(evidence.Sum(nil)), CompletedAt: completedAt,
	}
}

func managedBackupArtifactSetDigest(artifacts []privacy.ManagedBackupArtifact) string {
	digest := sha256.New()
	_, _ = io.WriteString(digest, "edu-agent-managed-backup-artifact-set-v1\n")
	for _, artifact := range artifacts {
		_, _ = fmt.Fprintf(digest, "%s\n%s\n%d\n%s\n%d\n%s\n", artifact.Path,
			artifact.CreatedAt.Format(time.RFC3339Nano), artifact.Size, artifact.SHA256,
			artifact.LearnerGeneration, artifact.WrappedKeyID)
	}
	return hex.EncodeToString(digest.Sum(nil))
}

var _ privacy.ManagedBackupVerifier = (*BackupController)(nil)

func sameBackupArtifacts(left, right []privacy.ManagedBackupArtifact) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if !sameArtifact(left[index], right[index]) {
			return false
		}
	}
	return true
}

func sameBackupPaths(left, right []string) bool {
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

func partitionBackupArtifacts(artifacts []privacy.ManagedBackupArtifact, cutoff time.Time) BackupRetentionInventory {
	result := BackupRetentionInventory{Cutoff: cutoff}
	for _, artifact := range artifacts {
		if artifact.CreatedAt.After(cutoff) {
			result.Retained = append(result.Retained, artifact)
		} else {
			result.Prune = append(result.Prune, artifact)
		}
	}
	return result
}

// RetentionInventory compares a strict local manifest snapshot and Nocturne GET
// snapshot before selecting the exact created_at <= UTC cutoff path set.
func (c *BackupController) RetentionInventory(ctx context.Context, cutoff time.Time, maintenance memory.BackupInventory) (BackupRetentionInventory, error) {
	if cutoff.IsZero() || cutoff.Location() != time.UTC {
		return BackupRetentionInventory{}, ErrBackupMaintenance
	}
	local, err := c.localBackupSnapshot(ctx)
	if err != nil {
		return BackupRetentionInventory{}, err
	}
	remote, err := remoteBackupSnapshot(maintenance)
	if err != nil || local.manifestSHA256 != remote.manifestSHA256 || !sameBackupArtifacts(local.artifacts, remote.artifacts) {
		return BackupRetentionInventory{}, ErrBackupMaintenance
	}
	result := partitionBackupArtifacts(local.artifacts, cutoff)
	result.ManifestSHA256 = local.manifestSHA256
	return result, nil
}

type BackupMaintenance interface {
	BackupInventoryReader
	PruneBackups(context.Context, memory.BackupPruneRequest) (memory.BackupPruneResult, error)
}

func (c *BackupController) reconcilePruneSnapshot(ctx context.Context, cutoff time.Time, maintenance BackupMaintenance) (BackupRetentionInventory, error) {
	local, err := c.localBackupSnapshot(ctx)
	if err != nil {
		return BackupRetentionInventory{}, err
	}
	databaseInventory, err := c.inventory.ManagedBackupInventory(ctx)
	if err != nil {
		return BackupRetentionInventory{}, fmt.Errorf("read managed backup database inventory: %w", err)
	}
	database, err := databaseBackupArtifacts(databaseInventory)
	if err != nil {
		return BackupRetentionInventory{}, err
	}
	remoteInventory, err := maintenance.Backups(ctx)
	if err != nil {
		return BackupRetentionInventory{}, fmt.Errorf("read Nocturne backup inventory: %w", err)
	}
	remote, err := remoteBackupSnapshot(remoteInventory)
	if err != nil || local.manifestSHA256 != remote.manifestSHA256 || !sameBackupArtifacts(local.artifacts, remote.artifacts) {
		return BackupRetentionInventory{}, ErrBackupMaintenance
	}
	localByPath := make(map[string]privacy.ManagedBackupArtifact, len(local.artifacts))
	for _, artifact := range local.artifacts {
		localByPath[artifact.Path] = artifact
	}
	databaseByPath := make(map[string]privacy.ManagedBackupArtifact, len(database))
	for _, artifact := range database {
		databaseByPath[artifact.Path] = artifact
	}
	for path, artifact := range localByPath {
		databaseArtifact, exists := databaseByPath[path]
		if !exists || !sameArtifact(artifact, databaseArtifact) {
			return BackupRetentionInventory{}, ErrBackupMaintenance
		}
	}
	var reconcilePaths []string
	for _, artifact := range database {
		if _, exists := localByPath[artifact.Path]; exists {
			continue
		}
		if artifact.CreatedAt.After(cutoff) {
			return BackupRetentionInventory{}, ErrBackupMaintenance
		}
		reconcilePaths = append(reconcilePaths, artifact.Path)
	}
	if len(reconcilePaths) > 0 {
		sort.Strings(reconcilePaths)
		if err := c.inventory.MarkManagedBackupsPruned(ctx, reconcilePaths, c.now().UTC().Truncate(time.Microsecond)); err != nil {
			return BackupRetentionInventory{}, fmt.Errorf("reconcile managed backup prune: %w", err)
		}
	}
	result := partitionBackupArtifacts(local.artifacts, cutoff)
	result.ManifestSHA256 = local.manifestSHA256
	return result, nil
}

func (c *BackupController) confirmPruneSnapshot(ctx context.Context, maintenance BackupMaintenance, retained []privacy.ManagedBackupArtifact, previousDigest, expectedDigest string) error {
	remoteInventory, err := maintenance.Backups(ctx)
	if err != nil {
		return err
	}
	remote, err := remoteBackupSnapshot(remoteInventory)
	if err != nil || remote.manifestSHA256 == previousDigest || expectedDigest != "" && remote.manifestSHA256 != expectedDigest ||
		!sameBackupArtifacts(remote.artifacts, retained) {
		return ErrBackupMaintenance
	}
	local, err := c.localBackupSnapshot(ctx)
	if err != nil || local.manifestSHA256 != remote.manifestSHA256 || !sameBackupArtifacts(local.artifacts, retained) {
		return ErrBackupMaintenance
	}
	return nil
}

func (c *BackupController) markConfirmedPrune(ctx context.Context, paths []string) error {
	if len(paths) == 0 {
		return nil
	}
	if err := c.inventory.MarkManagedBackupsPruned(ctx, paths, c.now().UTC().Truncate(time.Microsecond)); err != nil {
		return fmt.Errorf("persist managed backup prune: %w", err)
	}
	return nil
}

func (c *BackupController) Prune(ctx context.Context, cutoff time.Time, maintenance BackupMaintenance) error {
	if maintenance == nil || cutoff.IsZero() || cutoff.Location() != time.UTC {
		return ErrBackupMaintenance
	}
	retention, err := c.reconcilePruneSnapshot(ctx, cutoff, maintenance)
	if err != nil {
		return err
	}
	if len(retention.Prune) == 0 {
		return nil
	}
	paths := make([]string, len(retention.Prune))
	for index, artifact := range retention.Prune {
		paths[index] = artifact.Path
	}
	sort.Strings(paths)
	request := memory.BackupPruneRequest{
		OperationID: uuid.NewString(), Cutoff: cutoff, ExpectedManifestSHA256: retention.ManifestSHA256, Paths: paths,
	}
	result, pruneErr := maintenance.PruneBackups(ctx, request)
	if pruneErr != nil {
		recoveryCtx := context.WithoutCancel(ctx)
		confirmErr := c.confirmPruneSnapshot(recoveryCtx, maintenance, retention.Retained, retention.ManifestSHA256, "")
		if Category(pruneErr) == CategoryContractMismatch || confirmErr != nil {
			return fmt.Errorf("prune Nocturne backups: %w", pruneErr)
		}
		return c.markConfirmedPrune(recoveryCtx, paths)
	}
	if result.OperationID != request.OperationID || !validBackupDigest(result.ManifestSHA256) || result.ManifestSHA256 == request.ExpectedManifestSHA256 ||
		validateBackupPathSet(result.DeletedPaths, false) != nil || !sameBackupPaths(result.DeletedPaths, request.Paths) {
		return ErrBackupMaintenance
	}
	if err := c.confirmPruneSnapshot(ctx, maintenance, retention.Retained, retention.ManifestSHA256, result.ManifestSHA256); err != nil {
		return err
	}
	return c.markConfirmedPrune(ctx, paths)
}

type chunkEncryptWriter struct {
	destination io.Writer
	gcm         cipher.AEAD
	key         []byte
	header      []byte
	seed        []byte
	buffer      []byte
	used        int
	index       uint64
	closed      bool
	failed      bool
}

func newChunkEncryptWriter(destination io.Writer, key []byte, chunkSize int, randomSource io.Reader) (*chunkEncryptWriter, error) {
	if destination == nil || len(key) != 32 || chunkSize < 1 || chunkSize > backupMaximumChunkSize || randomSource == nil {
		return nil, privacy.ErrManagedBackupIntegrity
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, privacy.ErrManagedBackupIntegrity
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, privacy.ErrManagedBackupIntegrity
	}
	header := make([]byte, backupHeaderSize)
	copy(header[:8], backupMagic[:])
	binary.BigEndian.PutUint16(header[8:10], backupEncryptionVersion)
	binary.BigEndian.PutUint16(header[10:12], 0)
	binary.BigEndian.PutUint32(header[12:16], uint32(chunkSize))
	if _, err := io.ReadFull(randomSource, header[16:32]); err != nil {
		return nil, privacy.ErrManagedBackupIntegrity
	}
	if err := writeAll(destination, header); err != nil {
		return nil, err
	}
	return &chunkEncryptWriter{
		destination: destination, gcm: gcm, key: key, header: header, seed: header[16:32], buffer: make([]byte, chunkSize),
	}, nil
}

func (w *chunkEncryptWriter) Write(value []byte) (int, error) {
	if w.closed || w.failed {
		return 0, ErrBackupFilesystem
	}
	written := 0
	for len(value) > 0 {
		copied := copy(w.buffer[w.used:], value)
		w.used += copied
		written += copied
		value = value[copied:]
		if w.used == len(w.buffer) {
			if err := w.flush(uint32(w.used)); err != nil {
				w.failed = true
				return written, err
			}
		}
	}
	return written, nil
}

func (w *chunkEncryptWriter) Close() error {
	if w.closed {
		if w.failed {
			return ErrBackupFilesystem
		}
		return nil
	}
	defer erasePlaintext(w.buffer)
	if w.failed {
		w.closed = true
		return ErrBackupFilesystem
	}
	if w.used > 0 {
		if err := w.flush(uint32(w.used)); err != nil {
			w.failed = true
			w.closed = true
			return err
		}
	}
	if err := w.writeRecord(nil, 0); err != nil {
		w.failed = true
		w.closed = true
		return err
	}
	w.closed = true
	return nil
}

func (w *chunkEncryptWriter) abort() {
	erasePlaintext(w.buffer)
	w.used = 0
	w.closed = true
}

func (w *chunkEncryptWriter) flush(length uint32) error {
	if length == 0 || int(length) != w.used {
		return ErrBackupFilesystem
	}
	plaintext := w.buffer[:w.used]
	if err := w.writeRecord(plaintext, length); err != nil {
		return err
	}
	erasePlaintext(plaintext)
	w.used = 0
	w.index++
	return nil
}

func (w *chunkEncryptWriter) writeRecord(plaintext []byte, length uint32) error {
	var encodedLength [4]byte
	binary.BigEndian.PutUint32(encodedLength[:], length)
	ciphertext := w.gcm.Seal(nil, backupChunkNonce(w.key, w.seed, w.index), plaintext, backupChunkAAD(w.header, w.index, length))
	if err := writeAll(w.destination, encodedLength[:]); err != nil {
		return err
	}
	return writeAll(w.destination, ciphertext)
}

func backupChunkNonce(key, seed []byte, index uint64) []byte {
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte("edu-agent-managed-backup-chunk-nonce-v1\x00"))
	_, _ = mac.Write(seed)
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], index)
	_, _ = mac.Write(encoded[:])
	return mac.Sum(nil)[:12]
}

func backupChunkAAD(header []byte, index uint64, plaintextLength uint32) []byte {
	result := make([]byte, 0, len(header)+12)
	result = append(result, header...)
	var encoded [12]byte
	binary.BigEndian.PutUint64(encoded[:8], index)
	binary.BigEndian.PutUint32(encoded[8:], plaintextLength)
	return append(result, encoded[:]...)
}

func writeAll(destination io.Writer, value []byte) error {
	for len(value) > 0 {
		written, err := destination.Write(value)
		if err != nil {
			return err
		}
		if written <= 0 || written > len(value) {
			return io.ErrShortWrite
		}
		value = value[written:]
	}
	return nil
}

func erasePlaintext(value []byte) {
	for index := range value {
		value[index] = 0
	}
}

type fileIdentity struct {
	device uint64
	inode  uint64
}

type fileSnapshot struct {
	device uint64
	inode  uint64
	nlink  uint64
	size   int64
	mtime  int64
	ctime  int64
	sha256 string
}

type lockedBackupRoot struct {
	controller   *BackupController
	fd           int
	parentFD     int
	lockFD       int
	lockIdentity fileIdentity
}

func directoryChain(path string) ([]fileIdentity, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, ErrBackupFilesystem
	}
	components := strings.Split(strings.TrimPrefix(path, string(filepath.Separator)), string(filepath.Separator))
	current := string(filepath.Separator)
	chain := make([]fileIdentity, 0, len(components)+1)
	for index := -1; index < len(components); index++ {
		if index >= 0 {
			if components[index] == "" || components[index] == "." || components[index] == ".." {
				return nil, ErrBackupFilesystem
			}
			current = filepath.Join(current, components[index])
		}
		info, err := os.Lstat(current)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return nil, ErrBackupFilesystem
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			return nil, ErrBackupFilesystem
		}
		chain = append(chain, fileIdentity{device: uint64(stat.Dev), inode: stat.Ino})
	}
	return chain, nil
}

func (c *BackupController) lockRoot(exclusive bool) (*lockedBackupRoot, error) {
	chain, err := directoryChain(c.root)
	if err != nil || !sameIdentityChain(chain, c.rootChain) {
		return nil, ErrBackupFilesystem
	}
	rootFD, err := syscall.Open(c.root, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, ErrBackupFilesystem
	}
	parentFD, err := syscall.Open(c.journalParent, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0)
	if err != nil {
		_ = syscall.Close(rootFD)
		return nil, ErrBackupFilesystem
	}
	root := &lockedBackupRoot{controller: c, fd: rootFD, parentFD: parentFD, lockFD: -1}
	lockFD, err := syscall.Openat(rootFD, backupLockName, syscall.O_RDWR|syscall.O_CREAT|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		root.close()
		return nil, ErrBackupFilesystem
	}
	root.lockFD = lockFD
	lockInfo, err := descriptorIdentity(lockFD)
	if err != nil {
		root.close()
		return nil, ErrBackupFilesystem
	}
	operation := syscall.LOCK_SH
	if exclusive {
		operation = syscall.LOCK_EX
	}
	if err := syscall.Flock(lockFD, operation); err != nil {
		root.close()
		return nil, ErrBackupFilesystem
	}
	root.lockIdentity = lockInfo
	if err := root.assertStable(); err != nil {
		root.close()
		return nil, err
	}
	return root, nil
}

func (r *lockedBackupRoot) close() {
	if r.lockFD >= 0 {
		_ = syscall.Flock(r.lockFD, syscall.LOCK_UN)
		_ = syscall.Close(r.lockFD)
		r.lockFD = -1
	}
	if r.fd >= 0 {
		_ = syscall.Close(r.fd)
		r.fd = -1
	}
	if r.parentFD >= 0 {
		_ = syscall.Close(r.parentFD)
		r.parentFD = -1
	}
}

func (r *lockedBackupRoot) assertStable() error {
	chain, err := directoryChain(r.controller.root)
	if err != nil || !sameIdentityChain(chain, r.controller.rootChain) {
		return ErrBackupFilesystem
	}
	rootIdentity, err := descriptorIdentity(r.fd)
	if err != nil || rootIdentity != chain[len(chain)-1] {
		return ErrBackupFilesystem
	}
	parentIdentity, err := descriptorIdentity(r.parentFD)
	if err != nil || len(chain) < 2 || parentIdentity != chain[len(chain)-2] {
		return ErrBackupFilesystem
	}
	if r.lockFD < 0 {
		return ErrBackupFilesystem
	}
	held, err := descriptorIdentity(r.lockFD)
	if err != nil || held != r.lockIdentity {
		return ErrBackupFilesystem
	}
	namedFD, err := syscall.Openat(r.fd, backupLockName, syscall.O_RDONLY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return ErrBackupFilesystem
	}
	named, namedErr := descriptorIdentity(namedFD)
	_ = syscall.Close(namedFD)
	if namedErr != nil || named != r.lockIdentity {
		return ErrBackupFilesystem
	}
	return nil
}

func descriptorIdentity(fd int) (fileIdentity, error) {
	var stat syscall.Stat_t
	if err := syscall.Fstat(fd, &stat); err != nil {
		return fileIdentity{}, ErrBackupFilesystem
	}
	kind := stat.Mode & syscall.S_IFMT
	if kind != syscall.S_IFREG && kind != syscall.S_IFDIR || kind == syscall.S_IFREG && stat.Nlink != 1 {
		return fileIdentity{}, ErrBackupFilesystem
	}
	return fileIdentity{device: uint64(stat.Dev), inode: stat.Ino}, nil
}

func sameIdentityChain(left, right []fileIdentity) bool {
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

func (r *lockedBackupRoot) openRegular(name string) (*os.File, error) {
	return openRegularAt(r.fd, name)
}

func openRegularAt(directoryFD int, name string) (*os.File, error) {
	if !validFlatName(name) {
		return nil, ErrBackupFilesystem
	}
	fd, err := syscall.Openat(directoryFD, name, syscall.O_RDONLY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	var stat syscall.Stat_t
	if err := syscall.Fstat(fd, &stat); err != nil || stat.Mode&syscall.S_IFMT != syscall.S_IFREG || stat.Nlink != 1 {
		_ = syscall.Close(fd)
		return nil, ErrBackupFilesystem
	}
	return os.NewFile(uintptr(fd), name), nil
}

func snapshotFile(file *os.File) (fileSnapshot, error) {
	before, err := fileStat(file)
	if err != nil {
		return fileSnapshot{}, err
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return fileSnapshot{}, err
	}
	digest := sha256.New()
	if _, err := io.Copy(digest, file); err != nil {
		return fileSnapshot{}, err
	}
	after, err := fileStat(file)
	if err != nil || before.device != after.device || before.inode != after.inode || before.nlink != after.nlink ||
		before.size != after.size || before.mtime != after.mtime || before.ctime != after.ctime {
		return fileSnapshot{}, ErrBackupFilesystem
	}
	after.sha256 = hex.EncodeToString(digest.Sum(nil))
	return after, nil
}

func fileStat(file *os.File) (fileSnapshot, error) {
	var stat syscall.Stat_t
	if err := syscall.Fstat(int(file.Fd()), &stat); err != nil || stat.Mode&syscall.S_IFMT != syscall.S_IFREG || stat.Nlink != 1 {
		return fileSnapshot{}, ErrBackupFilesystem
	}
	return fileSnapshot{
		device: uint64(stat.Dev), inode: stat.Ino, nlink: uint64(stat.Nlink), size: stat.Size,
		mtime: stat.Mtim.Sec*1_000_000_000 + stat.Mtim.Nsec, ctime: stat.Ctim.Sec*1_000_000_000 + stat.Ctim.Nsec,
	}, nil
}

func (r *lockedBackupRoot) directoryNames() (map[string]struct{}, error) {
	fd, err := syscall.Openat(r.fd, ".", syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, ErrBackupFilesystem
	}
	file := os.NewFile(uintptr(fd), "backup-root")
	defer file.Close()
	entries, err := file.ReadDir(-1)
	if err != nil {
		return nil, ErrBackupFilesystem
	}
	result := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		result[entry.Name()] = struct{}{}
	}
	return result, nil
}

type manifestJSON struct {
	Format    *string         `json:"format"`
	Artifacts *[]artifactJSON `json:"artifacts"`
}

type artifactJSON struct {
	Path              *string `json:"path"`
	CreatedAt         *string `json:"created_at"`
	Size              *int64  `json:"size"`
	SHA256            *string `json:"sha256"`
	LearnerGeneration *int64  `json:"learner_generation"`
	WrappedKeyID      *string `json:"wrapped_key_id"`
}

type publicationJournal struct {
	Format     string                        `json:"format"`
	State      string                        `json:"state"`
	RootDevice uint64                        `json:"root_device"`
	RootInode  uint64                        `json:"root_inode"`
	TempPath   string                        `json:"temp_path"`
	Artifact   privacy.ManagedBackupArtifact `json:"artifact"`
}

func (j publicationJournal) validate(root fileIdentity) error {
	if j.Format != publicationJournalFormat || j.State != "prepared" && j.State != "published" ||
		j.RootDevice != root.device || j.RootInode != root.inode || j.Artifact.Validate() != nil ||
		!validFlatName(j.TempPath) || !strings.HasPrefix(j.TempPath, "."+j.Artifact.Path+".") || !strings.HasSuffix(j.TempPath, ".tmp") {
		return ErrBackupJournal
	}
	return nil
}

func (r *lockedBackupRoot) readJournalFile(name string) (publicationJournal, *fileSnapshot, error) {
	file, err := openRegularAt(r.parentFD, name)
	if err != nil {
		if errors.Is(err, syscall.ENOENT) {
			return publicationJournal{}, nil, nil
		}
		return publicationJournal{}, nil, ErrBackupJournal
	}
	defer file.Close()
	snapshot, err := snapshotFile(file)
	if err != nil || snapshot.size > publicationJournalLimit {
		return publicationJournal{}, nil, ErrBackupJournal
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return publicationJournal{}, nil, ErrBackupJournal
	}
	payload, err := io.ReadAll(io.LimitReader(file, publicationJournalLimit+1))
	if err != nil || len(payload) > publicationJournalLimit || rejectDuplicateJSONKeys(payload) != nil {
		return publicationJournal{}, nil, ErrBackupJournal
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	var journal publicationJournal
	if err := decoder.Decode(&journal); err != nil || requireJSONEOF(decoder) != nil {
		return publicationJournal{}, nil, ErrBackupJournal
	}
	rootIdentity, err := descriptorIdentity(r.fd)
	if err != nil || journal.validate(rootIdentity) != nil {
		return publicationJournal{}, nil, ErrBackupJournal
	}
	return journal, &snapshot, nil
}

func (r *lockedBackupRoot) reconcileJournalTemp() error {
	_, tempSnapshot, err := r.readJournalFile(r.controller.journalTempName)
	if err != nil || tempSnapshot == nil {
		return err
	}
	mainFile, mainErr := openRegularAt(r.parentFD, r.controller.journalName)
	if mainErr == nil {
		_ = mainFile.Close()
		if err := syscall.Unlinkat(r.parentFD, r.controller.journalTempName); err != nil || syscall.Fsync(r.parentFD) != nil {
			return ErrBackupJournal
		}
		return nil
	}
	if !errors.Is(mainErr, syscall.ENOENT) {
		return ErrBackupJournal
	}
	if err := syscall.Renameat(r.parentFD, r.controller.journalTempName, r.parentFD, r.controller.journalName); err != nil || syscall.Fsync(r.parentFD) != nil {
		return ErrBackupJournal
	}
	return nil
}

func (r *lockedBackupRoot) readPublicationJournal() (publicationJournal, *fileSnapshot, error) {
	if err := r.assertStable(); err != nil {
		return publicationJournal{}, nil, err
	}
	if err := r.reconcileJournalTemp(); err != nil {
		return publicationJournal{}, nil, err
	}
	return r.readJournalFile(r.controller.journalName)
}

func (r *lockedBackupRoot) assertPublicationJournal(expected *fileSnapshot) error {
	_, current, err := r.readJournalFile(r.controller.journalName)
	if expected == nil {
		if err == nil && current == nil {
			return nil
		}
		return ErrBackupJournal
	}
	if err != nil || current == nil || *current != *expected {
		return ErrBackupJournal
	}
	return nil
}

func (r *lockedBackupRoot) writePublicationJournal(journal publicationJournal, expected *fileSnapshot) (*fileSnapshot, error) {
	rootIdentity, err := descriptorIdentity(r.fd)
	if err != nil || journal.validate(rootIdentity) != nil {
		return nil, ErrBackupJournal
	}
	if err := r.reconcileJournalTemp(); err != nil {
		return nil, err
	}
	payload, err := json.Marshal(journal)
	if err != nil {
		return nil, ErrBackupJournal
	}
	payload = append(payload, '\n')
	fd, err := syscall.Openat(r.parentFD, r.controller.journalTempName, syscall.O_WRONLY|syscall.O_CREAT|syscall.O_EXCL|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, ErrBackupJournal
	}
	temporary := os.NewFile(uintptr(fd), r.controller.journalTempName)
	closed := false
	complete := false
	defer func() {
		if !closed {
			_ = temporary.Close()
		}
		if !complete {
			_ = syscall.Unlinkat(r.parentFD, r.controller.journalTempName)
			_ = syscall.Fsync(r.parentFD)
		}
	}()
	if err := writeAll(temporary, payload); err != nil || temporary.Sync() != nil || temporary.Close() != nil {
		return nil, ErrBackupJournal
	}
	closed = true
	complete = true
	if err := r.assertPublicationJournal(expected); err != nil || r.assertStable() != nil {
		return nil, ErrBackupJournal
	}
	if err := syscall.Renameat(r.parentFD, r.controller.journalTempName, r.parentFD, r.controller.journalName); err != nil || syscall.Fsync(r.parentFD) != nil {
		return nil, ErrBackupJournal
	}
	_, snapshot, err := r.readJournalFile(r.controller.journalName)
	if err != nil || snapshot == nil {
		return nil, ErrBackupJournal
	}
	return snapshot, nil
}

func (r *lockedBackupRoot) clearPublicationJournal(expected *fileSnapshot) error {
	if expected == nil {
		return ErrBackupJournal
	}
	if err := r.reconcileJournalTemp(); err != nil || r.assertPublicationJournal(expected) != nil || r.assertStable() != nil {
		return ErrBackupJournal
	}
	if err := syscall.Unlinkat(r.parentFD, r.controller.journalName); err != nil || syscall.Fsync(r.parentFD) != nil {
		return ErrBackupJournal
	}
	return nil
}

func (r *lockedBackupRoot) publicationEntrySnapshot(name string) (*fileSnapshot, error) {
	file, err := r.openRegular(name)
	if err != nil {
		if errors.Is(err, syscall.ENOENT) {
			return nil, nil
		}
		return nil, ErrBackupJournal
	}
	defer file.Close()
	snapshot, err := snapshotFile(file)
	if err != nil {
		return nil, ErrBackupJournal
	}
	return &snapshot, nil
}

func (r *lockedBackupRoot) removePublicationEntry(name string, artifact privacy.ManagedBackupArtifact, allowReservation bool) error {
	snapshot, err := r.publicationEntrySnapshot(name)
	if err != nil || snapshot == nil {
		return err
	}
	exactArtifact := snapshot.size == artifact.Size && snapshot.sha256 == artifact.SHA256
	if !exactArtifact && !(allowReservation && snapshot.size == 0) {
		return ErrBackupJournal
	}
	if err := syscall.Unlinkat(r.fd, name); err != nil || syscall.Fsync(r.fd) != nil {
		return ErrBackupJournal
	}
	return nil
}

func publicationArtifactIndex(artifacts []privacy.ManagedBackupArtifact, target privacy.ManagedBackupArtifact) int {
	for index, artifact := range artifacts {
		if sameArtifact(artifact, target) {
			return index
		}
		if artifact.Path == target.Path {
			return -2
		}
	}
	return -1
}

func (r *lockedBackupRoot) recoverPublication(ctx context.Context, keys privacy.GenerationKeyRepository, inventory privacy.ManagedBackupInventoryRepository, at time.Time) error {
	journal, journalSnapshot, err := r.readPublicationJournal()
	if err != nil || journalSnapshot == nil {
		return err
	}
	manifestPayload, manifestSnapshot, err := r.readManifest()
	if err != nil {
		return ErrBackupJournal
	}
	var artifacts []privacy.ManagedBackupArtifact
	if manifestPayload != nil {
		artifacts, err = decodeManifest(manifestPayload)
		if err != nil {
			return ErrBackupJournal
		}
	}
	artifactIndex := publicationArtifactIndex(artifacts, journal.Artifact)
	if artifactIndex == -2 {
		return ErrBackupJournal
	}
	artifactSnapshot, err := r.publicationEntrySnapshot(journal.Artifact.Path)
	if err != nil {
		return err
	}
	tempSnapshot, err := r.publicationEntrySnapshot(journal.TempPath)
	if err != nil {
		return err
	}
	if artifactIndex < 0 {
		if artifactSnapshot != nil {
			if err := r.removePublicationEntry(journal.Artifact.Path, journal.Artifact, true); err != nil {
				return err
			}
		}
		if tempSnapshot != nil {
			if err := r.removePublicationEntry(journal.TempPath, journal.Artifact, false); err != nil {
				return err
			}
		}
		if err := syscall.Fsync(r.fd); err != nil {
			return ErrBackupJournal
		}
		return r.clearPublicationJournal(journalSnapshot)
	}
	if artifactSnapshot == nil || artifactSnapshot.size != journal.Artifact.Size || artifactSnapshot.sha256 != journal.Artifact.SHA256 {
		return ErrBackupJournal
	}
	recoverErr := keys.WithGenerationKey(ctx, journal.Artifact.LearnerGeneration, func(lease privacy.GenerationKeyLease) error {
		if lease.WrappedKeyID() != journal.Artifact.WrappedKeyID || lease.Generation() != journal.Artifact.LearnerGeneration {
			return privacy.ErrGenerationKeyUnavailable
		}
		if err := lease.RecordManagedBackup(ctx, journal.Artifact); err != nil {
			return err
		}
		if tempSnapshot != nil {
			if err := r.removePublicationEntry(journal.TempPath, journal.Artifact, false); err != nil {
				return err
			}
		}
		return r.clearPublicationJournal(journalSnapshot)
	})
	if recoverErr == nil {
		return nil
	}
	destroyed := errors.Is(recoverErr, privacy.ErrGenerationKeyDestroyed)
	if !destroyed && errors.Is(recoverErr, privacy.ErrGenerationKeyUnavailable) {
		destroyed = keys.VerifyGenerationKeyDestroyed(ctx, journal.Artifact.LearnerGeneration, journal.Artifact.WrappedKeyID) == nil
	}
	if !destroyed {
		return ErrBackupJournal
	}
	if err := inventory.DiscardManagedBackupPublication(ctx, journal.Artifact, at); err != nil {
		return ErrBackupJournal
	}
	retained := append([]privacy.ManagedBackupArtifact(nil), artifacts[:artifactIndex]...)
	retained = append(retained, artifacts[artifactIndex+1:]...)
	if _, err := r.publishManifest(retained, manifestSnapshot); err != nil {
		return ErrBackupJournal
	}
	if err := r.removePublicationEntry(journal.Artifact.Path, journal.Artifact, false); err != nil {
		return err
	}
	if tempSnapshot != nil {
		if err := r.removePublicationEntry(journal.TempPath, journal.Artifact, false); err != nil {
			return err
		}
	}
	return r.clearPublicationJournal(journalSnapshot)
}

func (r *lockedBackupRoot) validatedInventory() ([]privacy.ManagedBackupArtifact, map[string]fileSnapshot, *fileSnapshot, error) {
	if err := r.assertStable(); err != nil {
		return nil, nil, nil, err
	}
	entriesBefore, err := r.directoryNames()
	if err != nil {
		return nil, nil, nil, err
	}
	manifest, manifestSnapshot, err := r.readManifest()
	if err != nil {
		return nil, nil, nil, err
	}
	if manifest == nil {
		if len(entriesBefore) != 1 {
			return nil, nil, nil, ErrBackupInventory
		}
		if _, ok := entriesBefore[backupLockName]; !ok {
			return nil, nil, nil, ErrBackupInventory
		}
		if err := r.assertManifest(nil); err != nil {
			return nil, nil, nil, err
		}
		return []privacy.ManagedBackupArtifact{}, map[string]fileSnapshot{}, nil, nil
	}
	artifacts, err := decodeManifest(manifest)
	if err != nil {
		return nil, nil, nil, err
	}
	snapshots := make(map[string]fileSnapshot, len(artifacts))
	seen := make(map[string]struct{}, len(artifacts))
	for _, artifact := range artifacts {
		if _, duplicate := seen[artifact.Path]; duplicate {
			return nil, nil, nil, ErrBackupInventory
		}
		seen[artifact.Path] = struct{}{}
		file, err := r.openRegular(artifact.Path)
		if err != nil {
			return nil, nil, nil, ErrBackupInventory
		}
		snapshot, snapshotErr := snapshotFile(file)
		_ = file.Close()
		if snapshotErr != nil || snapshot.size != artifact.Size || snapshot.sha256 != artifact.SHA256 {
			return nil, nil, nil, ErrBackupInventory
		}
		snapshots[artifact.Path] = snapshot
	}
	entriesAfter, err := r.directoryNames()
	if err != nil {
		return nil, nil, nil, err
	}
	expected := map[string]struct{}{backupLockName: {}, backupInventoryName: {}}
	for path := range seen {
		expected[path] = struct{}{}
	}
	if !sameNameSet(entriesBefore, expected) || !sameNameSet(entriesAfter, expected) {
		return nil, nil, nil, ErrBackupInventory
	}
	if err := r.assertManifest(manifestSnapshot); err != nil {
		return nil, nil, nil, err
	}
	if err := r.assertStable(); err != nil {
		return nil, nil, nil, err
	}
	sortArtifacts(artifacts)
	return artifacts, snapshots, manifestSnapshot, nil
}

func (r *lockedBackupRoot) readManifest() ([]byte, *fileSnapshot, error) {
	file, err := r.openRegular(backupInventoryName)
	if err != nil {
		if errors.Is(err, syscall.ENOENT) {
			return nil, nil, nil
		}
		return nil, nil, ErrBackupInventory
	}
	defer file.Close()
	snapshot, err := snapshotFile(file)
	if err != nil || snapshot.size > backupManifestLimit {
		return nil, nil, ErrBackupInventory
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, nil, ErrBackupInventory
	}
	payload, err := io.ReadAll(io.LimitReader(file, backupManifestLimit+1))
	if err != nil || len(payload) > backupManifestLimit {
		return nil, nil, ErrBackupInventory
	}
	return payload, &snapshot, nil
}

func decodeManifest(payload []byte) ([]privacy.ManagedBackupArtifact, error) {
	if err := rejectDuplicateJSONKeys(payload); err != nil {
		return nil, ErrBackupInventory
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	var manifest manifestJSON
	if err := decoder.Decode(&manifest); err != nil {
		return nil, ErrBackupInventory
	}
	if err := requireJSONEOF(decoder); err != nil || manifest.Format == nil || *manifest.Format != privacy.ManagedBackupInventoryFormat || manifest.Artifacts == nil {
		return nil, ErrBackupInventory
	}
	artifacts := make([]privacy.ManagedBackupArtifact, 0, len(*manifest.Artifacts))
	for _, item := range *manifest.Artifacts {
		if item.Path == nil || item.CreatedAt == nil || item.Size == nil || item.SHA256 == nil || item.LearnerGeneration == nil || item.WrappedKeyID == nil {
			return nil, ErrBackupInventory
		}
		createdAt, err := time.Parse(time.RFC3339Nano, *item.CreatedAt)
		if err != nil || createdAt.Location() != time.UTC || createdAt.Format(time.RFC3339Nano) != *item.CreatedAt {
			return nil, ErrBackupInventory
		}
		artifact := privacy.ManagedBackupArtifact{
			Path: *item.Path, CreatedAt: createdAt, Size: *item.Size, SHA256: *item.SHA256,
			LearnerGeneration: *item.LearnerGeneration, WrappedKeyID: *item.WrappedKeyID,
		}
		if artifact.Validate() != nil {
			return nil, ErrBackupInventory
		}
		artifacts = append(artifacts, artifact)
	}
	return artifacts, nil
}

func rejectDuplicateJSONKeys(payload []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	if err := validateJSONValue(decoder); err != nil {
		return err
	}
	return requireJSONEOF(decoder)
}

func validateJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delimiter {
	case '{':
		seen := make(map[string]struct{})
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return ErrBackupInventory
			}
			if _, duplicate := seen[key]; duplicate {
				return ErrBackupInventory
			}
			seen[key] = struct{}{}
			if err := validateJSONValue(decoder); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim('}') {
			return ErrBackupInventory
		}
	case '[':
		for decoder.More() {
			if err := validateJSONValue(decoder); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim(']') {
			return ErrBackupInventory
		}
	default:
		return ErrBackupInventory
	}
	return nil
}

func requireJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return ErrBackupInventory
	}
	return nil
}

func (r *lockedBackupRoot) assertManifest(expected *fileSnapshot) error {
	file, err := r.openRegular(backupInventoryName)
	if expected == nil {
		if errors.Is(err, syscall.ENOENT) {
			return nil
		}
		if err == nil {
			_ = file.Close()
		}
		return ErrBackupInventory
	}
	if err != nil {
		return ErrBackupInventory
	}
	current, snapshotErr := snapshotFile(file)
	_ = file.Close()
	if snapshotErr != nil || current != *expected {
		return ErrBackupInventory
	}
	return nil
}

func (r *lockedBackupRoot) publishManifest(artifacts []privacy.ManagedBackupArtifact, expected *fileSnapshot) (bool, error) {
	payload, err := encodeManifest(artifacts)
	if err != nil {
		return false, ErrBackupInventory
	}
	tempName := "." + backupInventoryName + "." + uuid.NewString() + ".tmp"
	fd, err := syscall.Openat(r.fd, tempName, syscall.O_WRONLY|syscall.O_CREAT|syscall.O_EXCL|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return false, ErrBackupFilesystem
	}
	temporary := os.NewFile(uintptr(fd), tempName)
	closed := false
	defer func() {
		if !closed {
			_ = temporary.Close()
		}
		_ = syscall.Unlinkat(r.fd, tempName)
	}()
	if err := writeAll(temporary, payload); err != nil {
		return false, ErrBackupFilesystem
	}
	if err := temporary.Sync(); err != nil {
		return false, ErrBackupFilesystem
	}
	if err := temporary.Close(); err != nil {
		return false, ErrBackupFilesystem
	}
	closed = true
	if err := r.assertManifest(expected); err != nil {
		return false, err
	}
	if err := r.assertStable(); err != nil {
		return false, err
	}
	if err := syscall.Renameat(r.fd, tempName, r.fd, backupInventoryName); err != nil {
		return false, ErrBackupFilesystem
	}
	if err := syscall.Fsync(r.fd); err != nil {
		return true, ErrBackupFilesystem
	}
	return true, nil
}

func encodeManifest(artifacts []privacy.ManagedBackupArtifact) ([]byte, error) {
	type encodedArtifact struct {
		Path              string `json:"path"`
		CreatedAt         string `json:"created_at"`
		Size              int64  `json:"size"`
		SHA256            string `json:"sha256"`
		LearnerGeneration int64  `json:"learner_generation"`
		WrappedKeyID      string `json:"wrapped_key_id"`
	}
	manifest := struct {
		Format    string            `json:"format"`
		Artifacts []encodedArtifact `json:"artifacts"`
	}{Format: privacy.ManagedBackupInventoryFormat, Artifacts: make([]encodedArtifact, len(artifacts))}
	for index, artifact := range artifacts {
		if artifact.Validate() != nil {
			return nil, privacy.ErrManagedBackupInvalid
		}
		manifest.Artifacts[index] = encodedArtifact{
			Path: artifact.Path, CreatedAt: artifact.CreatedAt.Format(time.RFC3339Nano), Size: artifact.Size,
			SHA256: artifact.SHA256, LearnerGeneration: artifact.LearnerGeneration, WrappedKeyID: artifact.WrappedKeyID,
		}
	}
	payload, err := json.Marshal(manifest)
	if err != nil {
		return nil, err
	}
	return append(payload, '\n'), nil
}

func validFlatName(name string) bool {
	return name != "" && name != "." && name != ".." && !strings.ContainsAny(name, "/\\\x00")
}

func sameNameSet(left, right map[string]struct{}) bool {
	if len(left) != len(right) {
		return false
	}
	for name := range left {
		if _, ok := right[name]; !ok {
			return false
		}
	}
	return true
}

func managedBackupManifestDigest(artifacts []privacy.ManagedBackupArtifact) (string, error) {
	ordered := append([]privacy.ManagedBackupArtifact(nil), artifacts...)
	sortArtifacts(ordered)
	encodedArtifacts := make([]map[string]any, 0, len(ordered))
	for _, artifact := range ordered {
		if err := artifact.Validate(); err != nil {
			return "", ErrBackupMaintenance
		}
		encodedArtifacts = append(encodedArtifacts, map[string]any{
			"created_at":         artifact.CreatedAt.Format(time.RFC3339Nano),
			"learner_generation": artifact.LearnerGeneration,
			"path":               artifact.Path,
			"sha256":             artifact.SHA256,
			"size":               artifact.Size,
			"wrapped_key_id":     artifact.WrappedKeyID,
		})
	}
	payload, err := json.Marshal(map[string]any{
		"artifacts": encodedArtifacts,
		"format":    privacy.ManagedBackupInventoryFormat,
	})
	if err != nil {
		return "", ErrBackupMaintenance
	}
	payload = append(payload, '\n')
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:]), nil
}

func sortArtifacts(artifacts []privacy.ManagedBackupArtifact) {
	sort.Slice(artifacts, func(left, right int) bool {
		if artifacts[left].CreatedAt.Equal(artifacts[right].CreatedAt) {
			return artifacts[left].Path < artifacts[right].Path
		}
		return artifacts[left].CreatedAt.Before(artifacts[right].CreatedAt)
	})
}

func sameArtifact(left, right privacy.ManagedBackupArtifact) bool {
	return left.Path == right.Path && left.CreatedAt.Equal(right.CreatedAt) && left.Size == right.Size &&
		left.SHA256 == right.SHA256 && left.LearnerGeneration == right.LearnerGeneration && left.WrappedKeyID == right.WrappedKeyID
}
