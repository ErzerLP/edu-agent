package postgresstore

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/memory"
	"github.com/edu-agent/edu-agent/server/internal/platform/outbox"
	outboxpostgres "github.com/edu-agent/edu-agent/server/internal/platform/outbox/postgresstore"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

type DBTX interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

type Store struct {
	pool *pgxpool.Pool
}

func New(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

type operationRecord struct {
	requestHash      []byte
	operationKind    memory.OperationKind
	candidateID      *string
	logicalMemoryID  *string
	recordRevisionID *string
	deliveryID       *string
}

type admissionIDs struct {
	logicalMemoryID   string
	recordRevisionID  string
	deliveryID        string
	deliveryPayloadID string
	receiptID         string
	outboxID          string
}

func (s *Store) CreateCandidate(ctx context.Context, plan memory.CreatePlan) (memory.OperationResult, error) {
	if err := plan.Operation.Validate(); err != nil {
		return memory.OperationResult{}, err
	}
	if plan.Operation.Kind != memory.OperationCreateCandidate {
		return memory.OperationResult{}, invalid("create_operation_kind")
	}
	if err := memory.ValidateProposedContent(plan.Candidate.Category, plan.Content); err != nil {
		return memory.OperationResult{}, err
	}
	if plan.Correction {
		if plan.Candidate.LogicalMemoryID != plan.LogicalMemoryID || !canonicalUUID(plan.LogicalMemoryID) ||
			plan.ExpectedRecordRevision < 1 || plan.ExpectedRecordGeneration < 1 {
			return memory.OperationResult{}, invalid("invalid_correction_candidate_plan")
		}
	} else {
		if plan.ExpectedRecordRevision != 0 || plan.ExpectedRecordGeneration != 0 {
			return memory.OperationResult{}, invalid("unexpected_correction_candidate_fence")
		}
		if plan.AutomaticDecision != nil {
			plan.Candidate.LogicalMemoryID = plan.LogicalMemoryID
		}
	}
	if err := plan.Candidate.Validate(); err != nil {
		return memory.OperationResult{}, err
	}
	if plan.AutomaticDecision == nil && (plan.Candidate.Status != memory.CandidatePending || plan.Candidate.Revision != 1) {
		return memory.OperationResult{}, invalid("pending_candidate_shape")
	}
	if plan.AutomaticDecision == nil && !plan.Correction && plan.Candidate.LogicalMemoryID != "" {
		return memory.OperationResult{}, invalid("unexpected_pending_candidate_logical_memory")
	}
	if plan.AutomaticDecision != nil {
		if plan.AutomaticDecision.OperationID != plan.Operation.OperationID || plan.AutomaticDecision.RequestHash != plan.Operation.RequestHash {
			return memory.OperationResult{}, invalid("automatic_decision_operation_mismatch")
		}
		if err := plan.AutomaticDecision.Validate(); err != nil {
			return memory.OperationResult{}, err
		}
		expectedStatus := memory.EvaluateAdmission(plan.Candidate, plan.Content, plan.Candidate.CreatedAt)
		if plan.Candidate.Revision != 2 || plan.Candidate.Status != expectedStatus ||
			memory.CandidateStatusForDecision(plan.AutomaticDecision.Decision) != expectedStatus ||
			(expectedStatus != memory.CandidateAdmitted && expectedStatus != memory.CandidateRejected) {
			return memory.OperationResult{}, invalid("automatic_decision_shape")
		}
		if expectedStatus == memory.CandidateAdmitted {
			if err := validateAdmissionIDs(admissionIDsFromCreate(plan)); err != nil {
				return memory.OperationResult{}, err
			}
		} else if plan.RecordRevisionID != "" || plan.DeliveryID != "" || plan.DeliveryPayloadID != "" || plan.ReceiptID != "" || plan.OutboxID != "" || (!plan.Correction && plan.LogicalMemoryID != "") {
			return memory.OperationResult{}, invalid("automatic_rejection_created_delivery_identity")
		}
	}

	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return memory.OperationResult{}, fmt.Errorf("begin memory candidate create: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if err := lockOperation(ctx, tx, plan.Operation); err != nil {
		return memory.OperationResult{}, err
	}
	replayed, err := operationArchived(ctx, tx, plan.Operation)
	if err != nil {
		return memory.OperationResult{}, err
	}
	if replayed {
		if _, err := lockReadableGeneration(ctx, tx); err != nil {
			return memory.OperationResult{}, err
		}
	}
	if archived, found, err := readOperation(ctx, tx, plan.Operation); err != nil {
		return memory.OperationResult{}, err
	} else if found {
		if archived.candidateID != nil {
			if _, err := expireCandidatesTx(ctx, tx, *archived.candidateID, 1); err != nil {
				return memory.OperationResult{}, err
			}
		}
		result, err := loadOperationResult(ctx, tx, archived)
		if err != nil {
			return memory.OperationResult{}, err
		}
		result.Replayed = true
		if err := tx.Commit(ctx); err != nil {
			return memory.OperationResult{}, fmt.Errorf("commit memory create replay: %w", err)
		}
		return result, nil
	}
	generation, err := lockWritableGeneration(ctx, tx)
	if err != nil {
		return memory.OperationResult{}, err
	}
	var correctionBaseValue *recordCorrectionBase
	if plan.Correction {
		base, err := lockRecordCorrectionBase(ctx, tx, plan.LogicalMemoryID, plan.ExpectedRecordRevision,
			plan.ExpectedRecordGeneration, generation)
		if err != nil {
			return memory.OperationResult{}, err
		}
		correctionBaseValue = &base
	}
	dbNow, err := databaseClock(ctx, tx)
	if err != nil {
		return memory.OperationResult{}, err
	}
	if !dbNow.Before(plan.Candidate.ValidUntil) {
		return memory.OperationResult{}, candidateExpiredConflict(plan.Candidate.ID, 0, 0)
	}
	if err := insertCandidateMetadata(ctx, tx, plan.Candidate); err != nil {
		return memory.OperationResult{}, err
	}
	var recordID, deliveryID, logicalID any
	if plan.AutomaticDecision == nil {
		if _, err := tx.Exec(ctx, `
			INSERT INTO memory_candidate_heads(candidate_id,revision,status,current_decision_id,payload_available,updated_at)
			VALUES($1,1,'pending_review',NULL,TRUE,$2)`, plan.Candidate.ID, plan.Candidate.CreatedAt); err != nil {
			return memory.OperationResult{}, fmt.Errorf("insert memory candidate head: %w", err)
		}
		hash, _ := decodeHash(plan.Candidate.ContentHash)
		if _, err := tx.Exec(ctx, `
			INSERT INTO memory_candidate_payloads(id,candidate_id,content,content_hash,valid_until,created_at)
			VALUES($1,$2,$3,$4,$5,$6)`, plan.Candidate.PayloadID, plan.Candidate.ID, plan.Content, hash, plan.Candidate.ValidUntil, plan.Candidate.CreatedAt); err != nil {
			return memory.OperationResult{}, fmt.Errorf("insert memory candidate payload: %w", err)
		}
	} else {
		if err := insertDecision(ctx, tx, *plan.AutomaticDecision); err != nil {
			return memory.OperationResult{}, err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO memory_candidate_heads(candidate_id,revision,status,current_decision_id,payload_available,updated_at)
			VALUES($1,2,$2,$3,FALSE,$4)`, plan.Candidate.ID, plan.Candidate.Status,
			plan.AutomaticDecision.ID, plan.Candidate.CreatedAt); err != nil {
			return memory.OperationResult{}, fmt.Errorf("insert automatic memory candidate decision: %w", err)
		}
		if plan.Candidate.Status == memory.CandidateAdmitted {
			ids := admissionIDsFromCreate(plan)
			if correctionBaseValue != nil {
				if err := insertRecordCorrection(ctx, tx, plan.Candidate, plan.Content, ids, generation, *correctionBaseValue, *plan.AutomaticDecision); err != nil {
					return memory.OperationResult{}, err
				}
			} else if err := insertAdmission(ctx, tx, plan.Candidate, plan.Content, ids, generation, *plan.AutomaticDecision); err != nil {
				return memory.OperationResult{}, err
			}
			logicalID, recordID, deliveryID = ids.logicalMemoryID, ids.recordRevisionID, ids.deliveryID
		}
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO memory_operation_inbox(
			device_id,operation_id,request_hash,operation_kind,terminal_status,
			candidate_id,logical_memory_id,record_revision_id,delivery_id,completed_at)
		VALUES($1,$2,$3,'create_candidate','succeeded',$4,$5,$6,$7,$8)`,
		plan.Operation.DeviceID, plan.Operation.OperationID, mustHash(plan.Operation.RequestHash),
		plan.Candidate.ID, logicalID, recordID, deliveryID, plan.Candidate.CreatedAt); err != nil {
		return memory.OperationResult{}, fmt.Errorf("archive memory create operation: %w", err)
	}
	archived := operationRecord{
		candidateID: stringPointer(plan.Candidate.ID), logicalMemoryID: anyStringPointer(logicalID),
		recordRevisionID: anyStringPointer(recordID), deliveryID: anyStringPointer(deliveryID),
	}
	result, err := loadOperationResult(ctx, tx, archived)
	if err != nil {
		return memory.OperationResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return memory.OperationResult{}, fmt.Errorf("commit memory candidate create: %w", err)
	}
	return result, nil
}

func (s *Store) DecideCandidate(ctx context.Context, plan memory.DecisionPlan) (memory.OperationResult, error) {
	if err := plan.Operation.Validate(); err != nil {
		return memory.OperationResult{}, err
	}
	if plan.Operation.Kind != memory.OperationCandidateDecision {
		return memory.OperationResult{}, invalid("decision_operation_kind")
	}
	if err := plan.Decision.Validate(); err != nil {
		return memory.OperationResult{}, err
	}
	if plan.CandidateID != plan.Decision.CandidateID || plan.ExpectedRevision < 1 ||
		(plan.ExpectedRecordRevision == 0) != (plan.ExpectedRecordGeneration == 0) ||
		plan.ExpectedRecordRevision < 0 || plan.ExpectedRecordGeneration < 0 ||
		plan.Decision.Revision != plan.ExpectedRevision+1 || plan.Decision.OperationID != plan.Operation.OperationID ||
		plan.Decision.RequestHash != plan.Operation.RequestHash ||
		(plan.Decision.Decision != memory.DecisionAdmit && plan.Decision.Decision != memory.DecisionReject) {
		return memory.OperationResult{}, invalid("candidate_decision_shape")
	}
	if plan.Decision.Decision == memory.DecisionAdmit {
		if err := validateAdmissionIDs(admissionIDsFromDecision(plan)); err != nil {
			return memory.OperationResult{}, err
		}
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return memory.OperationResult{}, fmt.Errorf("begin memory candidate decision: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if err := lockOperation(ctx, tx, plan.Operation); err != nil {
		return memory.OperationResult{}, err
	}
	replayed, err := operationArchived(ctx, tx, plan.Operation)
	if err != nil {
		return memory.OperationResult{}, err
	}
	if replayed {
		if _, err := lockReadableGeneration(ctx, tx); err != nil {
			return memory.OperationResult{}, err
		}
	}
	if archived, found, err := readOperation(ctx, tx, plan.Operation); err != nil {
		return memory.OperationResult{}, err
	} else if found {
		if archived.candidateID != nil {
			if _, err := expireCandidatesTx(ctx, tx, *archived.candidateID, 1); err != nil {
				return memory.OperationResult{}, err
			}
		}
		result, err := loadOperationResult(ctx, tx, archived)
		if err != nil {
			return memory.OperationResult{}, err
		}
		result.Replayed = true
		if err := tx.Commit(ctx); err != nil {
			return memory.OperationResult{}, fmt.Errorf("commit memory decision replay: %w", err)
		}
		return result, nil
	}
	generation, err := lockWritableGeneration(ctx, tx)
	if err != nil {
		return memory.OperationResult{}, err
	}
	var revision int64
	var status memory.CandidateStatus
	if err := tx.QueryRow(ctx, `
		SELECT revision,status FROM memory_candidate_heads WHERE candidate_id=$1 FOR UPDATE`, plan.CandidateID).Scan(&revision, &status); errors.Is(err, pgx.ErrNoRows) {
		return memory.OperationResult{}, &memory.Error{Code: memory.CodeNotFound, CandidateID: plan.CandidateID}
	} else if err != nil {
		return memory.OperationResult{}, fmt.Errorf("lock memory candidate head: %w", err)
	}
	dbNow, err := databaseClock(ctx, tx)
	if err != nil {
		return memory.OperationResult{}, err
	}
	if status != memory.CandidatePending || revision != plan.ExpectedRevision {
		return memory.OperationResult{}, &memory.Error{
			Code: memory.CodeCandidateConflict, CandidateID: plan.CandidateID,
			ExpectedRevision: plan.ExpectedRevision, CurrentRevision: revision,
		}
	}
	candidateView, err := loadCandidate(ctx, tx, plan.CandidateID)
	if err != nil {
		return memory.OperationResult{}, err
	}
	if !dbNow.Before(candidateView.Candidate.ValidUntil) {
		if err := expireCandidateLocked(ctx, tx, plan.CandidateID, revision, dbNow); err != nil {
			return memory.OperationResult{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return memory.OperationResult{}, fmt.Errorf("commit expired memory candidate decision: %w", err)
		}
		return memory.OperationResult{}, candidateExpiredConflict(plan.CandidateID, plan.ExpectedRevision, revision+1)
	}
	if candidateView.ContentStatus != "available" {
		return memory.OperationResult{}, &memory.Error{Code: memory.CodeContentRedacted, CandidateID: plan.CandidateID}
	}
	correction := candidateView.Candidate.LogicalMemoryID != ""
	if plan.Decision.Decision == memory.DecisionAdmit {
		if correction && (plan.ExpectedRecordRevision < 1 || plan.ExpectedRecordGeneration < 1) {
			return memory.OperationResult{}, invalid("correction_decision_fence_required")
		}
		if !correction && (plan.ExpectedRecordRevision != 0 || plan.ExpectedRecordGeneration != 0) {
			return memory.OperationResult{}, invalid("unexpected_initial_admission_fence")
		}
	} else if plan.ExpectedRecordRevision != 0 || plan.ExpectedRecordGeneration != 0 {
		return memory.OperationResult{}, invalid("unexpected_rejection_record_fence")
	}
	if err := insertDecision(ctx, tx, plan.Decision); err != nil {
		return memory.OperationResult{}, err
	}
	newStatus := memory.CandidateStatusForDecision(plan.Decision.Decision)
	if _, err := tx.Exec(ctx, `
		UPDATE memory_candidate_heads
		SET revision=$2,status=$3,current_decision_id=$4,payload_available=FALSE,updated_at=$5
		WHERE candidate_id=$1 AND revision=$6 AND status='pending_review'`,
		plan.CandidateID, plan.Decision.Revision, newStatus, plan.Decision.ID, plan.Decision.CreatedAt, plan.ExpectedRevision); err != nil {
		return memory.OperationResult{}, fmt.Errorf("advance memory candidate head: %w", err)
	}
	var recordID, deliveryID, logicalID any
	if plan.Decision.Decision == memory.DecisionAdmit {
		if err := memory.ValidateDeliveryPayload(deliveryPolicy(candidateView.Candidate, plan.Decision), candidateView.ProposedContent, candidateView.Candidate.ContentHash); err != nil {
			return memory.OperationResult{}, err
		}
		candidate := candidateView.Candidate
		candidate.Status = memory.CandidateAdmitted
		candidate.Revision = plan.Decision.Revision
		candidate.CreatedAt = plan.Decision.CreatedAt
		ids := admissionIDsFromDecision(plan)
		if correction {
			ids.logicalMemoryID = candidate.LogicalMemoryID
			base, err := lockRecordCorrectionBase(ctx, tx, candidate.LogicalMemoryID, plan.ExpectedRecordRevision,
				plan.ExpectedRecordGeneration, generation)
			if err != nil {
				return memory.OperationResult{}, err
			}
			if err := insertRecordCorrection(ctx, tx, candidate, candidateView.ProposedContent, ids, generation, base, plan.Decision); err != nil {
				return memory.OperationResult{}, err
			}
		} else {
			candidate.LogicalMemoryID = plan.LogicalMemoryID
			if err := insertAdmission(ctx, tx, candidate, candidateView.ProposedContent, ids, generation, plan.Decision); err != nil {
				return memory.OperationResult{}, err
			}
		}
		logicalID, recordID, deliveryID = ids.logicalMemoryID, ids.recordRevisionID, ids.deliveryID
	}
	if _, err := tx.Exec(ctx, `DELETE FROM memory_candidate_payloads WHERE candidate_id=$1`, plan.CandidateID); err != nil {
		return memory.OperationResult{}, fmt.Errorf("scrub decided memory candidate payload: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO memory_operation_inbox(
			device_id,operation_id,request_hash,operation_kind,terminal_status,
			candidate_id,logical_memory_id,record_revision_id,delivery_id,completed_at)
		VALUES($1,$2,$3,'candidate_decision','succeeded',$4,$5,$6,$7,$8)`,
		plan.Operation.DeviceID, plan.Operation.OperationID, mustHash(plan.Operation.RequestHash),
		plan.CandidateID, logicalID, recordID, deliveryID, plan.Decision.CreatedAt); err != nil {
		return memory.OperationResult{}, fmt.Errorf("archive memory decision operation: %w", err)
	}
	archived := operationRecord{
		candidateID: stringPointer(plan.CandidateID), logicalMemoryID: anyStringPointer(logicalID),
		recordRevisionID: anyStringPointer(recordID), deliveryID: anyStringPointer(deliveryID),
	}
	result, err := loadOperationResult(ctx, tx, archived)
	if err != nil {
		return memory.OperationResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return memory.OperationResult{}, fmt.Errorf("commit memory candidate decision: %w", err)
	}
	return result, nil
}

func (s *Store) DeleteRecord(ctx context.Context, plan memory.DeletePlan) (memory.OperationResult, error) {
	if err := plan.Operation.Validate(); err != nil {
		return memory.OperationResult{}, err
	}
	if plan.Operation.Kind != memory.OperationRecordDelete || !canonicalUUID(plan.LogicalMemoryID) ||
		plan.ExpectedRevision < 1 || plan.ExpectedRecordGeneration < 1 || !canonicalUUID(plan.DeliveryID) ||
		!canonicalUUID(plan.DeliveryPayloadID) || !canonicalUUID(plan.ReceiptID) || !canonicalUUID(plan.OutboxID) ||
		plan.ValidUntil.IsZero() || plan.ValidUntil.Location() != time.UTC {
		return memory.OperationResult{}, invalid("invalid_record_delete_plan")
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return memory.OperationResult{}, fmt.Errorf("begin memory record delete: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if err := lockOperation(ctx, tx, plan.Operation); err != nil {
		return memory.OperationResult{}, err
	}
	if archived, found, err := readOperation(ctx, tx, plan.Operation); err != nil {
		return memory.OperationResult{}, err
	} else if found {
		if _, err := lockReadableGeneration(ctx, tx); err != nil {
			return memory.OperationResult{}, err
		}
		result, err := loadOperationResult(ctx, tx, archived)
		if err != nil {
			return memory.OperationResult{}, err
		}
		result.Replayed = true
		if err := tx.Commit(ctx); err != nil {
			return memory.OperationResult{}, err
		}
		return result, nil
	}
	generation, err := lockWritableGeneration(ctx, tx)
	if err != nil {
		return memory.OperationResult{}, err
	}
	type currentRecord struct {
		revisionID, candidateID, externalURI          string
		revision, recordGeneration, learnerGeneration int64
		status                                        memory.RecordStatus
		contentHash                                   []byte
		externalNode                                  *string
		externalMemory                                *int64
	}
	var current currentRecord
	err = tx.QueryRow(ctx, `
		SELECT h.current_record_revision_id,r.candidate_id,r.external_uri,h.current_revision,h.record_generation,
		       r.learner_generation,h.status,r.content_hash,h.external_node_id::text,h.external_memory_id
		FROM memory_record_heads h
		JOIN memory_record_revisions r ON r.id=h.current_record_revision_id
		WHERE h.logical_memory_id=$1 FOR UPDATE OF h`, plan.LogicalMemoryID).Scan(
		&current.revisionID, &current.candidateID, &current.externalURI, &current.revision,
		&current.recordGeneration, &current.learnerGeneration, &current.status, &current.contentHash,
		&current.externalNode, &current.externalMemory)
	if errors.Is(err, pgx.ErrNoRows) {
		return memory.OperationResult{}, &memory.Error{Code: memory.CodeNotFound}
	}
	if err != nil {
		return memory.OperationResult{}, fmt.Errorf("lock memory record for delete: %w", err)
	}
	if current.revision != plan.ExpectedRevision || current.recordGeneration != plan.ExpectedRecordGeneration ||
		current.learnerGeneration != generation.LearnerGeneration {
		return memory.OperationResult{}, &memory.Error{Code: memory.CodeMemoryConflict, Reason: "record_delete_compare_and_swap_failed"}
	}
	if current.status != memory.RecordApplied || current.externalNode == nil || current.externalMemory == nil {
		return memory.OperationResult{}, &memory.Error{Code: memory.CodeInvalidMemoryTransition, Reason: "record_not_applied"}
	}
	dbNow, err := databaseClock(ctx, tx)
	if err != nil {
		return memory.OperationResult{}, err
	}
	if !plan.ValidUntil.After(dbNow) {
		return memory.OperationResult{}, invalid("record_delete_ttl_elapsed")
	}
	newRecordGeneration := current.recordGeneration + 1
	intent := memory.OutboxIntent{
		DeliveryID: plan.DeliveryID, PayloadHash: hex.EncodeToString(current.contentHash),
		RecordRevision: current.revision, LearnerGeneration: generation.LearnerGeneration, RecordGeneration: newRecordGeneration,
	}
	payload, err := memory.MemoryOutboxPayload(intent)
	if err != nil {
		return memory.OperationResult{}, err
	}
	message, err := outbox.NewMessage(outbox.NewMessageInput{
		BusinessType: "memory.delivery", AggregateID: plan.LogicalMemoryID,
		IdempotencyKey: "memory.delivery:" + plan.DeliveryID, Revision: current.revision + 1,
		Generation: generation.LearnerGeneration, Payload: payload,
		AuditMetadata: json.RawMessage(`{"operation_kind":"record_delete"}`), MaxAttempts: 5,
	}, dbNow)
	if err != nil {
		return memory.OperationResult{}, err
	}
	message.ID = plan.OutboxID
	if _, err := outboxpostgres.EnqueueWith(ctx, tx, message); err != nil {
		return memory.OperationResult{}, fmt.Errorf("enqueue memory record delete: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO memory_deliveries(
			id,kind,logical_memory_id,record_revision_id,record_revision,learner_generation,record_generation,
			payload_id,payload_hash,external_uri,outbox_id,outbox_idempotency_key,valid_until,created_at)
		VALUES($1,'delete',$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`,
		plan.DeliveryID, plan.LogicalMemoryID, current.revisionID, current.revision, generation.LearnerGeneration,
		newRecordGeneration, plan.DeliveryPayloadID, current.contentHash, current.externalURI,
		plan.OutboxID, message.IdempotencyKey, plan.ValidUntil, dbNow); err != nil {
		return memory.OperationResult{}, fmt.Errorf("insert memory delete delivery: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO memory_delivery_receipts(
			id,delivery_id,version,status,reason,verification_method,created_at)
		VALUES($1,$2,1,'pending','delete_queued','not_yet_verified',$3)`, plan.ReceiptID, plan.DeliveryID, dbNow); err != nil {
		return memory.OperationResult{}, fmt.Errorf("insert memory delete receipt: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO memory_delivery_heads(
			delivery_id,logical_memory_id,status,public_status,attempt_state,current_attempt_id,
			attempt_count,current_receipt_id,current_receipt_version,updated_at)
		VALUES($1,$2,'delete_pending','queued','prepared',NULL,0,$3,1,$4)`,
		plan.DeliveryID, plan.LogicalMemoryID, plan.ReceiptID, dbNow); err != nil {
		return memory.OperationResult{}, fmt.Errorf("insert memory delete delivery head: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO memory_record_tombstones(
			id,logical_memory_id,record_revision_id,record_revision,previous_record_generation,
			tombstone_record_generation,learner_generation,delivery_id,operation_device_id,operation_id,content_hash,created_at)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`, uuid.NewString(), plan.LogicalMemoryID,
		current.revisionID, current.revision, current.recordGeneration, newRecordGeneration,
		generation.LearnerGeneration, plan.DeliveryID, plan.Operation.DeviceID, plan.Operation.OperationID,
		current.contentHash, dbNow); err != nil {
		return memory.OperationResult{}, fmt.Errorf("insert memory record tombstone: %w", err)
	}
	tag, err := tx.Exec(ctx, `
		UPDATE memory_record_heads
		SET record_generation=$2,status='delete_pending',current_delivery_id=$3,receipt_id=$4,updated_at=$5
		WHERE logical_memory_id=$1 AND current_record_revision_id=$6 AND current_revision=$7
		  AND record_generation=$8 AND status='applied'`, plan.LogicalMemoryID, newRecordGeneration,
		plan.DeliveryID, plan.ReceiptID, dbNow, current.revisionID, current.revision, current.recordGeneration)
	if err != nil {
		return memory.OperationResult{}, err
	}
	if tag.RowsAffected() != 1 {
		return memory.OperationResult{}, &memory.Error{Code: memory.CodeMemoryConflict}
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO memory_operation_inbox(
			device_id,operation_id,request_hash,operation_kind,terminal_status,
			logical_memory_id,record_revision_id,delivery_id,completed_at)
		VALUES($1,$2,$3,'record_delete','succeeded',$4,$5,$6,$7)`,
		plan.Operation.DeviceID, plan.Operation.OperationID, mustHash(plan.Operation.RequestHash),
		plan.LogicalMemoryID, current.revisionID, plan.DeliveryID, dbNow); err != nil {
		return memory.OperationResult{}, fmt.Errorf("archive memory record delete: %w", err)
	}
	archived := operationRecord{
		logicalMemoryID: stringPointer(plan.LogicalMemoryID), recordRevisionID: stringPointer(current.revisionID),
		deliveryID: stringPointer(plan.DeliveryID),
	}
	result, err := loadOperationResult(ctx, tx, archived)
	if err != nil {
		return memory.OperationResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return memory.OperationResult{}, fmt.Errorf("commit memory record delete: %w", err)
	}
	return result, nil
}

type recordCorrectionBase struct {
	recordRevisionID  string
	revision          int64
	recordGeneration  int64
	learnerGeneration int64
	status            memory.RecordStatus
	externalURI       string
}

func lockRecordCorrectionBase(
	ctx context.Context,
	tx pgx.Tx,
	logicalMemoryID string,
	expectedRevision, expectedRecordGeneration int64,
	generation memory.Generation,
) (recordCorrectionBase, error) {
	var base recordCorrectionBase
	err := tx.QueryRow(ctx, `
		SELECT h.current_record_revision_id,h.current_revision,h.record_generation,
		       r.learner_generation,h.status,r.external_uri
		FROM memory_record_heads h
		JOIN memory_record_revisions r ON r.id=h.current_record_revision_id
		WHERE h.logical_memory_id=$1
		FOR UPDATE OF h`, logicalMemoryID).Scan(
		&base.recordRevisionID, &base.revision, &base.recordGeneration,
		&base.learnerGeneration, &base.status, &base.externalURI,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return base, &memory.Error{Code: memory.CodeNotFound, Reason: "correction_logical_memory_not_found"}
	}
	if err != nil {
		return base, fmt.Errorf("lock correction record head: %w", err)
	}
	if base.revision != expectedRevision || base.recordGeneration != expectedRecordGeneration ||
		base.learnerGeneration != generation.LearnerGeneration {
		return base, &memory.Error{
			Code: memory.CodeMemoryConflict, Reason: "correction_record_compare_and_swap_failed",
			ExpectedRevision: expectedRevision, CurrentRevision: base.revision,
		}
	}
	if base.status == memory.RecordDeleted || base.status == memory.RecordDeletePending {
		return base, &memory.Error{Code: memory.CodeMemoryConflict, Reason: "correction_logical_memory_deleted"}
	}
	if base.status != memory.RecordApplied {
		return base, &memory.Error{Code: memory.CodeMemoryConflict, Reason: "correction_record_not_applied"}
	}
	if base.externalURI != memory.DeterministicExternalURI(logicalMemoryID) {
		return base, &memory.Error{Code: memory.CodeMemoryConflict, Reason: "correction_external_uri_mismatch"}
	}
	return base, nil
}

func insertRecordCorrection(
	ctx context.Context,
	db DBTX,
	candidate memory.Candidate,
	content string,
	ids admissionIDs,
	generation memory.Generation,
	base recordCorrectionBase,
	decision memory.CandidateDecision,
) error {
	if candidate.LogicalMemoryID != ids.logicalMemoryID || base.externalURI != memory.DeterministicExternalURI(ids.logicalMemoryID) {
		return invalid("correction_logical_memory_mismatch")
	}
	if err := memory.ValidateDeliveryPayload(deliveryPolicy(candidate, decision), content, candidate.ContentHash); err != nil {
		return err
	}
	now := candidate.CreatedAt
	newRevision := base.revision + 1
	newRecordGeneration := base.recordGeneration + 1
	intent := memory.OutboxIntent{
		DeliveryID: ids.deliveryID, PayloadHash: candidate.ContentHash,
		RecordRevision: newRevision, LearnerGeneration: generation.LearnerGeneration,
		RecordGeneration: newRecordGeneration,
	}
	payload, err := memory.MemoryOutboxPayload(intent)
	if err != nil {
		return err
	}
	message, err := outbox.NewMessage(outbox.NewMessageInput{
		BusinessType: "memory.delivery", AggregateID: ids.logicalMemoryID,
		IdempotencyKey: "memory.delivery:" + ids.deliveryID, Revision: newRevision,
		Generation: generation.LearnerGeneration, Payload: payload,
		AuditMetadata: json.RawMessage(`{"policy_version":"memory-admission-v1","operation_kind":"record_correction"}`),
		MaxAttempts:   5,
	}, now)
	if err != nil {
		return fmt.Errorf("create correction outbox message: %w", err)
	}
	message.ID = ids.outboxID
	if _, err := outboxpostgres.EnqueueWith(ctx, db, message); err != nil {
		return fmt.Errorf("enqueue memory correction: %w", err)
	}
	contentHash := mustHash(candidate.ContentHash)
	uriDigest := mustHash(memory.SHA256String(base.externalURI))
	if _, err := db.Exec(ctx, `
		INSERT INTO memory_record_revisions(
			id,logical_memory_id,revision,record_generation,learner_generation,candidate_id,
			previous_revision_id,external_uri,external_uri_digest,content_hash,delivery_id,created_at)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`,
		ids.recordRevisionID, ids.logicalMemoryID, newRevision, newRecordGeneration,
		generation.LearnerGeneration, candidate.ID, base.recordRevisionID, base.externalURI,
		uriDigest, contentHash, ids.deliveryID, now); err != nil {
		return fmt.Errorf("insert correction record revision: %w", err)
	}
	if _, err := db.Exec(ctx, `
		INSERT INTO memory_deliveries(
			id,kind,logical_memory_id,record_revision_id,record_revision,learner_generation,record_generation,
			payload_id,payload_hash,external_uri,outbox_id,outbox_idempotency_key,valid_until,created_at)
		VALUES($1,'correction',$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`,
		ids.deliveryID, ids.logicalMemoryID, ids.recordRevisionID, newRevision,
		generation.LearnerGeneration, newRecordGeneration, ids.deliveryPayloadID, contentHash,
		base.externalURI, ids.outboxID, message.IdempotencyKey, candidate.ValidUntil, now); err != nil {
		return fmt.Errorf("insert correction delivery: %w", err)
	}
	if _, err := db.Exec(ctx, `
		INSERT INTO memory_delivery_payloads(id,delivery_id,content,content_hash,valid_until,created_at)
		VALUES($1,$2,$3,$4,$5,$6)`, ids.deliveryPayloadID, ids.deliveryID, content,
		contentHash, candidate.ValidUntil, now); err != nil {
		return fmt.Errorf("insert correction delivery payload: %w", err)
	}
	if _, err := db.Exec(ctx, `
		INSERT INTO memory_delivery_receipts(
			id,delivery_id,version,status,reason,verification_method,created_at)
		VALUES($1,$2,1,'pending','correction_queued','not_yet_verified',$3)`,
		ids.receiptID, ids.deliveryID, now); err != nil {
		return fmt.Errorf("insert correction delivery receipt: %w", err)
	}
	if _, err := db.Exec(ctx, `
		INSERT INTO memory_delivery_heads(
			delivery_id,logical_memory_id,status,public_status,attempt_state,current_attempt_id,
			attempt_count,current_receipt_id,current_receipt_version,updated_at)
		VALUES($1,$2,'queued','queued','prepared',NULL,0,$3,1,$4)`,
		ids.deliveryID, ids.logicalMemoryID, ids.receiptID, now); err != nil {
		return fmt.Errorf("insert correction delivery head: %w", err)
	}
	tag, err := db.Exec(ctx, `
		UPDATE memory_record_heads
		SET current_record_revision_id=$2,current_revision=$3,record_generation=$4,status='queued',
		    current_delivery_id=$5,receipt_id=$6,applied_at=NULL,superseded_at=NULL,deleted_at=NULL,updated_at=$7
		WHERE logical_memory_id=$1 AND current_record_revision_id=$8 AND current_revision=$9
		  AND record_generation=$10 AND status='applied'`,
		ids.logicalMemoryID, ids.recordRevisionID, newRevision, newRecordGeneration,
		ids.deliveryID, ids.receiptID, now, base.recordRevisionID, base.revision, base.recordGeneration)
	if err != nil {
		return fmt.Errorf("advance correction record head: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return &memory.Error{Code: memory.CodeMemoryConflict, Reason: "correction_record_head_changed"}
	}
	return nil
}

func insertCandidateMetadata(ctx context.Context, db DBTX, candidate memory.Candidate) error {
	hash, err := decodeHash(candidate.ContentHash)
	if err != nil {
		return err
	}
	sourceHashes := make([][]byte, 0, len(candidate.SourceReference.SourceHashes))
	for _, value := range candidate.SourceReference.SourceHashes {
		decoded, err := decodeHash(value)
		if err != nil {
			return err
		}
		sourceHashes = append(sourceHashes, decoded)
	}
	if _, err := db.Exec(ctx, `
		INSERT INTO memory_candidates(
			id,candidate_uri,logical_memory_id,payload_id,content_hash,source_kind,source_event_id,
			source_operation_id,source_model_id,source_prompt_revision,source_hashes,proposer_id,reason,
			category,sensitivity,stability,valid_until,admission_policy_version,created_at)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19)`,
		candidate.ID, candidate.URI, nullable(candidate.LogicalMemoryID), candidate.PayloadID, hash, candidate.Source,
		nullable(candidate.SourceReference.EventID), nullable(candidate.SourceReference.OperationID),
		nullable(candidate.SourceReference.ModelID), nullable(candidate.SourceReference.PromptRevision), sourceHashes,
		candidate.ProposerID, candidate.Reason, candidate.Category, candidate.Sensitivity, candidate.Stability,
		candidate.ValidUntil, candidate.PolicyVersion, candidate.CreatedAt); err != nil {
		return fmt.Errorf("insert memory candidate metadata: %w", err)
	}
	return nil
}

func insertDecision(ctx context.Context, db DBTX, decision memory.CandidateDecision) error {
	var requestHash any
	if decision.RequestHash != "" {
		requestHash = mustHash(decision.RequestHash)
	}
	if _, err := db.Exec(ctx, `
		INSERT INTO memory_candidate_decisions(
			id,candidate_id,revision,decision,reason,actor_kind,actor_device_id,operation_id,request_hash,created_at)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
		decision.ID, decision.CandidateID, decision.Revision, decision.Decision, decision.Reason,
		decision.ActorKind, nullable(decision.ActorID), nullable(decision.OperationID), requestHash, decision.CreatedAt); err != nil {
		return fmt.Errorf("insert memory candidate decision: %w", err)
	}
	return nil
}

func insertAdmission(ctx context.Context, db DBTX, candidate memory.Candidate, content string, ids admissionIDs, generation memory.Generation, decision memory.CandidateDecision) error {
	if err := memory.ValidateDeliveryPayload(deliveryPolicy(candidate, decision), content, candidate.ContentHash); err != nil {
		return err
	}
	now := candidate.CreatedAt
	externalURI := memory.DeterministicExternalURI(ids.logicalMemoryID)
	intent := memory.OutboxIntent{
		DeliveryID: ids.deliveryID, PayloadHash: candidate.ContentHash,
		RecordRevision: 1, LearnerGeneration: generation.LearnerGeneration, RecordGeneration: 1,
	}
	payload, err := memory.MemoryOutboxPayload(intent)
	if err != nil {
		return err
	}
	message, err := outbox.NewMessage(outbox.NewMessageInput{
		BusinessType: "memory.delivery", AggregateID: ids.logicalMemoryID,
		IdempotencyKey: "memory.delivery:" + ids.deliveryID, Revision: 1,
		Generation: generation.LearnerGeneration, Payload: payload,
		AuditMetadata: json.RawMessage(`{"policy_version":"memory-admission-v1"}`), MaxAttempts: 5,
	}, now)
	if err != nil {
		return fmt.Errorf("create memory outbox message: %w", err)
	}
	message.ID = ids.outboxID
	if _, err := outboxpostgres.EnqueueWith(ctx, db, message); err != nil {
		return fmt.Errorf("enqueue memory delivery: %w", err)
	}
	if _, err := db.Exec(ctx, `
		INSERT INTO memory_logical_memories(id,created_from_candidate_id,created_at) VALUES($1,$2,$3)`,
		ids.logicalMemoryID, candidate.ID, now); err != nil {
		return fmt.Errorf("insert logical memory: %w", err)
	}
	contentHash := mustHash(candidate.ContentHash)
	uriDigest := mustHash(memory.SHA256String(externalURI))
	if _, err := db.Exec(ctx, `
		INSERT INTO memory_record_revisions(
			id,logical_memory_id,revision,record_generation,learner_generation,candidate_id,
			previous_revision_id,external_uri,external_uri_digest,content_hash,delivery_id,created_at)
		VALUES($1,$2,1,1,$3,$4,NULL,$5,$6,$7,$8,$9)`,
		ids.recordRevisionID, ids.logicalMemoryID, generation.LearnerGeneration, candidate.ID,
		externalURI, uriDigest, contentHash, ids.deliveryID, now); err != nil {
		return fmt.Errorf("insert memory record revision: %w", err)
	}
	if _, err := db.Exec(ctx, `
		INSERT INTO memory_deliveries(
			id,kind,logical_memory_id,record_revision_id,record_revision,learner_generation,record_generation,
			payload_id,payload_hash,external_uri,outbox_id,outbox_idempotency_key,valid_until,created_at)
		VALUES($1,'admit',$2,$3,1,$4,1,$5,$6,$7,$8,$9,$10,$11)`,
		ids.deliveryID, ids.logicalMemoryID, ids.recordRevisionID, generation.LearnerGeneration,
		ids.deliveryPayloadID, contentHash, externalURI, ids.outboxID, message.IdempotencyKey, candidate.ValidUntil, now); err != nil {
		return fmt.Errorf("insert memory delivery: %w", err)
	}
	if _, err := db.Exec(ctx, `
		INSERT INTO memory_delivery_payloads(id,delivery_id,content,content_hash,valid_until,created_at)
		VALUES($1,$2,$3,$4,$5,$6)`, ids.deliveryPayloadID, ids.deliveryID, content, contentHash, candidate.ValidUntil, now); err != nil {
		return fmt.Errorf("insert memory delivery payload: %w", err)
	}
	if _, err := db.Exec(ctx, `
		INSERT INTO memory_delivery_receipts(
			id,delivery_id,version,status,reason,verification_method,created_at)
		VALUES($1,$2,1,'pending','queued','not_yet_verified',$3)`, ids.receiptID, ids.deliveryID, now); err != nil {
		return fmt.Errorf("insert memory delivery receipt: %w", err)
	}
	if _, err := db.Exec(ctx, `
		INSERT INTO memory_delivery_heads(
			delivery_id,logical_memory_id,status,public_status,attempt_state,current_attempt_id,
			attempt_count,current_receipt_id,current_receipt_version,updated_at)
		VALUES($1,$2,'queued','queued','prepared',NULL,0,$3,1,$4)`,
		ids.deliveryID, ids.logicalMemoryID, ids.receiptID, now); err != nil {
		return fmt.Errorf("insert memory delivery head: %w", err)
	}
	if _, err := db.Exec(ctx, `
		INSERT INTO memory_record_heads(
			logical_memory_id,current_record_revision_id,current_revision,record_generation,status,
			current_delivery_id,receipt_id,updated_at)
		VALUES($1,$2,1,1,'queued',$3,$4,$5)`,
		ids.logicalMemoryID, ids.recordRevisionID, ids.deliveryID, ids.receiptID, now); err != nil {
		return fmt.Errorf("insert memory record head: %w", err)
	}
	return nil
}

func deliveryPolicy(candidate memory.Candidate, decision memory.CandidateDecision) memory.DeliveryPolicy {
	return memory.DeliveryPolicy{
		CandidateID: candidate.ID, Source: candidate.Source, Category: candidate.Category,
		Sensitivity: candidate.Sensitivity, Stability: candidate.Stability,
		PolicyVersion: candidate.PolicyVersion, ContentHash: candidate.ContentHash,
		AdmissionDecision: decision,
	}
}

func lockOperation(ctx context.Context, tx pgx.Tx, operation memory.Operation) error {
	key := "memory-operation:" + operation.DeviceID + ":" + operation.OperationID
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, key); err != nil {
		return fmt.Errorf("lock memory operation: %w", err)
	}
	return nil
}

func operationArchived(ctx context.Context, db DBTX, operation memory.Operation) (bool, error) {
	var archived bool
	if err := db.QueryRow(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM memory_operation_inbox WHERE device_id=$1 AND operation_id=$2
		)`, operation.DeviceID, operation.OperationID).Scan(&archived); err != nil {
		return false, fmt.Errorf("probe memory operation replay: %w", err)
	}
	return archived, nil
}

func readOperation(ctx context.Context, db DBTX, operation memory.Operation) (operationRecord, bool, error) {
	var record operationRecord
	err := db.QueryRow(ctx, `
		SELECT request_hash,operation_kind,candidate_id::text,logical_memory_id::text,record_revision_id::text,delivery_id::text
		FROM memory_operation_inbox WHERE device_id=$1 AND operation_id=$2 FOR UPDATE`,
		operation.DeviceID, operation.OperationID).Scan(
		&record.requestHash, &record.operationKind, &record.candidateID, &record.logicalMemoryID, &record.recordRevisionID, &record.deliveryID,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return operationRecord{}, false, nil
	}
	if err != nil {
		return operationRecord{}, false, fmt.Errorf("read memory operation inbox: %w", err)
	}
	if hex.EncodeToString(record.requestHash) != operation.RequestHash || record.operationKind != operation.Kind {
		return operationRecord{}, true, &memory.Error{Code: memory.CodeIdempotencyConflict}
	}
	return record, true, nil
}

func lockWritableGeneration(ctx context.Context, db DBTX) (memory.Generation, error) {
	rows, err := db.Query(ctx, `
		SELECT owner_kind,learner_generation,read_open,write_open,updated_at
		FROM privacy_owner_generation_gates
		WHERE owner_kind IN ('memory','outbox')
		ORDER BY owner_kind
		FOR UPDATE`)
	if err != nil {
		return memory.Generation{}, fmt.Errorf("lock memory/outbox generation gates: %w", err)
	}
	defer rows.Close()
	type ownerGate struct {
		generation          int64
		readOpen, writeOpen bool
		updatedAt           time.Time
	}
	gates := make(map[string]ownerGate, 2)
	for rows.Next() {
		var owner string
		var gate ownerGate
		if err := rows.Scan(&owner, &gate.generation, &gate.readOpen, &gate.writeOpen, &gate.updatedAt); err != nil {
			return memory.Generation{}, fmt.Errorf("scan memory/outbox generation gates: %w", err)
		}
		gates[owner] = gate
	}
	if err := rows.Err(); err != nil {
		return memory.Generation{}, fmt.Errorf("iterate memory/outbox generation gates: %w", err)
	}
	memoryGate, memoryOK := gates["memory"]
	outboxGate, outboxOK := gates["outbox"]
	if !memoryOK || !outboxOK {
		return memory.Generation{}, fmt.Errorf("memory/outbox generation gates are incomplete")
	}
	value := memory.Generation{
		LearnerGeneration: memoryGate.generation,
		MemoryGeneration:  memoryGate.generation,
		ReadOpen:          memoryGate.readOpen,
		WriteOpen:         memoryGate.writeOpen && outboxGate.writeOpen,
		UpdatedAt:         memoryGate.updatedAt.UTC(),
	}
	if !memoryGate.writeOpen || !outboxGate.writeOpen {
		return value, &memory.Error{Code: memory.CodePrivacyClearInProgress, Reason: "memory_outbox_write_gate_closed"}
	}
	if memoryGate.generation != outboxGate.generation {
		return value, &memory.Error{Code: memory.CodePrivacyClearInProgress, Reason: "memory_outbox_generation_mismatch"}
	}
	return value, nil
}

func databaseClock(ctx context.Context, db DBTX) (time.Time, error) {
	var now time.Time
	if err := db.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		return time.Time{}, fmt.Errorf("read memory database clock: %w", err)
	}
	return now.UTC(), nil
}

func candidateExpiredConflict(candidateID string, expectedRevision, currentRevision int64) error {
	return &memory.Error{
		Code: memory.CodeCandidateConflict, Reason: "candidate_expired", CandidateID: candidateID,
		ExpectedRevision: expectedRevision, CurrentRevision: currentRevision,
	}
}

func admissionIDsFromCreate(plan memory.CreatePlan) admissionIDs {
	return admissionIDs{plan.LogicalMemoryID, plan.RecordRevisionID, plan.DeliveryID, plan.DeliveryPayloadID, plan.ReceiptID, plan.OutboxID}
}
func admissionIDsFromDecision(plan memory.DecisionPlan) admissionIDs {
	return admissionIDs{plan.LogicalMemoryID, plan.RecordRevisionID, plan.DeliveryID, plan.DeliveryPayloadID, plan.ReceiptID, plan.OutboxID}
}
func validateAdmissionIDs(ids admissionIDs) error {
	for _, value := range []string{ids.logicalMemoryID, ids.recordRevisionID, ids.deliveryID, ids.deliveryPayloadID, ids.receiptID, ids.outboxID} {
		if parsed, err := uuid.Parse(value); err != nil || parsed.String() != strings.ToLower(value) {
			return invalid("invalid_admission_identity")
		}
	}
	return nil
}
func decodeHash(value string) ([]byte, error) {
	decoded, err := hex.DecodeString(value)
	if err != nil || len(decoded) != 32 {
		return nil, invalid("invalid_sha256")
	}
	return decoded, nil
}
func mustHash(value string) []byte {
	decoded, _ := hex.DecodeString(value)
	return decoded
}
func nullable(value string) any {
	if value == "" {
		return nil
	}
	return value
}
func invalid(reason string) error {
	return &memory.Error{Code: memory.CodeInvalidRequest, Reason: reason}
}
func stringPointer(value string) *string { return &value }
func anyStringPointer(value any) *string {
	text, ok := value.(string)
	if !ok || text == "" {
		return nil
	}
	return &text
}
