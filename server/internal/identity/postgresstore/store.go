package postgresstore

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/identity"
	"github.com/edu-agent/edu-agent/server/internal/privacy"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Store struct {
	pool *pgxpool.Pool
}

func New(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

func (s *Store) InsertPairingCode(ctx context.Context, code identity.PairingCodeRecord) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO pairing_codes(lookup_id, code_hash, scopes, created_at, expires_at, attempts, max_attempts)
		VALUES($1,$2,$3,$4,$5,$6,$7)`,
		code.LookupID, code.CodeHash[:], code.Scopes, code.CreatedAt, code.ExpiresAt, code.Attempts, code.MaxAttempts)
	if err != nil {
		return fmt.Errorf("insert pairing code: %w", err)
	}
	return nil
}

func (s *Store) ConsumePairingCode(
	ctx context.Context,
	lookupID string,
	candidateHash [32]byte,
	device identity.Device,
	token identity.TokenRecord,
	now time.Time,
) ([]string, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return nil, fmt.Errorf("begin pairing exchange: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	var storedHash []byte
	var scopes []string
	var expiresAt time.Time
	var consumedAt *time.Time
	var attempts, maxAttempts int
	err = tx.QueryRow(ctx, `
		SELECT code_hash, scopes, expires_at, consumed_at, attempts, max_attempts
		FROM pairing_codes WHERE lookup_id=$1 FOR UPDATE`, lookupID).
		Scan(&storedHash, &scopes, &expiresAt, &consumedAt, &attempts, &maxAttempts)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, identity.ErrInvalidPairingCode
	}
	if err != nil {
		return nil, fmt.Errorf("lock pairing code: %w", err)
	}
	if consumedAt != nil || !now.Before(expiresAt) || attempts >= maxAttempts {
		return nil, identity.ErrInvalidPairingCode
	}
	if len(storedHash) != 32 || subtle.ConstantTimeCompare(storedHash, candidateHash[:]) != 1 {
		if _, err := tx.Exec(ctx, `UPDATE pairing_codes SET attempts=attempts+1 WHERE lookup_id=$1`, lookupID); err != nil {
			return nil, fmt.Errorf("record pairing failure: %w", err)
		}
		if err := tx.Commit(ctx); err != nil {
			return nil, fmt.Errorf("commit pairing failure: %w", err)
		}
		return nil, identity.ErrInvalidPairingCode
	}

	token.Scopes = append([]string(nil), scopes...)
	if _, err := tx.Exec(ctx, `
		INSERT INTO devices(id,display_name,created_at) VALUES($1,$2,$3)`,
		device.ID, device.DisplayName, device.CreatedAt); err != nil {
		return nil, fmt.Errorf("insert device: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO device_tokens(id,device_id,token_hash,scopes,created_at)
		VALUES($1,$2,$3,$4,$5)`,
		token.ID, token.DeviceID, token.TokenHash[:], token.Scopes, token.CreatedAt); err != nil {
		return nil, fmt.Errorf("insert device token: %w", err)
	}
	command, err := tx.Exec(ctx, `
		UPDATE pairing_codes SET consumed_at=$2
		WHERE lookup_id=$1 AND consumed_at IS NULL`, lookupID, now)
	if err != nil {
		return nil, fmt.Errorf("consume pairing code: %w", err)
	}
	if command.RowsAffected() != 1 {
		return nil, identity.ErrInvalidPairingCode
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit pairing exchange: %w", err)
	}
	return append([]string(nil), scopes...), nil
}

func (s *Store) FindCredentialByTokenHash(ctx context.Context, hash [32]byte) (identity.Credential, error) {
	var credential identity.Credential
	var storedHash []byte
	err := s.pool.QueryRow(ctx, `
		SELECT d.id,d.display_name,d.created_at,d.revoked_at,
		       dt.id,dt.token_hash,dt.scopes,dt.created_at,dt.last_used_at,dt.revoked_at
		FROM device_tokens dt JOIN devices d ON d.id=dt.device_id
		WHERE dt.token_hash=$1`, hash[:]).Scan(
		&credential.Device.ID, &credential.Device.DisplayName, &credential.Device.CreatedAt, &credential.Device.RevokedAt,
		&credential.TokenID, &storedHash, &credential.Scopes, &credential.CreatedAt, &credential.LastUsedAt, &credential.RevokedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return identity.Credential{}, identity.ErrNotFound
	}
	if err != nil {
		return identity.Credential{}, fmt.Errorf("find token: %w", err)
	}
	if len(storedHash) != 32 {
		return identity.Credential{}, errors.New("stored token hash has invalid length")
	}
	copy(credential.TokenHash[:], storedHash)
	return credential, nil
}

func (s *Store) TouchToken(ctx context.Context, tokenID string, now time.Time, minimumInterval time.Duration) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE device_tokens SET last_used_at=$2::timestamptz
		WHERE id=$1 AND revoked_at IS NULL
		  AND (last_used_at IS NULL OR last_used_at <= $2::timestamptz - ($3::bigint * interval '1 microsecond'))`,
		tokenID, now, minimumInterval.Microseconds())
	if err != nil {
		return fmt.Errorf("touch device token: %w", err)
	}
	return nil
}

func (s *Store) ListDevices(ctx context.Context) ([]identity.Device, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, fmt.Errorf("begin device list: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := privacy.LockOwnerRead(ctx, tx, privacy.OwnerIdentity); err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, `
		SELECT d.id,d.display_name,d.created_at,d.revoked_at,
		       max(dt.last_used_at),coalesce(array_agg(DISTINCT scope) FILTER (WHERE scope IS NOT NULL),'{}')
		FROM devices d
		LEFT JOIN device_tokens dt ON dt.device_id=d.id
		LEFT JOIN LATERAL unnest(dt.scopes) AS scope ON true
		GROUP BY d.id,d.display_name,d.created_at,d.revoked_at
		ORDER BY d.created_at,d.id`)
	if err != nil {
		return nil, fmt.Errorf("list devices: %w", err)
	}
	var devices []identity.Device
	for rows.Next() {
		var device identity.Device
		if err := rows.Scan(&device.ID, &device.DisplayName, &device.CreatedAt, &device.RevokedAt, &device.LastUsedAt, &device.Scopes); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan device: %w", err)
		}
		devices = append(devices, device)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("iterate devices: %w", err)
	}
	rows.Close()
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit device list: %w", err)
	}
	return devices, nil
}

func (s *Store) RevokeDevice(ctx context.Context, deviceID string, now time.Time) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin device revocation: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	command, err := tx.Exec(ctx, `UPDATE devices SET revoked_at=coalesce(revoked_at,$2) WHERE id=$1`, deviceID, now)
	if err != nil {
		return fmt.Errorf("revoke device: %w", err)
	}
	if command.RowsAffected() != 1 {
		return identity.ErrNotFound
	}
	if _, err := tx.Exec(ctx, `UPDATE device_tokens SET revoked_at=coalesce(revoked_at,$2) WHERE device_id=$1`, deviceID, now); err != nil {
		return fmt.Errorf("revoke device tokens: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit device revocation: %w", err)
	}
	return nil
}
