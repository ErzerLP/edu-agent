package postgresstore

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/memory"
	"github.com/edu-agent/edu-agent/server/internal/platform/outbox"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func lockMaintenanceAuthorization(ctx context.Context, tx pgx.Tx, auth memory.MaintenanceAuthorization) (time.Time, string, error) {
	if err := auth.Validate(); err != nil {
		return time.Time{}, "", err
	}
	var now time.Time
	var storeKind string
	err := tx.QueryRow(ctx, `
		SELECT clock_timestamp(),r.store_kind
		FROM privacy_owner_generation_gates g
		JOIN privacy_erasures e ON e.id=g.active_erasure_id AND e.target_learner_generation=g.learner_generation
		JOIN privacy_erasure_heads eh ON eh.erasure_id=e.id AND eh.status<>'verified'
		JOIN privacy_erasure_receipt_heads rh ON rh.erasure_id=e.id
		  AND rh.store_kind IN ('nocturne_paths','nocturne_orphan_history') AND rh.current_receipt_id=$2
		JOIN privacy_erasure_step_receipts r ON r.id=rh.current_receipt_id AND r.erasure_id=e.id
		WHERE g.owner_kind='memory' AND g.active_erasure_id=$1 AND g.learner_generation=$3
		  AND NOT g.read_open AND NOT g.write_open AND r.status IN ('pending','partial','unknown')
		FOR UPDATE OF g,eh,rh`, auth.ErasureID, auth.ReceiptID, auth.TargetLearnerGeneration).Scan(&now, &storeKind)
	if errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, "", &memory.Error{Code: memory.CodePrivacyClearInProgress, Reason: "maintenance_authorization_not_current"}
	}
	if err != nil {
		return time.Time{}, "", fmt.Errorf("lock maintenance reconciliation authorization: %w", err)
	}
	var permit string
	if err := tx.QueryRow(ctx, `SELECT privacy_begin_owner_scrub($1,$2,'memory',$3)::text`,
		auth.ErasureID, auth.TargetLearnerGeneration, auth.ReceiptID).Scan(&permit); err != nil {
		return time.Time{}, "", fmt.Errorf("authorize maintenance reconciliation mutation: %w", err)
	}
	return now.UTC(), storeKind, nil
}

type maintenanceDeliveryState struct {
	status      memory.DeliveryStatus
	disposition string
}

func lockMaintenanceDelivery(ctx context.Context, tx pgx.Tx, deliveryID string, targetGeneration int64) (maintenanceDeliveryState, error) {
	var state maintenanceDeliveryState
	err := tx.QueryRow(ctx, `
		SELECT h.status,COALESCE(h.terminal_disposition,'')
		FROM memory_delivery_heads h
		JOIN memory_deliveries d ON d.id=h.delivery_id
		WHERE h.delivery_id=$1 AND d.learner_generation<$2
		FOR UPDATE OF h`, deliveryID, targetGeneration).Scan(&state.status, &state.disposition)
	if errors.Is(err, pgx.ErrNoRows) {
		return state, &memory.Error{Code: memory.CodeDeliveryConflict, Reason: "maintenance_delivery_not_old_generation"}
	}
	if err != nil {
		return state, fmt.Errorf("lock maintenance delivery head: %w", err)
	}
	var logicalMemoryID string
	err = tx.QueryRow(ctx, `
		SELECT logical_memory_id::text
		FROM memory_record_heads
		WHERE current_delivery_id=$1
		FOR UPDATE`, deliveryID).Scan(&logicalMemoryID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return state, fmt.Errorf("lock maintenance record head: %w", err)
	}
	if !((state.status == memory.DeliveryStatusFenced && state.disposition == "privacy_erasure") ||
		state.status == memory.DeliveryStatusExpiryReconciling) {
		return state, &memory.Error{Code: memory.CodeDeliveryConflict, Reason: "maintenance_delivery_not_privacy_pending"}
	}
	return state, nil
}

func maintenanceReconciliationDeliveryID(ctx context.Context, tx pgx.Tx, reconciliationID string) (string, error) {
	var deliveryID string
	if err := tx.QueryRow(ctx, `SELECT delivery_id FROM memory_expiry_reconciliations WHERE id=$1`, reconciliationID).Scan(&deliveryID); errors.Is(err, pgx.ErrNoRows) {
		return "", &memory.Error{Code: memory.CodeNotFound}
	} else if err != nil {
		return "", err
	}
	return deliveryID, nil
}

func materializeMaintenanceErasureDeliveries(ctx context.Context, tx pgx.Tx, auth memory.MaintenanceAuthorization, now time.Time) error {
	if _, err := tx.Exec(ctx, `
		INSERT INTO memory_erasure_deliveries(
			id,erasure_id,logical_memory_id,target_learner_generation,created_at)
		SELECT gen_random_uuid(),$1,reconciliation.logical_memory_id,$2,min(reconciliation.created_at)
		FROM memory_expiry_reconciliations reconciliation
		WHERE reconciliation.learner_generation<$2
		GROUP BY reconciliation.logical_memory_id
		ON CONFLICT(erasure_id,logical_memory_id) DO NOTHING`,
		auth.ErasureID, auth.TargetLearnerGeneration); err != nil {
		return fmt.Errorf("materialize maintenance erasure deliveries: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO memory_erasure_delivery_sources(erasure_delivery_id,reconciliation_id,bound_at)
		SELECT erasure_delivery.id,reconciliation.id,$3
		FROM memory_erasure_deliveries erasure_delivery
		JOIN memory_expiry_reconciliations reconciliation
		  ON reconciliation.logical_memory_id=erasure_delivery.logical_memory_id
		 AND reconciliation.learner_generation<erasure_delivery.target_learner_generation
		WHERE erasure_delivery.erasure_id=$1
		  AND erasure_delivery.target_learner_generation=$2
		ON CONFLICT(erasure_delivery_id,reconciliation_id) DO NOTHING`,
		auth.ErasureID, auth.TargetLearnerGeneration, now); err != nil {
		return fmt.Errorf("bind maintenance erasure delivery sources: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO memory_erasure_delivery_receipts(
			id,erasure_delivery_id,store_kind,version,status,reason,verification_method,created_at)
		SELECT gen_random_uuid(),erasure_delivery.id,scope.store_kind,1,
		       CASE WHEN EXISTS (
		         SELECT 1 FROM memory_erasure_delivery_sources source
		         JOIN memory_expiry_reconciliations reconciliation ON reconciliation.id=source.reconciliation_id
		         WHERE source.erasure_delivery_id=erasure_delivery.id
		           AND reconciliation.status NOT IN ('absence_verified','verified')
		       ) THEN 'pending' ELSE 'succeeded' END,
		       CASE WHEN EXISTS (
		         SELECT 1 FROM memory_erasure_delivery_sources source
		         JOIN memory_expiry_reconciliations reconciliation ON reconciliation.id=source.reconciliation_id
		         WHERE source.erasure_delivery_id=erasure_delivery.id
		           AND reconciliation.status NOT IN ('absence_verified','verified')
		       ) THEN 'remote_erasure_pending' ELSE 'remote_reconciliation_already_verified' END,
		       CASE WHEN EXISTS (
		         SELECT 1 FROM memory_erasure_delivery_sources source
		         JOIN memory_expiry_reconciliations reconciliation ON reconciliation.id=source.reconciliation_id
		         WHERE source.erasure_delivery_id=erasure_delivery.id
		           AND reconciliation.status NOT IN ('absence_verified','verified')
		       ) THEN 'not_yet_verified' ELSE 'existing_reconciliation_terminal_state' END,
		       $2
		FROM memory_erasure_deliveries erasure_delivery
		CROSS JOIN (VALUES ('nocturne_paths'),('nocturne_orphan_history')) AS scope(store_kind)
		WHERE erasure_delivery.erasure_id=$1
		  AND NOT EXISTS (
		    SELECT 1 FROM memory_erasure_delivery_scopes existing
		    WHERE existing.erasure_delivery_id=erasure_delivery.id
		      AND existing.store_kind=scope.store_kind
		  )`, auth.ErasureID, now); err != nil {
		return fmt.Errorf("initialize maintenance erasure delivery receipts: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO memory_erasure_delivery_scopes(
			erasure_delivery_id,store_kind,status,current_attempt_id,attempt_count,
			current_receipt_id,current_receipt_version,updated_at)
		SELECT receipt.erasure_delivery_id,receipt.store_kind,receipt.status,NULL,0,
		       receipt.id,receipt.version,receipt.created_at
		FROM memory_erasure_delivery_receipts receipt
		JOIN memory_erasure_deliveries erasure_delivery ON erasure_delivery.id=receipt.erasure_delivery_id
		WHERE erasure_delivery.erasure_id=$1 AND receipt.version=1
		ON CONFLICT(erasure_delivery_id,store_kind) DO NOTHING`, auth.ErasureID); err != nil {
		return fmt.Errorf("initialize maintenance erasure delivery scopes: %w", err)
	}
	return nil
}

func (s *Store) ClaimMaintenanceExpiryReconciliation(ctx context.Context, auth memory.MaintenanceAuthorization, _ time.Time, lease time.Duration) (memory.ExpiryReconciliation, error) {
	if lease <= 0 {
		return memory.ExpiryReconciliation{}, invalid("invalid_maintenance_reconciliation_claim")
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return memory.ExpiryReconciliation{}, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	now, storeKind, err := lockMaintenanceAuthorization(ctx, tx, auth)
	if err != nil {
		return memory.ExpiryReconciliation{}, err
	}
	if err := materializeMaintenanceErasureDeliveries(ctx, tx, auth, now); err != nil {
		return memory.ExpiryReconciliation{}, err
	}
	var id, deliveryID, erasureDeliveryID string
	err = tx.QueryRow(ctx, `
		SELECT r.id,r.delivery_id,erasure_delivery.id
		FROM memory_expiry_reconciliations r
		JOIN memory_delivery_attempts attempt
		  ON attempt.delivery_id=r.delivery_id AND attempt.attempt_token=r.attempt_token
		LEFT JOIN memory_reconciliation_maintenance_claims c ON c.reconciliation_id=r.id
		LEFT JOIN privacy_erasure_step_receipts claimed_receipt ON claimed_receipt.id=c.receipt_id
		JOIN privacy_erasure_step_receipts requested_receipt ON requested_receipt.id=$3 AND requested_receipt.erasure_id=$2
		JOIN memory_erasure_delivery_sources source ON source.reconciliation_id=r.id
		JOIN memory_erasure_deliveries erasure_delivery
		  ON erasure_delivery.id=source.erasure_delivery_id
		 AND erasure_delivery.erasure_id=$2
		 AND erasure_delivery.target_learner_generation=$1
		JOIN memory_erasure_delivery_scopes erasure_scope
		  ON erasure_scope.erasure_delivery_id=erasure_delivery.id
		 AND erasure_scope.store_kind=requested_receipt.store_kind
		WHERE r.learner_generation<$1
		  AND erasure_scope.status IN ('pending','reconciling','delete_pending')
		  AND (r.status='pending' OR (r.status IN ('reconciling','delete_pending') AND r.lease_expires_at<=clock_timestamp()))
		  AND (c.reconciliation_id IS NULL OR (
		       c.erasure_id=$2 AND c.target_learner_generation=$1
		       AND claimed_receipt.store_kind=requested_receipt.store_kind))
		  AND NOT EXISTS (
		       SELECT 1 FROM memory_expiry_reconciliations active
		       WHERE active.delivery_id=r.delivery_id AND active.id<>r.id
		         AND active.status IN ('reconciling','delete_pending')
		         AND active.lease_expires_at>clock_timestamp())
		  AND NOT EXISTS (
		       SELECT 1
		       FROM memory_expiry_reconciliations newer
		       JOIN memory_delivery_attempts newer_attempt
		         ON newer_attempt.delivery_id=newer.delivery_id
		        AND newer_attempt.attempt_token=newer.attempt_token
		       WHERE newer.logical_memory_id=r.logical_memory_id AND newer.id<>r.id
		         AND newer.learner_generation<$1
		         AND newer.status NOT IN ('absence_verified','verified')
		         AND (newer.record_generation,newer.learner_generation,newer_attempt.created_at,newer.id)
		             >(r.record_generation,r.learner_generation,attempt.created_at,r.id))
		ORDER BY r.record_generation DESC,r.learner_generation DESC,attempt.created_at DESC,r.id DESC
		LIMIT 1`, auth.TargetLearnerGeneration, auth.ErasureID, auth.ReceiptID).Scan(&id, &deliveryID, &erasureDeliveryID)
	if errors.Is(err, pgx.ErrNoRows) {
		return memory.ExpiryReconciliation{}, &memory.Error{Code: memory.CodeNotFound}
	}
	if err != nil {
		return memory.ExpiryReconciliation{}, fmt.Errorf("select maintenance reconciliation: %w", err)
	}
	if _, err := lockMaintenanceDelivery(ctx, tx, deliveryID, auth.TargetLearnerGeneration); err != nil {
		return memory.ExpiryReconciliation{}, err
	}
	value, err := lockReconciliation(ctx, tx, id)
	if err != nil {
		return memory.ExpiryReconciliation{}, err
	}
	if value.DeliveryID != deliveryID || value.LearnerGeneration >= auth.TargetLearnerGeneration {
		return memory.ExpiryReconciliation{}, &memory.Error{Code: memory.CodeDeliveryConflict, Reason: "maintenance_reconciliation_generation_changed"}
	}
	var anotherClaimActive bool
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS(
		  SELECT 1 FROM memory_expiry_reconciliations
		  WHERE delivery_id=$1 AND id<>$2 AND status IN ('reconciling','delete_pending')
		    AND lease_expires_at>$3
		)`, deliveryID, id, now).Scan(&anotherClaimActive); err != nil {
		return memory.ExpiryReconciliation{}, err
	}
	if anotherClaimActive {
		return memory.ExpiryReconciliation{}, outbox.ErrLeaseLost
	}
	status := value.Status
	if status == memory.ReconciliationPending || status == memory.ReconciliationReconciling {
		status = memory.ReconciliationReconciling
	}
	leaseToken := uuid.NewString()
	tag, err := tx.Exec(ctx, `
		UPDATE memory_expiry_reconciliations SET status=$2,lease_token=$3,lease_expires_at=$4,updated_at=$5
		WHERE id=$1 AND (status='pending' OR lease_expires_at<=$5)`, id, status, leaseToken, now.Add(lease), now)
	if err != nil {
		return memory.ExpiryReconciliation{}, err
	}
	if tag.RowsAffected() != 1 {
		return memory.ExpiryReconciliation{}, outbox.ErrLeaseLost
	}
	var bound bool
	err = tx.QueryRow(ctx, `
		WITH rebound AS (
		  INSERT INTO memory_reconciliation_maintenance_claims AS existing(
		    reconciliation_id,erasure_id,target_learner_generation,receipt_id,claimed_at)
		  VALUES($1,$2,$3,$4,$5)
		  ON CONFLICT(reconciliation_id) DO UPDATE
		  SET receipt_id=EXCLUDED.receipt_id,claimed_at=EXCLUDED.claimed_at
		  WHERE existing.erasure_id=EXCLUDED.erasure_id
		    AND existing.target_learner_generation=EXCLUDED.target_learner_generation
		    AND (SELECT store_kind FROM privacy_erasure_step_receipts WHERE id=existing.receipt_id)
		        =(SELECT store_kind FROM privacy_erasure_step_receipts WHERE id=EXCLUDED.receipt_id)
		  RETURNING TRUE
		)
		SELECT EXISTS(SELECT 1 FROM rebound)`, id, auth.ErasureID, auth.TargetLearnerGeneration, auth.ReceiptID, now).Scan(&bound)
	if err != nil || !bound {
		if err != nil {
			return memory.ExpiryReconciliation{}, err
		}
		return memory.ExpiryReconciliation{}, &memory.Error{Code: memory.CodeDeliveryConflict, Reason: "maintenance_claim_binding_conflict"}
	}
	value.Status, value.LeaseToken = status, leaseToken
	expires := now.Add(lease)
	value.LeaseExpiresAt, value.UpdatedAt = &expires, now
	if err := claimErasureDeliveryAttempt(ctx, tx, auth, storeKind, erasureDeliveryID, value, leaseToken, expires, now); err != nil {
		return memory.ExpiryReconciliation{}, err
	}
	value.ErasureDeliveryID = erasureDeliveryID
	if err := tx.Commit(ctx); err != nil {
		return memory.ExpiryReconciliation{}, err
	}
	return value, nil
}

func claimErasureDeliveryAttempt(
	ctx context.Context,
	tx pgx.Tx,
	auth memory.MaintenanceAuthorization,
	storeKind, erasureDeliveryID string,
	value memory.ExpiryReconciliation,
	leaseToken string,
	leaseExpiresAt, now time.Time,
) error {
	var scopeStatus string
	if err := tx.QueryRow(ctx, `
		SELECT status
		FROM memory_erasure_delivery_scopes
		WHERE erasure_delivery_id=$1 AND store_kind=$2
		FOR UPDATE`, erasureDeliveryID, storeKind).Scan(&scopeStatus); err != nil {
		return fmt.Errorf("lock memory erasure delivery scope: %w", err)
	}
	if scopeStatus != "pending" && scopeStatus != "reconciling" && scopeStatus != "delete_pending" {
		return &memory.Error{Code: memory.CodeDeliveryConflict, Reason: "erasure_delivery_scope_not_claimable"}
	}
	attemptID := uuid.NewString()
	if _, err := tx.Exec(ctx, `
		INSERT INTO memory_erasure_delivery_attempts(
			id,erasure_delivery_id,store_kind,reconciliation_id,attempt_token,created_at)
		VALUES($1,$2,$3,$4,$5,$6)
		ON CONFLICT(erasure_delivery_id,store_kind,reconciliation_id) DO NOTHING`,
		attemptID, erasureDeliveryID, storeKind, value.ID, uuid.NewString(), now); err != nil {
		return fmt.Errorf("create memory erasure delivery attempt: %w", err)
	}
	if err := tx.QueryRow(ctx, `
		SELECT id FROM memory_erasure_delivery_attempts
		WHERE erasure_delivery_id=$1 AND store_kind=$2 AND reconciliation_id=$3`,
		erasureDeliveryID, storeKind, value.ID).Scan(&attemptID); err != nil {
		return fmt.Errorf("load memory erasure delivery attempt: %w", err)
	}
	var activeAttemptID string
	var activeLeaseExpiresAt time.Time
	err := tx.QueryRow(ctx, `
		SELECT attempt_id,lease_expires_at
		FROM memory_erasure_delivery_attempt_heads
		WHERE erasure_delivery_id=$1 AND state IN ('reconciling','delete_pending')
		FOR UPDATE`, erasureDeliveryID).Scan(&activeAttemptID, &activeLeaseExpiresAt)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("lock active memory erasure delivery attempt: %w", err)
	}
	if err == nil && activeAttemptID != attemptID {
		return outbox.ErrLeaseLost
	}
	state := string(memory.ReconciliationReconciling)
	if value.Status == memory.ReconciliationDeletePending {
		state = string(memory.ReconciliationDeletePending)
	}
	var claimed bool
	err = tx.QueryRow(ctx, `
		WITH claimed AS (
		  INSERT INTO memory_erasure_delivery_attempt_heads AS existing(
		    attempt_id,erasure_delivery_id,store_kind,state,authorization_receipt_id,
		    lease_token,lease_expires_at,updated_at)
		  VALUES($1,$2,$3,$4,$5,$6,$7,$8)
		  ON CONFLICT(attempt_id) DO UPDATE
		  SET state=EXCLUDED.state,authorization_receipt_id=EXCLUDED.authorization_receipt_id,
		      lease_token=EXCLUDED.lease_token,lease_expires_at=EXCLUDED.lease_expires_at,
		      updated_at=EXCLUDED.updated_at
		  WHERE existing.erasure_delivery_id=EXCLUDED.erasure_delivery_id
		    AND existing.store_kind=EXCLUDED.store_kind
		    AND existing.state IN ('reconciling','delete_pending')
		    AND existing.lease_expires_at<=$8
		  RETURNING TRUE
		)
		SELECT EXISTS(SELECT 1 FROM claimed)`, attemptID, erasureDeliveryID, storeKind,
		state, auth.ReceiptID, leaseToken, leaseExpiresAt, now).Scan(&claimed)
	if err != nil {
		return fmt.Errorf("claim memory erasure delivery attempt: %w", err)
	}
	if !claimed {
		return outbox.ErrLeaseLost
	}
	tag, err := tx.Exec(ctx, `
		UPDATE memory_erasure_delivery_scopes
		SET status=$3,current_attempt_id=$4,attempt_count=attempt_count+1,updated_at=$5
		WHERE erasure_delivery_id=$1 AND store_kind=$2
		  AND status IN ('pending','reconciling','delete_pending')`,
		erasureDeliveryID, storeKind, state, attemptID, now)
	if err != nil {
		return fmt.Errorf("advance memory erasure delivery scope: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return &memory.Error{Code: memory.CodeDeliveryConflict, Reason: "erasure_delivery_scope_claim_lost"}
	}
	return nil
}

func lockErasureDeliveryPermit(
	ctx context.Context,
	tx pgx.Tx,
	auth memory.MaintenanceAuthorization,
	storeKind string,
	value memory.ExpiryReconciliation,
	leaseToken string,
	now time.Time,
) (string, string, error) {
	var erasureDeliveryID, attemptID string
	err := tx.QueryRow(ctx, `
		SELECT erasure_delivery.id,attempt.id
		FROM memory_erasure_delivery_sources source
		JOIN memory_erasure_deliveries erasure_delivery
		  ON erasure_delivery.id=source.erasure_delivery_id
		JOIN memory_erasure_delivery_scopes scope
		  ON scope.erasure_delivery_id=erasure_delivery.id AND scope.store_kind=$4
		JOIN memory_erasure_delivery_attempts attempt
		  ON attempt.id=scope.current_attempt_id
		 AND attempt.erasure_delivery_id=erasure_delivery.id
		 AND attempt.store_kind=scope.store_kind
		 AND attempt.reconciliation_id=source.reconciliation_id
		JOIN memory_erasure_delivery_attempt_heads attempt_head
		  ON attempt_head.attempt_id=attempt.id
		WHERE source.reconciliation_id=$1
		  AND erasure_delivery.erasure_id=$2
		  AND erasure_delivery.target_learner_generation=$3
		  AND attempt_head.authorization_receipt_id=$5
		  AND attempt_head.lease_token=$6
		  AND attempt_head.lease_expires_at>$7
		  AND attempt_head.state IN ('reconciling','delete_pending')
		FOR UPDATE OF scope,attempt_head`, value.ID, auth.ErasureID,
		auth.TargetLearnerGeneration, storeKind, auth.ReceiptID, leaseToken, now).Scan(
		&erasureDeliveryID, &attemptID,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", outbox.ErrLeaseLost
	}
	if err != nil {
		return "", "", fmt.Errorf("lock memory erasure delivery permit: %w", err)
	}
	return erasureDeliveryID, attemptID, nil
}

func validateMaintenanceClaim(ctx context.Context, tx pgx.Tx, auth memory.MaintenanceAuthorization, value memory.ExpiryReconciliation) error {
	var valid bool
	err := tx.QueryRow(ctx, `
		SELECT EXISTS(
		 SELECT 1 FROM memory_reconciliation_maintenance_claims c
		 WHERE c.reconciliation_id=$1 AND c.erasure_id=$2 AND c.receipt_id=$3
		   AND c.target_learner_generation=$4 AND $5<$4
		)`, value.ID, auth.ErasureID, auth.ReceiptID, auth.TargetLearnerGeneration, value.LearnerGeneration).Scan(&valid)
	if err != nil {
		return err
	}
	if !valid {
		return &memory.Error{Code: memory.CodeDeliveryConflict, Reason: "maintenance_claim_not_authorized"}
	}
	return nil
}

func (s *Store) TransitionMaintenanceExpiryReconciliation(ctx context.Context, auth memory.MaintenanceAuthorization, input memory.ReconciliationTransition) (memory.ExpiryReconciliation, error) {
	if !canonicalUUID(input.ReconciliationID) || !canonicalUUID(input.LeaseToken) || input.From != memory.ReconciliationReconciling || input.To != memory.ReconciliationDeletePending || input.At.IsZero() {
		return memory.ExpiryReconciliation{}, invalid("invalid_maintenance_reconciliation_transition")
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return memory.ExpiryReconciliation{}, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	now, storeKind, err := lockMaintenanceAuthorization(ctx, tx, auth)
	if err != nil {
		return memory.ExpiryReconciliation{}, err
	}
	deliveryID, err := maintenanceReconciliationDeliveryID(ctx, tx, input.ReconciliationID)
	if err != nil {
		return memory.ExpiryReconciliation{}, err
	}
	if _, err := lockMaintenanceDelivery(ctx, tx, deliveryID, auth.TargetLearnerGeneration); err != nil {
		return memory.ExpiryReconciliation{}, err
	}
	value, err := lockReconciliation(ctx, tx, input.ReconciliationID)
	if err != nil {
		return memory.ExpiryReconciliation{}, err
	}
	if err := validateMaintenanceClaim(ctx, tx, auth, value); err != nil {
		return memory.ExpiryReconciliation{}, err
	}
	if value.Status != input.From || value.LeaseToken != input.LeaseToken || value.LeaseExpiresAt == nil || !value.LeaseExpiresAt.After(now) {
		return memory.ExpiryReconciliation{}, outbox.ErrLeaseLost
	}
	erasureDeliveryID, erasureAttemptID, err := lockErasureDeliveryPermit(ctx, tx, auth, storeKind, value, input.LeaseToken, now)
	if err != nil {
		return memory.ExpiryReconciliation{}, err
	}
	tag, err := tx.Exec(ctx, `UPDATE memory_expiry_reconciliations SET status='delete_pending',updated_at=$2 WHERE id=$1 AND status='reconciling' AND lease_token=$3 AND lease_expires_at>$2`, value.ID, now, input.LeaseToken)
	if err != nil {
		return memory.ExpiryReconciliation{}, err
	}
	if tag.RowsAffected() != 1 {
		return memory.ExpiryReconciliation{}, outbox.ErrLeaseLost
	}
	tag, err = tx.Exec(ctx, `
		UPDATE memory_erasure_delivery_attempt_heads
		SET state='delete_pending',updated_at=$3
		WHERE attempt_id=$1 AND erasure_delivery_id=$2 AND state='reconciling'
		  AND lease_token=$4 AND lease_expires_at>$3`,
		erasureAttemptID, erasureDeliveryID, now, input.LeaseToken)
	if err != nil {
		return memory.ExpiryReconciliation{}, fmt.Errorf("advance memory erasure delivery attempt: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return memory.ExpiryReconciliation{}, outbox.ErrLeaseLost
	}
	tag, err = tx.Exec(ctx, `
		UPDATE memory_erasure_delivery_scopes
		SET status='delete_pending',updated_at=$3
		WHERE erasure_delivery_id=$1 AND store_kind=$2 AND current_attempt_id=$4
		  AND status='reconciling'`, erasureDeliveryID, storeKind, now, erasureAttemptID)
	if err != nil {
		return memory.ExpiryReconciliation{}, fmt.Errorf("advance memory erasure delivery scope: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return memory.ExpiryReconciliation{}, &memory.Error{Code: memory.CodeDeliveryConflict, Reason: "erasure_delivery_scope_transition_lost"}
	}
	value.Status, value.UpdatedAt = memory.ReconciliationDeletePending, now
	value.ErasureDeliveryID = erasureDeliveryID
	if err := tx.Commit(ctx); err != nil {
		return memory.ExpiryReconciliation{}, err
	}
	return value, nil
}

func appendErasureDeliveryReceipt(
	ctx context.Context,
	tx pgx.Tx,
	erasureDeliveryID, storeKind, status, reason, verificationMethod, evidenceDigest string,
	now time.Time,
) (string, int64, error) {
	var version int64
	if err := tx.QueryRow(ctx, `
		SELECT current_receipt_version+1
		FROM memory_erasure_delivery_scopes
		WHERE erasure_delivery_id=$1 AND store_kind=$2
		FOR UPDATE`, erasureDeliveryID, storeKind).Scan(&version); err != nil {
		return "", 0, fmt.Errorf("lock memory erasure delivery receipt version: %w", err)
	}
	receiptID := uuid.NewString()
	var evidence any
	if evidenceDigest != "" {
		evidence = mustHash(evidenceDigest)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO memory_erasure_delivery_receipts(
			id,erasure_delivery_id,store_kind,version,status,reason,
			verification_method,evidence_digest,created_at)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
		receiptID, erasureDeliveryID, storeKind, version, status, reason,
		verificationMethod, evidence, now); err != nil {
		return "", 0, fmt.Errorf("append memory erasure delivery receipt: %w", err)
	}
	return receiptID, version, nil
}

func finalizeErasureDeliveryPermit(
	ctx context.Context,
	tx pgx.Tx,
	erasureDeliveryID, erasureAttemptID, storeKind, leaseToken string,
	result memory.ReconciliationResult,
	evidenceDigest string,
	now time.Time,
) error {
	attemptState := "succeeded"
	scopeStatus := "pending"
	receiptStatus := "partial"
	reason := "remote_reconciliation_pending"
	if result == memory.ReconciliationConflictResult {
		attemptState = "conflict"
		scopeStatus = "conflict"
		reason = "remote_reconciliation_conflict"
	} else {
		var remaining int
		if err := tx.QueryRow(ctx, `
			SELECT count(*)
			FROM memory_erasure_delivery_sources source
			JOIN memory_expiry_reconciliations reconciliation
			  ON reconciliation.id=source.reconciliation_id
			WHERE source.erasure_delivery_id=$1
			  AND reconciliation.status NOT IN ('absence_verified','verified')`,
			erasureDeliveryID).Scan(&remaining); err != nil {
			return fmt.Errorf("count memory erasure delivery sources: %w", err)
		}
		if result == memory.ReconciliationDeleteResult || remaining == 0 {
			scopeStatus = "succeeded"
			receiptStatus = "succeeded"
			reason = "remote_logical_delete_verified"
		}
	}
	tag, err := tx.Exec(ctx, `
		UPDATE memory_erasure_delivery_attempt_heads
		SET state=$3,lease_token=NULL,lease_expires_at=NULL,updated_at=$4
		WHERE attempt_id=$1 AND erasure_delivery_id=$2
		  AND state IN ('reconciling','delete_pending') AND lease_token=$5 AND lease_expires_at>$4`,
		erasureAttemptID, erasureDeliveryID, attemptState, now, leaseToken)
	if err != nil {
		return fmt.Errorf("finalize memory erasure delivery attempt: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return outbox.ErrLeaseLost
	}
	stores := []string{storeKind}
	if result == memory.ReconciliationDeleteResult {
		stores = []string{"nocturne_paths", "nocturne_orphan_history"}
	}
	for _, currentStore := range stores {
		currentStatus := scopeStatus
		currentReceiptStatus := receiptStatus
		currentReason := reason
		if result == memory.ReconciliationDeleteResult {
			currentStatus = "succeeded"
			currentReceiptStatus = "succeeded"
			currentReason = "remote_logical_delete_verified"
		}
		receiptID, receiptVersion, err := appendErasureDeliveryReceipt(
			ctx, tx, erasureDeliveryID, currentStore, currentReceiptStatus, currentReason,
			"erasure_bound_remote_reconciliation", evidenceDigest, now,
		)
		if err != nil {
			return err
		}
		tag, err = tx.Exec(ctx, `
			UPDATE memory_erasure_delivery_scopes
			SET status=$3,current_receipt_id=$4,current_receipt_version=$5,updated_at=$6
			WHERE erasure_delivery_id=$1 AND store_kind=$2`,
			erasureDeliveryID, currentStore, currentStatus, receiptID, receiptVersion, now)
		if err != nil {
			return fmt.Errorf("finalize memory erasure delivery scope: %w", err)
		}
		if tag.RowsAffected() != 1 {
			return &memory.Error{Code: memory.CodeDeliveryConflict, Reason: "erasure_delivery_scope_finalization_lost"}
		}
	}
	return nil
}

func (s *Store) FinalizeMaintenanceExpiryReconciliation(ctx context.Context, auth memory.MaintenanceAuthorization, input memory.ReconciliationFinalization) (memory.ExpiryReconciliation, error) {
	validPair := input.From == memory.ReconciliationReconciling && (input.Result == memory.ReconciliationAbsenceResult || input.Result == memory.ReconciliationConflictResult) ||
		input.From == memory.ReconciliationDeletePending && (input.Result == memory.ReconciliationDeleteResult || input.Result == memory.ReconciliationConflictResult)
	if !canonicalUUID(input.ReconciliationID) || !canonicalUUID(input.LeaseToken) || !canonicalUUID(input.ReceiptID) || !validPair || input.At.IsZero() {
		return memory.ExpiryReconciliation{}, invalid("invalid_maintenance_reconciliation_finalization")
	}
	if input.EvidenceDigest != "" {
		if _, err := decodeHash(input.EvidenceDigest); err != nil {
			return memory.ExpiryReconciliation{}, err
		}
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return memory.ExpiryReconciliation{}, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	now, storeKind, err := lockMaintenanceAuthorization(ctx, tx, auth)
	if err != nil {
		return memory.ExpiryReconciliation{}, err
	}
	deliveryID, err := maintenanceReconciliationDeliveryID(ctx, tx, input.ReconciliationID)
	if err != nil {
		return memory.ExpiryReconciliation{}, err
	}
	if _, err := lockMaintenanceDelivery(ctx, tx, deliveryID, auth.TargetLearnerGeneration); err != nil {
		return memory.ExpiryReconciliation{}, err
	}
	value, err := lockReconciliation(ctx, tx, input.ReconciliationID)
	if err != nil {
		return memory.ExpiryReconciliation{}, err
	}
	if err := validateMaintenanceClaim(ctx, tx, auth, value); err != nil {
		return memory.ExpiryReconciliation{}, err
	}
	if value.DeliveryID != deliveryID || value.Status != input.From || value.LeaseToken != input.LeaseToken || value.LeaseExpiresAt == nil || !value.LeaseExpiresAt.After(now) {
		return memory.ExpiryReconciliation{}, outbox.ErrLeaseLost
	}
	erasureDeliveryID, erasureAttemptID, err := lockErasureDeliveryPermit(ctx, tx, auth, storeKind, value, input.LeaseToken, now)
	if err != nil {
		return memory.ExpiryReconciliation{}, err
	}
	status := memory.ReconciliationVerified
	if input.Result == memory.ReconciliationAbsenceResult {
		status = memory.ReconciliationAbsenceVerified
	}
	if input.Result == memory.ReconciliationConflictResult {
		status = memory.ReconciliationConflict
	}
	tag, err := tx.Exec(ctx, `
		UPDATE memory_expiry_reconciliations
		SET status=$2,lease_token=NULL,lease_expires_at=NULL,reason=NULL,updated_at=$3
		WHERE id=$1 AND status=$4 AND lease_token=$5 AND lease_expires_at>$3`,
		value.ID, status, now, input.From, input.LeaseToken)
	if err != nil {
		return memory.ExpiryReconciliation{}, err
	}
	if tag.RowsAffected() != 1 {
		return memory.ExpiryReconciliation{}, outbox.ErrLeaseLost
	}
	if input.Result == memory.ReconciliationDeleteResult {
		if _, err := tx.Exec(ctx, `
			UPDATE memory_expiry_reconciliations sibling
			SET status='absence_verified',lease_token=NULL,lease_expires_at=NULL,reason=NULL,updated_at=$5
			WHERE sibling.logical_memory_id=$1 AND sibling.id<>$2
			  AND sibling.learner_generation<$3
			  AND sibling.status IN ('pending','conflict','reconciling','delete_pending')
			  AND (
			    NOT EXISTS (
			      SELECT 1 FROM memory_reconciliation_maintenance_claims claim
			      WHERE claim.reconciliation_id=sibling.id)
			    OR EXISTS (
			      SELECT 1 FROM memory_reconciliation_maintenance_claims claim
			      WHERE claim.reconciliation_id=sibling.id AND claim.erasure_id=$4
			        AND claim.target_learner_generation=$3)
			  )`, value.LogicalMemoryID, value.ID, auth.TargetLearnerGeneration, auth.ErasureID, now); err != nil {
			return memory.ExpiryReconciliation{}, fmt.Errorf("converge purged maintenance reconciliations: %w", err)
		}
		rows, err := tx.Query(ctx, `
			SELECT h.delivery_id::text
			FROM memory_delivery_heads h
			JOIN memory_deliveries d ON d.id=h.delivery_id
			WHERE d.logical_memory_id=$1 AND d.learner_generation<$2
			  AND EXISTS (
			    SELECT 1
			    FROM memory_expiry_reconciliations r
			    LEFT JOIN memory_reconciliation_maintenance_claims claim
			      ON claim.reconciliation_id=r.id
			    WHERE r.delivery_id=d.id
			      AND (claim.reconciliation_id IS NULL OR (
			        claim.erasure_id=$3 AND claim.target_learner_generation=$2))
			  )
			  AND NOT EXISTS (
			    SELECT 1 FROM memory_expiry_reconciliations remaining
			    WHERE remaining.delivery_id=d.id
			      AND remaining.status NOT IN ('absence_verified','verified'))
			  AND ((h.status='fenced' AND h.terminal_disposition='privacy_erasure')
			       OR h.status='expiry_reconciling')
			ORDER BY d.record_generation DESC,d.created_at DESC,d.id DESC
			FOR UPDATE OF h`, value.LogicalMemoryID, auth.TargetLearnerGeneration, auth.ErasureID)
		if err != nil {
			return memory.ExpiryReconciliation{}, fmt.Errorf("lock purged maintenance deliveries: %w", err)
		}
		var deliveryIDs []string
		for rows.Next() {
			var convergedDeliveryID string
			if err := rows.Scan(&convergedDeliveryID); err != nil {
				rows.Close()
				return memory.ExpiryReconciliation{}, err
			}
			deliveryIDs = append(deliveryIDs, convergedDeliveryID)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return memory.ExpiryReconciliation{}, err
		}
		rows.Close()
		currentFinalized := false
		for _, convergedDeliveryID := range deliveryIDs {
			receiptID := uuid.NewString()
			if convergedDeliveryID == value.DeliveryID {
				receiptID = input.ReceiptID
				currentFinalized = true
			}
			receipt, err := appendReceiptLocked(ctx, tx, convergedDeliveryID, receiptID, memory.ReceiptSucceeded,
				memoryRedactionTombstone, "erasure_bound_remote_reconciliation", input.EvidenceDigest, now)
			if err != nil {
				return memory.ExpiryReconciliation{}, err
			}
			tag, err = tx.Exec(ctx, `
				UPDATE memory_delivery_heads
				SET status='deleted',public_status='rejected',terminal_disposition='privacy_erasure',
				    attempt_state='fenced',current_receipt_id=$2,current_receipt_version=$3,
				    last_error_category='privacy_erasure',updated_at=$4
				WHERE delivery_id=$1 AND (
				      (status='fenced' AND terminal_disposition='privacy_erasure')
				      OR status='expiry_reconciling')`, convergedDeliveryID, receipt.ID, receipt.Version, now)
			if err != nil {
				return memory.ExpiryReconciliation{}, err
			}
			if tag.RowsAffected() != 1 {
				return memory.ExpiryReconciliation{}, &memory.Error{Code: memory.CodeDeliveryConflict, Reason: "maintenance_delivery_finalization_lost"}
			}
			if _, err := tx.Exec(ctx, `
				UPDATE memory_record_heads
				SET status='deleted',receipt_id=$2,deleted_at=COALESCE(deleted_at,$3),updated_at=$3
				WHERE current_delivery_id=$1`, convergedDeliveryID, receipt.ID, now); err != nil {
				return memory.ExpiryReconciliation{}, err
			}
		}
		if !currentFinalized {
			return memory.ExpiryReconciliation{}, &memory.Error{Code: memory.CodeDeliveryConflict, Reason: "maintenance_current_delivery_not_converged"}
		}
		if err := finalizeErasureDeliveryPermit(ctx, tx, erasureDeliveryID, erasureAttemptID, storeKind,
			input.LeaseToken, input.Result, input.EvidenceDigest, now); err != nil {
			return memory.ExpiryReconciliation{}, err
		}
		value.Status, value.LeaseToken, value.LeaseExpiresAt, value.Reason, value.UpdatedAt = status, "", nil, "", now
		value.ErasureDeliveryID = erasureDeliveryID
		if err := tx.Commit(ctx); err != nil {
			return memory.ExpiryReconciliation{}, err
		}
		return value, nil
	}
	var remaining int
	if err := tx.QueryRow(ctx, `
		SELECT count(*) FROM memory_expiry_reconciliations
		WHERE delivery_id=$1 AND status NOT IN ('absence_verified','verified')`, value.DeliveryID).Scan(&remaining); err != nil {
		return memory.ExpiryReconciliation{}, err
	}
	receiptStatus := memory.ReceiptPartial
	if remaining == 0 {
		receiptStatus = memory.ReceiptSucceeded
	}
	receipt, err := appendReceiptLocked(ctx, tx, value.DeliveryID, input.ReceiptID, receiptStatus,
		memoryRedactionTombstone, "erasure_bound_remote_reconciliation", input.EvidenceDigest, now)
	if err != nil {
		return memory.ExpiryReconciliation{}, err
	}
	if remaining == 0 {
		tag, err = tx.Exec(ctx, `
			UPDATE memory_delivery_heads
			SET status='deleted',public_status='rejected',terminal_disposition='privacy_erasure',
			    attempt_state='fenced',current_receipt_id=$2,current_receipt_version=$3,
			    last_error_category='privacy_erasure',updated_at=$4
			WHERE delivery_id=$1 AND (
			      (status='fenced' AND terminal_disposition='privacy_erasure')
			      OR status='expiry_reconciling')`, value.DeliveryID, receipt.ID, receipt.Version, now)
		if err != nil {
			return memory.ExpiryReconciliation{}, err
		}
		if tag.RowsAffected() != 1 {
			return memory.ExpiryReconciliation{}, &memory.Error{Code: memory.CodeDeliveryConflict, Reason: "maintenance_delivery_finalization_lost"}
		}
		if _, err := tx.Exec(ctx, `
			UPDATE memory_record_heads
			SET status='deleted',receipt_id=$2,deleted_at=COALESCE(deleted_at,$3),updated_at=$3
			WHERE current_delivery_id=$1`, value.DeliveryID, receipt.ID, now); err != nil {
			return memory.ExpiryReconciliation{}, err
		}
	} else {
		if _, err := tx.Exec(ctx, `
			UPDATE memory_delivery_heads
			SET current_receipt_id=$2,current_receipt_version=$3,updated_at=$4
			WHERE delivery_id=$1 AND (
			      (status='fenced' AND terminal_disposition='privacy_erasure')
			      OR status='expiry_reconciling')`, value.DeliveryID, receipt.ID, receipt.Version, now); err != nil {
			return memory.ExpiryReconciliation{}, err
		}
		if _, err := tx.Exec(ctx, `
			UPDATE memory_record_heads SET receipt_id=$2,updated_at=$3
			WHERE current_delivery_id=$1 AND status='delete_pending'`, value.DeliveryID, receipt.ID, now); err != nil {
			return memory.ExpiryReconciliation{}, err
		}
	}
	if err := finalizeErasureDeliveryPermit(ctx, tx, erasureDeliveryID, erasureAttemptID, storeKind,
		input.LeaseToken, input.Result, input.EvidenceDigest, now); err != nil {
		return memory.ExpiryReconciliation{}, err
	}
	value.Status, value.LeaseToken, value.LeaseExpiresAt, value.Reason, value.UpdatedAt = status, "", nil, "", now
	value.ErasureDeliveryID = erasureDeliveryID
	if err := tx.Commit(ctx); err != nil {
		return memory.ExpiryReconciliation{}, err
	}
	return value, nil
}

func (s *Store) LoadMaintenanceRemoteDeletePlan(ctx context.Context, auth memory.MaintenanceAuthorization, erasureDeliveryID string) (memory.RemoteDeletePlan, error) {
	if !canonicalUUID(erasureDeliveryID) {
		return memory.RemoteDeletePlan{}, invalid("invalid_erasure_delivery_id")
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return memory.RemoteDeletePlan{}, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	_, storeKind, err := lockMaintenanceAuthorization(ctx, tx, auth)
	if err != nil {
		return memory.RemoteDeletePlan{}, err
	}
	var deliveryID string
	err = tx.QueryRow(ctx, `
		SELECT plan.delivery_id
		FROM memory_erasure_deliveries erasure_delivery
		JOIN memory_erasure_delivery_scopes scope
		  ON scope.erasure_delivery_id=erasure_delivery.id AND scope.store_kind=$3
		JOIN memory_remote_delete_plans plan ON plan.erasure_delivery_id=erasure_delivery.id
		WHERE erasure_delivery.id=$1 AND erasure_delivery.erasure_id=$2`,
		erasureDeliveryID, auth.ErasureID, storeKind).Scan(&deliveryID)
	if errors.Is(err, pgx.ErrNoRows) {
		return memory.RemoteDeletePlan{}, &memory.Error{Code: memory.CodeNotFound}
	}
	if err != nil {
		return memory.RemoteDeletePlan{}, fmt.Errorf("load maintenance remote delete plan identity: %w", err)
	}
	plan, err := loadRemoteDeletePlan(ctx, tx, deliveryID)
	if err != nil {
		return memory.RemoteDeletePlan{}, err
	}
	if plan.ErasureDeliveryID != erasureDeliveryID {
		return memory.RemoteDeletePlan{}, &memory.Error{Code: memory.CodeDeliveryConflict, Reason: "maintenance_delete_plan_erasure_mismatch"}
	}
	if err := tx.Commit(ctx); err != nil {
		return memory.RemoteDeletePlan{}, err
	}
	return plan, nil
}

func (s *Store) SaveMaintenanceRemoteDeletePlan(ctx context.Context, auth memory.MaintenanceAuthorization, plan memory.RemoteDeletePlan) (memory.RemoteDeletePlan, error) {
	if err := plan.Validate(); err != nil {
		return memory.RemoteDeletePlan{}, err
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return memory.RemoteDeletePlan{}, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	now, storeKind, err := lockMaintenanceAuthorization(ctx, tx, auth)
	if err != nil {
		return memory.RemoteDeletePlan{}, err
	}
	if _, err := lockMaintenanceDelivery(ctx, tx, plan.DeliveryID, auth.TargetLearnerGeneration); err != nil {
		return memory.RemoteDeletePlan{}, err
	}
	var reconciliationID string
	if err := tx.QueryRow(ctx, `
		SELECT r.id
		FROM memory_expiry_reconciliations r
		JOIN memory_reconciliation_maintenance_claims c ON c.reconciliation_id=r.id
		WHERE r.delivery_id=$1 AND r.status IN ('reconciling','delete_pending')
		  AND r.lease_expires_at>clock_timestamp()
		  AND c.erasure_id=$2 AND c.receipt_id=$3 AND c.target_learner_generation=$4`,
		plan.DeliveryID, auth.ErasureID, auth.ReceiptID, auth.TargetLearnerGeneration).Scan(&reconciliationID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return memory.RemoteDeletePlan{}, &memory.Error{Code: memory.CodeDeliveryConflict, Reason: "maintenance_delete_plan_not_claimed"}
		}
		return memory.RemoteDeletePlan{}, err
	}
	value, err := lockReconciliation(ctx, tx, reconciliationID)
	if err != nil {
		return memory.RemoteDeletePlan{}, err
	}
	if err := validateMaintenanceClaim(ctx, tx, auth, value); err != nil {
		return memory.RemoteDeletePlan{}, err
	}
	if value.Status != memory.ReconciliationReconciling && value.Status != memory.ReconciliationDeletePending ||
		value.LeaseExpiresAt == nil || !value.LeaseExpiresAt.After(now) {
		return memory.RemoteDeletePlan{}, &memory.Error{Code: memory.CodeDeliveryConflict, Reason: "maintenance_delete_plan_lease_lost"}
	}
	erasureDeliveryID, _, err := lockErasureDeliveryPermit(ctx, tx, auth, storeKind, value, value.LeaseToken, now)
	if err != nil {
		return memory.RemoteDeletePlan{}, err
	}
	if plan.ErasureDeliveryID != erasureDeliveryID {
		return memory.RemoteDeletePlan{}, &memory.Error{Code: memory.CodeDeliveryConflict, Reason: "maintenance_delete_plan_erasure_mismatch"}
	}
	if _, err := tx.Exec(ctx, `
		UPDATE memory_remote_delete_plans
		SET erasure_delivery_id=$2
		WHERE delivery_id=$1 AND erasure_delivery_id IS NULL`, plan.DeliveryID, erasureDeliveryID); err != nil {
		return memory.RemoteDeletePlan{}, fmt.Errorf("bind legacy maintenance remote delete plan: %w", err)
	}
	tag, err := tx.Exec(ctx, `
		INSERT INTO memory_remote_delete_plans(
			id,delivery_id,erasure_delivery_id,node_uuid,external_uri,active_memory_id,
			review_cleanup_needed,snapshot_digest,created_at)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)
		ON CONFLICT DO NOTHING`, plan.ID, plan.DeliveryID, erasureDeliveryID, plan.NodeID,
		plan.ExternalURI, plan.ActiveMemoryID, plan.ReviewCleanupNeeded, mustHash(plan.SnapshotDigest), now)
	if err != nil {
		return memory.RemoteDeletePlan{}, err
	}
	if tag.RowsAffected() == 1 {
		for _, id := range plan.MemoryIDs {
			if _, err := tx.Exec(ctx, `INSERT INTO memory_remote_delete_versions(plan_id,memory_id,was_active) VALUES($1,$2,$3)`, plan.ID, id, id == plan.ActiveMemoryID); err != nil {
				return memory.RemoteDeletePlan{}, err
			}
		}
		for _, ref := range plan.Paths {
			if _, err := tx.Exec(ctx, `INSERT INTO memory_remote_delete_paths(plan_id,namespace,domain,path,uri,is_alias) VALUES($1,$2,$3,$4,$5,$6)`, plan.ID, ref.Namespace, ref.Domain, ref.Path, ref.URI, ref.Alias); err != nil {
				return memory.RemoteDeletePlan{}, err
			}
		}
	}
	var storedDeliveryID string
	if err := tx.QueryRow(ctx, `
		SELECT delivery_id FROM memory_remote_delete_plans WHERE erasure_delivery_id=$1`,
		erasureDeliveryID).Scan(&storedDeliveryID); err != nil {
		return memory.RemoteDeletePlan{}, fmt.Errorf("load maintenance remote delete plan binding: %w", err)
	}
	stored, err := loadRemoteDeletePlan(ctx, tx, storedDeliveryID)
	if err != nil {
		return memory.RemoteDeletePlan{}, err
	}
	if stored.ErasureDeliveryID != erasureDeliveryID || stored.SnapshotDigest != plan.SnapshotDigest ||
		stored.NodeID != plan.NodeID || stored.ActiveMemoryID != plan.ActiveMemoryID {
		return memory.RemoteDeletePlan{}, &memory.Error{Code: memory.CodeDeliveryConflict, Reason: "maintenance_delete_snapshot_conflict"}
	}
	if err := tx.Commit(ctx); err != nil {
		return memory.RemoteDeletePlan{}, err
	}
	return stored, nil
}

func (s *Store) MaintenanceReconciliationSummary(ctx context.Context, auth memory.MaintenanceAuthorization) (memory.MaintenanceReconciliationSummary, error) {
	if err := auth.Validate(); err != nil {
		return memory.MaintenanceReconciliationSummary{}, err
	}
	var authorized bool
	var summary memory.MaintenanceReconciliationSummary
	err := s.pool.QueryRow(ctx, `
		WITH authorized_scope AS (
			SELECT receipt.store_kind
			FROM privacy_owner_generation_gates g
			JOIN privacy_erasures e
			  ON e.id=g.active_erasure_id AND e.target_learner_generation=g.learner_generation
			JOIN privacy_erasure_heads eh ON eh.erasure_id=e.id AND eh.status<>'verified'
			JOIN privacy_erasure_receipt_heads rh
			  ON rh.erasure_id=e.id
			 AND rh.store_kind IN ('nocturne_paths','nocturne_orphan_history')
			 AND rh.current_receipt_id=$2
			JOIN privacy_erasure_step_receipts receipt
			  ON receipt.id=rh.current_receipt_id AND receipt.erasure_id=e.id
			WHERE g.owner_kind='memory' AND g.active_erasure_id=$1 AND g.learner_generation=$3
			  AND NOT g.read_open AND NOT g.write_open
			  AND receipt.status IN ('pending','partial','unknown')
		), scoped AS (
			SELECT scope.status
			FROM authorized_scope authorized
			JOIN memory_erasure_deliveries erasure_delivery
			  ON erasure_delivery.erasure_id=$1
			 AND erasure_delivery.target_learner_generation=$3
			JOIN memory_erasure_delivery_scopes scope
			  ON scope.erasure_delivery_id=erasure_delivery.id
			 AND scope.store_kind=authorized.store_kind
		)
		SELECT EXISTS(SELECT 1 FROM authorized_scope),
		       (SELECT count(*) FROM scoped WHERE status IN ('pending','reconciling','delete_pending')),
		       (SELECT count(*) FROM scoped WHERE status='conflict')`,
		auth.ErasureID, auth.ReceiptID, auth.TargetLearnerGeneration).
		Scan(&authorized, &summary.Pending, &summary.Conflicts)
	if err != nil {
		return memory.MaintenanceReconciliationSummary{}, fmt.Errorf("summarize maintenance reconciliations: %w", err)
	}
	if !authorized {
		return memory.MaintenanceReconciliationSummary{}, &memory.Error{Code: memory.CodePrivacyClearInProgress, Reason: "maintenance_authorization_not_current"}
	}
	return summary, nil
}

var _ memory.MaintenanceReconciliationPersistence = (*Store)(nil)
