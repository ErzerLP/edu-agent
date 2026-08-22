package postgresstore

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/privacy"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type GrantStore struct {
	pool *pgxpool.Pool
}

func NewGrantStore(pool *pgxpool.Pool) *GrantStore {
	return &GrantStore{pool: pool}
}

// ReplaceErasureGrant serializes on the device row and invalidates any still-active
// grant before inserting the replacement. Expired grants remain as bounded audit rows.
func (s *GrantStore) ReplaceErasureGrant(ctx context.Context, request privacy.ErasureGrantCreate) (privacy.ErasureGrant, error) {
	if s == nil || s.pool == nil || request.Validate() != nil {
		return privacy.ErasureGrant{}, privacy.ErrErasureGrantStoreUnavailable
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return privacy.ErasureGrant{}, privacy.ErrErasureGrantStoreUnavailable
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	var revokedAt *time.Time
	if err := tx.QueryRow(ctx, `SELECT revoked_at FROM devices WHERE id=$1 FOR UPDATE`, request.DeviceID).Scan(&revokedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return privacy.ErasureGrant{}, privacy.ErrErasureGrantDeviceUnavailable
		}
		return privacy.ErasureGrant{}, privacy.ErrErasureGrantStoreUnavailable
	}
	if revokedAt != nil {
		return privacy.ErasureGrant{}, privacy.ErrErasureGrantDeviceUnavailable
	}
	var now time.Time
	if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		return privacy.ErasureGrant{}, privacy.ErrErasureGrantStoreUnavailable
	}
	now = now.UTC()
	if _, err := tx.Exec(ctx, `
		UPDATE privacy_erasure_grants
		SET consumed_at=$2
		WHERE device_id=$1 AND consumed_at IS NULL AND expires_at>$2 AND attempts<max_attempts`,
		request.DeviceID, now); err != nil {
		return privacy.ErasureGrant{}, privacy.ErrErasureGrantStoreUnavailable
	}
	expiresAt := now.Add(request.TTL)
	if _, err := tx.Exec(ctx, `
		INSERT INTO privacy_erasure_grants(
			id,device_id,token_hash,created_at,expires_at,attempts,max_attempts,created_by
		) VALUES($1,$2,$3,$4,$5,0,$6,$7)`,
		request.ID, request.DeviceID, request.TokenHash[:], now, expiresAt, request.MaxAttempts, request.CreatedBy); err != nil {
		return privacy.ErasureGrant{}, privacy.ErrErasureGrantStoreUnavailable
	}
	if err := tx.Commit(ctx); err != nil {
		return privacy.ErasureGrant{}, privacy.ErrErasureGrantStoreUnavailable
	}
	return privacy.ErasureGrant{
		ID: request.ID, DeviceID: request.DeviceID, TokenHash: request.TokenHash,
		CreatedAt: now, ExpiresAt: expiresAt, MaxAttempts: request.MaxAttempts, CreatedBy: request.CreatedBy,
	}, nil
}

func (s *GrantStore) ConsumeErasureGrant(ctx context.Context, request privacy.ErasureGrantAuthorization) error {
	if s == nil || s.pool == nil || request.Validate() != nil {
		return privacy.ErrErasureGrantInvalid
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return privacy.ErrErasureGrantStoreUnavailable
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	err = consumeErasureGrantTx(ctx, tx, request)
	if err != nil && !errors.Is(err, privacy.ErrErasureGrantInvalid) {
		return err
	}
	if commitErr := tx.Commit(ctx); commitErr != nil {
		return privacy.ErrErasureGrantStoreUnavailable
	}
	return err
}

func consumeErasureGrantTx(ctx context.Context, tx pgx.Tx, request privacy.ErasureGrantAuthorization) error {
	if request.Validate() != nil {
		return privacy.ErrErasureGrantInvalid
	}
	var revokedAt *time.Time
	if err := tx.QueryRow(ctx, `SELECT revoked_at FROM devices WHERE id=$1 FOR UPDATE`, request.DeviceID).Scan(&revokedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return privacy.ErrErasureGrantInvalid
		}
		return privacy.ErrErasureGrantStoreUnavailable
	}
	var id string
	var storedHash []byte
	var expiresAt, databaseNow time.Time
	var consumedAt *time.Time
	var attempts, maxAttempts int
	err := tx.QueryRow(ctx, `
		SELECT id,token_hash,expires_at,consumed_at,attempts,max_attempts,clock_timestamp()
		FROM privacy_erasure_grants
		WHERE device_id=$1
		ORDER BY created_at DESC,id DESC
		LIMIT 1
		FOR UPDATE`, request.DeviceID).
		Scan(&id, &storedHash, &expiresAt, &consumedAt, &attempts, &maxAttempts, &databaseNow)
	if errors.Is(err, pgx.ErrNoRows) {
		return privacy.ErrErasureGrantInvalid
	}
	if err != nil || len(storedHash) != sha256.Size {
		return privacy.ErrErasureGrantStoreUnavailable
	}
	databaseNow = databaseNow.UTC()
	matches := subtle.ConstantTimeCompare(storedHash, request.CandidateHash[:]) == 1
	valid := request.Canonical && matches && consumedAt == nil && revokedAt == nil && databaseNow.Before(expiresAt) && attempts < maxAttempts
	if valid {
		command, err := tx.Exec(ctx, `
			UPDATE privacy_erasure_grants SET consumed_at=$2
			WHERE id=$1 AND consumed_at IS NULL`, id, databaseNow)
		if err != nil || command.RowsAffected() != 1 {
			return privacy.ErrErasureGrantStoreUnavailable
		}
		return nil
	}
	if _, err := tx.Exec(ctx, `
		UPDATE privacy_erasure_grants
		SET attempts=LEAST(attempts+1,max_attempts)
		WHERE id=$1`, id); err != nil {
		return privacy.ErrErasureGrantStoreUnavailable
	}
	return privacy.ErrErasureGrantInvalid
}
