package postgresstore

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/privacy"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func (s *Store) CurrentOfflineDevicePurge(ctx context.Context, deviceID string) (privacy.OfflinePurgeChallenge, bool, error) {
	if s.challengeKeys == nil {
		return privacy.OfflinePurgeChallenge{}, false, &privacy.Error{Code: privacy.CodeOfflineChallengeUnavailable, Reason: "offline_purge_challenge_keyring_unavailable"}
	}
	if uuid.Validate(deviceID) != nil {
		return privacy.OfflinePurgeChallenge{}, false, &privacy.Error{Code: privacy.CodeInvalidRequest, Reason: "invalid_device_id"}
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return privacy.OfflinePurgeChallenge{}, false, fmt.Errorf("begin current offline purge challenge: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	var value privacy.OfflinePurgeChallenge
	var childID, revisionID string
	var stateRevision int64
	var keyVersion int
	var storedHash []byte
	err = tx.QueryRow(ctx, `
		SELECT c.id::text,r.id::text,r.revision,c.erasure_id::text,c.device_id::text,c.source_generation,e.target_learner_generation,
		       r.challenge_revision,r.challenge_key_version,r.challenge_hash,r.issued_at,r.status
		FROM privacy_offline_device_children c
		JOIN privacy_erasures e ON e.id=c.erasure_id
		JOIN privacy_offline_device_child_heads h ON h.child_id=c.id
		JOIN privacy_offline_device_child_revisions r ON r.id=h.current_revision_id
		WHERE c.device_id=$1 AND r.status IN ('pending','failed','unknown')
		ORDER BY e.requested_at DESC,c.erasure_id DESC
		LIMIT 1
		FOR UPDATE OF h`, deviceID).Scan(
		&childID, &revisionID, &stateRevision, &value.ErasureID, &value.DeviceID, &value.OldGeneration, &value.CurrentGeneration,
		&value.ChallengeRevision, &keyVersion, &storedHash, &value.IssuedAt, &value.Status,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return privacy.OfflinePurgeChallenge{}, false, nil
	}
	if err != nil {
		return privacy.OfflinePurgeChallenge{}, false, fmt.Errorf("load current offline purge challenge: %w", err)
	}
	if value.Status == privacy.OfflineDeviceChildFailed || value.Status == privacy.OfflineDeviceChildUnknown {
		now := time.Now().UTC()
		value.ChallengeRevision++
		keyVersion = s.challengeKeys.CurrentVersion()
		value.Challenge, err = s.challengeKeys.Challenge(keyVersion, value.ErasureID, value.DeviceID, value.OldGeneration, value.CurrentGeneration, value.ChallengeRevision)
		if err != nil {
			return privacy.OfflinePurgeChallenge{}, false, err
		}
		digest := privacy.OfflineChallengeDigest(value.Challenge)
		newRevisionID := uuid.NewString()
		if _, err := tx.Exec(ctx, `
			INSERT INTO privacy_offline_device_child_revisions(
				id,child_id,revision,challenge_revision,status,challenge_key_version,challenge_hash,issued_at,stable_reason,updated_at)
			VALUES($1,$2,$3,$4,'pending',$5,$6,$7,'offline_device_purge_retry_issued',$7)`,
			newRevisionID, childID, stateRevision+1, value.ChallengeRevision, keyVersion, digest[:], now); err != nil {
			return privacy.OfflinePurgeChallenge{}, false, fmt.Errorf("append offline purge retry challenge: %w", err)
		}
		if tag, err := tx.Exec(ctx, `
			UPDATE privacy_offline_device_child_heads
			SET current_revision_id=$2,current_revision=$3,status='pending',updated_at=$4
			WHERE child_id=$1 AND current_revision_id=$5`, childID, newRevisionID, stateRevision+1, now, revisionID); err != nil {
			return privacy.OfflinePurgeChallenge{}, false, fmt.Errorf("advance offline purge retry challenge: %w", err)
		} else if tag.RowsAffected() != 1 {
			return privacy.OfflinePurgeChallenge{}, false, &privacy.Error{Code: privacy.CodeOfflinePurgeNotCurrent, Reason: "offline_purge_receipt_changed"}
		}
		if err := appendOfflineDeviceStepTx(ctx, tx, value.ErasureID, privacy.StepPending, "offline_device_ack_pending", nil, now); err != nil {
			return privacy.OfflinePurgeChallenge{}, false, err
		}
		storedHash = digest[:]
		value.IssuedAt = now
		value.Status = privacy.OfflineDeviceChildPending
	} else {
		value.Challenge, err = s.challengeKeys.Challenge(keyVersion, value.ErasureID, value.DeviceID, value.OldGeneration, value.CurrentGeneration, value.ChallengeRevision)
		if err != nil {
			return privacy.OfflinePurgeChallenge{}, false, err
		}
	}
	digest := privacy.OfflineChallengeDigest(value.Challenge)
	if len(storedHash) != len(digest) || subtle.ConstantTimeCompare(storedHash, digest[:]) != 1 {
		return privacy.OfflinePurgeChallenge{}, false, &privacy.Error{Code: privacy.CodeVerificationFailed, Reason: "offline_purge_challenge_integrity_failed"}
	}
	if err := tx.Commit(ctx); err != nil {
		return privacy.OfflinePurgeChallenge{}, false, fmt.Errorf("commit current offline purge challenge: %w", err)
	}
	return value, true, nil
}

func (s *Store) AcknowledgeOfflineDevicePurge(ctx context.Context, erasureID, deviceID string, acknowledgment privacy.OfflineDevicePurgeAcknowledgment) (privacy.OfflineDeviceChildReceipt, error) {
	if s.challengeKeys == nil {
		return privacy.OfflineDeviceChildReceipt{}, &privacy.Error{Code: privacy.CodeOfflineChallengeUnavailable, Reason: "offline_purge_challenge_keyring_unavailable"}
	}
	if uuid.Validate(erasureID) != nil || uuid.Validate(deviceID) != nil {
		return privacy.OfflineDeviceChildReceipt{}, &privacy.Error{Code: privacy.CodeInvalidRequest, Reason: "invalid_offline_device_ack_identity"}
	}
	if err := acknowledgment.Validate(); err != nil {
		return privacy.OfflineDeviceChildReceipt{}, err
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return privacy.OfflineDeviceChildReceipt{}, fmt.Errorf("begin offline device acknowledgment: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var childID, revisionID string
	var stateRevision int64
	var current privacy.OfflineDeviceChildReceipt
	var keyVersion int
	var challengeHash []byte
	var receiptStatus privacy.ErasureStatus
	var issuedAt time.Time
	if err := tx.QueryRow(ctx, `
		SELECT c.id::text,r.id::text,r.revision,c.erasure_id::text,c.device_id::text,c.source_generation,e.target_learner_generation,
		       r.challenge_revision,r.status,r.challenge_key_version,r.challenge_hash,r.issued_at,r.updated_at,r.stable_reason,eh.status
		FROM privacy_offline_device_children c
		JOIN privacy_erasures e ON e.id=c.erasure_id
		JOIN privacy_erasure_heads eh ON eh.erasure_id=e.id
		JOIN privacy_offline_device_child_heads h ON h.child_id=c.id
		JOIN privacy_offline_device_child_revisions r ON r.id=h.current_revision_id
		WHERE c.erasure_id=$1 AND c.device_id=$2
		FOR UPDATE OF eh,h`, erasureID, deviceID).Scan(
		&childID, &revisionID, &stateRevision, &current.ErasureID, &current.DeviceID, &current.SourceGeneration, &current.CurrentGeneration,
		&current.ChallengeRevision, &current.Status, &keyVersion, &challengeHash, &issuedAt,
		&current.UpdatedAt, &current.StableReason, &receiptStatus,
	); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return privacy.OfflineDeviceChildReceipt{}, &privacy.Error{Code: privacy.CodeNotFound, Reason: "offline_device_purge_not_found"}
		}
		return privacy.OfflineDeviceChildReceipt{}, fmt.Errorf("lock offline device purge receipt: %w", err)
	}
	if acknowledgment.ChallengeRevision != current.ChallengeRevision {
		return privacy.OfflineDeviceChildReceipt{}, &privacy.Error{Code: privacy.CodeOfflinePurgeNotCurrent, Reason: "offline_purge_challenge_revision_mismatch"}
	}
	if !s.challengeKeys.Verify(keyVersion, erasureID, deviceID, current.SourceGeneration, current.CurrentGeneration, current.ChallengeRevision, acknowledgment.Challenge) {
		return privacy.OfflineDeviceChildReceipt{}, &privacy.Error{Code: privacy.CodeOfflineChallengeInvalid, Reason: "offline_purge_challenge_mismatch"}
	}
	digest := privacy.OfflineChallengeDigest(acknowledgment.Challenge)
	if len(challengeHash) != len(digest) || subtle.ConstantTimeCompare(challengeHash, digest[:]) != 1 {
		return privacy.OfflineDeviceChildReceipt{}, &privacy.Error{Code: privacy.CodeOfflineChallengeInvalid, Reason: "offline_purge_challenge_hash_mismatch"}
	}

	desiredStatus := privacy.OfflineDeviceChildSucceeded
	desiredReason := "device_acknowledged"
	if acknowledgment.Outcome == privacy.OfflinePurgeOutcomeFailed {
		desiredStatus = privacy.OfflineDeviceChildFailed
		desiredReason = string(acknowledgment.FailureCode)
	}
	if current.Status != privacy.OfflineDeviceChildPending {
		if current.Status == desiredStatus && current.StableReason == desiredReason {
			return current, nil
		}
		return privacy.OfflineDeviceChildReceipt{}, &privacy.Error{Code: privacy.CodeOfflinePurgeAckConflict, Reason: "offline_purge_ack_outcome_conflict"}
	}

	now := time.Now().UTC()
	newRevisionID := uuid.NewString()
	if _, err := tx.Exec(ctx, `
		INSERT INTO privacy_offline_device_child_revisions(
			id,child_id,revision,challenge_revision,status,challenge_key_version,challenge_hash,issued_at,acknowledged_at,stable_reason,updated_at)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$9)`,
		newRevisionID, childID, stateRevision+1, current.ChallengeRevision, desiredStatus, keyVersion, challengeHash, issuedAt, now, desiredReason); err != nil {
		return privacy.OfflineDeviceChildReceipt{}, fmt.Errorf("append offline device acknowledgment: %w", err)
	}
	if tag, err := tx.Exec(ctx, `
		UPDATE privacy_offline_device_child_heads
		SET current_revision_id=$2,current_revision=$3,status=$4,updated_at=$5
		WHERE child_id=$1 AND current_revision_id=$6`, childID, newRevisionID, stateRevision+1, desiredStatus, now, revisionID); err != nil {
		return privacy.OfflineDeviceChildReceipt{}, fmt.Errorf("advance offline device acknowledgment head: %w", err)
	} else if tag.RowsAffected() != 1 {
		return privacy.OfflineDeviceChildReceipt{}, &privacy.Error{Code: privacy.CodeOfflinePurgeNotCurrent, Reason: "offline_purge_receipt_changed"}
	}
	current.Status = desiredStatus
	current.StableReason = desiredReason
	current.UpdatedAt = now
	if err := s.advanceOfflineDeviceParentTx(ctx, tx, erasureID, receiptStatus, desiredStatus, now); err != nil {
		return privacy.OfflineDeviceChildReceipt{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return privacy.OfflineDeviceChildReceipt{}, fmt.Errorf("commit offline device acknowledgment: %w", err)
	}
	return current, nil
}

func (s *Store) advanceOfflineDeviceParentTx(ctx context.Context, tx pgx.Tx, erasureID string, receiptStatus privacy.ErasureStatus, desiredStatus privacy.OfflineDeviceChildStatus, now time.Time) error {
	var pending, failed int
	if err := tx.QueryRow(ctx, `
		SELECT count(*) FILTER (WHERE h.status<>'succeeded'),count(*) FILTER (WHERE h.status='failed')
		FROM privacy_offline_device_children c
		JOIN privacy_offline_device_child_heads h ON h.child_id=c.id
		WHERE c.erasure_id=$1`, erasureID).Scan(&pending, &failed); err != nil {
		return fmt.Errorf("count pending offline device receipts: %w", err)
	}
	stepStatus := privacy.StepPartial
	stepReason := "offline_device_ack_pending"
	completedAt := &now
	if pending == 0 {
		stepStatus = privacy.StepSucceeded
		stepReason = "offline_device_crypto_erasure_acknowledged"
	} else if desiredStatus == privacy.OfflineDeviceChildFailed || failed > 0 {
		stepReason = "offline_device_failure_reported"
	}
	if err := appendOfflineDeviceStepTx(ctx, tx, erasureID, stepStatus, stepReason, completedAt, now); err != nil {
		return err
	}
	parentStatus := receiptStatus
	parentReason := stepReason
	if desiredStatus == privacy.OfflineDeviceChildFailed {
		parentStatus = privacy.StatusPartial
	} else if pending == 0 {
		complete, err := erasureStepSetCompleteTx(ctx, tx, erasureID)
		if err != nil {
			return err
		}
		if complete && (receiptStatus == privacy.StatusRemotePurged || receiptStatus == privacy.StatusPartial) {
			parentStatus = privacy.StatusVerified
			parentReason = "active_stores_erased_offline_devices_acknowledged"
		} else if receiptStatus == privacy.StatusBlocked {
			parentStatus = privacy.StatusPartial
			parentReason = "offline_device_ack_resolved_parent_verification_incomplete"
		} else if receiptStatus == privacy.StatusRemotePurged || receiptStatus == privacy.StatusPartial {
			parentStatus = privacy.StatusPartial
			parentReason = "erasure_step_verification_incomplete"
		}
	}
	if parentStatus != receiptStatus && !privacy.CanTransitionErasure(receiptStatus, parentStatus) {
		return &privacy.Error{Code: privacy.CodeReceiptNotCurrent, Reason: "offline_device_parent_transition_invalid"}
	}
	if _, err := tx.Exec(ctx, `
		UPDATE privacy_erasure_heads
		SET summary_version=summary_version+1,status=$2,stable_reason=$3,updated_at=$4
		WHERE erasure_id=$1`, erasureID, parentStatus, parentReason, now); err != nil {
		return fmt.Errorf("advance offline device parent receipt: %w", err)
	}
	return nil
}

func erasureStepSetCompleteTx(ctx context.Context, tx pgx.Tx, erasureID string) (bool, error) {
	statuses := make(map[privacy.StoreKind]privacy.StepStatus, len(privacy.ReceiptSlots))
	rows, err := tx.Query(ctx, `
		SELECT h.store_kind,r.status
		FROM privacy_erasure_receipt_heads h
		JOIN privacy_erasure_step_receipts r ON r.id=h.current_receipt_id
		WHERE h.erasure_id=$1
		ORDER BY h.store_kind
		FOR UPDATE OF h`, erasureID)
	if err != nil {
		return false, fmt.Errorf("lock erasure step set: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var store privacy.StoreKind
		var status privacy.StepStatus
		if err := rows.Scan(&store, &status); err != nil {
			return false, err
		}
		statuses[store] = status
	}
	if err := rows.Err(); err != nil {
		return false, err
	}
	if len(statuses) != len(privacy.ReceiptSlots) {
		return false, nil
	}
	for _, store := range privacy.ReceiptSlots {
		status, ok := statuses[store]
		if !ok {
			return false, nil
		}
		if store == privacy.StoreExternalProvider {
			if status != privacy.StepSucceeded && status != privacy.StepNotApplicable && status != privacy.StepUnsupported {
				return false, nil
			}
			continue
		}
		if status != privacy.StepSucceeded && status != privacy.StepNotApplicable {
			return false, nil
		}
	}
	return true, nil
}

func appendOfflineDeviceStepTx(ctx context.Context, tx pgx.Tx, erasureID string, status privacy.StepStatus, reason string, completedAt *time.Time, now time.Time) error {
	var receiptID string
	var version int64
	var scope []byte
	var startedAt time.Time
	if err := tx.QueryRow(ctx, `
		SELECT r.id::text,r.version,r.scope_digest,r.started_at
		FROM privacy_erasure_receipt_heads h
		JOIN privacy_erasure_step_receipts r ON r.id=h.current_receipt_id
		WHERE h.erasure_id=$1 AND h.store_kind=$2
		FOR UPDATE OF h`, erasureID, privacy.StoreOfflineDeviceCache).Scan(&receiptID, &version, &scope, &startedAt); err != nil {
		return fmt.Errorf("lock offline device step receipt: %w", err)
	}
	newReceiptID := uuid.NewString()
	if _, err := tx.Exec(ctx, `
		INSERT INTO privacy_erasure_step_receipts(
			id,erasure_id,store_kind,version,scope_digest,started_at,completed_at,status,stable_reason,verification_method)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,'generation_bound_device_ack')`,
		newReceiptID, erasureID, privacy.StoreOfflineDeviceCache, version+1, scope, startedAt, completedAt, status, reason); err != nil {
		return fmt.Errorf("append offline device step receipt: %w", err)
	}
	if tag, err := tx.Exec(ctx, `
		UPDATE privacy_erasure_receipt_heads
		SET current_receipt_id=$3,current_version=$4,updated_at=$5
		WHERE erasure_id=$1 AND store_kind=$2 AND current_receipt_id=$6`,
		erasureID, privacy.StoreOfflineDeviceCache, newReceiptID, version+1, now, receiptID); err != nil {
		return fmt.Errorf("advance offline device step receipt head: %w", err)
	} else if tag.RowsAffected() != 1 {
		return &privacy.Error{Code: privacy.CodeOfflinePurgeNotCurrent, Reason: "offline_device_step_changed"}
	}
	return nil
}
