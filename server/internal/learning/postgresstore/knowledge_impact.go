package postgresstore

import (
	"context"
	"fmt"
	"sort"

	"github.com/edu-agent/edu-agent/server/internal/knowledge"
	"github.com/edu-agent/edu-agent/server/internal/privacy"
	"github.com/jackc/pgx/v5"
)

// AcceptedEvidenceImpact is the learning-owned read adapter used by knowledge
// maintenance. It never mutates evidence, events, or projections.
func (s *Store) AcceptedEvidenceImpact(ctx context.Context, nodeRevisionIDs []string) (knowledge.AcceptedEvidenceImpact, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return knowledge.AcceptedEvidenceImpact{}, fmt.Errorf("begin accepted evidence impact read: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	generation, err := privacy.LockOwnerRead(ctx, tx, privacy.OwnerLearning)
	if err != nil {
		return knowledge.AcceptedEvidenceImpact{}, err
	}
	ids := append([]string(nil), nodeRevisionIDs...)
	sort.Strings(ids)
	impact := knowledge.AcceptedEvidenceImpact{Generation: generation, References: []knowledge.AcceptedEvidenceReference{}}
	if len(ids) != 0 {
		rows, err := tx.Query(ctx, `
			SELECT evidence.id::text,evidence.node_revision_id::text,evidence.knowledge_revision_id::text
			FROM learning_evidence evidence
			WHERE evidence.node_revision_id=ANY($1::uuid[])
			  AND evidence.accepted_event_seq IS NOT NULL
			  AND NOT EXISTS (
			    SELECT 1 FROM learning_evidence_invalidations invalidation
			    WHERE invalidation.evidence_id=evidence.id
			  )
			ORDER BY evidence.id`, ids)
		if err != nil {
			return knowledge.AcceptedEvidenceImpact{}, fmt.Errorf("query accepted evidence impact: %w", err)
		}
		for rows.Next() {
			var reference knowledge.AcceptedEvidenceReference
			if err := rows.Scan(&reference.EvidenceID, &reference.NodeRevisionID, &reference.KnowledgeRevisionID); err != nil {
				rows.Close()
				return knowledge.AcceptedEvidenceImpact{}, fmt.Errorf("scan accepted evidence impact: %w", err)
			}
			impact.References = append(impact.References, reference)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return knowledge.AcceptedEvidenceImpact{}, fmt.Errorf("iterate accepted evidence impact: %w", err)
		}
		rows.Close()
	}
	if err := tx.Commit(ctx); err != nil {
		return knowledge.AcceptedEvidenceImpact{}, fmt.Errorf("commit accepted evidence impact read: %w", err)
	}
	impact.Count = len(impact.References)
	return impact, nil
}
