package postgresstore

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/privacy"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const generationKeyWrapVersion byte = 1

type ManagedBackupRepository struct {
	pool      *pgxpool.Pool
	masterKey [32]byte
}

func NewManagedBackupRepository(pool *pgxpool.Pool, masterWrappingKey []byte) (*ManagedBackupRepository, error) {
	if pool == nil || len(masterWrappingKey) != 32 {
		return nil, errors.New("managed backup repository requires a database and 32-byte master wrapping key")
	}
	repository := &ManagedBackupRepository{pool: pool}
	copy(repository.masterKey[:], masterWrappingKey)
	return repository, nil
}

func (r *ManagedBackupRepository) WithGenerationKey(ctx context.Context, generation int64, callback func(privacy.GenerationKeyLease) error) error {
	if generation < 1 || callback == nil {
		return privacy.ErrGenerationKeyUnavailable
	}
	return r.withBackupProducerFence(ctx, generation, func(connection *pgxpool.Conn) error {
		candidate := make([]byte, 32)
		if _, err := rand.Read(candidate); err != nil {
			return errors.New("managed backup generation key could not be created")
		}
		defer erase(candidate)
		keyID := uuid.NewString()
		wrapped, err := r.wrap(candidate, keyID, generation)
		if err != nil {
			return err
		}
		digest := sha256.Sum256(candidate)
		if _, err := connection.Exec(ctx, `
			INSERT INTO memory_generation_keys(id,learner_generation,wrapped_key,key_digest,created_at)
			VALUES($1,$2,$3,$4,clock_timestamp())
			ON CONFLICT (learner_generation) DO NOTHING`, keyID, generation, wrapped, digest[:]); err != nil {
			return fmt.Errorf("ensure managed backup generation key: %w", err)
		}
		return r.withStoredKeyUsing(ctx, connection, generation, "", true, callback)
	})
}

func (r *ManagedBackupRepository) withBackupProducerFence(ctx context.Context, generation int64, callback func(*pgxpool.Conn) error) error {
	connection, err := r.pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("lease managed backup producer generation: %w", err)
	}
	advisoryLocked := false
	defer func() {
		if advisoryLocked {
			releaseBackupProducerAdvisoryLock(connection)
			return
		}
		connection.Release()
	}()
	if _, err := connection.Exec(ctx, `SELECT pg_advisory_lock(hashtextextended('privacy-owner:memory',0))`); err != nil {
		return fmt.Errorf("lease managed backup producer generation: %w", err)
	}
	advisoryLocked = true
	var currentGeneration int64
	var readOpen, writeOpen bool
	if err := connection.QueryRow(ctx, `
		SELECT learner_generation,read_open,write_open
		FROM privacy_owner_generation_gates
		WHERE owner_kind='memory'`).Scan(&currentGeneration, &readOpen, &writeOpen); err != nil {
		return fmt.Errorf("verify managed backup producer generation: %w", err)
	}
	if currentGeneration != generation || !readOpen || !writeOpen {
		return privacy.ErrGenerationKeyUnavailable
	}
	return callback(connection)
}

func releaseBackupProducerAdvisoryLock(connection *pgxpool.Conn) {
	unlockContext, cancelUnlock := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelUnlock()
	var unlocked bool
	if err := connection.QueryRow(unlockContext, `SELECT pg_advisory_unlock(hashtextextended('privacy-owner:memory',0))`).Scan(&unlocked); err == nil && unlocked {
		connection.Release()
		return
	}
	hijacked := connection.Hijack()
	closeContext, cancelClose := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelClose()
	_ = hijacked.Close(closeContext)
}

func (r *ManagedBackupRepository) WithExistingGenerationKey(ctx context.Context, generation int64, keyID string, callback func(privacy.GenerationKeyLease) error) error {
	if generation < 1 || uuid.Validate(keyID) != nil || callback == nil {
		return privacy.ErrGenerationKeyUnavailable
	}
	parsed, err := uuid.Parse(keyID)
	if err != nil || parsed.String() != keyID {
		return privacy.ErrGenerationKeyUnavailable
	}
	return r.withStoredKeyUsing(ctx, r.pool, generation, keyID, false, callback)
}

type transactionBeginner interface {
	BeginTx(context.Context, pgx.TxOptions) (pgx.Tx, error)
}

func (r *ManagedBackupRepository) withStoredKeyUsing(ctx context.Context, database transactionBeginner, generation int64, expectedID string, recordRequired bool, callback func(privacy.GenerationKeyLease) error) error {
	tx, err := database.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return fmt.Errorf("lease managed backup generation key: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	var keyID string
	var wrapped, expectedDigest []byte
	var destroyedAt *time.Time
	query := `
		SELECT id,wrapped_key,key_digest,destroyed_at
		FROM memory_generation_keys
		WHERE learner_generation=$1
		FOR SHARE`
	arguments := []any{generation}
	if expectedID != "" {
		query = `
			SELECT id,wrapped_key,key_digest,destroyed_at
			FROM memory_generation_keys
			WHERE learner_generation=$1 AND id=$2
			FOR SHARE`
		arguments = append(arguments, expectedID)
	}
	if err := tx.QueryRow(ctx, query, arguments...).Scan(&keyID, &wrapped, &expectedDigest, &destroyedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return privacy.ErrGenerationKeyUnavailable
		}
		return fmt.Errorf("lease managed backup generation key: %w", err)
	}
	if destroyedAt != nil || len(wrapped) == 0 {
		return privacy.ErrGenerationKeyDestroyed
	}
	plaintext, err := r.unwrap(wrapped, keyID, generation)
	if err != nil {
		return err
	}
	lease := &generationKeyLease{
		id: keyID, generation: generation, key: plaintext, active: true,
		tx: tx, recordAllowed: recordRequired,
	}
	defer lease.invalidate()
	digest := sha256.Sum256(plaintext)
	if len(expectedDigest) != sha256.Size || subtle.ConstantTimeCompare(digest[:], expectedDigest) != 1 {
		return privacy.ErrManagedBackupIntegrity
	}
	callbackErr := callback(lease)
	lease.invalidate()
	if callbackErr != nil {
		return callbackErr
	}
	if recordRequired {
		if !lease.recordCommitted() {
			return privacy.ErrManagedBackupConflict
		}
		return nil
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("release managed backup generation key: %w", err)
	}
	return nil
}

func (r *ManagedBackupRepository) VerifyGenerationKeyDestroyed(ctx context.Context, generation int64, keyID string) error {
	if generation < 1 || uuid.Validate(keyID) != nil {
		return privacy.ErrGenerationKeyUnavailable
	}
	var wrapped []byte
	var destroyedAt *time.Time
	var evidence []byte
	if err := r.pool.QueryRow(ctx, `
		SELECT wrapped_key,destroyed_at,destruction_evidence_digest
		FROM memory_generation_keys
		WHERE learner_generation=$1 AND id=$2`, generation, keyID).Scan(&wrapped, &destroyedAt, &evidence); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return privacy.ErrGenerationKeyUnavailable
		}
		return fmt.Errorf("verify managed backup generation key destruction: %w", err)
	}
	if destroyedAt == nil || len(wrapped) != 0 || len(evidence) != sha256.Size {
		return privacy.ErrGenerationKeyUnavailable
	}
	return nil
}

func (r *ManagedBackupRepository) wrap(plaintext []byte, keyID string, generation int64) ([]byte, error) {
	block, err := aes.NewCipher(r.masterKey[:])
	if err != nil {
		return nil, errors.New("managed backup generation key could not be wrapped")
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, errors.New("managed backup generation key could not be wrapped")
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, errors.New("managed backup generation key could not be wrapped")
	}
	wrapped := make([]byte, 1, 1+len(nonce)+len(plaintext)+gcm.Overhead())
	wrapped[0] = generationKeyWrapVersion
	wrapped = append(wrapped, nonce...)
	wrapped = gcm.Seal(wrapped, nonce, plaintext, generationKeyAAD(keyID, generation))
	return wrapped, nil
}

func (r *ManagedBackupRepository) unwrap(wrapped []byte, keyID string, generation int64) ([]byte, error) {
	block, err := aes.NewCipher(r.masterKey[:])
	if err != nil {
		return nil, privacy.ErrManagedBackupIntegrity
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil || len(wrapped) < 1+gcm.NonceSize()+gcm.Overhead() || wrapped[0] != generationKeyWrapVersion {
		return nil, privacy.ErrManagedBackupIntegrity
	}
	nonce := wrapped[1 : 1+gcm.NonceSize()]
	plaintext, err := gcm.Open(nil, nonce, wrapped[1+gcm.NonceSize():], generationKeyAAD(keyID, generation))
	if err != nil || len(plaintext) != 32 {
		erase(plaintext)
		return nil, privacy.ErrManagedBackupIntegrity
	}
	return plaintext, nil
}

func generationKeyAAD(keyID string, generation int64) []byte {
	result := make([]byte, 0, len(keyID)+48)
	result = append(result, "edu-agent-generation-key-wrap-v1\x00"...)
	result = append(result, keyID...)
	result = append(result, 0)
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], uint64(generation))
	return append(result, encoded[:]...)
}

type generationKeyLease struct {
	mu            sync.Mutex
	id            string
	generation    int64
	key           []byte
	active        bool
	tx            pgx.Tx
	recordAllowed bool
	recording     bool
	recorded      bool
}

func (l *generationKeyLease) WrappedKeyID() string { return l.id }
func (l *generationKeyLease) Generation() int64    { return l.generation }
func (l *generationKeyLease) Use(callback func([]byte) error) error {
	if callback == nil {
		return privacy.ErrGenerationKeyUnavailable
	}
	l.mu.Lock()
	if !l.active || len(l.key) != 32 {
		l.mu.Unlock()
		return privacy.ErrGenerationKeyUnavailable
	}
	key := l.key
	l.mu.Unlock()
	return callback(key)
}
func (l *generationKeyLease) RecordManagedBackup(ctx context.Context, artifact privacy.ManagedBackupArtifact) error {
	l.mu.Lock()
	if !l.active || !l.recordAllowed || l.recording || l.recorded || l.tx == nil ||
		artifact.LearnerGeneration != l.generation || artifact.WrappedKeyID != l.id {
		l.mu.Unlock()
		return privacy.ErrManagedBackupConflict
	}
	l.recording = true
	tx := l.tx
	l.mu.Unlock()
	if err := recordManagedBackupTx(ctx, tx, artifact); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit managed backup metadata: %w", err)
	}
	l.mu.Lock()
	l.recorded = true
	l.tx = nil
	l.mu.Unlock()
	return nil
}
func (l *generationKeyLease) recordCommitted() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.recorded
}
func (l *generationKeyLease) invalidate() {
	l.mu.Lock()
	erase(l.key)
	l.key = nil
	l.active = false
	l.mu.Unlock()
}

func erase(value []byte) {
	for index := range value {
		value[index] = 0
	}
	runtime.KeepAlive(value)
}

func (r *ManagedBackupRepository) RecordManagedBackup(ctx context.Context, artifact privacy.ManagedBackupArtifact) error {
	if err := artifact.Validate(); err != nil {
		return err
	}
	return r.withBackupProducerFence(ctx, artifact.LearnerGeneration, func(connection *pgxpool.Conn) error {
		tx, err := connection.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
		if err != nil {
			return fmt.Errorf("record managed backup metadata: %w", err)
		}
		defer func() { _ = tx.Rollback(context.Background()) }()
		if err := recordManagedBackupTx(ctx, tx, artifact); err != nil {
			return err
		}
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("record managed backup metadata: %w", err)
		}
		return nil
	})
}

func recordManagedBackupTx(ctx context.Context, db privacy.DBTX, artifact privacy.ManagedBackupArtifact) error {
	if err := artifact.Validate(); err != nil {
		return err
	}
	var destroyedAt *time.Time
	var wrappedPresent bool
	var gateGeneration int64
	var readOpen, writeOpen bool
	if err := db.QueryRow(ctx, `
		SELECT keys.destroyed_at,keys.wrapped_key IS NOT NULL,
		       gates.learner_generation,gates.read_open,gates.write_open
		FROM memory_generation_keys keys
		JOIN privacy_owner_generation_gates gates ON gates.owner_kind='memory'
		WHERE keys.id=$1 AND keys.learner_generation=$2
		FOR SHARE OF keys,gates`, artifact.WrappedKeyID, artifact.LearnerGeneration).Scan(
		&destroyedAt, &wrappedPresent, &gateGeneration, &readOpen, &writeOpen); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return privacy.ErrGenerationKeyUnavailable
		}
		return fmt.Errorf("verify live managed backup generation: %w", err)
	}
	if destroyedAt != nil || !wrappedPresent {
		return privacy.ErrGenerationKeyDestroyed
	}
	if gateGeneration != artifact.LearnerGeneration || !readOpen || !writeOpen {
		return privacy.ErrGenerationKeyUnavailable
	}
	digest, _ := hex.DecodeString(artifact.SHA256)
	tag, err := db.Exec(ctx, `
		INSERT INTO memory_managed_backup_inventory(
			id,relative_path,created_at,size_bytes,artifact_hash,learner_generation,wrapped_key_id)
		VALUES($1,$2,$3,$4,$5,$6,$7)
		ON CONFLICT (relative_path) DO NOTHING`,
		uuid.NewString(), artifact.Path, artifact.CreatedAt, artifact.Size, digest,
		artifact.LearnerGeneration, artifact.WrappedKeyID)
	if err != nil {
		return fmt.Errorf("record managed backup metadata: %w", err)
	}
	if tag.RowsAffected() == 0 {
		var existing privacy.ManagedBackupArtifact
		var existingDigest []byte
		var prunedAt *time.Time
		if err := db.QueryRow(ctx, `
			SELECT relative_path,created_at,size_bytes,artifact_hash,learner_generation,wrapped_key_id,pruned_at
			FROM memory_managed_backup_inventory
			WHERE relative_path=$1`, artifact.Path).Scan(
			&existing.Path, &existing.CreatedAt, &existing.Size, &existingDigest,
			&existing.LearnerGeneration, &existing.WrappedKeyID, &prunedAt); err != nil {
			return fmt.Errorf("verify managed backup metadata: %w", err)
		}
		existing.CreatedAt = existing.CreatedAt.UTC()
		existing.SHA256 = hex.EncodeToString(existingDigest)
		if prunedAt != nil || !sameManagedBackup(existing, artifact) {
			return privacy.ErrManagedBackupConflict
		}
	}
	return nil
}

func (r *ManagedBackupRepository) DiscardManagedBackupPublication(ctx context.Context, artifact privacy.ManagedBackupArtifact, at time.Time) error {
	if err := artifact.Validate(); err != nil || at.IsZero() || at.Location() != time.UTC {
		return privacy.ErrManagedBackupInvalid
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return fmt.Errorf("discard managed backup publication: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	var existing privacy.ManagedBackupArtifact
	var existingDigest []byte
	var prunedAt *time.Time
	err = tx.QueryRow(ctx, `
		SELECT relative_path,created_at,size_bytes,artifact_hash,learner_generation,wrapped_key_id,pruned_at
		FROM memory_managed_backup_inventory
		WHERE relative_path=$1
		FOR UPDATE`, artifact.Path).Scan(
		&existing.Path, &existing.CreatedAt, &existing.Size, &existingDigest,
		&existing.LearnerGeneration, &existing.WrappedKeyID, &prunedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("discard managed backup publication: %w", err)
	}
	existing.CreatedAt = existing.CreatedAt.UTC()
	existing.SHA256 = hex.EncodeToString(existingDigest)
	if !sameManagedBackup(existing, artifact) {
		return privacy.ErrManagedBackupConflict
	}
	if prunedAt == nil {
		if _, err := tx.Exec(ctx, `UPDATE memory_managed_backup_inventory SET pruned_at=$2 WHERE relative_path=$1`, artifact.Path, at); err != nil {
			return fmt.Errorf("discard managed backup publication: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("discard managed backup publication: %w", err)
	}
	return nil
}

func (r *ManagedBackupRepository) ManagedBackupInventory(ctx context.Context) ([]privacy.ManagedBackupArtifact, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT relative_path,created_at,size_bytes,artifact_hash,learner_generation,wrapped_key_id
		FROM memory_managed_backup_inventory
		WHERE pruned_at IS NULL
		ORDER BY created_at,relative_path`)
	if err != nil {
		return nil, fmt.Errorf("read managed backup metadata: %w", err)
	}
	defer rows.Close()
	var inventory []privacy.ManagedBackupArtifact
	for rows.Next() {
		var artifact privacy.ManagedBackupArtifact
		var digest []byte
		if err := rows.Scan(&artifact.Path, &artifact.CreatedAt, &artifact.Size, &digest, &artifact.LearnerGeneration, &artifact.WrappedKeyID); err != nil {
			return nil, fmt.Errorf("read managed backup metadata: %w", err)
		}
		artifact.CreatedAt = artifact.CreatedAt.UTC()
		artifact.SHA256 = hex.EncodeToString(digest)
		if err := artifact.Validate(); err != nil {
			return nil, privacy.ErrManagedBackupIntegrity
		}
		inventory = append(inventory, artifact)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read managed backup metadata: %w", err)
	}
	return inventory, nil
}

func (r *ManagedBackupRepository) MarkManagedBackupsPruned(ctx context.Context, paths []string, at time.Time) error {
	if at.IsZero() || at.Location() != time.UTC {
		return privacy.ErrManagedBackupInvalid
	}
	unique := make(map[string]struct{}, len(paths))
	for _, path := range paths {
		if path == "" || path == "." || path == ".." || path == "managed-inventory.json" ||
			path == ".edu-agent-backup.lock" || strings.ContainsAny(path, "/\\\x00") {
			return privacy.ErrManagedBackupInvalid
		}
		unique[path] = struct{}{}
	}
	if len(unique) == 0 {
		return nil
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return fmt.Errorf("mark managed backups pruned: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	for path := range unique {
		var prunedAt *time.Time
		if err := tx.QueryRow(ctx, `SELECT pruned_at FROM memory_managed_backup_inventory WHERE relative_path=$1 FOR UPDATE`, path).Scan(&prunedAt); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return privacy.ErrManagedBackupConflict
			}
			return fmt.Errorf("mark managed backups pruned: %w", err)
		}
		if prunedAt == nil {
			if _, err := tx.Exec(ctx, `UPDATE memory_managed_backup_inventory SET pruned_at=$2 WHERE relative_path=$1`, path, at); err != nil {
				return fmt.Errorf("mark managed backups pruned: %w", err)
			}
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("mark managed backups pruned: %w", err)
	}
	return nil
}

func sameManagedBackup(left, right privacy.ManagedBackupArtifact) bool {
	return left.Path == right.Path && left.CreatedAt.Equal(right.CreatedAt) && left.Size == right.Size &&
		left.SHA256 == right.SHA256 && left.LearnerGeneration == right.LearnerGeneration &&
		left.WrappedKeyID == right.WrappedKeyID
}

var _ privacy.ManagedBackupRepository = (*ManagedBackupRepository)(nil)
