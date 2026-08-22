package app

import (
	"context"

	"github.com/edu-agent/edu-agent/server/internal/privacy"
	"github.com/jackc/pgx/v5/pgxpool"
)

type disabledNocturneEvidenceReader struct {
	pool *pgxpool.Pool
}

func (r disabledNocturneEvidenceReader) ReadDisabledNocturneEvidence(ctx context.Context, request privacy.DisabledNocturneEvidenceRequest) (privacy.DisabledNocturneEvidence, error) {
	if err := request.Validate(); err != nil {
		return privacy.DisabledNocturneEvidence{}, err
	}
	var authorized bool
	var evidence privacy.DisabledNocturneEvidence
	err := r.pool.QueryRow(ctx, `
		WITH authorized AS (
			SELECT TRUE
			FROM privacy_erasures e
			JOIN privacy_erasure_heads h ON h.erasure_id=e.id
			WHERE e.id=$1 AND e.target_learner_generation=$2 AND h.status<>'verified'
		), reconciliations AS (
			SELECT status
			FROM memory_expiry_reconciliations
			WHERE learner_generation<$2 AND EXISTS (SELECT 1 FROM authorized)
		)
		SELECT EXISTS(SELECT 1 FROM authorized),
		       (SELECT count(*) FROM reconciliations WHERE status IN ('pending','reconciling','delete_pending')),
		       (SELECT count(*) FROM reconciliations WHERE status='conflict'),
		       (SELECT count(*) FROM reconciliations WHERE status IN ('absence_verified','verified')),
		       (
		         SELECT count(*)
		         FROM memory_record_heads h
		         JOIN memory_record_revisions revision ON revision.id=h.current_record_revision_id
		         WHERE revision.learner_generation<$2
		           AND (h.external_node_id IS NOT NULL OR h.external_memory_id IS NOT NULL)
		       ) + (
		         SELECT count(*)
		         FROM memory_delivery_attempt_heads attempt
		         JOIN memory_delivery_attempts identity ON identity.id=attempt.attempt_id
		         JOIN memory_deliveries delivery ON delivery.id=identity.delivery_id
		         WHERE delivery.learner_generation<$2 AND attempt.sent_at IS NOT NULL
		       ) + (
		         SELECT count(*)
		         FROM memory_remote_delete_plans plan
		         JOIN memory_deliveries delivery ON delivery.id=plan.delivery_id
		         WHERE delivery.learner_generation<$2
		       ),
		       (
		         SELECT count(*)
		         FROM memory_managed_backup_inventory
		         WHERE learner_generation<$2
		       ),
		       (
		         SELECT count(*)
		         FROM memory_generation_keys
		         WHERE learner_generation<$2 AND (destroyed_at IS NULL OR wrapped_key IS NOT NULL)
		       )`, request.ErasureID, request.LearnerGeneration).Scan(
		&authorized,
		&evidence.PendingReconciliations,
		&evidence.ReconciliationConflicts,
		&evidence.CompletedReconciliations,
		&evidence.RemoteReferences,
		&evidence.ManagedBackups,
		&evidence.LiveGenerationKeys,
	)
	if err != nil {
		return privacy.DisabledNocturneEvidence{}, err
	}
	if !authorized {
		return privacy.DisabledNocturneEvidence{}, &privacy.Error{Code: privacy.CodeReceiptNotCurrent, Reason: "disabled_nocturne_erasure_not_active"}
	}
	return evidence, nil
}

var _ privacy.DisabledNocturneEvidenceReader = disabledNocturneEvidenceReader{}
