package postgresstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/memory"
	"github.com/edu-agent/edu-agent/server/internal/platform/outbox"
	outboxpostgres "github.com/edu-agent/edu-agent/server/internal/platform/outbox/postgresstore"
	"github.com/jackc/pgx/v5"
)

func (s *Store) ReplayDelivery(ctx context.Context, plan memory.ReplayPlan) (memory.OperationResult, error) {
	if err := plan.Operation.Validate(); err != nil {
		return memory.OperationResult{}, err
	}
	if plan.Operation.Kind != memory.OperationDeliveryReplay || !canonicalUUID(plan.DeliveryID) {
		return memory.OperationResult{}, invalid("invalid_delivery_replay_plan")
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return memory.OperationResult{}, fmt.Errorf("begin memory delivery replay: %w", err)
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
		result, err := loadOperationResult(ctx, tx, archived)
		if err != nil {
			return memory.OperationResult{}, err
		}
		result.Replayed = true
		if err := tx.Commit(ctx); err != nil {
			return memory.OperationResult{}, fmt.Errorf("commit memory delivery replay read: %w", err)
		}
		return result, nil
	}

	locked, err := lockDelivery(ctx, tx, plan.DeliveryID)
	if err != nil {
		return memory.OperationResult{}, err
	}
	if !locked.currentForMutation() ||
		(locked.deliveryKind != memory.DeliveryAdmit && locked.deliveryKind != memory.DeliveryCorrection && locked.deliveryKind != memory.DeliveryDelete) {
		return memory.OperationResult{}, &memory.Error{Code: memory.CodeDeliveryConflict, Reason: "delivery_replay_not_current"}
	}

	var publicStatus memory.DeliveryPublicStatus
	var policy memory.DeliveryPolicy
	var deliveryPayloadID string
	var deliveryPayloadHash, policyContentHash, decisionRequestHash []byte
	var deliveryValidUntil time.Time
	var payloadID, content, decisionActorID, decisionOperationID *string
	var payloadHash []byte
	var payloadValidUntil *time.Time
	var outboxStatus outbox.Status
	var businessType, aggregateID, idempotencyKey string
	var outboxRevision, outboxGeneration int64
	var outboxPayload json.RawMessage
	var currentTombstone bool
	err = tx.QueryRow(ctx, `
		SELECT dh.public_status,d.payload_id,d.payload_hash,d.valid_until,
		       c.id,c.source_kind,c.category,c.sensitivity,c.stability,c.admission_policy_version,c.content_hash,
		       cd.id,cd.candidate_id,cd.revision,cd.decision,cd.reason,cd.actor_device_id::text,cd.actor_kind,
		       cd.operation_id::text,cd.request_hash,cd.created_at,
		       p.id::text,p.content,p.content_hash,p.valid_until,
		       o.status,o.business_type,o.aggregate_id,o.idempotency_key,o.revision,o.generation,o.payload,
		       EXISTS(
		         SELECT 1 FROM memory_record_tombstones t
		         WHERE t.delivery_id=d.id AND t.logical_memory_id=d.logical_memory_id
		           AND t.record_revision_id=d.record_revision_id AND t.record_revision=d.record_revision
		           AND t.tombstone_record_generation=d.record_generation
		           AND t.learner_generation=d.learner_generation
		       )
		FROM memory_deliveries d
		JOIN memory_delivery_heads dh ON dh.delivery_id=d.id
		JOIN memory_record_revisions r ON r.id=d.record_revision_id
		JOIN memory_candidates c ON c.id=r.candidate_id
		JOIN memory_candidate_heads ch ON ch.candidate_id=c.id AND ch.status='admitted'
		JOIN memory_candidate_decisions cd ON cd.id=ch.current_decision_id AND cd.decision='admit'
		LEFT JOIN memory_delivery_payloads p ON p.delivery_id=d.id
		JOIN outbox_messages o ON o.id=d.outbox_id
		WHERE d.id=$1
		FOR UPDATE OF o`, plan.DeliveryID).Scan(
		&publicStatus, &deliveryPayloadID, &deliveryPayloadHash, &deliveryValidUntil,
		&policy.CandidateID, &policy.Source, &policy.Category, &policy.Sensitivity, &policy.Stability,
		&policy.PolicyVersion, &policyContentHash,
		&policy.AdmissionDecision.ID, &policy.AdmissionDecision.CandidateID,
		&policy.AdmissionDecision.Revision, &policy.AdmissionDecision.Decision,
		&policy.AdmissionDecision.Reason, &decisionActorID, &policy.AdmissionDecision.ActorKind,
		&decisionOperationID, &decisionRequestHash, &policy.AdmissionDecision.CreatedAt,
		&payloadID, &content, &payloadHash, &payloadValidUntil,
		&outboxStatus, &businessType, &aggregateID, &idempotencyKey,
		&outboxRevision, &outboxGeneration, &outboxPayload, &currentTombstone,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return memory.OperationResult{}, &memory.Error{Code: memory.CodeNotFound}
	}
	if err != nil {
		return memory.OperationResult{}, fmt.Errorf("lock dead memory delivery replay tuple: %w", err)
	}
	deliveryValidUntil = deliveryValidUntil.UTC()
	if publicStatus != memory.DeliveryQueued || outboxStatus != outbox.StatusDead {
		return memory.OperationResult{}, &memory.Error{Code: memory.CodeDeliveryConflict, Reason: "delivery_replay_requires_dead_queued_intent"}
	}
	payloadHashText := fmt.Sprintf("%x", deliveryPayloadHash)
	if _, err := tx.Exec(ctx, `
		UPDATE memory_delivery_attempt_heads
		SET lease_expires_at=$2,updated_at=$2
		WHERE delivery_id=$1 AND state='prepared' AND sent_at IS NULL`, locked.deliveryID, locked.dbNow); err != nil {
		return memory.OperationResult{}, fmt.Errorf("release unsent replay attempt: %w", err)
	}
	if locked.deliveryKind == memory.DeliveryDelete {
		if !currentTombstone {
			return memory.OperationResult{}, &memory.Error{Code: memory.CodeDeliveryConflict, Reason: "delete_replay_tombstone_not_current"}
		}
		// Purger persists the complete remote delete plan before its first mutation. A never-sent
		// delete can restart from discovery; a previously sent delete resumes that idempotent plan.
	} else {
		if payloadID == nil || content == nil || payloadValidUntil == nil || *payloadID != deliveryPayloadID ||
			len(payloadHash) != 32 || string(payloadHash) != string(deliveryPayloadHash) ||
			!payloadValidUntil.UTC().Equal(deliveryValidUntil) || !locked.dbNow.Before(payloadValidUntil.UTC()) {
			return memory.OperationResult{}, &memory.Error{Code: memory.CodeDeliveryConflict, Reason: "delivery_replay_payload_unavailable"}
		}
		policy.ContentHash = fmt.Sprintf("%x", policyContentHash)
		policy.AdmissionDecision.ActorID = derefProtocol(decisionActorID)
		policy.AdmissionDecision.OperationID = derefProtocol(decisionOperationID)
		policy.AdmissionDecision.RequestHash = fmt.Sprintf("%x", decisionRequestHash)
		policy.AdmissionDecision.CreatedAt = policy.AdmissionDecision.CreatedAt.UTC()
		if err := memory.ValidateDeliveryPayload(policy, *content, payloadHashText); err != nil {
			return memory.OperationResult{}, &memory.Error{Code: memory.CodeDeliveryConflict, Reason: "delivery_replay_payload_invalid", Cause: err}
		}
	}
	intent, err := memory.DecodeOutboxIntent(outboxPayload)
	if err != nil {
		return memory.OperationResult{}, &memory.Error{Code: memory.CodeDeliveryConflict, Reason: "delivery_replay_outbox_payload_invalid", Cause: err}
	}
	expectedOutboxRevision := locked.recordRevision
	if locked.deliveryKind == memory.DeliveryDelete {
		expectedOutboxRevision++
	}
	if intent.DeliveryID != locked.deliveryID || intent.PayloadHash != payloadHashText ||
		intent.RecordRevision != locked.recordRevision || intent.LearnerGeneration != locked.learnerGeneration ||
		intent.RecordGeneration != locked.recordGeneration || businessType != "memory.delivery" ||
		aggregateID != locked.logicalMemoryID || idempotencyKey != locked.outboxKey ||
		outboxRevision != expectedOutboxRevision || outboxGeneration != locked.learnerGeneration {
		return memory.OperationResult{}, &memory.Error{Code: memory.CodeDeliveryConflict, Reason: "delivery_replay_outbox_tuple_mismatch"}
	}
	if err := outboxpostgres.RequeueDeadWith(ctx, tx, outbox.RequeueRequest{
		BusinessType: businessType, AggregateID: aggregateID, IdempotencyKey: idempotencyKey,
		Revision: outboxRevision, Generation: outboxGeneration, Payload: outboxPayload,
		AvailableAt: locked.dbNow,
	}); err != nil {
		return memory.OperationResult{}, fmt.Errorf("requeue dead memory delivery: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO memory_operation_inbox(
			device_id,operation_id,request_hash,operation_kind,terminal_status,
			logical_memory_id,record_revision_id,delivery_id,completed_at)
		VALUES($1,$2,$3,'delivery_replay','succeeded',$4,$5,$6,$7)`,
		plan.Operation.DeviceID, plan.Operation.OperationID, mustHash(plan.Operation.RequestHash),
		locked.logicalMemoryID, locked.recordRevisionID, locked.deliveryID, locked.dbNow); err != nil {
		return memory.OperationResult{}, fmt.Errorf("archive memory delivery replay operation: %w", err)
	}
	archived := operationRecord{
		logicalMemoryID:  stringPointer(locked.logicalMemoryID),
		recordRevisionID: stringPointer(locked.recordRevisionID),
		deliveryID:       stringPointer(locked.deliveryID),
	}
	result, err := loadOperationResult(ctx, tx, archived)
	if err != nil {
		return memory.OperationResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return memory.OperationResult{}, fmt.Errorf("commit memory delivery replay: %w", err)
	}
	return result, nil
}
