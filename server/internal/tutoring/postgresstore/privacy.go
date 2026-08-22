package postgresstore

import (
	"context"
	"fmt"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/privacy"
	"github.com/jackc/pgx/v5"
)

var _ privacy.LocalOwnerPort = (*Store)(nil)

func (s *Store) Owner() privacy.OwnerKind { return privacy.OwnerTutoring }

func (s *Store) CloseGenerationTx(ctx context.Context, db privacy.DBTX, transition privacy.GenerationTransition) error {
	if db == nil {
		return &privacy.Error{Code: privacy.CodeInvalidRequest, Reason: "tutoring_generation_database_required"}
	}
	if err := transition.Validate(false); err != nil {
		return err
	}
	tag, err := db.Exec(ctx, `
		UPDATE privacy_owner_generation_gates
		SET learner_generation=$3,read_open=FALSE,write_open=FALSE,active_erasure_id=$1,updated_at=$4
		WHERE owner_kind='tutoring' AND learner_generation=$2
		  AND read_open AND write_open AND active_erasure_id IS NULL`,
		transition.ErasureID, transition.FromGeneration, transition.TargetGeneration, transition.At.UTC())
	if err != nil {
		return fmt.Errorf("close tutoring privacy generation: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return &privacy.Error{Code: privacy.CodeErasureInProgress, Reason: "tutoring_generation_close_cas_failed"}
	}
	return nil
}

func (s *Store) OpenGenerationTx(ctx context.Context, db privacy.DBTX, transition privacy.GenerationTransition) error {
	if db == nil {
		return &privacy.Error{Code: privacy.CodeInvalidRequest, Reason: "tutoring_generation_database_required"}
	}
	if err := transition.Validate(true); err != nil {
		return err
	}
	tag, err := db.Exec(ctx, `
		UPDATE privacy_owner_generation_gates g
		SET read_open=TRUE,write_open=TRUE,active_erasure_id=NULL,updated_at=$4
		WHERE g.owner_kind='tutoring' AND g.learner_generation=$2
		  AND NOT g.read_open AND NOT g.write_open AND g.active_erasure_id=$1
		  AND EXISTS (
			SELECT 1
			FROM privacy_erasure_receipt_heads h
			JOIN privacy_erasure_step_receipts r
			  ON r.id=h.current_receipt_id AND r.erasure_id=h.erasure_id AND r.store_kind=h.store_kind
			WHERE h.erasure_id=$1 AND h.store_kind='tutoring_payload'
			  AND h.current_receipt_id=$3 AND r.status IN ('succeeded','not_applicable')
		  )`, transition.ErasureID, transition.TargetGeneration, transition.ReceiptID, transition.At.UTC())
	if err != nil {
		return fmt.Errorf("open tutoring privacy generation: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return &privacy.Error{Code: privacy.CodeReceiptNotCurrent, Reason: "tutoring_generation_open_cas_failed"}
	}
	return nil
}

func (s *Store) RedactTx(ctx context.Context, request privacy.LocalRedactionRequest) error {
	if err := request.Validate(privacy.OwnerTutoring); err != nil {
		return err
	}
	if request.Store != privacy.StoreTutoringPayload {
		return &privacy.Error{Code: privacy.CodeUnsupportedReceiptStore, Reason: string(request.Store)}
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return fmt.Errorf("begin tutoring privacy redaction: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	var permit string
	var now time.Time
	if err := tx.QueryRow(ctx, `SELECT privacy_begin_owner_scrub($1,$2,'tutoring',$3)::text,clock_timestamp()`, request.ErasureID, request.LearnerGeneration, request.ReceiptID).Scan(&permit, &now); err != nil {
		return fmt.Errorf("authorize tutoring privacy redaction: %w", err)
	}
	statements := []struct {
		name string
		sql  string
		args []any
	}{
		{"tutoring sessions", `UPDATE tutoring_sessions SET state='Completed',goal_revision_id=NULL,route_revision_id=NULL,route_step_id=NULL,knowledge_revision_id=NULL,focus_node_revision_id=NULL,activity_id=NULL,attempt_id=NULL,attached_quiz=FALSE,updated_at=$1,completed_at=$1`, []any{now.UTC()}},
		{"tutoring focus frames", `UPDATE tutoring_focus_frames SET goal_revision_id=NULL,route_revision_id=NULL,route_step_id=NULL,knowledge_revision_id=NULL,focus_node_revision_id=NULL,activity_id=NULL,attempt_id=NULL,invalidated_at=$1,invalidation_reason='privacy_erasure',resumed_at=NULL`, []any{now.UTC()}},
		{"tutoring free questions", `UPDATE tutoring_free_questions SET question_text='[redacted]',references_snapshot='[]'::jsonb`, nil},
		{"tutoring free answers", `UPDATE tutoring_free_answers SET answer_text='[redacted]',references_snapshot='[]'::jsonb,source_proposal_id=NULL`, nil},
		{"tutoring proposal requests", `UPDATE tutoring_proposal_requests SET input='{"redacted":true}'::jsonb,status='failed',lease_token=NULL,lease_expires_at=NULL,attempt_categories='{}'::text[],result_proposal_id=NULL,error_category='privacy_erasure',updated_at=$1`, []any{now.UTC()}},
		{"tutoring proposal artifacts", `UPDATE tutoring_proposal_artifacts SET input_hash=decode('e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855','hex'),goal_revision_id=NULL,route_revision_id=NULL,activity_id=NULL,attempt_id=NULL,artifact='{"redacted":true}'::jsonb,trusted_model_id='[redacted]',model_parameters='{}'::jsonb,prompt_revision='[redacted]',attempt_categories='{}'::text[]`, nil},
	}
	for _, statement := range statements {
		if _, err := tx.Exec(ctx, statement.sql, statement.args...); err != nil {
			return fmt.Errorf("redact %s: %w", statement.name, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit tutoring privacy redaction: %w", err)
	}
	return nil
}

func (s *Store) VerifyRedacted(ctx context.Context, request privacy.LocalRedactionRequest) (int64, error) {
	if err := request.Validate(privacy.OwnerTutoring); err != nil {
		return 0, err
	}
	if request.Store != privacy.StoreTutoringPayload {
		return 0, &privacy.Error{Code: privacy.CodeUnsupportedReceiptStore, Reason: string(request.Store)}
	}
	var remaining int64
	err := s.pool.QueryRow(ctx, `
		SELECT COALESCE(sum(remaining),0)::bigint FROM (
			SELECT count(*)::bigint AS remaining
			FROM tutoring_sessions
			WHERE state<>'Completed' OR goal_revision_id IS NOT NULL OR route_revision_id IS NOT NULL
			   OR route_step_id IS NOT NULL OR knowledge_revision_id IS NOT NULL OR focus_node_revision_id IS NOT NULL
			   OR activity_id IS NOT NULL OR attempt_id IS NOT NULL OR attached_quiz OR completed_at IS NULL
			UNION ALL SELECT count(*) FROM tutoring_focus_frames
			WHERE goal_revision_id IS NOT NULL OR route_revision_id IS NOT NULL OR route_step_id IS NOT NULL
			   OR knowledge_revision_id IS NOT NULL OR focus_node_revision_id IS NOT NULL
			   OR activity_id IS NOT NULL OR attempt_id IS NOT NULL OR invalidated_at IS NULL
			   OR invalidation_reason IS DISTINCT FROM 'privacy_erasure' OR resumed_at IS NOT NULL
			UNION ALL SELECT count(*) FROM tutoring_free_questions
			WHERE question_text<>'[redacted]' OR references_snapshot<>'[]'::jsonb
			UNION ALL SELECT count(*) FROM tutoring_free_answers
			WHERE answer_text<>'[redacted]' OR references_snapshot<>'[]'::jsonb OR source_proposal_id IS NOT NULL
			UNION ALL SELECT count(*) FROM tutoring_proposal_requests
			WHERE input<>'{"redacted":true}'::jsonb OR status<>'failed' OR lease_token IS NOT NULL
			   OR lease_expires_at IS NOT NULL OR attempt_categories<>'{}'::text[] OR result_proposal_id IS NOT NULL
			   OR error_category IS DISTINCT FROM 'privacy_erasure'
			UNION ALL SELECT count(*) FROM tutoring_proposal_artifacts
			WHERE input_hash<>decode('e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855','hex')
			   OR goal_revision_id IS NOT NULL OR route_revision_id IS NOT NULL OR activity_id IS NOT NULL OR attempt_id IS NOT NULL
			   OR artifact<>'{"redacted":true}'::jsonb OR trusted_model_id<>'[redacted]'
			   OR model_parameters<>'{}'::jsonb OR prompt_revision<>'[redacted]' OR attempt_categories<>'{}'::text[]
		) residuals`).Scan(&remaining)
	if err != nil {
		return 0, fmt.Errorf("verify tutoring payload redaction: %w", err)
	}
	return remaining, nil
}
