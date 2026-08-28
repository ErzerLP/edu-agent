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
			WITH valid_source_evidence AS (
			  SELECT evidence.id,evidence.node_revision_id,evidence.knowledge_revision_id
			  FROM learning_evidence evidence
			  WHERE evidence.accepted_event_seq IS NOT NULL
			    AND NOT EXISTS (
			      SELECT 1 FROM learning_evidence_invalidations invalidation
			      WHERE invalidation.evidence_id=evidence.id
			    )
			), impact_references AS (
			  SELECT evidence.id AS evidence_id,evidence.node_revision_id,evidence.knowledge_revision_id
			  FROM valid_source_evidence evidence
			  WHERE evidence.node_revision_id=ANY($1::uuid[])
			  UNION
			  SELECT evidence.id,link.target_node_revision_id,link.target_knowledge_revision_id
			  FROM valid_source_evidence evidence
			  JOIN learning_evidence_carryover_links link ON link.source_evidence_id=evidence.id
			  JOIN learning_evidence_carryover_proposals proposal
			    ON proposal.proposal_id=link.proposal_id AND proposal.status='approved'
			  WHERE link.target_node_revision_id=ANY($1::uuid[])
			)
			SELECT evidence_id::text,node_revision_id::text,knowledge_revision_id::text
			FROM impact_references
			ORDER BY evidence_id,node_revision_id,knowledge_revision_id`, ids)
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
