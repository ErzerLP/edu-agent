package postgresstore

import (
	"context"
	"errors"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/identity"
	"github.com/jackc/pgx/v5"
)

func (s *Store) CreateWebOfflineSession(ctx context.Context, source, hash [32]byte, now, expires time.Time) error {
	// 从仍有效的原浏览器会话派生，调用方不能指定设备、权限或代次。
	tag, err := s.pool.Exec(ctx, `INSERT INTO identity_web_offline_sessions(session_hash,token_id,learner_generation,expires_at)
 SELECT $2,ws.token_id,ws.learner_generation,$4 FROM identity_web_sessions ws
 JOIN device_tokens dt ON dt.id=ws.token_id JOIN devices d ON d.id=dt.device_id
 JOIN privacy_owner_generation_gates g ON g.owner_kind='identity' AND g.learner_generation=ws.learner_generation
 WHERE ws.session_hash=$1 AND ws.expires_at>$3 AND dt.revoked_at IS NULL AND d.revoked_at IS NULL
 AND g.read_open AND g.write_open AND dt.scopes @> ARRAY['learning:read','learning:write','knowledge:read']::text[]`, source[:], hash[:], now, expires)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return identity.ErrUnauthenticated
	}
	return nil
}

func (s *Store) FindWebOfflineSession(ctx context.Context, hash [32]byte, now time.Time) (identity.WebOfflinePrincipal, error) {
	var p identity.WebOfflinePrincipal
	err := s.pool.QueryRow(ctx, `SELECT d.id,d.created_at,dt.id,dt.scopes,ws.learner_generation,ws.expires_at,
 (g.learner_generation=ws.learner_generation AND g.read_open AND g.write_open)
 FROM identity_web_offline_sessions ws JOIN device_tokens dt ON dt.id=ws.token_id JOIN devices d ON d.id=dt.device_id
 JOIN privacy_owner_generation_gates g ON g.owner_kind='identity'
 WHERE ws.session_hash=$1 AND ws.expires_at>$2 AND dt.revoked_at IS NULL AND d.revoked_at IS NULL`, hash[:], now).Scan(
		&p.Device.ID, &p.Device.CreatedAt, &p.Credential.TokenID, &p.Credential.Scopes, &p.Generation, &p.ExpiresAt, &p.ContentAllowed)
	if errors.Is(err, pgx.ErrNoRows) {
		return p, identity.ErrNotFound
	}
	if err != nil {
		return p, err
	}
	// 专用路由只保留原有读写权限；设备清除是此凭据自身的必备收尾能力。
	scopes := []string{"privacy:device"}
	for _, scope := range p.Credential.Scopes {
		if scope == "learning:read" || scope == "learning:write" || scope == "knowledge:read" {
			scopes = append(scopes, scope)
		}
	}
	p.Credential.Scopes = scopes
	p.Device.Scopes = scopes
	p.Credential.Device = p.Device
	return p, nil
}

func (s *Store) DeleteWebOfflineSession(ctx context.Context, hash [32]byte) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM identity_web_offline_sessions WHERE session_hash=$1`, hash[:])
	return err
}

func (s *Store) RenewWebOfflineSession(ctx context.Context, hash [32]byte, now, expires time.Time) error {
	tag, err := s.pool.Exec(ctx, `UPDATE identity_web_offline_sessions ws SET expires_at=$3
	FROM device_tokens dt,devices d,privacy_owner_generation_gates g
	WHERE ws.session_hash=$1 AND ws.expires_at>$2 AND dt.id=ws.token_id AND d.id=dt.device_id
	AND dt.revoked_at IS NULL AND d.revoked_at IS NULL AND g.owner_kind='identity'
	AND g.learner_generation=ws.learner_generation AND g.read_open AND g.write_open
	AND dt.scopes @> ARRAY['learning:read','learning:write','knowledge:read']::text[]`, hash[:], now, expires)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return identity.ErrUnauthenticated
	}
	return nil
}
