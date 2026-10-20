package mentorrun

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/edu-agent/edu-agent/server/internal/identity"
	"github.com/jackc/pgx/v5"
	"time"
)

func (s *Service) ReadWebOperation(ctx context.Context, actor identity.Credential, space, id string, send func(json.RawMessage) error) error {
	if !validID(space) || !validID(id) {
		return ErrInvalid
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background())
	g, err := gates(ctx, tx, false)
	if err != nil {
		return err
	}
	if err = actorGate(ctx, tx, actor.Device.ID, actor.TokenID, false); err != nil {
		return err
	}
	var key string
	var ciphertext []byte
	err = tx.QueryRow(ctx, `SELECT o.id::text,o.ciphertext FROM learning_web_operations o JOIN learning_tutor_conversations c ON c.id=o.conversation_id LEFT JOIN learning_mentor_runs r ON r.id=o.run_id WHERE o.operation_id=$1 AND o.device_id=$2 AND o.space_id=$3 AND o.privacy_generation=$4 AND NOT c.deleted AND (o.run_id IS NULL OR (r.state->>'body_available')::boolean AND r.expires_at>clock_timestamp()) FOR SHARE OF o,c`, id, actor.Device.ID, space, g).Scan(&key, &ciphertext)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	var raw json.RawMessage
	if err = s.openWeb("operation", key, g, ciphertext, &raw); err != nil {
		return err
	}
	var preview WebPreview
	if err = json.Unmarshal(raw, &preview); err != nil {
		return ErrStorage
	}
	if preview.ID != "" {
		if time.Now().After(preview.ExpiresAt) {
			return ErrConflict
		}
		var document, run string
		if err = tx.QueryRow(ctx, `SELECT document_id::text,owner_id::text FROM learning_web_import_previews WHERE id=$1 AND device_id=$2 AND space_id=$3 AND privacy_generation=$4`, preview.ID, actor.Device.ID, space, g).Scan(&document, &run); errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return err
		}
		item, e := scan(tx.QueryRow(ctx, `SELECT `+columns+` FROM learning_mentor_runs WHERE id=$1 FOR SHARE`, run))
		if e != nil {
			return e
		}
		preview.Items[0].Document, err = s.webDocumentTx(ctx, tx, item, document)
		if err != nil {
			return err
		}
		raw, err = json.Marshal(preview)
		if err != nil {
			return err
		}
	}
	return send(raw)
}
