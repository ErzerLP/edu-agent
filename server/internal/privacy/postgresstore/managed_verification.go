package postgresstore

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/edu-agent/edu-agent/server/internal/privacy"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const managedBackupVerificationMethod = "three_way_inventory_destroyed_key_restore_failure_sha256"

func (r *ManagedBackupRepository) VerifyManagedBackupBarrier(ctx context.Context, erasureID string, targetGeneration int64) (privacy.ManagedBackupBarrierState, error) {
	if uuid.Validate(erasureID) != nil || targetGeneration < 2 {
		return privacy.ManagedBackupBarrierState{}, privacy.ErrManagedBackupInvalid
	}
	var requestedAt, scheduledAt time.Time
	var verifiedAt *time.Time
	if err := r.pool.QueryRow(ctx, `
		SELECT requested_at,managed_backup_scheduled_unrecoverable_after,
		       managed_backup_verified_unrecoverable_at
		FROM privacy_erasures
		WHERE id=$1 AND target_learner_generation=$2`, erasureID, targetGeneration).Scan(
		&requestedAt, &scheduledAt, &verifiedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return privacy.ManagedBackupBarrierState{}, privacy.ErrManagedBackupBarrierUnproven
		}
		return privacy.ManagedBackupBarrierState{}, fmt.Errorf("read managed backup barrier proof: %w", err)
	}
	if verifiedAt == nil || verifiedAt.Before(requestedAt) || verifiedAt.After(scheduledAt) {
		return privacy.ManagedBackupBarrierState{}, privacy.ErrManagedBackupBarrierUnproven
	}
	var oldKeys, invalidOldKeys int64
	if err := r.pool.QueryRow(ctx, `
		SELECT count(*),count(*) FILTER (WHERE
			destroyed_at IS NULL OR destroyed_at>$2 OR wrapped_key IS NOT NULL OR
			destruction_evidence_digest IS NULL OR octet_length(destruction_evidence_digest)<>32)
		FROM memory_generation_keys
		WHERE learner_generation<$1`, targetGeneration, verifiedAt.UTC()).Scan(&oldKeys, &invalidOldKeys); err != nil {
		return privacy.ManagedBackupBarrierState{}, fmt.Errorf("verify managed backup old generation keys: %w", err)
	}
	if invalidOldKeys != 0 {
		return privacy.ManagedBackupBarrierState{}, privacy.ErrManagedBackupLiveOldKey
	}
	var unboundInventory int64
	if err := r.pool.QueryRow(ctx, `
		SELECT count(*) FROM memory_managed_backup_inventory
		WHERE learner_generation<$1 AND erasure_id IS NULL`, targetGeneration).Scan(&unboundInventory); err != nil {
		return privacy.ManagedBackupBarrierState{}, fmt.Errorf("verify managed backup erasure binding: %w", err)
	}
	if unboundInventory != 0 {
		return privacy.ManagedBackupBarrierState{}, privacy.ErrManagedBackupBarrierUnproven
	}
	return privacy.ManagedBackupBarrierState{
		VerifiedUnrecoverableAt: verifiedAt.UTC(),
		DestroyedOldKeyCount:    oldKeys,
	}, nil
}

// RunManagedBackupVerification invokes the filesystem/Nocturne verifier without
// a database transaction, then appends the managed_backup receipt with CAS.
func (s *Store) RunManagedBackupVerification(ctx context.Context, erasureID string, verifier privacy.ManagedBackupVerifier) (privacy.ErasureReceipt, error) {
	current, err := s.Receipt(ctx, erasureID)
	if err != nil {
		return privacy.ErasureReceipt{}, err
	}
	if current.Status == privacy.StatusVerified {
		return current, nil
	}
	switch current.Status {
	case privacy.StatusRemotePurged, privacy.StatusPartial:
	default:
		return current, &privacy.Error{Code: privacy.CodeReceiptNotCurrent, Reason: "erasure_not_ready_for_managed_backup_verification"}
	}
	if !nocturneReceiptsComplete(current) {
		return current, &privacy.Error{Code: privacy.CodeReceiptNotCurrent, Reason: "nocturne_verification_incomplete"}
	}
	expected, err := currentManagedBackupReceipt(current)
	if err != nil {
		return current, err
	}
	if expected.Status == privacy.StepSucceeded || expected.Status == privacy.StepNotApplicable {
		if err := s.commitManagedBackupVerification(ctx, erasureID, current.LearnerGeneration, expected, time.Time{}, nil, nil); err != nil {
			latest, receiptErr := s.Receipt(ctx, erasureID)
			if receiptErr != nil {
				return privacy.ErasureReceipt{}, errors.Join(err, receiptErr)
			}
			return latest, err
		}
		return s.Receipt(ctx, erasureID)
	}
	if verifier == nil {
		return current, &privacy.Error{Code: privacy.CodeInvalidRequest, Reason: "managed_backup_verifier_missing"}
	}
	startedAt := timeNowUTC()
	result, err := verifier.VerifyManagedBackups(ctx, privacy.ManagedBackupVerificationRequest{
		ErasureID: erasureID, LearnerGeneration: current.LearnerGeneration,
	})
	if err != nil {
		return current, fmt.Errorf("verify managed backups for erasure %s: %w", erasureID, err)
	}
	result, evidence, err := validateManagedBackupVerificationResult(result)
	if err != nil {
		return current, err
	}
	if err := s.commitManagedBackupVerification(ctx, erasureID, current.LearnerGeneration, expected, startedAt, &result, evidence); err != nil {
		latest, receiptErr := s.Receipt(ctx, erasureID)
		if receiptErr != nil {
			return privacy.ErasureReceipt{}, errors.Join(err, receiptErr)
		}
		return latest, err
	}
	return s.Receipt(ctx, erasureID)
}

func currentManagedBackupReceipt(receipt privacy.ErasureReceipt) (privacy.StepReceipt, error) {
	for _, step := range receipt.Steps {
		if step.Store == privacy.StoreManagedBackup {
			if step.ID == "" || step.Version < 1 {
				break
			}
			return step, nil
		}
	}
	return privacy.StepReceipt{}, &privacy.Error{Code: privacy.CodeReceiptNotCurrent, Reason: "managed_backup_receipt_missing"}
}

func nocturneReceiptsComplete(receipt privacy.ErasureReceipt) bool {
	complete := 0
	for _, step := range receipt.Steps {
		for _, store := range nocturneManagedStores {
			if step.Store == store && (step.Status == privacy.StepSucceeded || step.Status == privacy.StepNotApplicable) {
				complete++
				break
			}
		}
	}
	return complete == len(nocturneManagedStores)
}

func validateManagedBackupVerificationResult(result privacy.ManagedBackupVerificationResult) (privacy.ManagedBackupVerificationResult, []byte, error) {
	switch result.Status {
	case privacy.StepSucceeded, privacy.StepPartial, privacy.StepFailed, privacy.StepUnknown, privacy.StepNotApplicable:
	default:
		return result, nil, &privacy.Error{Code: privacy.CodeInvalidRequest, Reason: "invalid_managed_backup_verification_status"}
	}
	result.StableReason = strings.TrimSpace(result.StableReason)
	if result.StableReason == "" || utf8.RuneCountInString(result.StableReason) > 1000 || claimsSecureErase(result.StableReason) {
		return result, nil, &privacy.Error{Code: privacy.CodeInvalidRequest, Reason: "invalid_managed_backup_verification_reason"}
	}
	if result.CompletedAt.IsZero() {
		return result, nil, &privacy.Error{Code: privacy.CodeInvalidRequest, Reason: "managed_backup_verification_completion_missing"}
	}
	result.CompletedAt = result.CompletedAt.UTC()
	if len(result.EvidenceDigest) != 64 || result.EvidenceDigest != strings.ToLower(result.EvidenceDigest) {
		return result, nil, &privacy.Error{Code: privacy.CodeInvalidRequest, Reason: "invalid_managed_backup_evidence_digest"}
	}
	evidence, err := hex.DecodeString(result.EvidenceDigest)
	if err != nil || len(evidence) != 32 {
		return result, nil, &privacy.Error{Code: privacy.CodeInvalidRequest, Reason: "invalid_managed_backup_evidence_digest", Cause: err}
	}
	return result, evidence, nil
}

func claimsSecureErase(reason string) bool {
	normalized := strings.NewReplacer("_", " ", "-", " ").Replace(strings.ToLower(reason))
	return strings.Contains(normalized, "secure erase") || strings.Contains(normalized, "securely erased")
}

func (s *Store) commitManagedBackupVerification(
	ctx context.Context,
	erasureID string,
	generation int64,
	expected privacy.StepReceipt,
	startedAt time.Time,
	result *privacy.ManagedBackupVerificationResult,
	evidence []byte,
) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	var currentStatus privacy.ErasureStatus
	var currentGeneration int64
	if err := tx.QueryRow(ctx, `
		SELECT h.status,e.target_learner_generation
		FROM privacy_erasure_heads h
		JOIN privacy_erasures e ON e.id=h.erasure_id
		WHERE h.erasure_id=$1
		FOR UPDATE OF h`, erasureID).Scan(&currentStatus, &currentGeneration); err != nil {
		return &privacy.Error{Code: privacy.CodeReceiptNotCurrent, Reason: "active_erasure_missing", Cause: err}
	}
	if currentStatus == privacy.StatusVerified {
		return tx.Commit(ctx)
	}
	if currentGeneration != generation {
		return &privacy.Error{Code: privacy.CodeReceiptNotCurrent, Reason: "learner_generation_changed"}
	}
	switch currentStatus {
	case privacy.StatusRemotePurged, privacy.StatusPartial:
	default:
		return &privacy.Error{Code: privacy.CodeReceiptNotCurrent, Reason: "erasure_summary_changed"}
	}

	var current privacy.StepReceipt
	var scope []byte
	if err := tx.QueryRow(ctx, `
		SELECT r.id,r.version,r.status,r.scope_digest
		FROM privacy_erasure_receipt_heads h
		JOIN privacy_erasure_step_receipts r ON r.id=h.current_receipt_id
		WHERE h.erasure_id=$1 AND h.store_kind='managed_backup'
		FOR UPDATE OF h`, erasureID).Scan(&current.ID, &current.Version, &current.Status, &scope); err != nil {
		return &privacy.Error{Code: privacy.CodeReceiptNotCurrent, Reason: "managed_backup_receipt_missing", Cause: err}
	}
	if current.ID != expected.ID || current.Version != expected.Version {
		return &privacy.Error{Code: privacy.CodeReceiptNotCurrent, Reason: "managed_backup_receipt_head_changed"}
	}

	now := timeNowUTC()
	if result != nil {
		newReceiptID := uuid.NewString()
		newVersion := current.Version + 1
		if _, err := tx.Exec(ctx, `
			INSERT INTO privacy_erasure_step_receipts(
				id,erasure_id,store_kind,version,scope_digest,started_at,completed_at,
				status,stable_reason,verification_method,evidence_digest)
			VALUES($1,$2,'managed_backup',$3,$4,$5,$6,$7,$8,$9,$10)`,
			newReceiptID, erasureID, newVersion, scope, startedAt, result.CompletedAt,
			result.Status, result.StableReason, managedBackupVerificationMethod, evidence); err != nil {
			return fmt.Errorf("append managed backup verification receipt: %w", err)
		}
		tag, err := tx.Exec(ctx, `
			UPDATE privacy_erasure_receipt_heads
			SET current_receipt_id=$4,current_version=$5,updated_at=$6
			WHERE erasure_id=$1 AND store_kind='managed_backup'
			  AND current_receipt_id=$2 AND current_version=$3`,
			erasureID, current.ID, current.Version, newReceiptID, newVersion, now)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return &privacy.Error{Code: privacy.CodeReceiptNotCurrent, Reason: "managed_backup_receipt_head_changed"}
		}
		current.ID, current.Version, current.Status = newReceiptID, newVersion, result.Status
	} else if current.Status != privacy.StepSucceeded && current.Status != privacy.StepNotApplicable {
		return &privacy.Error{Code: privacy.CodeReceiptNotCurrent, Reason: "managed_backup_receipt_not_complete"}
	}

	statuses := make(map[privacy.StoreKind]privacy.StepStatus, len(privacy.ReceiptSlots))
	rows, err := tx.Query(ctx, `
		SELECT h.store_kind,r.status
		FROM privacy_erasure_receipt_heads h
		JOIN privacy_erasure_step_receipts r ON r.id=h.current_receipt_id
		WHERE h.erasure_id=$1
		ORDER BY h.store_kind
		FOR UPDATE OF h`, erasureID)
	if err != nil {
		return err
	}
	for rows.Next() {
		var store privacy.StoreKind
		var status privacy.StepStatus
		if err := rows.Scan(&store, &status); err != nil {
			rows.Close()
			return err
		}
		statuses[store] = status
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	if len(statuses) != len(privacy.ReceiptSlots) {
		return &privacy.Error{Code: privacy.CodeReceiptNotCurrent, Reason: "erasure_receipt_set_incomplete"}
	}

	activeComplete := true
	noFailedOrUnknown := true
	externalAllowed := false
	for _, store := range privacy.ReceiptSlots {
		status, ok := statuses[store]
		if !ok {
			activeComplete = false
			continue
		}
		if status == privacy.StepFailed || status == privacy.StepUnknown {
			noFailedOrUnknown = false
		}
		if store == privacy.StoreExternalProvider {
			externalAllowed = status == privacy.StepSucceeded || status == privacy.StepNotApplicable || status == privacy.StepUnsupported
			continue
		}
		if status != privacy.StepSucceeded && status != privacy.StepNotApplicable {
			activeComplete = false
		}
	}
	nocturneComplete := true
	for _, store := range nocturneManagedStores {
		status := statuses[store]
		if status != privacy.StepSucceeded && status != privacy.StepNotApplicable {
			nocturneComplete = false
		}
	}
	if activeComplete && noFailedOrUnknown && externalAllowed && nocturneComplete {
		if _, err := tx.Exec(ctx, `
			UPDATE privacy_erasure_heads
			SET status='verified',summary_version=summary_version+1,
				stable_reason='active_stores_erased_backup_unrecoverable',updated_at=$2
			WHERE erasure_id=$1 AND status IN ('remote_purged','partial')`, erasureID, now); err != nil {
			return err
		}
	} else {
		reason := "active_managed_store_verification_incomplete"
		if result != nil && result.Status != privacy.StepSucceeded && result.Status != privacy.StepNotApplicable {
			reason = result.StableReason
		}
		if _, err := tx.Exec(ctx, `
			UPDATE privacy_erasure_heads
			SET status='partial',summary_version=summary_version+1,
				stable_reason=$2,updated_at=$3
			WHERE erasure_id=$1 AND status IN ('remote_purged','partial')`, erasureID, reason, now); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

var _ privacy.ManagedBackupBarrierRepository = (*ManagedBackupRepository)(nil)
