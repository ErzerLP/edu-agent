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

func lockMaintenanceAuthorization(ctx context.Context, tx pgx.Tx, auth memory.MaintenanceAuthorization) (time.Time, error) {
	if err := auth.Validate(); err != nil {
		return time.Time{}, err
	}
	var now time.Time
	err := tx.QueryRow(ctx, `
		SELECT clock_timestamp()
		FROM privacy_owner_generation_gates g
		JOIN privacy_erasures e ON e.id=g.active_erasure_id AND e.target_learner_generation=g.learner_generation
		JOIN privacy_erasure_heads eh ON eh.erasure_id=e.id AND eh.status<>'verified'
		JOIN privacy_erasure_receipt_heads rh ON rh.erasure_id=e.id
		  AND rh.store_kind IN ('nocturne_paths','nocturne_orphan_history') AND rh.current_receipt_id=$2
		JOIN privacy_erasure_step_receipts r ON r.id=rh.current_receipt_id AND r.erasure_id=e.id
		WHERE g.owner_kind='memory' AND g.active_erasure_id=$1 AND g.learner_generation=$3
		  AND NOT g.read_open AND NOT g.write_open AND r.status IN ('pending','partial','unknown')
		FOR UPDATE OF g,eh,rh`, auth.ErasureID, auth.ReceiptID, auth.TargetLearnerGeneration).Scan(&now)
	if errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, &memory.Error{Code: memory.CodePrivacyClearInProgress, Reason: "maintenance_authorization_not_current"}
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("lock maintenance reconciliation authorization: %w", err)
	}
	var permit string
	if err := tx.QueryRow(ctx, `SELECT privacy_begin_owner_scrub($1,$2,'memory',$3)::text`,
		auth.ErasureID, auth.TargetLearnerGeneration, auth.ReceiptID).Scan(&permit); err != nil {
		return time.Time{}, fmt.Errorf("authorize maintenance reconciliation mutation: %w", err)
	}
	return now.UTC(), nil
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

func (s *Store) ClaimMaintenanceExpiryReconciliation(ctx context.Context, auth memory.MaintenanceAuthorization, _ time.Time, lease time.Duration) (memory.ExpiryReconciliation, error) {
	if lease <= 0 {
		return memory.ExpiryReconciliation{}, invalid("invalid_maintenance_reconciliation_claim")
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return memory.ExpiryReconciliation{}, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	now, err := lockMaintenanceAuthorization(ctx, tx, auth)
	if err != nil {
		return memory.ExpiryReconciliation{}, err
	}
	var id, deliveryID string
	err = tx.QueryRow(ctx, `
		SELECT r.id,r.delivery_id
		FROM memory_expiry_reconciliations r
		JOIN memory_delivery_attempts attempt
		  ON attempt.delivery_id=r.delivery_id AND attempt.attempt_token=r.attempt_token
		LEFT JOIN memory_reconciliation_maintenance_claims c ON c.reconciliation_id=r.id
		LEFT JOIN privacy_erasure_step_receipts claimed_receipt ON claimed_receipt.id=c.receipt_id
		JOIN privacy_erasure_step_receipts requested_receipt ON requested_receipt.id=$3 AND requested_receipt.erasure_id=$2
		WHERE r.learner_generation<$1
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
		LIMIT 1`, auth.TargetLearnerGeneration, auth.ErasureID, auth.ReceiptID).Scan(&id, &deliveryID)
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
	if err := tx.Commit(ctx); err != nil {
		return memory.ExpiryReconciliation{}, err
	}
	return value, nil
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
	now, err := lockMaintenanceAuthorization(ctx, tx, auth)
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
	tag, err := tx.Exec(ctx, `UPDATE memory_expiry_reconciliations SET status='delete_pending',updated_at=$2 WHERE id=$1 AND status='reconciling' AND lease_token=$3 AND lease_expires_at>$2`, value.ID, now, input.LeaseToken)
	if err != nil {
		return memory.ExpiryReconciliation{}, err
	}
	if tag.RowsAffected() != 1 {
		return memory.ExpiryReconciliation{}, outbox.ErrLeaseLost
	}
	value.Status, value.UpdatedAt = memory.ReconciliationDeletePending, now
	if err := tx.Commit(ctx); err != nil {
		return memory.ExpiryReconciliation{}, err
	}
	return value, nil
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
	now, err := lockMaintenanceAuthorization(ctx, tx, auth)
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
		value.Status, value.LeaseToken, value.LeaseExpiresAt, value.Reason, value.UpdatedAt = status, "", nil, "", now
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
	value.Status, value.LeaseToken, value.LeaseExpiresAt, value.Reason, value.UpdatedAt = status, "", nil, "", now
	if err := tx.Commit(ctx); err != nil {
		return memory.ExpiryReconciliation{}, err
	}
	return value, nil
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
	now, err := lockMaintenanceAuthorization(ctx, tx, auth)
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
	tag, err := tx.Exec(ctx, `INSERT INTO memory_remote_delete_plans(id,delivery_id,node_uuid,external_uri,active_memory_id,review_cleanup_needed,snapshot_digest,created_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8) ON CONFLICT(delivery_id) DO NOTHING`, plan.ID, plan.DeliveryID, plan.NodeID, plan.ExternalURI, plan.ActiveMemoryID, plan.ReviewCleanupNeeded, mustHash(plan.SnapshotDigest), now)
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
	stored, err := loadRemoteDeletePlan(ctx, tx, plan.DeliveryID)
	if err != nil {
		return memory.RemoteDeletePlan{}, err
	}
	if stored.SnapshotDigest != plan.SnapshotDigest || stored.NodeID != plan.NodeID || stored.ActiveMemoryID != plan.ActiveMemoryID {
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
			SELECT TRUE AS authorized
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
			SELECT r.status
			FROM authorized_scope
			JOIN memory_expiry_reconciliations r ON r.learner_generation<$3
			LEFT JOIN memory_reconciliation_maintenance_claims c ON c.reconciliation_id=r.id
			LEFT JOIN privacy_erasure_step_receipts claimed_receipt ON claimed_receipt.id=c.receipt_id
			JOIN privacy_erasure_step_receipts requested_receipt ON requested_receipt.id=$2 AND requested_receipt.erasure_id=$1
			WHERE c.reconciliation_id IS NULL OR (
			      c.erasure_id=$1 AND c.target_learner_generation=$3
			      AND claimed_receipt.store_kind=requested_receipt.store_kind)
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
