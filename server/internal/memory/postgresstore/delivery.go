package postgresstore

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/memory"
	"github.com/edu-agent/edu-agent/server/internal/platform/outbox"
	outboxpostgres "github.com/edu-agent/edu-agent/server/internal/platform/outbox/postgresstore"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type lockedDelivery struct {
	dbNow                   time.Time
	generation              memory.Generation
	deliveryID              string
	deliveryKind            memory.DeliveryKind
	logicalMemoryID         string
	recordRevisionID        string
	recordRevision          int64
	learnerGeneration       int64
	recordGeneration        int64
	validUntil              time.Time
	outboxKey               string
	deliveryStatus          memory.DeliveryStatus
	currentAttemptID        *string
	currentRecordRevisionID string
	currentRecordRevision   int64
	currentRecordGeneration int64
	currentRecordDeliveryID string
	currentRecordStatus     memory.RecordStatus
}

func (v lockedDelivery) currentLineage() bool {
	return v.generation.WriteOpen &&
		v.learnerGeneration == v.generation.LearnerGeneration &&
		v.recordRevisionID == v.currentRecordRevisionID &&
		v.recordRevision == v.currentRecordRevision &&
		v.recordGeneration == v.currentRecordGeneration &&
		v.deliveryID == v.currentRecordDeliveryID
}

func (v lockedDelivery) activeStatusPair() bool {
	switch v.deliveryKind {
	case memory.DeliveryAdmit, memory.DeliveryCorrection:
		return v.deliveryStatus == memory.DeliveryStatusQueued && v.currentRecordStatus == memory.RecordQueued
	case memory.DeliveryDelete, memory.DeliveryErasure:
		return v.deliveryStatus == memory.DeliveryStatusDeletePending && v.currentRecordStatus == memory.RecordDeletePending
	default:
		return false
	}
}

func (v lockedDelivery) currentForMutation() bool {
	if !v.currentLineage() || !v.activeStatusPair() {
		return false
	}
	if v.deliveryKind == memory.DeliveryDelete || v.deliveryKind == memory.DeliveryErasure {
		return true
	}
	return v.dbNow.Before(v.validUntil)
}

func (v lockedDelivery) currentForFence() bool {
	return v.currentLineage() && v.activeStatusPair()
}

func lockDelivery(ctx context.Context, tx pgx.Tx, deliveryID string) (lockedDelivery, error) {
	var value lockedDelivery
	value.deliveryID = deliveryID
	generation, err := lockWritableGeneration(ctx, tx)
	if err != nil {
		return value, err
	}
	value.generation = generation
	err = tx.QueryRow(ctx, `
		SELECT clock_timestamp(),d.kind,d.logical_memory_id,d.record_revision_id,d.record_revision,d.learner_generation,
		       d.record_generation,d.valid_until,d.outbox_idempotency_key,h.status,h.current_attempt_id::text,
		       rh.current_record_revision_id,rh.current_revision,rh.record_generation,rh.current_delivery_id,rh.status
		FROM memory_deliveries d
		JOIN memory_delivery_heads h ON h.delivery_id=d.id
		JOIN memory_record_heads rh ON rh.logical_memory_id=d.logical_memory_id
		WHERE d.id=$1
		FOR UPDATE OF h,rh`, deliveryID).Scan(
		&value.dbNow, &value.deliveryKind, &value.logicalMemoryID, &value.recordRevisionID, &value.recordRevision, &value.learnerGeneration,
		&value.recordGeneration, &value.validUntil, &value.outboxKey, &value.deliveryStatus,
		&value.currentAttemptID, &value.currentRecordRevisionID, &value.currentRecordRevision,
		&value.currentRecordGeneration, &value.currentRecordDeliveryID, &value.currentRecordStatus,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return value, &memory.Error{Code: memory.CodeNotFound}
	}
	if err != nil {
		return value, fmt.Errorf("lock memory delivery protocol state: %w", err)
	}
	value.dbNow = value.dbNow.UTC()
	value.generation.UpdatedAt = value.generation.UpdatedAt.UTC()
	value.validUntil = value.validUntil.UTC()
	return value, nil
}

func (s *Store) ExpireCandidates(ctx context.Context, _ time.Time, limit int) (int, error) {
	limit = normalizeLimit(limit)
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return 0, fmt.Errorf("begin candidate expiry: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	// The read gate and lazy expiry share the same transaction with the sweep.
	if _, err := lockReadableGeneration(ctx, tx); err != nil {
		return 0, err
	}
	count, err := expireCandidatesTx(ctx, tx, "", limit)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit candidate expiry: %w", err)
	}
	return count, nil
}

func (s *Store) ExpireDeliveries(ctx context.Context, _ time.Time, limit int) (int, error) {
	limit = normalizeLimit(limit)
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return 0, fmt.Errorf("begin delivery expiry: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	generation, err := lockWritableGeneration(ctx, tx)
	if err != nil {
		return 0, err
	}
	rows, err := tx.Query(ctx, `
		SELECT d.id
		FROM memory_deliveries d
		JOIN memory_delivery_heads h ON h.delivery_id=d.id
		JOIN memory_record_heads rh ON rh.logical_memory_id=d.logical_memory_id
		WHERE d.kind IN ('admit','correction')
		  AND h.status='queued' AND rh.status='queued'
		  AND d.valid_until <= clock_timestamp()
		  AND d.learner_generation=$2
		  AND rh.current_record_revision_id=d.record_revision_id
		  AND rh.current_revision=d.record_revision
		  AND rh.record_generation=d.record_generation
		  AND rh.current_delivery_id=d.id
		ORDER BY d.valid_until,d.id
		FOR UPDATE OF h,rh SKIP LOCKED LIMIT $1`, limit, generation.LearnerGeneration)
	if err != nil {
		return 0, fmt.Errorf("claim expired deliveries: %w", err)
	}
	var deliveryIDs []string
	for rows.Next() {
		var deliveryID string
		if err := rows.Scan(&deliveryID); err != nil {
			rows.Close()
			return 0, err
		}
		deliveryIDs = append(deliveryIDs, deliveryID)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	rows.Close()
	for _, deliveryID := range deliveryIDs {
		locked, err := lockDelivery(ctx, tx, deliveryID)
		if err != nil {
			return 0, err
		}
		if err := expireDeliveryLocked(ctx, tx, locked); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit delivery expiry: %w", err)
	}
	return len(deliveryIDs), nil
}

func expireDeliveryLocked(ctx context.Context, tx pgx.Tx, locked lockedDelivery) error {
	if !locked.currentLineage() || locked.deliveryStatus != memory.DeliveryStatusQueued ||
		(locked.deliveryKind != memory.DeliveryAdmit && locked.deliveryKind != memory.DeliveryCorrection) ||
		locked.currentRecordStatus != memory.RecordQueued || locked.dbNow.Before(locked.validUntil) {
		return &memory.Error{Code: memory.CodeDeliveryConflict, Reason: "delivery_not_expirable"}
	}
	type sentAttempt struct {
		token     string
		bootEpoch string
	}
	attemptRows, err := tx.Query(ctx, `
		SELECT a.attempt_token,COALESCE(h.boot_epoch,''),h.sent_at IS NOT NULL
		FROM memory_delivery_attempts a
		JOIN memory_delivery_attempt_heads h ON h.attempt_id=a.id
		WHERE a.delivery_id=$1
		ORDER BY a.created_at,a.id
		FOR UPDATE OF h`, locked.deliveryID)
	if err != nil {
		return fmt.Errorf("lock expiry attempts: %w", err)
	}
	var sentAttempts []sentAttempt
	for attemptRows.Next() {
		var sent sentAttempt
		var wasSent bool
		if err := attemptRows.Scan(&sent.token, &sent.bootEpoch, &wasSent); err != nil {
			attemptRows.Close()
			return err
		}
		if wasSent {
			sentAttempts = append(sentAttempts, sent)
		}
	}
	if err := attemptRows.Err(); err != nil {
		attemptRows.Close()
		return err
	}
	attemptRows.Close()
	if err := cancelOutboxLocked(ctx, tx, locked.outboxKey, outbox.DispositionExpired, locked.dbNow); err != nil {
		return err
	}
	for _, sent := range sentAttempts {
		if _, err := tx.Exec(ctx, `
			INSERT INTO memory_expiry_reconciliations(
				id,delivery_id,logical_memory_id,external_uri,content_hash,attempt_token,
				sent_boot_epoch,learner_generation,record_generation,status,created_at,updated_at)
			SELECT $1,d.id,d.logical_memory_id,d.external_uri,d.payload_hash,$2,$3,
			       d.learner_generation,d.record_generation,'pending',$4,$4
			FROM memory_deliveries d WHERE d.id=$5
			ON CONFLICT(delivery_id,attempt_token) DO NOTHING`,
			uuid.NewString(), sent.token, sent.bootEpoch, locked.dbNow, locked.deliveryID); err != nil {
			return fmt.Errorf("preserve sent expiry reconciliation: %w", err)
		}
	}
	if _, err := tx.Exec(ctx, `
		UPDATE memory_delivery_attempt_heads
		SET state='fenced',lease_token=NULL,lease_expires_at=NULL,updated_at=$2
		WHERE delivery_id=$1 AND state IN ('prepared','sent','unknown','reconciling')`, locked.deliveryID, locked.dbNow); err != nil {
		return fmt.Errorf("fence expired delivery attempts: %w", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM memory_delivery_payloads WHERE delivery_id=$1`, locked.deliveryID); err != nil {
		return fmt.Errorf("scrub expired delivery payload: %w", err)
	}
	status := memory.DeliveryStatusExpired
	publicStatus := memory.DeliveryRejected
	disposition := string(outbox.DispositionExpired)
	receiptStatus := memory.ReceiptSucceeded
	reason := "delivery_expired_without_remote_send"
	recordStatus := memory.RecordPermanentlyRejected
	if len(sentAttempts) > 0 {
		status = memory.DeliveryStatusExpiryReconciling
		publicStatus = memory.DeliveryQueued
		disposition = ""
		receiptStatus = memory.ReceiptPartial
		reason = "expiry_reconciliation_pending"
		recordStatus = memory.RecordQueued
	}
	receipt, err := appendReceiptLocked(ctx, tx, locked.deliveryID, uuid.NewString(), receiptStatus,
		reason, "uri_hash_reconciliation", "", locked.dbNow)
	if err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, `
		UPDATE memory_delivery_heads
		SET status=$2,public_status=$3,terminal_disposition=NULLIF($4,''),attempt_state='fenced',
		    current_receipt_id=$5,current_receipt_version=$6,updated_at=$7
		WHERE delivery_id=$1 AND status='queued'`,
		locked.deliveryID, status, publicStatus, disposition, receipt.ID, receipt.Version, locked.dbNow)
	if err != nil {
		return fmt.Errorf("finalize delivery expiry head: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return &memory.Error{Code: memory.CodeDeliveryConflict}
	}
	tag, err = tx.Exec(ctx, `
		UPDATE memory_record_heads
		SET status=$2,receipt_id=$3,updated_at=$4
		WHERE logical_memory_id=$5 AND current_delivery_id=$1
		  AND current_record_revision_id=$6 AND current_revision=$7
		  AND record_generation=$8 AND status='queued'`,
		locked.deliveryID, recordStatus, receipt.ID, locked.dbNow, locked.logicalMemoryID,
		locked.recordRevisionID, locked.recordRevision, locked.recordGeneration)
	if err != nil {
		return fmt.Errorf("update expired record receipt: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return &memory.Error{Code: memory.CodeMemoryConflict, Reason: "expired_record_not_current"}
	}
	return nil
}

func (s *Store) ClaimAttempt(ctx context.Context, deliveryID string, _ time.Time, lease time.Duration) (memory.Attempt, error) {
	if !canonicalUUID(deliveryID) || lease <= 0 {
		return memory.Attempt{}, invalid("invalid_attempt_claim")
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return memory.Attempt{}, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	locked, err := lockDelivery(ctx, tx, deliveryID)
	if err != nil {
		return memory.Attempt{}, err
	}
	if !locked.currentForMutation() {
		return memory.Attempt{}, &memory.Error{Code: memory.CodeDeliveryConflict, Reason: "delivery_not_current"}
	}
	leaseToken := uuid.NewString()
	leaseExpiresAt := locked.dbNow.Add(lease)
	var existing memory.Attempt
	err = scanAttempt(tx.QueryRow(ctx, `
		SELECT a.id,a.delivery_id,a.attempt_token,h.state,COALESCE(h.lease_token::text,''),
		       COALESCE(h.lease_expires_at,'epoch'::timestamptz),COALESCE(h.boot_epoch,''),h.sent_at,h.unknown_at,
		       COALESCE(encode(h.result_digest,'hex'),''),COALESCE(h.error_category,''),a.created_at,h.updated_at
		FROM memory_delivery_attempts a
		JOIN memory_delivery_attempt_heads h ON h.attempt_id=a.id
		WHERE a.delivery_id=$1 AND h.state IN ('prepared','sent','unknown','reconciling')
		FOR UPDATE OF h`, deliveryID), &existing)
	if err == nil {
		if locked.currentAttemptID == nil || *locked.currentAttemptID != existing.ID {
			return memory.Attempt{}, &memory.Error{Code: memory.CodeDeliveryConflict, Reason: "attempt_not_current"}
		}
		if existing.LeaseExpiresAt.After(locked.dbNow) {
			return memory.Attempt{}, &memory.Error{Code: memory.CodeDeliveryConflict, Reason: "attempt_lease_active"}
		}
		if existing.State == memory.AttemptSent || existing.State == memory.AttemptUnknown || existing.State == memory.AttemptReconciling {
			previousState := existing.State
			tag, err := tx.Exec(ctx, `
				UPDATE memory_delivery_attempt_heads
				SET state='reconciling',lease_token=$2,lease_expires_at=$3,updated_at=$4
				WHERE attempt_id=$1 AND delivery_id=$5 AND state=$6 AND lease_expires_at <= $4`,
				existing.ID, leaseToken, leaseExpiresAt, locked.dbNow, deliveryID, previousState)
			if err != nil {
				return memory.Attempt{}, err
			}
			if tag.RowsAffected() != 1 {
				return memory.Attempt{}, outbox.ErrLeaseLost
			}
			tag, err = tx.Exec(ctx, `
				UPDATE memory_delivery_heads
				SET attempt_state='reconciling',updated_at=$3
				WHERE delivery_id=$1 AND current_attempt_id=$2 AND attempt_state=$4`,
				deliveryID, existing.ID, locked.dbNow, previousState)
			if err != nil {
				return memory.Attempt{}, err
			}
			if tag.RowsAffected() != 1 {
				return memory.Attempt{}, outbox.ErrLeaseLost
			}
			existing.State = memory.AttemptReconciling
			existing.LeaseToken = leaseToken
			existing.LeaseExpiresAt = leaseExpiresAt
			existing.UpdatedAt = locked.dbNow
			if err := tx.Commit(ctx); err != nil {
				return memory.Attempt{}, err
			}
			return existing, nil
		}
		if existing.State == memory.AttemptPrepared {
			tag, err := tx.Exec(ctx, `
				UPDATE memory_delivery_attempt_heads
				SET lease_token=$2,lease_expires_at=$3,updated_at=$4
				WHERE attempt_id=$1 AND state='prepared' AND lease_expires_at <= $4`,
				existing.ID, leaseToken, leaseExpiresAt, locked.dbNow)
			if err != nil {
				return memory.Attempt{}, err
			}
			if tag.RowsAffected() != 1 {
				return memory.Attempt{}, outbox.ErrLeaseLost
			}
			existing.LeaseToken = leaseToken
			existing.LeaseExpiresAt = leaseExpiresAt
			existing.UpdatedAt = locked.dbNow
			if err := tx.Commit(ctx); err != nil {
				return memory.Attempt{}, err
			}
			return existing, nil
		}
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return memory.Attempt{}, err
	}

	var previouslySent memory.Attempt
	err = scanAttempt(tx.QueryRow(ctx, `
		SELECT a.id,a.delivery_id,a.attempt_token,h.state,COALESCE(h.lease_token::text,''),
		       COALESCE(h.lease_expires_at,'epoch'::timestamptz),COALESCE(h.boot_epoch,''),h.sent_at,h.unknown_at,
		       COALESCE(encode(h.result_digest,'hex'),''),COALESCE(h.error_category,''),a.created_at,h.updated_at
		FROM memory_delivery_attempts a
		JOIN memory_delivery_attempt_heads h ON h.attempt_id=a.id
		WHERE a.delivery_id=$1 AND h.sent_at IS NOT NULL
		ORDER BY a.created_at DESC,a.id DESC LIMIT 1
		FOR UPDATE OF h`, deliveryID), &previouslySent)
	if err == nil {
		return memory.Attempt{}, &memory.Error{Code: memory.CodeDeliveryConflict, Reason: "sent_attempt_requires_reconciliation"}
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return memory.Attempt{}, err
	}

	attempt := memory.Attempt{
		ID: uuid.NewString(), DeliveryID: deliveryID, AttemptToken: uuid.NewString(), State: memory.AttemptPrepared,
		LeaseToken: leaseToken, LeaseExpiresAt: leaseExpiresAt, CreatedAt: locked.dbNow, UpdatedAt: locked.dbNow,
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO memory_delivery_attempts(id,delivery_id,attempt_token,created_at) VALUES($1,$2,$3,$4)`,
		attempt.ID, deliveryID, attempt.AttemptToken, locked.dbNow); err != nil {
		return memory.Attempt{}, err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO memory_delivery_attempt_heads(attempt_id,delivery_id,state,lease_token,lease_expires_at,updated_at)
		VALUES($1,$2,'prepared',$3,$4,$5)`, attempt.ID, deliveryID, leaseToken, leaseExpiresAt, locked.dbNow); err != nil {
		return memory.Attempt{}, err
	}
	tag, err := tx.Exec(ctx, `
		UPDATE memory_delivery_heads
		SET current_attempt_id=$2,attempt_state='prepared',attempt_count=attempt_count+1,updated_at=$3
		WHERE delivery_id=$1 AND status IN ('queued','delete_pending') AND current_attempt_id IS NULL`, deliveryID, attempt.ID, locked.dbNow)
	if err != nil {
		return memory.Attempt{}, err
	}
	if tag.RowsAffected() != 1 {
		return memory.Attempt{}, &memory.Error{Code: memory.CodeDeliveryConflict}
	}
	if err := tx.Commit(ctx); err != nil {
		return memory.Attempt{}, err
	}
	return attempt, nil
}

func (s *Store) ReleasePreparedAttempt(ctx context.Context, input memory.PreparedAttemptRelease) error {
	if uuid.Validate(input.AttemptID) != nil || uuid.Validate(input.AttemptToken) != nil || uuid.Validate(input.LeaseToken) != nil {
		return invalid("invalid prepared attempt release")
	}
	deliveryID, err := s.attemptDeliveryID(ctx, input.AttemptID)
	if err != nil {
		return err
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	locked, err := lockDelivery(ctx, tx, deliveryID)
	if err != nil {
		return err
	}
	attempt, err := lockAttempt(ctx, tx, input.AttemptID)
	if err != nil {
		return err
	}
	if locked.currentAttemptID == nil || *locked.currentAttemptID != attempt.ID ||
		attempt.AttemptToken != input.AttemptToken || attempt.LeaseToken != input.LeaseToken ||
		attempt.State != memory.AttemptPrepared || attempt.SentAt != nil {
		return outbox.ErrLeaseLost
	}
	tag, err := tx.Exec(ctx, `
UPDATE memory_delivery_attempt_heads
SET lease_expires_at=$2,error_category=NULLIF($3,''),updated_at=$2
WHERE attempt_id=$1 AND delivery_id=$4 AND state='prepared' AND lease_token=$5 AND sent_at IS NULL`,
		attempt.ID, locked.dbNow, input.ErrorCategory, deliveryID, input.LeaseToken)
	if err != nil {
		return fmt.Errorf("release prepared delivery attempt: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return outbox.ErrLeaseLost
	}
	return tx.Commit(ctx)
}

func (s *Store) TransitionAttempt(ctx context.Context, input memory.AttemptTransition) (memory.Attempt, error) {
	if !canonicalUUID(input.AttemptID) || !canonicalUUID(input.AttemptToken) || !canonicalUUID(input.LeaseToken) || input.At.IsZero() ||
		!((input.From == memory.AttemptPrepared && input.To == memory.AttemptSent) ||
			(input.From == memory.AttemptSent && (input.To == memory.AttemptUnknown || input.To == memory.AttemptReconciling)) ||
			(input.From == memory.AttemptUnknown && input.To == memory.AttemptReconciling)) {
		return memory.Attempt{}, invalid("invalid_attempt_transition")
	}
	deliveryID, err := s.attemptDeliveryID(ctx, input.AttemptID)
	if err != nil {
		return memory.Attempt{}, err
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return memory.Attempt{}, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	locked, err := lockDelivery(ctx, tx, deliveryID)
	if err != nil {
		return memory.Attempt{}, err
	}
	attempt, err := lockAttempt(ctx, tx, input.AttemptID)
	if err != nil {
		return memory.Attempt{}, err
	}
	if attempt.AttemptToken != input.AttemptToken || locked.currentAttemptID == nil ||
		*locked.currentAttemptID != input.AttemptID {
		return memory.Attempt{}, outbox.ErrLeaseLost
	}
	if locked.deliveryStatus == memory.DeliveryStatusExpiryReconciling {
		return memory.Attempt{}, &memory.Error{Code: memory.CodeDeliveryConflict, Reason: "expiry_reconciliation_pending"}
	}
	if locked.deliveryStatus == memory.DeliveryStatusQueued && !locked.dbNow.Before(locked.validUntil) {
		if attempt.LeaseToken != input.LeaseToken || attempt.State != input.From {
			return memory.Attempt{}, outbox.ErrLeaseLost
		}
		if err := expireDeliveryLocked(ctx, tx, locked); err != nil {
			return memory.Attempt{}, err
		}
		attempt, err = loadAttempt(ctx, tx, input.AttemptID)
		if err != nil {
			return memory.Attempt{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return memory.Attempt{}, err
		}
		return attempt, nil
	}
	if attempt.LeaseToken != input.LeaseToken || attempt.State != input.From ||
		!attempt.LeaseExpiresAt.After(locked.dbNow) {
		return memory.Attempt{}, outbox.ErrLeaseLost
	}
	if !locked.currentForMutation() {
		return memory.Attempt{}, &memory.Error{Code: memory.CodeDeliveryConflict, Reason: "delivery_not_current"}
	}
	bootEpoch := attempt.BootEpoch
	sentAt := attempt.SentAt
	unknownAt := attempt.UnknownAt
	if input.To == memory.AttemptSent {
		if input.BootEpoch == "" {
			return memory.Attempt{}, invalid("boot_epoch_required")
		}
		bootEpoch = input.BootEpoch
		sentAt = &locked.dbNow
	}
	if input.To == memory.AttemptUnknown {
		unknownAt = &locked.dbNow
	}
	var digest any
	if input.ResultDigest != "" {
		decoded, err := decodeHash(input.ResultDigest)
		if err != nil {
			return memory.Attempt{}, err
		}
		digest = decoded
	}
	tag, err := tx.Exec(ctx, `
		UPDATE memory_delivery_attempt_heads
		SET state=$2,boot_epoch=NULLIF($3,''),sent_at=$4,unknown_at=$5,
		    result_digest=COALESCE($6,result_digest),error_category=NULLIF($7,''),updated_at=$8
		WHERE attempt_id=$1 AND state=$9 AND lease_token=$10 AND lease_expires_at > $8`,
		input.AttemptID, input.To, bootEpoch, sentAt, unknownAt, digest, input.ErrorCategory,
		locked.dbNow, input.From, input.LeaseToken)
	if err != nil {
		return memory.Attempt{}, err
	}
	if tag.RowsAffected() != 1 {
		return memory.Attempt{}, outbox.ErrLeaseLost
	}
	if _, err := tx.Exec(ctx, `
		UPDATE memory_delivery_heads SET attempt_state=$2,updated_at=$3
		WHERE delivery_id=$1 AND current_attempt_id=$4`, deliveryID, input.To, locked.dbNow, input.AttemptID); err != nil {
		return memory.Attempt{}, err
	}
	attempt, err = loadAttempt(ctx, tx, input.AttemptID)
	if err != nil {
		return memory.Attempt{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return memory.Attempt{}, err
	}
	return attempt, nil
}

func attemptOutcomeMatchesDelivery(locked lockedDelivery, outcome memory.AttemptOutcomeKind) bool {
	switch locked.deliveryKind {
	case memory.DeliveryAdmit, memory.DeliveryCorrection:
		return locked.deliveryStatus == memory.DeliveryStatusQueued && locked.currentRecordStatus == memory.RecordQueued &&
			(outcome == memory.AttemptOutcomeApplied || outcome == memory.AttemptOutcomePermanentlyRejected || outcome == memory.AttemptOutcomeFenced)
	case memory.DeliveryDelete, memory.DeliveryErasure:
		return locked.deliveryStatus == memory.DeliveryStatusDeletePending && locked.currentRecordStatus == memory.RecordDeletePending &&
			(outcome == memory.AttemptOutcomeDeleted || outcome == memory.AttemptOutcomeFenced)
	default:
		return false
	}
}

func (s *Store) FinalizeAttempt(ctx context.Context, input memory.AttemptOutcome) (memory.Attempt, error) {
	if !canonicalUUID(input.AttemptID) || !canonicalUUID(input.AttemptToken) || !canonicalUUID(input.LeaseToken) ||
		!canonicalUUID(input.ReceiptID) || input.At.IsZero() {
		return memory.Attempt{}, invalid("invalid_attempt_outcome")
	}
	to := memory.AttemptConfirmed
	if input.Kind == memory.AttemptOutcomeFenced {
		to = memory.AttemptFenced
	}
	if input.Kind != memory.AttemptOutcomeApplied && input.Kind != memory.AttemptOutcomePermanentlyRejected &&
		input.Kind != memory.AttemptOutcomeFenced && input.Kind != memory.AttemptOutcomeDeleted {
		return memory.Attempt{}, invalid("invalid_attempt_outcome_kind")
	}
	if !memory.CanTransitionAttempt(input.From, to) {
		return memory.Attempt{}, invalid("invalid_attempt_outcome_transition")
	}
	if input.Kind == memory.AttemptOutcomeApplied && (!canonicalUUID(input.ExternalNodeID) || input.ExternalMemoryID <= 0 || input.ReceiptStatus != memory.ReceiptSucceeded) {
		return memory.Attempt{}, invalid("invalid_applied_outcome")
	}
	if input.Kind == memory.AttemptOutcomePermanentlyRejected && input.ReceiptStatus != memory.ReceiptFailed {
		return memory.Attempt{}, invalid("invalid_rejected_outcome")
	}
	if input.Kind == memory.AttemptOutcomeFenced && input.ReceiptStatus != memory.ReceiptPartial {
		return memory.Attempt{}, invalid("invalid_fenced_outcome")
	}
	if input.Kind == memory.AttemptOutcomeDeleted && input.ReceiptStatus != memory.ReceiptSucceeded {
		return memory.Attempt{}, invalid("invalid_deleted_outcome")
	}
	if input.Kind != memory.AttemptOutcomeApplied && (input.ExternalNodeID != "" || input.ExternalMemoryID != 0) {
		return memory.Attempt{}, invalid("unexpected_outcome_external_identity")
	}
	deliveryID, err := s.attemptDeliveryID(ctx, input.AttemptID)
	if err != nil {
		return memory.Attempt{}, err
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return memory.Attempt{}, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	locked, err := lockDelivery(ctx, tx, deliveryID)
	if err != nil {
		return memory.Attempt{}, err
	}
	attempt, err := lockAttempt(ctx, tx, input.AttemptID)
	if err != nil {
		return memory.Attempt{}, err
	}
	if attempt.AttemptToken != input.AttemptToken || locked.currentAttemptID == nil ||
		*locked.currentAttemptID != input.AttemptID {
		return memory.Attempt{}, outbox.ErrLeaseLost
	}
	if locked.deliveryStatus == memory.DeliveryStatusExpiryReconciling {
		return memory.Attempt{}, &memory.Error{Code: memory.CodeDeliveryConflict, Reason: "expiry_reconciliation_pending"}
	}
	if locked.deliveryStatus == memory.DeliveryStatusQueued && !locked.dbNow.Before(locked.validUntil) {
		if attempt.LeaseToken != input.LeaseToken || attempt.State != input.From {
			return memory.Attempt{}, outbox.ErrLeaseLost
		}
		if err := expireDeliveryLocked(ctx, tx, locked); err != nil {
			return memory.Attempt{}, err
		}
		attempt, err = loadAttempt(ctx, tx, input.AttemptID)
		if err != nil {
			return memory.Attempt{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return memory.Attempt{}, err
		}
		return attempt, nil
	}
	if attempt.LeaseToken != input.LeaseToken || attempt.State != input.From ||
		!attempt.LeaseExpiresAt.After(locked.dbNow) {
		return memory.Attempt{}, outbox.ErrLeaseLost
	}
	if !locked.currentForMutation() {
		return memory.Attempt{}, &memory.Error{Code: memory.CodeDeliveryConflict, Reason: "delivery_not_current"}
	}
	if !attemptOutcomeMatchesDelivery(locked, input.Kind) {
		return memory.Attempt{}, invalid("attempt_outcome_delivery_mismatch")
	}
	if input.Kind == memory.AttemptOutcomeFenced {
		attempt, err = fenceAttemptLocked(ctx, tx, locked, attempt, input.Reason)
		if err != nil {
			return memory.Attempt{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return memory.Attempt{}, err
		}
		return attempt, nil
	}
	if input.ResultDigest != "" {
		if _, err := decodeHash(input.ResultDigest); err != nil {
			return memory.Attempt{}, err
		}
	}
	receipt, err := appendReceiptLocked(ctx, tx, deliveryID, input.ReceiptID, input.ReceiptStatus,
		input.Reason, input.VerificationMethod, input.EvidenceDigest, locked.dbNow)
	if err != nil {
		return memory.Attempt{}, err
	}
	var resultDigest any
	if input.ResultDigest != "" {
		resultDigest = mustHash(input.ResultDigest)
	}
	tag, err := tx.Exec(ctx, `
		UPDATE memory_delivery_attempt_heads
		SET state='confirmed',lease_token=NULL,lease_expires_at=NULL,result_digest=COALESCE($2,result_digest),
		    error_category=NULLIF($3,''),updated_at=$4
		WHERE attempt_id=$1 AND state=$5 AND lease_token=$6 AND lease_expires_at > $4`,
		input.AttemptID, resultDigest, input.ErrorCategory, locked.dbNow, input.From, input.LeaseToken)
	if err != nil {
		return memory.Attempt{}, err
	}
	if tag.RowsAffected() != 1 {
		return memory.Attempt{}, outbox.ErrLeaseLost
	}
	if input.Kind == memory.AttemptOutcomeApplied {
		tag, err = tx.Exec(ctx, `
			INSERT INTO memory_record_external_refs(
				record_revision_id,delivery_id,external_node_id,external_memory_id,
				delivery_attempt_id,delivery_receipt_id,observed_at)
			VALUES($1,$2,$3,$4,$5,$6,$7)
			ON CONFLICT(record_revision_id) DO NOTHING`,
			locked.recordRevisionID, deliveryID, input.ExternalNodeID, input.ExternalMemoryID,
			input.AttemptID, receipt.ID, locked.dbNow)
		if err != nil {
			return memory.Attempt{}, fmt.Errorf("persist memory revision external reference: %w", err)
		}
		var storedDeliveryID, storedNodeID, storedAttemptID, storedReceiptID string
		var storedMemoryID int64
		if err := tx.QueryRow(ctx, `
			SELECT delivery_id,external_node_id,external_memory_id,delivery_attempt_id,delivery_receipt_id
			FROM memory_record_external_refs WHERE record_revision_id=$1`, locked.recordRevisionID).Scan(
			&storedDeliveryID, &storedNodeID, &storedMemoryID, &storedAttemptID, &storedReceiptID,
		); err != nil {
			return memory.Attempt{}, fmt.Errorf("verify memory revision external reference: %w", err)
		}
		if storedDeliveryID != deliveryID || storedNodeID != input.ExternalNodeID || storedMemoryID != input.ExternalMemoryID ||
			storedAttemptID != input.AttemptID || storedReceiptID != receipt.ID {
			return memory.Attempt{}, &memory.Error{Code: memory.CodeDeliveryConflict, Reason: "revision_external_reference_conflict"}
		}
	}
	deliveryStatus := memory.DeliveryStatusApplied
	publicStatus := memory.DeliveryApplied
	disposition := ""
	recordStatus := memory.RecordApplied
	switch input.Kind {
	case memory.AttemptOutcomePermanentlyRejected:
		deliveryStatus = memory.DeliveryStatusPermanentlyRejected
		publicStatus = memory.DeliveryRejected
		disposition = string(outbox.DispositionPermanentlyRejected)
		recordStatus = memory.RecordPermanentlyRejected
	case memory.AttemptOutcomeDeleted:
		deliveryStatus = memory.DeliveryStatusDeleted
		publicStatus = memory.DeliveryRejected
		disposition = "deleted"
		recordStatus = memory.RecordDeleted
	}
	tag, err = tx.Exec(ctx, `
		UPDATE memory_delivery_heads
		SET status=$2,public_status=$3,terminal_disposition=NULLIF($4,''),attempt_state='confirmed',
		    current_receipt_id=$5,current_receipt_version=$6,last_error_category=NULLIF($7,''),updated_at=$8
		WHERE delivery_id=$1 AND current_attempt_id=$9 AND status=$10`,
		deliveryID, deliveryStatus, publicStatus, disposition, receipt.ID, receipt.Version,
		input.ErrorCategory, locked.dbNow, input.AttemptID, locked.deliveryStatus)
	if err != nil {
		return memory.Attempt{}, err
	}
	if tag.RowsAffected() != 1 {
		return memory.Attempt{}, &memory.Error{Code: memory.CodeDeliveryConflict}
	}
	if _, err := tx.Exec(ctx, `DELETE FROM memory_delivery_payloads WHERE delivery_id=$1`, deliveryID); err != nil {
		return memory.Attempt{}, fmt.Errorf("scrub finalized delivery payload: %w", err)
	}
	var externalNode any
	var externalMemory any
	if input.ExternalNodeID != "" {
		externalNode = input.ExternalNodeID
	}
	if input.ExternalMemoryID > 0 {
		externalMemory = input.ExternalMemoryID
	}
	tag, err = tx.Exec(ctx, `
		UPDATE memory_record_heads
		SET status=$2,receipt_id=$3,external_node_id=COALESCE($4,external_node_id),
		    external_memory_id=COALESCE($5,external_memory_id),
		    applied_at=CASE WHEN $2='applied' THEN $6 ELSE applied_at END,
		    deleted_at=CASE WHEN $2='deleted' THEN $6 ELSE deleted_at END,updated_at=$6
		WHERE logical_memory_id=$1 AND current_record_revision_id=$7 AND current_revision=$8
		  AND record_generation=$9 AND current_delivery_id=$10 AND status=$11`,
		locked.logicalMemoryID, recordStatus, receipt.ID, externalNode, externalMemory, locked.dbNow,
		locked.recordRevisionID, locked.recordRevision, locked.recordGeneration, deliveryID, locked.currentRecordStatus)
	if err != nil {
		return memory.Attempt{}, err
	}
	if tag.RowsAffected() != 1 {
		return memory.Attempt{}, &memory.Error{Code: memory.CodeMemoryConflict}
	}
	attempt, err = loadAttempt(ctx, tx, input.AttemptID)
	if err != nil {
		return memory.Attempt{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return memory.Attempt{}, err
	}
	return attempt, nil
}

func (s *Store) PermanentlyRejectDelivery(ctx context.Context, input memory.PolicyRejection) error {
	if !canonicalUUID(input.DeliveryID) || !canonicalUUID(input.ReceiptID) || input.Reason == "" || input.ErrorCategory == "" || input.At.IsZero() {
		return invalid("invalid_policy_rejection")
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	locked, err := lockDelivery(ctx, tx, input.DeliveryID)
	if err != nil {
		return err
	}
	if !locked.currentForMutation() || locked.deliveryStatus != memory.DeliveryStatusQueued ||
		(locked.deliveryKind != memory.DeliveryAdmit && locked.deliveryKind != memory.DeliveryCorrection) {
		return &memory.Error{Code: memory.CodeDeliveryConflict, Reason: "delivery_not_policy_rejectable"}
	}
	var outboxStatus outbox.Status
	var outboxLease *string
	if err := tx.QueryRow(ctx, `SELECT status,lease_token::text FROM outbox_messages WHERE idempotency_key=$1 FOR UPDATE`, locked.outboxKey).Scan(&outboxStatus, &outboxLease); err != nil {
		return fmt.Errorf("lock policy rejection outbox: %w", err)
	}
	if outboxStatus == outbox.StatusProcessing {
		if outboxLease == nil || input.OutboxLeaseToken == "" || *outboxLease != input.OutboxLeaseToken {
			return outbox.ErrLeaseLost
		}
	} else if input.OutboxLeaseToken != "" || (outboxStatus != outbox.StatusPending && outboxStatus != outbox.StatusDead) {
		return outbox.ErrInvalidTransition
	}
	if err := cancelOutboxLocked(ctx, tx, locked.outboxKey, outbox.DispositionPermanentlyRejected, locked.dbNow); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE memory_delivery_attempt_heads
		SET state='fenced',lease_token=NULL,lease_expires_at=NULL,error_category=$2,updated_at=$3
		WHERE delivery_id=$1 AND state IN ('prepared','sent','unknown','reconciling')`, input.DeliveryID, input.ErrorCategory, locked.dbNow); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM memory_delivery_payloads WHERE delivery_id=$1`, input.DeliveryID); err != nil {
		return fmt.Errorf("scrub policy-rejected payload: %w", err)
	}
	receipt, err := appendReceiptLocked(ctx, tx, input.DeliveryID, input.ReceiptID, memory.ReceiptFailed,
		input.Reason, "adapter_policy_gate", "", locked.dbNow)
	if err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, `
		UPDATE memory_delivery_heads
		SET status='permanently_rejected',public_status='rejected',terminal_disposition='permanently_rejected',
		    attempt_state='fenced',current_receipt_id=$2,current_receipt_version=$3,last_error_category=$4,updated_at=$5
		WHERE delivery_id=$1 AND status='queued'`, input.DeliveryID, receipt.ID, receipt.Version, input.ErrorCategory, locked.dbNow)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return &memory.Error{Code: memory.CodeDeliveryConflict}
	}
	tag, err = tx.Exec(ctx, `
		UPDATE memory_record_heads SET status='permanently_rejected',receipt_id=$2,updated_at=$3
		WHERE logical_memory_id=$4 AND current_delivery_id=$1 AND current_record_revision_id=$5
		  AND current_revision=$6 AND record_generation=$7 AND status='queued'`,
		input.DeliveryID, receipt.ID, locked.dbNow, locked.logicalMemoryID, locked.recordRevisionID,
		locked.recordRevision, locked.recordGeneration)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return &memory.Error{Code: memory.CodeMemoryConflict}
	}
	return tx.Commit(ctx)
}

func (s *Store) FenceDelivery(ctx context.Context, deliveryID, disposition string, _ time.Time) error {
	valid := map[string]outbox.TerminalDisposition{
		"fenced": outbox.DispositionFenced, "superseded": outbox.DispositionSuperseded,
		"privacy_erasure": outbox.DispositionPrivacyErasure, "expired": outbox.DispositionExpired,
		"permanently_rejected": outbox.DispositionPermanentlyRejected,
	}
	outDisposition, ok := valid[disposition]
	if !ok || !canonicalUUID(deliveryID) {
		return invalid("invalid_fence_disposition")
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	locked, err := lockDelivery(ctx, tx, deliveryID)
	if err != nil {
		return err
	}
	if !locked.currentForFence() {
		return &memory.Error{Code: memory.CodeDeliveryConflict, Reason: "delivery_not_fenceable"}
	}
	attemptRows, err := tx.Query(ctx, `
		SELECT h.attempt_id
		FROM memory_delivery_attempt_heads h
		JOIN memory_delivery_attempts a ON a.id=h.attempt_id
		WHERE h.delivery_id=$1
		ORDER BY a.created_at,a.id
		FOR UPDATE OF h`, deliveryID)
	if err != nil {
		return fmt.Errorf("lock delivery attempts for fence: %w", err)
	}
	for attemptRows.Next() {
		var attemptID string
		if err := attemptRows.Scan(&attemptID); err != nil {
			attemptRows.Close()
			return err
		}
	}
	if err := attemptRows.Err(); err != nil {
		attemptRows.Close()
		return err
	}
	attemptRows.Close()
	if err := cancelOutboxLocked(ctx, tx, locked.outboxKey, outDisposition, locked.dbNow); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE memory_delivery_attempt_heads
		SET state='fenced',lease_token=NULL,lease_expires_at=NULL,updated_at=$2
		WHERE delivery_id=$1 AND state IN ('prepared','sent','unknown','reconciling')`, deliveryID, locked.dbNow); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM memory_delivery_payloads WHERE delivery_id=$1`, deliveryID); err != nil {
		return err
	}
	receipt, err := appendReceiptLocked(ctx, tx, deliveryID, uuid.NewString(), memory.ReceiptPartial,
		"delivery_"+disposition, "generation_fence", "", locked.dbNow)
	if err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, `
		UPDATE memory_delivery_heads
		SET status='fenced',public_status='rejected',terminal_disposition=$2,attempt_state='fenced',
		    current_receipt_id=$3,current_receipt_version=$4,updated_at=$5
		WHERE delivery_id=$1 AND status=$6`,
		deliveryID, disposition, receipt.ID, receipt.Version, locked.dbNow, locked.deliveryStatus)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return &memory.Error{Code: memory.CodeDeliveryConflict}
	}
	tag, err = tx.Exec(ctx, `
		UPDATE memory_record_heads SET status=$2,receipt_id=$3,updated_at=$4
		WHERE logical_memory_id=$5 AND current_delivery_id=$1
		  AND current_record_revision_id=$6 AND current_revision=$7
		  AND record_generation=$8 AND status=$9`,
		deliveryID, recordStatusAfterFence(locked.deliveryKind), receipt.ID, locked.dbNow,
		locked.logicalMemoryID, locked.recordRevisionID, locked.recordRevision,
		locked.recordGeneration, locked.currentRecordStatus)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return &memory.Error{Code: memory.CodeMemoryConflict, Reason: "fenced_record_not_current"}
	}
	return tx.Commit(ctx)
}

func recordStatusAfterFence(kind memory.DeliveryKind) memory.RecordStatus {
	if kind == memory.DeliveryDelete || kind == memory.DeliveryErasure {
		return memory.RecordApplied
	}
	return memory.RecordPermanentlyRejected
}

func (s *Store) ClaimExpiryReconciliation(ctx context.Context, _ time.Time, lease time.Duration) (memory.ExpiryReconciliation, error) {
	if lease <= 0 {
		return memory.ExpiryReconciliation{}, invalid("invalid_reconciliation_claim")
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return memory.ExpiryReconciliation{}, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := lockWritableGeneration(ctx, tx); err != nil {
		return memory.ExpiryReconciliation{}, err
	}
	// Lock order for every reconciliation mutation is generation gate -> delivery/record heads -> reconciliation row.
	// Candidate selection is deliberately lock-free; the ordered locks below revalidate ownership and lease state.
	var candidateID, deliveryID string
	if err := tx.QueryRow(ctx, `
		SELECT r.id,r.delivery_id
		FROM privacy_owner_generation_gates g
		JOIN memory_expiry_reconciliations r ON TRUE
		WHERE g.owner_kind='memory' AND g.write_open
		  AND (r.status='pending' OR (r.status IN ('reconciling','delete_pending') AND r.lease_expires_at <= clock_timestamp()))
		  AND NOT EXISTS (
		      SELECT 1 FROM memory_expiry_reconciliations active
		      WHERE active.delivery_id=r.delivery_id AND active.id<>r.id
		        AND active.status IN ('reconciling','delete_pending')
		        AND active.lease_expires_at > clock_timestamp()
		  )
		ORDER BY CASE WHEN r.status IN ('reconciling','delete_pending') THEN 0 ELSE 1 END,r.created_at,r.id
		LIMIT 1`).Scan(&candidateID, &deliveryID); errors.Is(err, pgx.ErrNoRows) {
		return memory.ExpiryReconciliation{}, &memory.Error{Code: memory.CodeNotFound}
	} else if err != nil {
		return memory.ExpiryReconciliation{}, fmt.Errorf("select expiry reconciliation: %w", err)
	}
	locked, err := lockDelivery(ctx, tx, deliveryID)
	if err != nil {
		return memory.ExpiryReconciliation{}, err
	}
	value, err := lockReconciliation(ctx, tx, candidateID)
	if err != nil {
		return memory.ExpiryReconciliation{}, err
	}
	if err := validateReconciliationCurrent(locked, value); err != nil {
		return memory.ExpiryReconciliation{}, err
	}
	var anotherClaimActive bool
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM memory_expiry_reconciliations
			WHERE delivery_id=$1 AND id<>$2 AND status IN ('reconciling','delete_pending')
			  AND lease_expires_at > $3
		)`, deliveryID, candidateID, locked.dbNow).Scan(&anotherClaimActive); err != nil {
		return memory.ExpiryReconciliation{}, err
	}
	if anotherClaimActive {
		return memory.ExpiryReconciliation{}, outbox.ErrLeaseLost
	}
	leaseToken := uuid.NewString()
	status := value.Status
	if status == memory.ReconciliationPending || status == memory.ReconciliationReconciling {
		status = memory.ReconciliationReconciling
	}
	tag, err := tx.Exec(ctx, `
		UPDATE memory_expiry_reconciliations
		SET status=$2,lease_token=$3,lease_expires_at=$4,updated_at=$5
		WHERE id=$1 AND (status='pending' OR lease_expires_at <= $5)`,
		value.ID, status, leaseToken, locked.dbNow.Add(lease), locked.dbNow)
	if err != nil {
		return memory.ExpiryReconciliation{}, err
	}
	if tag.RowsAffected() != 1 {
		return memory.ExpiryReconciliation{}, outbox.ErrLeaseLost
	}
	value.Status = status
	value.LeaseToken = leaseToken
	expires := locked.dbNow.Add(lease)
	value.LeaseExpiresAt = &expires
	value.UpdatedAt = locked.dbNow
	if err := tx.Commit(ctx); err != nil {
		return memory.ExpiryReconciliation{}, err
	}
	return value, nil
}

func (s *Store) TransitionExpiryReconciliation(ctx context.Context, input memory.ReconciliationTransition) (memory.ExpiryReconciliation, error) {
	if !canonicalUUID(input.ReconciliationID) || !canonicalUUID(input.LeaseToken) || input.At.IsZero() ||
		input.From != memory.ReconciliationReconciling || input.To != memory.ReconciliationDeletePending {
		return memory.ExpiryReconciliation{}, invalid("invalid_reconciliation_transition")
	}
	return s.mutateReconciliation(ctx, input.ReconciliationID, input.LeaseToken, func(
		ctx context.Context, tx pgx.Tx, locked lockedDelivery, value memory.ExpiryReconciliation,
	) (memory.ExpiryReconciliation, error) {
		if value.Status != input.From || value.LeaseExpiresAt == nil || !value.LeaseExpiresAt.After(locked.dbNow) {
			return value, outbox.ErrLeaseLost
		}
		tag, err := tx.Exec(ctx, `
			UPDATE memory_expiry_reconciliations SET status='delete_pending',updated_at=$2
			WHERE id=$1 AND status='reconciling' AND lease_token=$3 AND lease_expires_at > $2`,
			value.ID, locked.dbNow, input.LeaseToken)
		if err != nil {
			return value, err
		}
		if tag.RowsAffected() != 1 {
			return value, outbox.ErrLeaseLost
		}
		value.Status = memory.ReconciliationDeletePending
		value.UpdatedAt = locked.dbNow
		return value, nil
	})
}

func (s *Store) FinalizeExpiryReconciliation(ctx context.Context, input memory.ReconciliationFinalization) (memory.ExpiryReconciliation, error) {
	validPair := (input.From == memory.ReconciliationReconciling &&
		(input.Result == memory.ReconciliationAbsenceResult || input.Result == memory.ReconciliationConflictResult)) ||
		(input.From == memory.ReconciliationDeletePending &&
			(input.Result == memory.ReconciliationDeleteResult || input.Result == memory.ReconciliationConflictResult))
	if !canonicalUUID(input.ReconciliationID) || !canonicalUUID(input.LeaseToken) || !canonicalUUID(input.ReceiptID) || input.At.IsZero() || !validPair {
		return memory.ExpiryReconciliation{}, invalid("invalid_reconciliation_finalization")
	}
	if input.EvidenceDigest != "" {
		if _, err := decodeHash(input.EvidenceDigest); err != nil {
			return memory.ExpiryReconciliation{}, err
		}
	}
	return s.mutateReconciliation(ctx, input.ReconciliationID, input.LeaseToken, func(
		ctx context.Context, tx pgx.Tx, locked lockedDelivery, value memory.ExpiryReconciliation,
	) (memory.ExpiryReconciliation, error) {
		if value.Status != input.From || value.LeaseExpiresAt == nil || !value.LeaseExpiresAt.After(locked.dbNow) {
			return value, outbox.ErrLeaseLost
		}
		if input.Result == memory.ReconciliationConflictResult {
			tag, err := tx.Exec(ctx, `
				UPDATE memory_expiry_reconciliations
				SET status='conflict',lease_token=NULL,lease_expires_at=NULL,reason=$2,updated_at=$3
				WHERE id=$1 AND status=$4 AND lease_token=$5 AND lease_expires_at > $3`,
				value.ID, input.Reason, locked.dbNow, input.From, input.LeaseToken)
			if err != nil {
				return value, err
			}
			if tag.RowsAffected() != 1 {
				return value, outbox.ErrLeaseLost
			}
			receipt, err := appendReceiptLocked(ctx, tx, value.DeliveryID, input.ReceiptID, memory.ReceiptPartial,
				input.Reason, "expiry_remote_conflict", input.EvidenceDigest, locked.dbNow)
			if err != nil {
				return value, err
			}
			if _, err := tx.Exec(ctx, `
				UPDATE memory_delivery_heads
				SET current_receipt_id=$2,current_receipt_version=$3,updated_at=$4
				WHERE delivery_id=$1 AND status='expiry_reconciling'`,
				value.DeliveryID, receipt.ID, receipt.Version, locked.dbNow); err != nil {
				return value, err
			}
			if _, err := tx.Exec(ctx, `UPDATE memory_record_heads SET receipt_id=$2,updated_at=$3 WHERE current_delivery_id=$1`, value.DeliveryID, receipt.ID, locked.dbNow); err != nil {
				return value, err
			}
			value.Status = memory.ReconciliationConflict
			value.LeaseToken = ""
			value.LeaseExpiresAt = nil
			value.Reason = input.Reason
			value.UpdatedAt = locked.dbNow
			return value, nil
		}
		verifiedStatus := memory.ReconciliationVerified
		if input.Result == memory.ReconciliationAbsenceResult {
			verifiedStatus = memory.ReconciliationAbsenceVerified
		}
		tag, err := tx.Exec(ctx, `
			UPDATE memory_expiry_reconciliations
			SET status=$6,lease_token=NULL,lease_expires_at=NULL,reason=$2,updated_at=$3
			WHERE id=$1 AND status=$4 AND lease_token=$5 AND lease_expires_at > $3`,
			value.ID, input.Reason, locked.dbNow, input.From, input.LeaseToken, verifiedStatus)
		if err != nil {
			return value, err
		}
		if tag.RowsAffected() != 1 {
			return value, outbox.ErrLeaseLost
		}
		var remaining int
		if err := tx.QueryRow(ctx, `
			SELECT count(*) FROM memory_expiry_reconciliations
			WHERE delivery_id=$1 AND status NOT IN ('absence_verified','verified')`, value.DeliveryID).Scan(&remaining); err != nil {
			return value, err
		}
		receiptStatus := memory.ReceiptPartial
		reason := "expiry_reconciliation_remaining"
		if remaining == 0 {
			receiptStatus = memory.ReceiptSucceeded
			reason = string(input.Result)
		}
		receipt, err := appendReceiptLocked(ctx, tx, value.DeliveryID, input.ReceiptID, receiptStatus,
			reason, string(input.Result), input.EvidenceDigest, locked.dbNow)
		if err != nil {
			return value, err
		}
		if remaining == 0 {
			tag, err := tx.Exec(ctx, `
				UPDATE memory_delivery_heads
				SET status='expired',public_status='rejected',terminal_disposition='expired',
				    current_receipt_id=$2,current_receipt_version=$3,updated_at=$4
				WHERE delivery_id=$1 AND status='expiry_reconciling'`,
				value.DeliveryID, receipt.ID, receipt.Version, locked.dbNow)
			if err != nil {
				return value, err
			}
			if tag.RowsAffected() != 1 {
				return value, &memory.Error{Code: memory.CodeDeliveryConflict}
			}
			if _, err := tx.Exec(ctx, `
				UPDATE memory_record_heads
				SET status='permanently_rejected',receipt_id=$2,updated_at=$3
				WHERE current_delivery_id=$1 AND current_record_revision_id=$4 AND record_generation=$5`,
				value.DeliveryID, receipt.ID, locked.dbNow, locked.recordRevisionID, locked.recordGeneration); err != nil {
				return value, err
			}
		} else if _, err := tx.Exec(ctx, `
			UPDATE memory_delivery_heads
			SET current_receipt_id=$2,current_receipt_version=$3,updated_at=$4
			WHERE delivery_id=$1 AND status='expiry_reconciling'`,
			value.DeliveryID, receipt.ID, receipt.Version, locked.dbNow); err != nil {
			return value, err
		}
		value.Status = verifiedStatus
		value.LeaseToken = ""
		value.LeaseExpiresAt = nil
		value.Reason = input.Reason
		value.UpdatedAt = locked.dbNow
		return value, nil
	})
}

func (s *Store) mutateReconciliation(
	ctx context.Context,
	reconciliationID, leaseToken string,
	mutate func(context.Context, pgx.Tx, lockedDelivery, memory.ExpiryReconciliation) (memory.ExpiryReconciliation, error),
) (memory.ExpiryReconciliation, error) {
	var deliveryID string
	if err := s.pool.QueryRow(ctx, `SELECT delivery_id FROM memory_expiry_reconciliations WHERE id=$1`, reconciliationID).Scan(&deliveryID); errors.Is(err, pgx.ErrNoRows) {
		return memory.ExpiryReconciliation{}, &memory.Error{Code: memory.CodeNotFound}
	} else if err != nil {
		return memory.ExpiryReconciliation{}, err
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return memory.ExpiryReconciliation{}, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	locked, err := lockDelivery(ctx, tx, deliveryID)
	if err != nil {
		return memory.ExpiryReconciliation{}, err
	}
	value, err := lockReconciliation(ctx, tx, reconciliationID)
	if err != nil {
		return memory.ExpiryReconciliation{}, err
	}
	if value.LeaseToken != leaseToken {
		return memory.ExpiryReconciliation{}, outbox.ErrLeaseLost
	}
	if err := validateReconciliationCurrent(locked, value); err != nil {
		return memory.ExpiryReconciliation{}, err
	}
	value, err = mutate(ctx, tx, locked, value)
	if err != nil {
		return memory.ExpiryReconciliation{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return memory.ExpiryReconciliation{}, err
	}
	return value, nil
}

func validateReconciliationCurrent(locked lockedDelivery, value memory.ExpiryReconciliation) error {
	if !locked.currentLineage() || locked.generation.LearnerGeneration != value.LearnerGeneration ||
		locked.learnerGeneration != value.LearnerGeneration || locked.recordGeneration != value.RecordGeneration ||
		locked.currentRecordStatus != memory.RecordQueued || locked.currentRecordDeliveryID != value.DeliveryID ||
		locked.deliveryStatus != memory.DeliveryStatusExpiryReconciling {
		return &memory.Error{Code: memory.CodeDeliveryConflict, Reason: "reconciliation_not_current"}
	}
	return nil
}

func lockReconciliation(ctx context.Context, tx pgx.Tx, id string) (memory.ExpiryReconciliation, error) {
	var value memory.ExpiryReconciliation
	var contentHash []byte
	var leaseExpiresAt *time.Time
	err := tx.QueryRow(ctx, `
		SELECT id,delivery_id,logical_memory_id,external_uri,content_hash,attempt_token,sent_boot_epoch,
		       learner_generation,record_generation,status,COALESCE(lease_token::text,''),lease_expires_at,
		       COALESCE(reason,''),created_at,updated_at
		FROM memory_expiry_reconciliations WHERE id=$1 FOR UPDATE`, id).Scan(
		&value.ID, &value.DeliveryID, &value.LogicalMemoryID, &value.ExternalURI, &contentHash,
		&value.AttemptToken, &value.SentBootEpoch, &value.LearnerGeneration, &value.RecordGeneration,
		&value.Status, &value.LeaseToken, &leaseExpiresAt, &value.Reason, &value.CreatedAt, &value.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return value, &memory.Error{Code: memory.CodeNotFound}
	}
	if err != nil {
		return value, err
	}
	value.ContentHash = fmt.Sprintf("%x", contentHash)
	value.LeaseExpiresAt = leaseExpiresAt
	value.CreatedAt = value.CreatedAt.UTC()
	value.UpdatedAt = value.UpdatedAt.UTC()
	if value.LeaseExpiresAt != nil {
		expires := value.LeaseExpiresAt.UTC()
		value.LeaseExpiresAt = &expires
	}
	return value, nil
}

func (s *Store) attemptDeliveryID(ctx context.Context, attemptID string) (string, error) {
	var deliveryID string
	if err := s.pool.QueryRow(ctx, `SELECT delivery_id FROM memory_delivery_attempts WHERE id=$1`, attemptID).Scan(&deliveryID); errors.Is(err, pgx.ErrNoRows) {
		return "", &memory.Error{Code: memory.CodeNotFound}
	} else if err != nil {
		return "", err
	}
	return deliveryID, nil
}

func lockAttempt(ctx context.Context, tx pgx.Tx, id string) (memory.Attempt, error) {
	var value memory.Attempt
	err := scanAttempt(tx.QueryRow(ctx, `
		SELECT a.id,a.delivery_id,a.attempt_token,h.state,COALESCE(h.lease_token::text,''),
		       COALESCE(h.lease_expires_at,'epoch'::timestamptz),COALESCE(h.boot_epoch,''),h.sent_at,h.unknown_at,
		       COALESCE(encode(h.result_digest,'hex'),''),COALESCE(h.error_category,''),a.created_at,h.updated_at
		FROM memory_delivery_attempts a
		JOIN memory_delivery_attempt_heads h ON h.attempt_id=a.id
		WHERE a.id=$1 FOR UPDATE OF h`, id), &value)
	if errors.Is(err, pgx.ErrNoRows) {
		return value, &memory.Error{Code: memory.CodeNotFound}
	}
	return value, err
}

func fenceAttemptLocked(ctx context.Context, tx pgx.Tx, locked lockedDelivery, attempt memory.Attempt, reason string) (memory.Attempt, error) {
	if !locked.currentForFence() || locked.currentAttemptID == nil || *locked.currentAttemptID != attempt.ID {
		return memory.Attempt{}, &memory.Error{Code: memory.CodeDeliveryConflict, Reason: "delivery_not_fenceable"}
	}
	if reason == "" {
		reason = "delivery_fenced"
	}
	if err := cancelOutboxLocked(ctx, tx, locked.outboxKey, outbox.DispositionFenced, locked.dbNow); err != nil {
		return memory.Attempt{}, err
	}
	tag, err := tx.Exec(ctx, `
		UPDATE memory_delivery_attempt_heads
		SET state='fenced',lease_token=NULL,lease_expires_at=NULL,error_category=$2,updated_at=$3
		WHERE attempt_id=$1 AND state IN ('prepared','sent','unknown','reconciling')`, attempt.ID, reason, locked.dbNow)
	if err != nil {
		return memory.Attempt{}, err
	}
	if tag.RowsAffected() != 1 {
		return memory.Attempt{}, outbox.ErrLeaseLost
	}
	if _, err := tx.Exec(ctx, `DELETE FROM memory_delivery_payloads WHERE delivery_id=$1`, locked.deliveryID); err != nil {
		return memory.Attempt{}, err
	}
	receipt, err := appendReceiptLocked(ctx, tx, locked.deliveryID, uuid.NewString(), memory.ReceiptPartial,
		reason, "generation_fence", "", locked.dbNow)
	if err != nil {
		return memory.Attempt{}, err
	}
	tag, err = tx.Exec(ctx, `
		UPDATE memory_delivery_heads
		SET status='fenced',public_status='rejected',terminal_disposition='fenced',attempt_state='fenced',
		    current_receipt_id=$2,current_receipt_version=$3,last_error_category=$4,updated_at=$5
		WHERE delivery_id=$1 AND current_attempt_id=$6 AND status=$7`,
		locked.deliveryID, receipt.ID, receipt.Version, reason, locked.dbNow, attempt.ID, locked.deliveryStatus)
	if err != nil {
		return memory.Attempt{}, err
	}
	if tag.RowsAffected() != 1 {
		return memory.Attempt{}, &memory.Error{Code: memory.CodeDeliveryConflict}
	}
	tag, err = tx.Exec(ctx, `
		UPDATE memory_record_heads SET status=$2,receipt_id=$3,updated_at=$4
		WHERE logical_memory_id=$5 AND current_delivery_id=$1
		  AND current_record_revision_id=$6 AND current_revision=$7
		  AND record_generation=$8 AND status=$9`,
		locked.deliveryID, recordStatusAfterFence(locked.deliveryKind), receipt.ID, locked.dbNow,
		locked.logicalMemoryID, locked.recordRevisionID, locked.recordRevision,
		locked.recordGeneration, locked.currentRecordStatus)
	if err != nil {
		return memory.Attempt{}, err
	}
	if tag.RowsAffected() != 1 {
		return memory.Attempt{}, &memory.Error{Code: memory.CodeMemoryConflict, Reason: "fenced_record_not_current"}
	}
	return loadAttempt(ctx, tx, attempt.ID)
}

func appendReceiptLocked(
	ctx context.Context,
	tx pgx.Tx,
	deliveryID, receiptID string,
	status memory.ReceiptStatus,
	reason, verificationMethod, evidenceDigest string,
	createdAt time.Time,
) (memory.Receipt, error) {
	var version int64
	if err := tx.QueryRow(ctx, `
		SELECT r.version+1
		FROM memory_delivery_heads h
		JOIN memory_delivery_receipts r ON r.id=h.current_receipt_id
		WHERE h.delivery_id=$1`, deliveryID).Scan(&version); err != nil {
		return memory.Receipt{}, fmt.Errorf("read current delivery receipt version: %w", err)
	}
	receipt := memory.Receipt{
		ID: receiptID, DeliveryID: deliveryID, Version: version, Status: status, Reason: reason,
		VerificationMethod: verificationMethod, EvidenceDigest: evidenceDigest, CreatedAt: createdAt,
	}
	if err := receipt.Validate(); err != nil {
		return memory.Receipt{}, err
	}
	var evidence any
	if evidenceDigest != "" {
		evidence = mustHash(evidenceDigest)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO memory_delivery_receipts(
			id,delivery_id,version,status,reason,verification_method,evidence_digest,created_at)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, receipt.ID, receipt.DeliveryID, receipt.Version,
		receipt.Status, receipt.Reason, receipt.VerificationMethod, evidence, receipt.CreatedAt); err != nil {
		return memory.Receipt{}, fmt.Errorf("append delivery receipt: %w", err)
	}
	return receipt, nil
}

func cancelOutboxLocked(ctx context.Context, tx pgx.Tx, key string, disposition outbox.TerminalDisposition, at time.Time) error {
	var status outbox.Status
	var lease *string
	if err := tx.QueryRow(ctx, `
		SELECT status,lease_token::text FROM outbox_messages WHERE idempotency_key=$1 FOR UPDATE`, key).Scan(&status, &lease); err != nil {
		return fmt.Errorf("lock memory outbox intent: %w", err)
	}
	if status != outbox.StatusPending && status != outbox.StatusDead &&
		status != outbox.StatusProcessing && status != outbox.StatusCanceled {
		return outbox.ErrInvalidTransition
	}
	request := outbox.CancelRequest{IdempotencyKey: key, Disposition: disposition, CanceledAt: at}
	if lease != nil {
		request.LeaseToken = *lease
	}
	return outboxpostgres.CancelWith(ctx, tx, request)
}

func scanAttempt(row rowScanner, value *memory.Attempt) error {
	if err := row.Scan(&value.ID, &value.DeliveryID, &value.AttemptToken, &value.State, &value.LeaseToken,
		&value.LeaseExpiresAt, &value.BootEpoch, &value.SentAt, &value.UnknownAt, &value.ResultDigest,
		&value.ErrorCategory, &value.CreatedAt, &value.UpdatedAt); err != nil {
		return err
	}
	value.LeaseExpiresAt = value.LeaseExpiresAt.UTC()
	value.CreatedAt = value.CreatedAt.UTC()
	value.UpdatedAt = value.UpdatedAt.UTC()
	if value.SentAt != nil {
		at := value.SentAt.UTC()
		value.SentAt = &at
	}
	if value.UnknownAt != nil {
		at := value.UnknownAt.UTC()
		value.UnknownAt = &at
	}
	return nil
}

func loadAttempt(ctx context.Context, db DBTX, id string) (memory.Attempt, error) {
	var value memory.Attempt
	err := scanAttempt(db.QueryRow(ctx, `
		SELECT a.id,a.delivery_id,a.attempt_token,h.state,COALESCE(h.lease_token::text,''),
		       COALESCE(h.lease_expires_at,'epoch'::timestamptz),COALESCE(h.boot_epoch,''),h.sent_at,h.unknown_at,
		       COALESCE(encode(h.result_digest,'hex'),''),COALESCE(h.error_category,''),a.created_at,h.updated_at
		FROM memory_delivery_attempts a
		JOIN memory_delivery_attempt_heads h ON h.attempt_id=a.id WHERE a.id=$1`, id), &value)
	return value, err
}

func canonicalUUID(value string) bool {
	parsed, err := uuid.Parse(value)
	return err == nil && parsed.String() == value
}
