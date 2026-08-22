package postgresstore

import (
	"context"
	"fmt"

	"github.com/edu-agent/edu-agent/server/internal/privacy"
	"github.com/jackc/pgx/v5"
)

func (s *Store) Owner() privacy.OwnerKind { return privacy.OwnerOutbox }

func (s *Store) CloseGenerationTx(ctx context.Context, db privacy.DBTX, transition privacy.GenerationTransition) error {
	if err := transition.Validate(false); err != nil {
		return fmt.Errorf("validate outbox generation close: %w", err)
	}
	tag, err := db.Exec(ctx, `
		UPDATE privacy_owner_generation_gates
		SET learner_generation=$3,read_open=FALSE,write_open=FALSE,
		    active_erasure_id=$2,updated_at=$4
		WHERE owner_kind='outbox' AND learner_generation=$1
		  AND read_open AND write_open AND active_erasure_id IS NULL
		  AND EXISTS (
		    SELECT 1 FROM privacy_erasures e
		    JOIN privacy_erasure_heads h ON h.erasure_id=e.id AND h.status<>'verified'
		    JOIN privacy_redaction_barriers b
		      ON b.erasure_id=e.id AND b.learner_generation=e.target_learner_generation
		    WHERE e.id=$2 AND e.target_learner_generation=$3
		  )`, transition.FromGeneration, transition.ErasureID, transition.TargetGeneration, transition.At)
	if err != nil {
		return fmt.Errorf("close outbox generation %d for erasure %s: %w", transition.TargetGeneration, transition.ErasureID, err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("close outbox generation %d for erasure %s: gate compare-and-swap affected %d rows", transition.TargetGeneration, transition.ErasureID, tag.RowsAffected())
	}
	return nil
}

func (s *Store) OpenGenerationTx(ctx context.Context, db privacy.DBTX, transition privacy.GenerationTransition) error {
	if err := transition.Validate(true); err != nil {
		return fmt.Errorf("validate outbox generation open: %w", err)
	}
	tag, err := db.Exec(ctx, `
		UPDATE privacy_owner_generation_gates
		SET read_open=TRUE,write_open=TRUE,active_erasure_id=NULL,updated_at=$4
		WHERE owner_kind='outbox' AND learner_generation=$2
		  AND NOT read_open AND NOT write_open AND active_erasure_id=$1
		  AND EXISTS (
		    SELECT 1
		    FROM privacy_erasure_receipt_heads h
		    JOIN privacy_erasure_step_receipts r ON r.id=h.current_receipt_id
		    WHERE h.erasure_id=$1 AND h.current_receipt_id=$3
		      AND r.status IN ('succeeded','not_applicable')
		      AND privacy_scrub_receipt_matches_owner('outbox',h.store_kind)
		  )`, transition.ErasureID, transition.TargetGeneration, transition.ReceiptID, transition.At)
	if err != nil {
		return fmt.Errorf("open outbox generation %d for erasure %s: %w", transition.TargetGeneration, transition.ErasureID, err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("open outbox generation %d for erasure %s: gate compare-and-swap affected %d rows", transition.TargetGeneration, transition.ErasureID, tag.RowsAffected())
	}
	return nil
}

func (s *Store) RedactTx(ctx context.Context, request privacy.LocalRedactionRequest) error {
	if err := request.Validate(privacy.OwnerOutbox); err != nil {
		return fmt.Errorf("validate outbox redaction request: %w", err)
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return fmt.Errorf("begin outbox privacy scrub: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	var permit string
	if err := tx.QueryRow(ctx, `SELECT privacy_begin_owner_scrub($1,$2,'outbox',$3)::text`,
		request.ErasureID, request.LearnerGeneration, request.ReceiptID).Scan(&permit); err != nil {
		return fmt.Errorf("acquire outbox privacy scrub permit for erasure %s generation %d: %w", request.ErasureID, request.LearnerGeneration, err)
	}
	if permit == "" {
		return fmt.Errorf("acquire outbox privacy scrub permit for erasure %s generation %d: empty permit", request.ErasureID, request.LearnerGeneration)
	}

	if _, err := tx.Exec(ctx, `
		UPDATE outbox_messages
		SET status='canceled',terminal_disposition='privacy_erasure',
		    payload='{"redacted":true}'::jsonb,audit_metadata='{}'::jsonb,
		    last_error_category=NULL,last_error_at=NULL,
		    lease_expires_at=NULL,lease_token=NULL,updated_at=clock_timestamp()
		WHERE generation<$1
		  AND (status<>'canceled'
		       OR terminal_disposition IS DISTINCT FROM 'privacy_erasure'
		       OR payload IS DISTINCT FROM '{"redacted":true}'::jsonb
		       OR audit_metadata IS DISTINCT FROM '{}'::jsonb
		       OR last_error_category IS NOT NULL OR last_error_at IS NOT NULL
		       OR lease_expires_at IS NOT NULL OR lease_token IS NOT NULL)`, request.LearnerGeneration); err != nil {
		return fmt.Errorf("scrub outbox messages for erasure %s generation %d: %w", request.ErasureID, request.LearnerGeneration, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit outbox privacy scrub for erasure %s generation %d: %w", request.ErasureID, request.LearnerGeneration, err)
	}
	return nil
}

func (s *Store) VerifyRedacted(ctx context.Context, request privacy.LocalRedactionRequest) (int64, error) {
	if err := request.Validate(privacy.OwnerOutbox); err != nil {
		return 0, fmt.Errorf("validate outbox redaction verification request: %w", err)
	}
	var residual int64
	err := s.pool.QueryRow(ctx, `
		SELECT count(*)
		FROM outbox_messages
		WHERE generation<$1
		  AND (status<>'canceled'
		       OR terminal_disposition IS DISTINCT FROM 'privacy_erasure'
		       OR payload IS DISTINCT FROM '{"redacted":true}'::jsonb
		       OR audit_metadata IS DISTINCT FROM '{}'::jsonb
		       OR last_error_category IS NOT NULL OR last_error_at IS NOT NULL
		       OR lease_expires_at IS NOT NULL OR lease_token IS NOT NULL)`, request.LearnerGeneration).Scan(&residual)
	if err != nil {
		return 0, fmt.Errorf("verify outbox privacy scrub for erasure %s generation %d: %w", request.ErasureID, request.LearnerGeneration, err)
	}
	return residual, nil
}

var _ privacy.LocalOwnerPort = (*Store)(nil)
