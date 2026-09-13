package postgresstore

import (
	"context"
	"errors"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/identity"
	"github.com/jackc/pgx/v5"
)

func (s *Store) FindWebSession(ctx context.Context, hash [32]byte, now time.Time) (identity.WebPrincipal, error) {
	var p identity.WebPrincipal
	err := s.pool.QueryRow(ctx, `SELECT d.id,d.display_name,d.created_at,dt.id,dt.scopes,ws.learner_generation,ws.expires_at
		FROM identity_web_sessions ws JOIN device_tokens dt ON dt.id=ws.token_id JOIN devices d ON d.id=dt.device_id
		JOIN privacy_owner_generation_gates g ON g.owner_kind='identity' AND g.learner_generation=ws.learner_generation
		WHERE ws.session_hash=$1 AND ws.expires_at>$2 AND dt.revoked_at IS NULL AND d.revoked_at IS NULL
		AND g.read_open AND g.write_open`, hash[:], now).Scan(
		&p.Device.ID, &p.Device.DisplayName, &p.Device.CreatedAt, &p.Credential.TokenID, &p.Credential.Scopes, &p.Generation, &p.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return p, identity.ErrNotFound
	}
	if err != nil {
		return p, err
	}
	p.Credential.Scopes = identity.WebScopes(p.Credential.Scopes)
	p.Device.Scopes = p.Credential.Scopes
	p.Credential.Device = p.Device
	return p, nil
}

func (s *Store) DeleteWebSession(ctx context.Context, hash [32]byte) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM identity_web_sessions WHERE session_hash=$1`, hash[:])
	return err
}
