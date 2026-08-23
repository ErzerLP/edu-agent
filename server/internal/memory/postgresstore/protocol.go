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

func (s *Store) LoadDeliveryWork(ctx context.Context, intent memory.OutboxIntent) (memory.DeliveryWork, outbox.ApplyDecision, error) {
	var work memory.DeliveryWork
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return work, outbox.ApplyDecision{}, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	locked, err := lockDelivery(ctx, tx, intent.DeliveryID)
	if err != nil {
		return work, outbox.ApplyDecision{}, err
	}
	work.Delivery, err = loadDelivery(ctx, tx, intent.DeliveryID)
	if err != nil {
		return work, outbox.ApplyDecision{}, err
	}
	work.CurrentGeneration = locked.generation
	var content, previousHash, externalNode, decisionActorID, decisionOperationID *string
	var sourceEventID, sourceOperationID, sourceModelID, sourcePromptRevision *string
	var externalMemory *int64
	var policyContentHash, decisionRequestHash []byte
	var sourceHashes [][]byte
	err = tx.QueryRow(ctx, `
		SELECT c.id,c.source_kind,c.source_event_id::text,c.source_operation_id::text,
		       c.source_model_id,c.source_prompt_revision,c.source_hashes,
		       c.category,c.sensitivity,c.stability,c.admission_policy_version,c.content_hash,
		       cd.id,cd.candidate_id,cd.revision,cd.decision,cd.reason,cd.actor_device_id::text,cd.actor_kind,
		       cd.operation_id::text,cd.request_hash,cd.created_at,
		       p.content,encode(previous.content_hash,'hex'),
		       CASE WHEN d.kind='correction' THEN previous_ref.external_node_id::text ELSE current_ref.external_node_id::text END,
		       CASE WHEN d.kind='correction' THEN previous_ref.external_memory_id ELSE current_ref.external_memory_id END
		FROM memory_deliveries d
		JOIN memory_record_revisions r ON r.id=d.record_revision_id
		JOIN memory_candidates c ON c.id=r.candidate_id
		JOIN memory_candidate_heads ch ON ch.candidate_id=c.id AND ch.status='admitted'
		JOIN memory_candidate_decisions cd ON cd.id=ch.current_decision_id AND cd.decision='admit'
		JOIN memory_record_heads rh ON rh.logical_memory_id=d.logical_memory_id
		LEFT JOIN memory_delivery_payloads p ON p.delivery_id=d.id
		LEFT JOIN memory_record_revisions previous ON previous.id=r.previous_revision_id
		LEFT JOIN memory_record_external_refs previous_ref ON previous_ref.record_revision_id=previous.id
		LEFT JOIN memory_record_external_refs current_ref ON current_ref.record_revision_id=r.id
		WHERE d.id=$1`, intent.DeliveryID).Scan(
		&work.Policy.CandidateID, &work.Policy.Source, &sourceEventID, &sourceOperationID,
		&sourceModelID, &sourcePromptRevision, &sourceHashes,
		&work.Policy.Category, &work.Policy.Sensitivity, &work.Policy.Stability,
		&work.Policy.PolicyVersion, &policyContentHash,
		&work.Policy.AdmissionDecision.ID, &work.Policy.AdmissionDecision.CandidateID,
		&work.Policy.AdmissionDecision.Revision, &work.Policy.AdmissionDecision.Decision,
		&work.Policy.AdmissionDecision.Reason, &decisionActorID, &work.Policy.AdmissionDecision.ActorKind,
		&decisionOperationID, &decisionRequestHash, &work.Policy.AdmissionDecision.CreatedAt,
		&content, &previousHash, &externalNode, &externalMemory,
	)
	if err != nil {
		return work, outbox.ApplyDecision{}, fmt.Errorf("load memory delivery work: %w", err)
	}
	work.Policy.ContentHash = fmt.Sprintf("%x", policyContentHash)
	work.Policy.SourceReference = memory.SourceReference{
		EventID: derefProtocol(sourceEventID), OperationID: derefProtocol(sourceOperationID),
		ModelID: derefProtocol(sourceModelID), PromptRevision: derefProtocol(sourcePromptRevision),
	}
	for _, hash := range sourceHashes {
		work.Policy.SourceReference.SourceHashes = append(work.Policy.SourceReference.SourceHashes, fmt.Sprintf("%x", hash))
	}
	work.Policy.AdmissionDecision.ActorID = derefProtocol(decisionActorID)
	work.Policy.AdmissionDecision.OperationID = derefProtocol(decisionOperationID)
	if len(decisionRequestHash) > 0 {
		work.Policy.AdmissionDecision.RequestHash = fmt.Sprintf("%x", decisionRequestHash)
	}
	work.Policy.AdmissionDecision.CreatedAt = work.Policy.AdmissionDecision.CreatedAt.UTC()
	if content != nil {
		work.Content = *content
	}
	if previousHash != nil {
		work.PreviousContentHash = *previousHash
	}
	if externalNode != nil {
		work.ExternalNodeID = *externalNode
	}
	if externalMemory != nil {
		work.ExternalMemoryID = *externalMemory
	}
	if err := work.ValidateIntent(intent); err != nil {
		return work, outbox.ApplyDecision{}, err
	}
	decision := outbox.ApplyDecision{Apply: true}
	switch {
	case locked.learnerGeneration != locked.generation.LearnerGeneration:
		decision = outbox.ApplyDecision{TerminalDisposition: outbox.DispositionPrivacyErasure}
	case !locked.currentLineage():
		decision = outbox.ApplyDecision{TerminalDisposition: outbox.DispositionSuperseded}
	case locked.deliveryStatus == memory.DeliveryStatusExpiryReconciling || locked.deliveryStatus == memory.DeliveryStatusExpired || !locked.dbNow.Before(locked.validUntil):
		decision = outbox.ApplyDecision{TerminalDisposition: outbox.DispositionExpired}
	case locked.deliveryStatus == memory.DeliveryStatusPermanentlyRejected:
		decision = outbox.ApplyDecision{TerminalDisposition: outbox.DispositionPermanentlyRejected}
	case locked.deliveryStatus == memory.DeliveryStatusFenced:
		decision = outbox.ApplyDecision{TerminalDisposition: outbox.DispositionFenced}
	case locked.deliveryStatus == memory.DeliveryStatusDeleted:
		decision = outbox.ApplyDecision{TerminalDisposition: outbox.DispositionDeleted}
	case locked.deliveryStatus != memory.DeliveryStatusQueued && locked.deliveryStatus != memory.DeliveryStatusApplied && locked.deliveryStatus != memory.DeliveryStatusDeletePending:
		return work, outbox.ApplyDecision{}, &memory.Error{Code: memory.CodeDeliveryConflict, Reason: "delivery_status_not_applicable"}
	}
	if decision.Apply && (work.Delivery.Kind == memory.DeliveryAdmit || work.Delivery.Kind == memory.DeliveryCorrection) && work.Delivery.Status == memory.DeliveryStatusQueued && content == nil {
		return work, outbox.ApplyDecision{}, &memory.Error{Code: memory.CodeDeliveryConflict, Reason: "delivery_payload_unavailable"}
	}
	if err := decision.Validate(); err != nil {
		return work, outbox.ApplyDecision{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return work, outbox.ApplyDecision{}, err
	}
	return work, decision, nil
}

func (s *Store) LoadDeliveryWorkByID(ctx context.Context, deliveryID string) (memory.DeliveryWork, error) {
	if !canonicalUUID(deliveryID) {
		return memory.DeliveryWork{}, invalid("invalid_delivery_id")
	}
	var intent memory.OutboxIntent
	var hash []byte
	err := s.pool.QueryRow(ctx, `SELECT id,payload_hash,record_revision,learner_generation,record_generation FROM memory_deliveries WHERE id=$1`, deliveryID).Scan(
		&intent.DeliveryID, &hash, &intent.RecordRevision, &intent.LearnerGeneration, &intent.RecordGeneration)
	if errors.Is(err, pgx.ErrNoRows) {
		return memory.DeliveryWork{}, &memory.Error{Code: memory.CodeNotFound}
	}
	if err != nil {
		return memory.DeliveryWork{}, err
	}
	intent.PayloadHash = fmt.Sprintf("%x", hash)
	work, _, err := s.LoadDeliveryWork(ctx, intent)
	return work, err
}

func derefProtocol(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func (s *Store) ClaimUnknownAttempt(ctx context.Context, _ time.Time, lease time.Duration) (memory.Attempt, error) {
	if lease <= 0 {
		return memory.Attempt{}, invalid("invalid_attempt_claim")
	}
	var deliveryID string
	err := s.pool.QueryRow(ctx, `
		SELECT a.delivery_id
		FROM memory_delivery_attempts a
		JOIN memory_delivery_attempt_heads ah ON ah.attempt_id=a.id
		JOIN memory_delivery_heads dh ON dh.delivery_id=a.delivery_id
		WHERE ah.state IN ('sent','unknown','reconciling') AND ah.lease_expires_at <= clock_timestamp()
		  AND dh.status='queued'
		ORDER BY ah.updated_at,a.id LIMIT 1`).Scan(&deliveryID)
	if errors.Is(err, pgx.ErrNoRows) {
		return memory.Attempt{}, &memory.Error{Code: memory.CodeNotFound}
	}
	if err != nil {
		return memory.Attempt{}, fmt.Errorf("select unknown memory attempt: %w", err)
	}
	return s.ClaimAttempt(ctx, deliveryID, time.Time{}, lease)
}

func (s *Store) AuthorizeAttemptRetry(ctx context.Context, input memory.AttemptRetryAuthorization) (memory.Attempt, error) {
	if !canonicalUUID(input.AttemptID) || !canonicalUUID(input.AttemptToken) || !canonicalUUID(input.LeaseToken) ||
		input.From != memory.AttemptReconciling || input.AbsenceObservations < 2 || input.ObservedBootEpoch == "" || input.At.IsZero() {
		return memory.Attempt{}, invalid("invalid_attempt_retry_authorization")
	}
	evidence, err := decodeHash(input.EvidenceDigest)
	if err != nil {
		return memory.Attempt{}, err
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
	if !locked.currentForMutation() || locked.currentAttemptID == nil || *locked.currentAttemptID != attempt.ID ||
		attempt.AttemptToken != input.AttemptToken || attempt.LeaseToken != input.LeaseToken || attempt.State != input.From ||
		!attempt.LeaseExpiresAt.After(locked.dbNow) || attempt.BootEpoch == "" || attempt.BootEpoch == input.ObservedBootEpoch {
		return memory.Attempt{}, outbox.ErrLeaseLost
	}
	tag, err := tx.Exec(ctx, `
		UPDATE memory_delivery_attempt_heads
		SET state='fenced',lease_token=NULL,lease_expires_at=NULL,restart_boot_epoch=$2,
		    absence_observations=$3,restart_verified_at=$4,restart_evidence_digest=$5,
		    error_category='boot_epoch_absence_verified',updated_at=$4
		WHERE attempt_id=$1 AND state='reconciling' AND lease_token=$6 AND lease_expires_at>$4`,
		attempt.ID, input.ObservedBootEpoch, input.AbsenceObservations, locked.dbNow, evidence, input.LeaseToken)
	if err != nil {
		return memory.Attempt{}, err
	}
	if tag.RowsAffected() != 1 {
		return memory.Attempt{}, outbox.ErrLeaseLost
	}
	replacement := memory.Attempt{
		ID: uuid.NewString(), DeliveryID: deliveryID, AttemptToken: uuid.NewString(), State: memory.AttemptPrepared,
		LeaseToken: uuid.NewString(), LeaseExpiresAt: attempt.LeaseExpiresAt, CreatedAt: locked.dbNow, UpdatedAt: locked.dbNow,
	}
	// Preserve the configured lease duration from the reconciler claim without trusting caller time.
	replacement.LeaseExpiresAt = locked.dbNow.Add(attempt.LeaseExpiresAt.Sub(attempt.UpdatedAt))
	if !replacement.LeaseExpiresAt.After(locked.dbNow) {
		replacement.LeaseExpiresAt = locked.dbNow.Add(time.Second)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO memory_delivery_attempts(
			id,delivery_id,attempt_token,authorized_by_attempt_id,authorization_boot_epoch,
			authorization_evidence_digest,created_at)
		VALUES($1,$2,$3,$4,$5,$6,$7)`, replacement.ID, deliveryID, replacement.AttemptToken,
		attempt.ID, input.ObservedBootEpoch, evidence, locked.dbNow); err != nil {
		return memory.Attempt{}, err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO memory_delivery_attempt_heads(attempt_id,delivery_id,state,lease_token,lease_expires_at,updated_at)
		VALUES($1,$2,'prepared',$3,$4,$5)`, replacement.ID, deliveryID, replacement.LeaseToken,
		replacement.LeaseExpiresAt, locked.dbNow); err != nil {
		return memory.Attempt{}, err
	}
	tag, err = tx.Exec(ctx, `
		UPDATE memory_delivery_heads
		SET current_attempt_id=$3,attempt_state='prepared',attempt_count=attempt_count+1,updated_at=$4
		WHERE delivery_id=$1 AND current_attempt_id=$2 AND status IN ('queued','delete_pending')`,
		deliveryID, attempt.ID, replacement.ID, locked.dbNow)
	if err != nil {
		return memory.Attempt{}, err
	}
	if tag.RowsAffected() != 1 {
		return memory.Attempt{}, outbox.ErrLeaseLost
	}
	if err := tx.Commit(ctx); err != nil {
		return memory.Attempt{}, err
	}
	return replacement, nil
}

func (s *Store) SaveRemoteDeletePlan(ctx context.Context, plan memory.RemoteDeletePlan) (memory.RemoteDeletePlan, error) {
	if err := plan.Validate(); err != nil {
		return memory.RemoteDeletePlan{}, err
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return memory.RemoteDeletePlan{}, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	locked, err := lockDelivery(ctx, tx, plan.DeliveryID)
	if err != nil {
		return memory.RemoteDeletePlan{}, err
	}
	if !locked.currentLineage() || (locked.deliveryStatus != memory.DeliveryStatusExpiryReconciling && locked.deliveryStatus != memory.DeliveryStatusDeletePending) {
		return memory.RemoteDeletePlan{}, &memory.Error{Code: memory.CodeDeliveryConflict, Reason: "remote_delete_plan_not_current"}
	}
	tag, err := tx.Exec(ctx, `
		INSERT INTO memory_remote_delete_plans(id,delivery_id,node_uuid,external_uri,active_memory_id,review_cleanup_needed,snapshot_digest,created_at)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8) ON CONFLICT(delivery_id) DO NOTHING`,
		plan.ID, plan.DeliveryID, plan.NodeID, plan.ExternalURI, plan.ActiveMemoryID, plan.ReviewCleanupNeeded, mustHash(plan.SnapshotDigest), locked.dbNow)
	if err != nil {
		return memory.RemoteDeletePlan{}, fmt.Errorf("persist remote delete plan: %w", err)
	}
	if tag.RowsAffected() == 1 {
		for _, memoryID := range plan.MemoryIDs {
			if _, err := tx.Exec(ctx, `INSERT INTO memory_remote_delete_versions(plan_id,memory_id,was_active) VALUES($1,$2,$3)`, plan.ID, memoryID, memoryID == plan.ActiveMemoryID); err != nil {
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
	if stored.NodeID != plan.NodeID || stored.ExternalURI != plan.ExternalURI || stored.ActiveMemoryID != plan.ActiveMemoryID || stored.SnapshotDigest != plan.SnapshotDigest {
		return memory.RemoteDeletePlan{}, &memory.Error{Code: memory.CodeDeliveryConflict, Reason: "remote_delete_snapshot_conflict"}
	}
	if err := tx.Commit(ctx); err != nil {
		return memory.RemoteDeletePlan{}, err
	}
	return stored, nil
}

func (s *Store) LoadRemoteDeletePlan(ctx context.Context, deliveryID string) (memory.RemoteDeletePlan, error) {
	if !canonicalUUID(deliveryID) {
		return memory.RemoteDeletePlan{}, invalid("invalid_delivery_id")
	}
	return loadRemoteDeletePlan(ctx, s.pool, deliveryID)
}

func loadRemoteDeletePlan(ctx context.Context, db DBTX, deliveryID string) (memory.RemoteDeletePlan, error) {
	var value memory.RemoteDeletePlan
	var erasureDeliveryID *string
	var digest []byte
	err := db.QueryRow(ctx, `SELECT id,delivery_id,erasure_delivery_id::text,node_uuid,external_uri,active_memory_id,review_cleanup_needed,snapshot_digest,created_at FROM memory_remote_delete_plans WHERE delivery_id=$1`, deliveryID).Scan(
		&value.ID, &value.DeliveryID, &erasureDeliveryID, &value.NodeID, &value.ExternalURI, &value.ActiveMemoryID, &value.ReviewCleanupNeeded, &digest, &value.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return value, &memory.Error{Code: memory.CodeNotFound}
	}
	if err != nil {
		return value, err
	}
	value.ErasureDeliveryID = derefProtocol(erasureDeliveryID)
	value.SnapshotDigest = fmt.Sprintf("%x", digest)
	value.CreatedAt = value.CreatedAt.UTC()
	rows, err := db.Query(ctx, `SELECT memory_id FROM memory_remote_delete_versions WHERE plan_id=$1 ORDER BY memory_id`, value.ID)
	if err != nil {
		return value, err
	}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return value, err
		}
		value.MemoryIDs = append(value.MemoryIDs, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return value, err
	}
	rows.Close()
	rows, err = db.Query(ctx, `SELECT namespace,domain,path,uri,is_alias FROM memory_remote_delete_paths WHERE plan_id=$1 ORDER BY namespace,domain,path`, value.ID)
	if err != nil {
		return value, err
	}
	for rows.Next() {
		var ref memory.RemotePathReference
		if err := rows.Scan(&ref.Namespace, &ref.Domain, &ref.Path, &ref.URI, &ref.Alias); err != nil {
			rows.Close()
			return value, err
		}
		value.Paths = append(value.Paths, ref)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return value, err
	}
	rows.Close()
	return value, nil
}

var _ memory.DeliveryProtocolPersistence = (*Store)(nil)
