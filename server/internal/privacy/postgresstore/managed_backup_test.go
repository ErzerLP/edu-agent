package postgresstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/privacy"
	"github.com/edu-agent/edu-agent/server/migrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestManagedBackupRepositoryConcurrentEnsureInventoryAndDestroyedLease(t *testing.T) {
	pool := managedBackupTestPool(t)
	repository, err := NewManagedBackupRepository(pool, bytes.Repeat([]byte{0x4d}, 32))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	createdAt := time.Now().UTC().Truncate(time.Microsecond)
	artifactDigest := sha256.Sum256([]byte("encrypted fixture"))
	type observation struct {
		id     string
		digest [sha256.Size]byte
		err    error
	}
	observations := make(chan observation, 12)
	var wait sync.WaitGroup
	for index := 0; index < cap(observations); index++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			var value observation
			value.err = repository.WithGenerationKey(ctx, 1, func(lease privacy.GenerationKeyLease) error {
				value.id = lease.WrappedKeyID()
				if err := lease.Use(func(key []byte) error {
					value.digest = sha256.Sum256(key)
					return nil
				}); err != nil {
					return err
				}
				return lease.RecordManagedBackup(ctx, privacy.ManagedBackupArtifact{
					Path: "managed-g00000000000000000001-fixture.backup.enc", CreatedAt: createdAt,
					Size: 37, SHA256: hex.EncodeToString(artifactDigest[:]), LearnerGeneration: 1, WrappedKeyID: lease.WrappedKeyID(),
				})
			})
			observations <- value
		}()
	}
	wait.Wait()
	close(observations)
	var expected observation
	for value := range observations {
		if value.err != nil {
			t.Fatal(value.err)
		}
		if expected.id == "" {
			expected = value
			continue
		}
		if value.id != expected.id || value.digest != expected.digest {
			t.Fatalf("concurrent ensure returned distinct keys: first=%s next=%s", expected.id, value.id)
		}
	}
	if uuid.Validate(expected.id) != nil {
		t.Fatalf("invalid wrapped key id %q", expected.id)
	}

	artifact := privacy.ManagedBackupArtifact{
		Path: "managed-g00000000000000000001-fixture.backup.enc", CreatedAt: createdAt,
		Size: 37, SHA256: hex.EncodeToString(artifactDigest[:]), LearnerGeneration: 1, WrappedKeyID: expected.id,
	}
	if err := repository.RecordManagedBackup(ctx, artifact); err != nil {
		t.Fatalf("idempotent inventory record: %v", err)
	}
	inventory, err := repository.ManagedBackupInventory(ctx)
	if err != nil || len(inventory) != 1 || inventory[0] != artifact {
		t.Fatalf("inventory=%+v err=%v", inventory, err)
	}

	var escaped privacy.GenerationKeyLease
	if err := repository.WithExistingGenerationKey(ctx, 1, expected.id, func(lease privacy.GenerationKeyLease) error {
		escaped = lease
		return lease.Use(func(key []byte) error {
			if sha256.Sum256(key) != expected.digest {
				t.Fatal("unwrapped generation key changed")
			}
			return nil
		})
	}); err != nil {
		t.Fatal(err)
	}
	if err := escaped.Use(func([]byte) error { return nil }); !errors.Is(err, privacy.ErrGenerationKeyUnavailable) {
		t.Fatalf("lease remained usable after callback: %v", err)
	}
	barrierAcquired := make(chan error, 1)
	barrierStarted := make(chan struct{})
	linearizedArtifact := artifact
	linearizedArtifact.Path = "managed-g00000000000000000001-linearized.backup.enc"
	if err := repository.WithGenerationKey(ctx, 1, func(lease privacy.GenerationKeyLease) error {
		linearizedArtifact.WrappedKeyID = lease.WrappedKeyID()
		if err := lease.RecordManagedBackup(ctx, linearizedArtifact); err != nil {
			return err
		}
		go func() {
			close(barrierStarted)
			connection, err := pool.Acquire(ctx)
			if err == nil {
				_, err = connection.Exec(ctx, `SELECT pg_advisory_lock(hashtextextended('privacy-owner:memory',0))`)
			}
			barrierAcquired <- err
			if err == nil {
				_, _ = connection.Exec(context.Background(), `SELECT pg_advisory_unlock(hashtextextended('privacy-owner:memory',0))`)
			}
			if connection != nil {
				connection.Release()
			}
		}()
		<-barrierStarted
		select {
		case err := <-barrierAcquired:
			return fmt.Errorf("barrier crossed producer fence before callback returned: %w", err)
		case <-time.After(25 * time.Millisecond):
			return nil
		}
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-barrierAcquired:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("barrier did not acquire advisory lock after producer returned")
	}
	if _, err := pool.Exec(ctx, `UPDATE memory_generation_keys SET key_digest=decode(repeat('ee',32),'hex') WHERE learner_generation=1`); err == nil {
		t.Fatal("live generation key digest mutation was accepted")
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO memory_generation_keys(id,learner_generation,wrapped_key,key_digest,created_at)
		VALUES($1,2,decode(repeat('aa',61),'hex'),decode(repeat('bb',32),'hex'),clock_timestamp())`, uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO memory_managed_backup_inventory(id,relative_path,created_at,size_bytes,artifact_hash,learner_generation,wrapped_key_id)
		VALUES($1,'mismatched-key-generation.backup.enc',clock_timestamp(),1,decode(repeat('cc',32),'hex'),2,$2)`, uuid.NewString(), expected.id); err == nil {
		t.Fatal("inventory accepted a wrapped key from another generation")
	}

	destructionEvidence := sha256.Sum256([]byte("test destruction evidence"))
	if _, err := pool.Exec(ctx, `
		UPDATE memory_generation_keys
		SET wrapped_key=NULL,destroyed_at=clock_timestamp(),destruction_evidence_digest=$2
		WHERE learner_generation=$1`, 1, destructionEvidence[:]); err != nil {
		t.Fatal(err)
	}
	called := false
	err = repository.WithExistingGenerationKey(ctx, 1, expected.id, func(privacy.GenerationKeyLease) error {
		called = true
		return nil
	})
	if !errors.Is(err, privacy.ErrGenerationKeyDestroyed) || called {
		t.Fatalf("destroyed key lease err=%v called=%v", err, called)
	}
	if err := repository.VerifyGenerationKeyDestroyed(ctx, 1, expected.id); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE memory_generation_keys
		SET wrapped_key=decode(repeat('aa',61),'hex'),destroyed_at=NULL,destruction_evidence_digest=NULL
		WHERE learner_generation=1`); err == nil {
		t.Fatal("destroyed generation key was resurrected")
	}
	if _, err := pool.Exec(ctx, `DELETE FROM memory_generation_keys WHERE learner_generation=1`); err == nil {
		t.Fatal("generation key deletion was accepted")
	}
	if err := repository.RecordManagedBackup(ctx, artifact); !errors.Is(err, privacy.ErrGenerationKeyDestroyed) {
		t.Fatalf("destroyed key accepted inventory record: %v", err)
	}
	if err := repository.WithGenerationKey(ctx, 1, func(lease privacy.GenerationKeyLease) error {
		return lease.RecordManagedBackup(ctx, artifact)
	}); !errors.Is(err, privacy.ErrGenerationKeyDestroyed) {
		t.Fatalf("destroyed generation was recreated: %v", err)
	}
}

func TestManagedBackupBarrierVerificationRejectsLiveOldKey(t *testing.T) {
	pool := managedBackupTestPool(t)
	repository, err := NewManagedBackupRepository(pool, bytes.Repeat([]byte{0x6d}, 32))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	deviceID, erasureID, keyID := uuid.NewString(), uuid.NewString(), uuid.NewString()
	now := time.Now().UTC().Truncate(time.Microsecond)
	if _, err := pool.Exec(ctx, `INSERT INTO devices(id,display_name,created_at) VALUES($1,'backup verifier',clock_timestamp())`, deviceID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO privacy_erasures(
			id,device_id,operation_id,request_hash,reason_code,actor_device_id,requested_at,
			target_learner_generation,managed_backup_scheduled_unrecoverable_after,
			managed_backup_verified_unrecoverable_at)
		VALUES($1,$2,$3,decode(repeat('ab',32),'hex'),'learner_request',$2,$4,2,$5,$6)`,
		erasureID, deviceID, uuid.NewString(), now.Add(-time.Minute), now.Add(time.Hour), now); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO memory_generation_keys(id,learner_generation,wrapped_key,key_digest,created_at)
		VALUES($1,1,decode(repeat('aa',48),'hex'),decode(repeat('bb',32),'hex'),$2)`, keyID, now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.VerifyManagedBackupBarrier(ctx, erasureID, 2); !errors.Is(err, privacy.ErrManagedBackupLiveOldKey) {
		t.Fatalf("live old key verification err=%v", err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE memory_generation_keys
		SET wrapped_key=NULL,destroyed_at=$2,destruction_evidence_digest=decode(repeat('cc',32),'hex')
		WHERE id=$1`, keyID, now); err != nil {
		t.Fatal(err)
	}
	state, err := repository.VerifyManagedBackupBarrier(ctx, erasureID, 2)
	if err != nil || state.DestroyedOldKeyCount != 1 || !state.VerifiedUnrecoverableAt.Equal(now) {
		t.Fatalf("barrier state=%+v err=%v", state, err)
	}
}

func managedBackupTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	schema := "managed_backup_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	identifier := pgx.Identifier{schema}.Sanitize()
	if _, err := admin.Exec(ctx, `CREATE SCHEMA `+identifier); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		_, _ = admin.Exec(ctx, `DROP SCHEMA `+identifier+` CASCADE`)
		admin.Close()
		t.Fatal(err)
	}
	config.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		_, _ = admin.Exec(ctx, `DROP SCHEMA `+identifier+` CASCADE`)
		admin.Close()
		t.Fatal(err)
	}
	if err := migrations.Run(ctx, pool); err != nil {
		pool.Close()
		_, _ = admin.Exec(ctx, `DROP SCHEMA `+identifier+` CASCADE`)
		admin.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Close()
		_, _ = admin.Exec(context.Background(), `DROP SCHEMA `+identifier+` CASCADE`)
		admin.Close()
	})
	return pool
}
