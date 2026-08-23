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

var nocturneManagedStores = [...]privacy.StoreKind{
	privacy.StoreNocturnePaths,
	privacy.StoreNocturneOrphanHistory,
	privacy.StoreNocturneSnapshotChangeset,
}

type remoteReceiptHead struct {
	receipt privacy.StepReceipt
	scope   []byte
}

// RunNocturneErase invokes the remote eraser without holding a database
// transaction, then atomically advances all Nocturne receipt heads.
func (s *Store) RunNocturneErase(ctx context.Context, erasureID string, eraser privacy.RemoteEraser) (privacy.ErasureReceipt, error) {
	current, err := s.Receipt(ctx, erasureID)
	if err != nil {
		return privacy.ErasureReceipt{}, err
	}
	if current.Status == privacy.StatusRemotePurged {
		return current, nil
	}
	if eraser == nil {
		return current, &privacy.Error{Code: privacy.CodeInvalidRequest, Reason: "remote_eraser_missing"}
	}
	switch current.Status {
	case privacy.StatusLocalScrubbed, privacy.StatusRemoteDraining, privacy.StatusPartial:
	default:
		return current, &privacy.Error{Code: privacy.CodeReceiptNotCurrent, Reason: "erasure_not_ready_for_remote_purge"}
	}

	expected, pathReceipt, err := currentNocturneReceipts(current)
	if err != nil {
		return current, err
	}
	startedAt := timeNowUTC()
	result, err := eraser.Erase(ctx, privacy.RemoteEraseRequest{
		ErasureID:         erasureID,
		LearnerGeneration: current.LearnerGeneration,
		Receipt:           pathReceipt,
	})
	if err != nil {
		return current, fmt.Errorf("erase Nocturne generation %d for erasure %s: %w", current.LearnerGeneration, erasureID, err)
	}
	result, evidence, err := validateRemoteEraseResult(result)
	if err != nil {
		return current, err
	}

	if err := s.commitNocturneErase(ctx, erasureID, current.LearnerGeneration, expected, startedAt, result, evidence); err != nil {
		latest, receiptErr := s.Receipt(ctx, erasureID)
		if receiptErr != nil {
			return privacy.ErasureReceipt{}, errors.Join(err, receiptErr)
		}
		return latest, err
	}
	return s.Receipt(ctx, erasureID)
}

func currentNocturneReceipts(receipt privacy.ErasureReceipt) (map[privacy.StoreKind]privacy.StepReceipt, privacy.StepReceipt, error) {
	current := make(map[privacy.StoreKind]privacy.StepReceipt, len(nocturneManagedStores))
	for _, step := range receipt.Steps {
		for _, store := range nocturneManagedStores {
			if step.Store == store {
				current[store] = step
				break
			}
		}
	}
	for _, store := range nocturneManagedStores {
		step, ok := current[store]
		if !ok || step.ID == "" || step.Version < 1 {
			return nil, privacy.StepReceipt{}, &privacy.Error{Code: privacy.CodeReceiptNotCurrent, Reason: string(store) + "_receipt_missing"}
		}
	}
	return current, current[privacy.StoreNocturnePaths], nil
}

func validateRemoteEraseResult(result privacy.RemoteEraseResult) (privacy.RemoteEraseResult, []byte, error) {
	switch result.Status {
	case privacy.StepSucceeded, privacy.StepPartial, privacy.StepUnknown, privacy.StepFailed, privacy.StepNotApplicable:
	default:
		return result, nil, &privacy.Error{Code: privacy.CodeInvalidRequest, Reason: "invalid_remote_erase_status"}
	}
	result.StableReason = strings.TrimSpace(result.StableReason)
	if result.StableReason == "" || utf8.RuneCountInString(result.StableReason) > 1000 {
		return result, nil, &privacy.Error{Code: privacy.CodeInvalidRequest, Reason: "invalid_remote_erase_reason"}
	}
	if result.CompletedAt.IsZero() {
		return result, nil, &privacy.Error{Code: privacy.CodeInvalidRequest, Reason: "remote_erase_completion_missing"}
	}
	result.CompletedAt = result.CompletedAt.UTC()
	if len(result.EvidenceDigest) != 64 {
		return result, nil, &privacy.Error{Code: privacy.CodeInvalidRequest, Reason: "invalid_remote_evidence_digest"}
	}
	evidence, err := hex.DecodeString(result.EvidenceDigest)
	if err != nil || len(evidence) != 32 {
		return result, nil, &privacy.Error{Code: privacy.CodeInvalidRequest, Reason: "invalid_remote_evidence_digest", Cause: err}
	}
	return result, evidence, nil
}

func (s *Store) commitNocturneErase(
	ctx context.Context,
	erasureID string,
	generation int64,
	expected map[privacy.StoreKind]privacy.StepReceipt,
	startedAt time.Time,
	result privacy.RemoteEraseResult,
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
		WHERE h.erasure_id=$1 AND h.status<>'verified'
		FOR UPDATE OF h`, erasureID).Scan(&currentStatus, &currentGeneration); err != nil {
		return &privacy.Error{Code: privacy.CodeReceiptNotCurrent, Reason: "active_erasure_missing", Cause: err}
	}
	if currentGeneration != generation {
		return &privacy.Error{Code: privacy.CodeReceiptNotCurrent, Reason: "learner_generation_changed"}
	}
	switch currentStatus {
	case privacy.StatusLocalScrubbed, privacy.StatusRemoteDraining, privacy.StatusPartial:
	default:
		return &privacy.Error{Code: privacy.CodeReceiptNotCurrent, Reason: "erasure_summary_changed"}
	}

	heads := make(map[privacy.StoreKind]remoteReceiptHead, len(nocturneManagedStores))
	for _, store := range nocturneManagedStores {
		var head remoteReceiptHead
		head.receipt.Store = store
		if err := tx.QueryRow(ctx, `
			SELECT r.id,r.version,r.status,r.scope_digest
			FROM privacy_erasure_receipt_heads h
			JOIN privacy_erasure_step_receipts r ON r.id=h.current_receipt_id
			WHERE h.erasure_id=$1 AND h.store_kind=$2
			FOR UPDATE OF h`, erasureID, store).Scan(
			&head.receipt.ID,
			&head.receipt.Version,
			&head.receipt.Status,
			&head.scope,
		); err != nil {
			return &privacy.Error{Code: privacy.CodeReceiptNotCurrent, Reason: string(store) + "_receipt_missing", Cause: err}
		}
		observed := expected[store]
		if head.receipt.ID != observed.ID || head.receipt.Version != observed.Version {
			return &privacy.Error{Code: privacy.CodeReceiptNotCurrent, Reason: string(store) + "_receipt_head_changed"}
		}
		heads[store] = head
	}

	now := timeNowUTC()
	newReceiptIDs := make(map[privacy.StoreKind]string, len(nocturneManagedStores))
	for _, store := range nocturneManagedStores {
		head := heads[store]
		newReceiptID := uuid.NewString()
		newVersion := head.receipt.Version + 1
		if _, err := tx.Exec(ctx, `
			INSERT INTO privacy_erasure_step_receipts(
				id,erasure_id,store_kind,version,scope_digest,started_at,completed_at,
				status,stable_reason,verification_method,evidence_digest)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,'remote_eraser_sha256_receipt',$10)`,
			newReceiptID, erasureID, store, newVersion, head.scope, startedAt,
			result.CompletedAt, result.Status, result.StableReason, evidence); err != nil {
			return fmt.Errorf("append %s remote erasure receipt: %w", store, err)
		}
		tag, err := tx.Exec(ctx, `
			UPDATE privacy_erasure_receipt_heads
			SET current_receipt_id=$5,current_version=$6,updated_at=$7
			WHERE erasure_id=$1 AND store_kind=$2
			  AND current_receipt_id=$3 AND current_version=$4`,
			erasureID, store, head.receipt.ID, head.receipt.Version,
			newReceiptID, newVersion, now)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return &privacy.Error{Code: privacy.CodeReceiptNotCurrent, Reason: string(store) + "_receipt_head_changed"}
		}
		newReceiptIDs[store] = newReceiptID
	}

	var remoteComplete bool
	if err := tx.QueryRow(ctx, `
		SELECT count(*)=$3 AND bool_and(r.status IN ('succeeded','not_applicable'))
		FROM privacy_erasure_receipt_heads h
		JOIN privacy_erasure_step_receipts r ON r.id=h.current_receipt_id
		WHERE h.erasure_id=$1 AND h.store_kind=ANY($2::text[])`,
		erasureID, storeStrings(nocturneManagedStores[:]), len(nocturneManagedStores)).Scan(&remoteComplete); err != nil {
		return err
	}

	if remoteComplete {
		memoryPort, err := s.ownerPort(privacy.OwnerMemory)
		if err != nil {
			return err
		}
		transition := privacy.GenerationTransition{
			ErasureID:        erasureID,
			FromGeneration:   generation,
			TargetGeneration: generation,
			ReceiptID:        newReceiptIDs[privacy.StoreNocturnePaths],
			At:               now,
		}
		if err := memoryPort.OpenGenerationTx(ctx, tx, transition); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO privacy_owner_redaction_audit(
				id,erasure_id,owner_kind,learner_generation,receipt_id,action,evidence_digest,created_at)
			VALUES($1,$2,'memory',$3,$4,'gate_opened',$5,$6)
			ON CONFLICT DO NOTHING`, uuid.NewString(), erasureID, generation,
			newReceiptIDs[privacy.StoreNocturnePaths], evidence, now); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			UPDATE privacy_erasure_heads
			SET status='remote_purged',summary_version=summary_version+1,
				stable_reason='all_nocturne_managed_steps_verified',updated_at=$2
			WHERE erasure_id=$1`, erasureID, now); err != nil {
			return err
		}
	} else {
		if _, err := tx.Exec(ctx, `
			UPDATE privacy_erasure_heads
			SET status='partial',summary_version=summary_version+1,
				stable_reason=$2,updated_at=$3
			WHERE erasure_id=$1`, erasureID, result.StableReason, now); err != nil {
			return err
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return err
	}
	if remoteComplete {
		s.permits.Open(generation, privacy.OwnerMemory)
	}
	return nil
}

var timeNowUTC = func() time.Time { return time.Now().UTC() }
