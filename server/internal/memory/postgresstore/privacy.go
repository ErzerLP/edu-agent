package postgresstore

import (
	"context"
	"fmt"

	"github.com/edu-agent/edu-agent/server/internal/privacy"
	"github.com/jackc/pgx/v5"
)

const memoryRedactionTombstone = "[redacted]"

func lockPrivacyRows(ctx context.Context, tx pgx.Tx, query string, args ...any) error {
	rows, err := tx.Query(ctx, query, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return err
		}
	}
	return rows.Err()
}

func lockMemoryPrivacyGeneration(ctx context.Context, tx pgx.Tx, targetGeneration int64) error {
	var lockedGeneration int64
	if err := tx.QueryRow(ctx, `
		SELECT learner_generation
		FROM privacy_owner_generation_gates
		WHERE owner_kind='memory' AND learner_generation=$1
		  AND NOT read_open AND NOT write_open
		FOR UPDATE`, targetGeneration).Scan(&lockedGeneration); err != nil {
		return fmt.Errorf("lock memory privacy generation %d: %w", targetGeneration, err)
	}
	return nil
}

func preparePrivacyRemoteCleanup(ctx context.Context, tx pgx.Tx, targetGeneration int64) error {
	if err := lockPrivacyRows(ctx, tx, `
		SELECT h.delivery_id::text
		FROM memory_delivery_heads h
		JOIN memory_deliveries d ON d.id=h.delivery_id
		WHERE d.learner_generation<$1
		ORDER BY h.delivery_id
		FOR UPDATE OF h`, targetGeneration); err != nil {
		return fmt.Errorf("lock old memory delivery heads: %w", err)
	}
	if err := lockPrivacyRows(ctx, tx, `
		SELECT h.logical_memory_id::text
		FROM memory_record_heads h
		JOIN memory_deliveries d ON d.id=h.current_delivery_id
		WHERE d.learner_generation<$1
		ORDER BY h.logical_memory_id
		FOR UPDATE OF h`, targetGeneration); err != nil {
		return fmt.Errorf("lock old memory record heads: %w", err)
	}
	if err := lockPrivacyRows(ctx, tx, `
		SELECT h.attempt_id::text
		FROM memory_delivery_attempt_heads h
		JOIN memory_delivery_attempts a ON a.id=h.attempt_id
		JOIN memory_deliveries d ON d.id=a.delivery_id
		WHERE d.learner_generation<$1
		ORDER BY d.id,a.created_at,a.id
		FOR UPDATE OF h`, targetGeneration); err != nil {
		return fmt.Errorf("lock old memory delivery attempts: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO memory_expiry_reconciliations(
			id,delivery_id,logical_memory_id,external_uri,content_hash,attempt_token,
			sent_boot_epoch,learner_generation,record_generation,status,created_at,updated_at)
		SELECT gen_random_uuid(),d.id,d.logical_memory_id,d.external_uri,d.payload_hash,a.attempt_token,
		       h.boot_epoch,d.learner_generation,d.record_generation,'pending',clock_timestamp(),clock_timestamp()
		FROM memory_deliveries d
		JOIN memory_delivery_attempts a ON a.delivery_id=d.id
		JOIN memory_delivery_attempt_heads h ON h.attempt_id=a.id
		WHERE d.learner_generation<$1 AND h.sent_at IS NOT NULL
		ON CONFLICT(delivery_id,attempt_token) DO NOTHING`, targetGeneration); err != nil {
		return fmt.Errorf("materialize old sent memory reconciliations: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE memory_delivery_attempt_heads h
		SET state='fenced',lease_token=NULL,lease_expires_at=NULL,
		    error_category=COALESCE(error_category,'privacy_erasure'),updated_at=clock_timestamp()
		FROM memory_delivery_attempts a
		JOIN memory_deliveries d ON d.id=a.delivery_id
		WHERE h.attempt_id=a.id AND d.learner_generation<$1
		  AND h.state IN ('prepared','sent','unknown','reconciling','confirmed')`, targetGeneration); err != nil {
		return fmt.Errorf("fence old memory delivery attempts: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		WITH targets AS MATERIALIZED (
			SELECT h.delivery_id,h.current_receipt_version+1 AS next_version,
			       EXISTS(
			         SELECT 1 FROM memory_expiry_reconciliations r
			         WHERE r.delivery_id=h.delivery_id
			           AND r.status NOT IN ('absence_verified','verified')
			       ) AS remote_pending
			FROM memory_delivery_heads h
			JOIN memory_deliveries d ON d.id=h.delivery_id
			WHERE d.learner_generation<$1
		), changed AS MATERIALIZED (
			SELECT * FROM targets
			WHERE NOT EXISTS (
				SELECT 1 FROM memory_delivery_heads current
				WHERE current.delivery_id=targets.delivery_id
				  AND current.terminal_disposition='privacy_erasure'
				  AND current.status=CASE WHEN targets.remote_pending THEN 'fenced' ELSE 'deleted' END
			)
		), receipts AS (
			INSERT INTO memory_delivery_receipts(
				id,delivery_id,version,status,reason,verification_method,created_at)
			SELECT gen_random_uuid(),delivery_id,next_version,
			       CASE WHEN remote_pending THEN 'partial' ELSE 'succeeded' END,
			       $2,
			       CASE WHEN remote_pending THEN 'privacy_remote_reconciliation' ELSE 'privacy_local_only' END,
			       clock_timestamp()
			FROM changed
			RETURNING id,delivery_id,version
		)
		UPDATE memory_delivery_heads h
		SET status=CASE WHEN t.remote_pending THEN 'fenced' ELSE 'deleted' END,
		    public_status='rejected',terminal_disposition='privacy_erasure',attempt_state='fenced',
		    current_receipt_id=r.id,current_receipt_version=r.version,
		    last_error_category='privacy_erasure',updated_at=clock_timestamp()
		FROM changed t
		JOIN receipts r ON r.delivery_id=t.delivery_id
		WHERE h.delivery_id=t.delivery_id`, targetGeneration, memoryRedactionTombstone); err != nil {
		return fmt.Errorf("mark old memory deliveries for privacy cleanup: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE memory_record_heads h
		SET status=CASE WHEN dh.status='deleted' THEN 'deleted' ELSE 'delete_pending' END,
		    receipt_id=dh.current_receipt_id,
		    deleted_at=CASE WHEN dh.status='deleted' THEN COALESCE(h.deleted_at,clock_timestamp()) ELSE NULL END,
		    updated_at=clock_timestamp()
		FROM memory_deliveries d
		JOIN memory_delivery_heads dh ON dh.delivery_id=d.id
		WHERE h.current_delivery_id=d.id AND d.learner_generation<$1
		  AND dh.terminal_disposition='privacy_erasure'
		  AND (h.status IS DISTINCT FROM CASE WHEN dh.status='deleted' THEN 'deleted' ELSE 'delete_pending' END
		       OR h.receipt_id IS DISTINCT FROM dh.current_receipt_id
		       OR (dh.status='deleted' AND h.deleted_at IS NULL)
		       OR (dh.status<>'deleted' AND h.deleted_at IS NOT NULL))`, targetGeneration); err != nil {
		return fmt.Errorf("mark old memory records for privacy cleanup: %w", err)
	}
	return nil
}

func (s *Store) Owner() privacy.OwnerKind { return privacy.OwnerMemory }

func (s *Store) CloseGenerationTx(ctx context.Context, db privacy.DBTX, transition privacy.GenerationTransition) error {
	if err := transition.Validate(false); err != nil {
		return fmt.Errorf("validate memory generation close: %w", err)
	}
	tag, err := db.Exec(ctx, `
		UPDATE privacy_owner_generation_gates
		SET learner_generation=$3,read_open=FALSE,write_open=FALSE,
		    active_erasure_id=$2,updated_at=$4
		WHERE owner_kind='memory' AND learner_generation=$1
		  AND read_open AND write_open AND active_erasure_id IS NULL
		  AND EXISTS (
		    SELECT 1 FROM privacy_erasures e
		    JOIN privacy_erasure_heads h ON h.erasure_id=e.id AND h.status<>'verified'
		    JOIN privacy_redaction_barriers b
		      ON b.erasure_id=e.id AND b.learner_generation=e.target_learner_generation
		    WHERE e.id=$2 AND e.target_learner_generation=$3
		  )`, transition.FromGeneration, transition.ErasureID, transition.TargetGeneration, transition.At)
	if err != nil {
		return fmt.Errorf("close memory generation %d for erasure %s: %w", transition.TargetGeneration, transition.ErasureID, err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("close memory generation %d for erasure %s: gate compare-and-swap affected %d rows", transition.TargetGeneration, transition.ErasureID, tag.RowsAffected())
	}
	return nil
}

func (s *Store) OpenGenerationTx(ctx context.Context, db privacy.DBTX, transition privacy.GenerationTransition) error {
	if err := transition.Validate(true); err != nil {
		return fmt.Errorf("validate memory generation open: %w", err)
	}
	tag, err := db.Exec(ctx, `
		UPDATE privacy_owner_generation_gates
		SET read_open=TRUE,write_open=TRUE,active_erasure_id=NULL,updated_at=$4
		WHERE owner_kind='memory' AND learner_generation=$2
		  AND NOT read_open AND NOT write_open AND active_erasure_id=$1
		  AND EXISTS (
		    SELECT 1
		    FROM privacy_erasure_receipt_heads h
		    JOIN privacy_erasure_step_receipts r ON r.id=h.current_receipt_id
		    WHERE h.erasure_id=$1 AND h.current_receipt_id=$3
		      AND r.status IN ('succeeded','not_applicable')
		      AND privacy_scrub_receipt_matches_owner('memory',h.store_kind)
		  )`, transition.ErasureID, transition.TargetGeneration, transition.ReceiptID, transition.At)
	if err != nil {
		return fmt.Errorf("open memory generation %d for erasure %s: %w", transition.TargetGeneration, transition.ErasureID, err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("open memory generation %d for erasure %s: gate compare-and-swap affected %d rows", transition.TargetGeneration, transition.ErasureID, tag.RowsAffected())
	}
	return nil
}

func (s *Store) RedactTx(ctx context.Context, request privacy.LocalRedactionRequest) error {
	if err := request.Validate(privacy.OwnerMemory); err != nil {
		return fmt.Errorf("validate memory redaction request: %w", err)
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return fmt.Errorf("begin memory privacy scrub: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	if err := lockMemoryPrivacyGeneration(ctx, tx, request.LearnerGeneration); err != nil {
		return fmt.Errorf("lock memory privacy scrub for erasure %s generation %d: %w", request.ErasureID, request.LearnerGeneration, err)
	}
	var permit string
	if err := tx.QueryRow(ctx, `SELECT privacy_begin_owner_scrub($1,$2,'memory',$3)::text`,
		request.ErasureID, request.LearnerGeneration, request.ReceiptID).Scan(&permit); err != nil {
		return fmt.Errorf("acquire memory privacy scrub permit for erasure %s generation %d: %w", request.ErasureID, request.LearnerGeneration, err)
	}
	if permit == "" {
		return fmt.Errorf("acquire memory privacy scrub permit for erasure %s generation %d: empty permit", request.ErasureID, request.LearnerGeneration)
	}
	if err := preparePrivacyRemoteCleanup(ctx, tx, request.LearnerGeneration); err != nil {
		return fmt.Errorf("prepare memory privacy cleanup for erasure %s generation %d: %w", request.ErasureID, request.LearnerGeneration, err)
	}

	statements := []struct {
		name string
		sql  string
		args []any
	}{
		{"candidate reasons", `UPDATE memory_candidates SET reason=$1 WHERE reason<>$1`, []any{memoryRedactionTombstone}},
		{"candidate decision reasons", `UPDATE memory_candidate_decisions SET reason=$1 WHERE reason<>$1`, []any{memoryRedactionTombstone}},
		{"pending candidate expiry", `
			WITH pending AS (
				SELECT h.candidate_id,h.revision+1 AS next_revision
				FROM memory_candidate_heads h
				WHERE h.status='pending_review'
				FOR UPDATE
			), decisions AS (
				INSERT INTO memory_candidate_decisions(
					id,candidate_id,revision,decision,reason,actor_kind,created_at)
				SELECT gen_random_uuid(),candidate_id,next_revision,'expire',$1,'system',clock_timestamp()
				FROM pending
				RETURNING id,candidate_id,revision
			)
			UPDATE memory_candidate_heads h
			SET revision=d.revision,status='expired',current_decision_id=d.id,
				payload_available=FALSE,updated_at=clock_timestamp()
			FROM decisions d
			WHERE h.candidate_id=d.candidate_id`, []any{memoryRedactionTombstone}},
		{"candidate payloads", `DELETE FROM memory_candidate_payloads`, nil},
		{"delivery payloads", `DELETE FROM memory_delivery_payloads`, nil},
		{"delivery receipt reasons", `UPDATE memory_delivery_receipts SET reason=$1 WHERE reason<>$1`, []any{memoryRedactionTombstone}},
		{"expiry reconciliation reasons", `UPDATE memory_expiry_reconciliations SET reason=NULL WHERE reason IS NOT NULL`, nil},
	}
	for _, statement := range statements {
		if _, err := tx.Exec(ctx, statement.sql, statement.args...); err != nil {
			return fmt.Errorf("scrub memory %s for erasure %s generation %d: %w", statement.name, request.ErasureID, request.LearnerGeneration, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit memory privacy scrub for erasure %s generation %d: %w", request.ErasureID, request.LearnerGeneration, err)
	}
	return nil
}

func (s *Store) VerifyRedacted(ctx context.Context, request privacy.LocalRedactionRequest) (int64, error) {
	if err := request.Validate(privacy.OwnerMemory); err != nil {
		return 0, fmt.Errorf("validate memory redaction verification request: %w", err)
	}
	var residual int64
	err := s.pool.QueryRow(ctx, `
		SELECT
		  (SELECT count(*) FROM memory_candidates WHERE reason<>$1) +
		  (SELECT count(*) FROM memory_candidate_decisions WHERE reason<>$1) +
		  (SELECT count(*) FROM memory_candidate_heads WHERE status='pending_review' OR payload_available) +
		  (SELECT count(*) FROM memory_candidate_payloads) +
		  (SELECT count(*) FROM memory_delivery_payloads) +
		  (SELECT count(*) FROM memory_delivery_receipts WHERE reason<>$1) +
		  (SELECT count(*) FROM memory_expiry_reconciliations WHERE reason IS NOT NULL) +
		  (SELECT count(*)
		   FROM memory_delivery_attempts a
		   JOIN memory_delivery_attempt_heads ah ON ah.attempt_id=a.id
		   JOIN memory_deliveries d ON d.id=a.delivery_id
		   LEFT JOIN memory_expiry_reconciliations r
		     ON r.delivery_id=a.delivery_id AND r.attempt_token=a.attempt_token
		   WHERE d.learner_generation<$2 AND ah.sent_at IS NOT NULL AND r.id IS NULL) +
		  (SELECT count(*)
		   FROM memory_delivery_attempts a
		   JOIN memory_delivery_attempt_heads ah ON ah.attempt_id=a.id
		   JOIN memory_deliveries d ON d.id=a.delivery_id
		   WHERE d.learner_generation<$2
		     AND ah.state IN ('prepared','sent','unknown','reconciling','confirmed')) +
		  (SELECT count(*)
		   FROM memory_deliveries d
		   JOIN memory_delivery_heads dh ON dh.delivery_id=d.id
		   WHERE d.learner_generation<$2 AND (
		     (EXISTS (
		       SELECT 1 FROM memory_expiry_reconciliations r
		       WHERE r.delivery_id=d.id AND r.status NOT IN ('absence_verified','verified')
		      ) AND NOT (dh.status='fenced' AND dh.terminal_disposition='privacy_erasure'))
		     OR
		     (NOT EXISTS (
		       SELECT 1 FROM memory_expiry_reconciliations r
		       WHERE r.delivery_id=d.id AND r.status NOT IN ('absence_verified','verified')
		      ) AND NOT (dh.status='deleted' AND dh.terminal_disposition='privacy_erasure'))
		   )) +
		  (SELECT count(*)
		   FROM memory_record_heads rh
		   JOIN memory_deliveries d ON d.id=rh.current_delivery_id
		   JOIN memory_delivery_heads dh ON dh.delivery_id=d.id
		   WHERE d.learner_generation<$2 AND (
		     (dh.status='fenced' AND rh.status<>'delete_pending')
		     OR (dh.status='deleted' AND (rh.status<>'deleted' OR rh.deleted_at IS NULL))
		   ))`, memoryRedactionTombstone, request.LearnerGeneration).Scan(&residual)
	if err != nil {
		return 0, fmt.Errorf("verify memory privacy scrub for erasure %s generation %d: %w", request.ErasureID, request.LearnerGeneration, err)
	}
	return residual, nil
}

var _ privacy.LocalOwnerPort = (*Store)(nil)
