package postgresstore

import (
	"context"
	"fmt"

	"github.com/edu-agent/edu-agent/server/internal/identity"
	"github.com/edu-agent/edu-agent/server/internal/privacy"
	"github.com/jackc/pgx/v5"
)

var _ privacy.LocalOwnerPort = (*Store)(nil)

func (s *Store) Owner() privacy.OwnerKind {
	return privacy.OwnerIdentity
}

func (s *Store) CloseGenerationTx(ctx context.Context, db privacy.DBTX, transition privacy.GenerationTransition) error {
	if err := transition.Validate(false); err != nil {
		return err
	}
	tag, err := db.Exec(ctx, `
		UPDATE privacy_owner_generation_gates
		SET learner_generation=$2,read_open=FALSE,write_open=FALSE,active_erasure_id=$3,updated_at=$4
		WHERE owner_kind=$1 AND learner_generation=$5
		  AND read_open=TRUE AND write_open=TRUE AND active_erasure_id IS NULL`,
		privacy.OwnerIdentity, transition.TargetGeneration, transition.ErasureID, transition.At.UTC(), transition.FromGeneration)
	if err != nil {
		return fmt.Errorf("close identity generation gate: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return &privacy.Error{Code: privacy.CodeErasureInProgress, Reason: "identity_generation_close_cas_failed"}
	}
	return nil
}

func (s *Store) OpenGenerationTx(ctx context.Context, db privacy.DBTX, transition privacy.GenerationTransition) error {
	if err := transition.Validate(true); err != nil {
		return err
	}
	tag, err := db.Exec(ctx, `
		UPDATE privacy_owner_generation_gates
		SET read_open=TRUE,write_open=TRUE,active_erasure_id=NULL,updated_at=$4
		WHERE owner_kind=$1 AND learner_generation=$2
		  AND read_open=FALSE AND write_open=FALSE AND active_erasure_id=$3`,
		privacy.OwnerIdentity, transition.TargetGeneration, transition.ErasureID, transition.At.UTC())
	if err != nil {
		return fmt.Errorf("open identity generation gate: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return &privacy.Error{Code: privacy.CodeReceiptNotCurrent, Reason: "identity_generation_open_cas_failed"}
	}
	return nil
}

func (s *Store) RedactTx(ctx context.Context, request privacy.LocalRedactionRequest) error {
	if err := request.Validate(privacy.OwnerIdentity); err != nil {
		return err
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return fmt.Errorf("begin identity privacy scrub: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	var permit string
	if err := tx.QueryRow(ctx, `SELECT privacy_begin_owner_scrub($1,$2,$3,$4)::text`,
		request.ErasureID, request.LearnerGeneration, privacy.OwnerIdentity, request.ReceiptID).Scan(&permit); err != nil {
		return fmt.Errorf("authorize identity privacy scrub: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE devices
		SET display_name='[redacted]'
		WHERE privacy_owner_scrub_permitted('identity')`); err != nil {
		return fmt.Errorf("redact identity device labels: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE device_tokens
		SET last_used_at=NULL
		WHERE privacy_owner_scrub_permitted('identity')`); err != nil {
		return fmt.Errorf("redact identity token usage metadata: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit identity privacy scrub: %w", err)
	}
	return nil
}

func (s *Store) VerifyRedacted(ctx context.Context, request privacy.LocalRedactionRequest) (int64, error) {
	if err := request.Validate(privacy.OwnerIdentity); err != nil {
		return 0, err
	}
	var residual int64
	err := s.pool.QueryRow(ctx, `
		SELECT
			(SELECT count(*) FROM devices WHERE display_name <> '[redacted]')+
			(SELECT count(*) FROM device_tokens WHERE last_used_at IS NOT NULL)`).Scan(&residual)
	if err != nil {
		return 0, fmt.Errorf("verify identity privacy scrub: %w", err)
	}
	return residual, nil
}

var _ identity.Store = (*Store)(nil)
