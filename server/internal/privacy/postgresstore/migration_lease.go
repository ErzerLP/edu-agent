package postgresstore

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/privacy"
	"github.com/jackc/pgx/v5"
)

const privacyMigrationAdvisoryLock = "privacy-erasure-migration-singleton"

func (s *Store) AcquireMigrationLease(ctx context.Context, request privacy.MigrationLeaseRequest) (privacy.MigrationLease, error) {
	if err := request.Validate(); err != nil {
		return privacy.MigrationLease{}, err
	}
	identity, _ := hex.DecodeString(request.BackupIdentity)
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return privacy.MigrationLease{}, fmt.Errorf("begin privacy migration lease acquire: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, privacyMigrationAdvisoryLock); err != nil {
		return privacy.MigrationLease{}, fmt.Errorf("lock privacy migration singleton: %w", err)
	}
	var activeErasure bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM privacy_erasure_heads WHERE status<>'verified')`).Scan(&activeErasure); err != nil {
		return privacy.MigrationLease{}, fmt.Errorf("check active privacy erasure: %w", err)
	}
	if activeErasure {
		return privacy.MigrationLease{}, &privacy.Error{Code: privacy.CodeMigrationLeaseConflict, Reason: "privacy_erasure_active"}
	}
	var operationID *string
	var storedIdentity []byte
	var acquiredAt, releasedAt *time.Time
	if err := tx.QueryRow(ctx, `
		SELECT operation_id::text,backup_identity,acquired_at,released_at
		FROM privacy_migration_lease WHERE singleton_id=1 FOR UPDATE`).Scan(
		&operationID, &storedIdentity, &acquiredAt, &releasedAt,
	); err != nil {
		return privacy.MigrationLease{}, fmt.Errorf("lock privacy migration lease: %w", err)
	}
	if operationID != nil && releasedAt == nil {
		if *operationID != request.OperationID || !bytes.Equal(storedIdentity, identity) || acquiredAt == nil {
			return privacy.MigrationLease{}, &privacy.Error{Code: privacy.CodeMigrationLeaseConflict, Reason: "migration_lease_active"}
		}
		lease := privacy.MigrationLease{OperationID: request.OperationID, AcquiredAt: acquiredAt.UTC(), Replayed: true}
		if err := tx.Commit(ctx); err != nil {
			return privacy.MigrationLease{}, fmt.Errorf("replay privacy migration lease: %w", err)
		}
		return lease, nil
	}
	var now time.Time
	if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		return privacy.MigrationLease{}, fmt.Errorf("timestamp privacy migration lease: %w", err)
	}
	tag, err := tx.Exec(ctx, `
		UPDATE privacy_migration_lease
		SET operation_id=$1,backup_identity=$2,acquired_at=$3,released_at=NULL
		WHERE singleton_id=1`, request.OperationID, identity, now.UTC())
	if err != nil {
		return privacy.MigrationLease{}, fmt.Errorf("acquire privacy migration lease: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return privacy.MigrationLease{}, &privacy.Error{Code: privacy.CodeMigrationLeaseConflict, Reason: "migration_lease_missing"}
	}
	if err := tx.Commit(ctx); err != nil {
		return privacy.MigrationLease{}, fmt.Errorf("commit privacy migration lease acquire: %w", err)
	}
	return privacy.MigrationLease{OperationID: request.OperationID, AcquiredAt: now.UTC()}, nil
}

func (s *Store) ReleaseMigrationLease(ctx context.Context, request privacy.MigrationLeaseRequest) error {
	if err := request.Validate(); err != nil {
		return err
	}
	identity, _ := hex.DecodeString(request.BackupIdentity)
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return fmt.Errorf("begin privacy migration lease release: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, privacyMigrationAdvisoryLock); err != nil {
		return fmt.Errorf("lock privacy migration singleton: %w", err)
	}
	var operationID *string
	var storedIdentity []byte
	var releasedAt *time.Time
	if err := tx.QueryRow(ctx, `
		SELECT operation_id::text,backup_identity,released_at
		FROM privacy_migration_lease WHERE singleton_id=1 FOR UPDATE`).Scan(
		&operationID, &storedIdentity, &releasedAt,
	); err != nil {
		return fmt.Errorf("lock privacy migration lease: %w", err)
	}
	if operationID == nil || *operationID != request.OperationID || !bytes.Equal(storedIdentity, identity) {
		return &privacy.Error{Code: privacy.CodeMigrationLeaseConflict, Reason: "migration_lease_identity_mismatch"}
	}
	if releasedAt != nil {
		return tx.Commit(ctx)
	}
	var now time.Time
	if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		return fmt.Errorf("timestamp privacy migration lease release: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE privacy_migration_lease SET released_at=$1 WHERE singleton_id=1`, now.UTC()); err != nil {
		return fmt.Errorf("release privacy migration lease: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit privacy migration lease release: %w", err)
	}
	return nil
}

func activeMigrationLeaseTx(ctx context.Context, tx pgx.Tx) (bool, error) {
	var active bool
	err := tx.QueryRow(ctx, `SELECT operation_id IS NOT NULL AND released_at IS NULL FROM privacy_migration_lease WHERE singleton_id=1 FOR UPDATE`).Scan(&active)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, &privacy.Error{Code: privacy.CodeMigrationLeaseConflict, Reason: "migration_lease_missing", Cause: err}
	}
	return active, err
}
