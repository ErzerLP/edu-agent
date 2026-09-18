package postgresstore

import (
	"context"
	"slices"

	"github.com/edu-agent/edu-agent/server/internal/knowledge"
	"github.com/edu-agent/edu-agent/server/internal/learningspace"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ReviewedStructureContextTx 仅由正式教学变更调用；读取页面不会创建或切换现场。
// 新上下文保留原政策和冻结范围，不把概念维护当作来源范围扩张的批准。
func (s *Store) ReviewedStructureContextTx(ctx context.Context, tx pgx.Tx, base string) (string, error) {
	if base == "" {
		return "", nil
	}
	old, err := s.ContextTx(ctx, tx, base)
	if err != nil {
		return "", err
	}
	scope, err := readScope(ctx, tx, old.ScopeSnapshotID)
	if err != nil {
		return "", err
	}
	ids := []string{}
	previous := []string{}
	seen := map[string]bool{}
	var add func(string, int) error
	add = func(id string, depth int) error {
		if seen[id] {
			return nil
		}
		if depth > 8 || len(seen) >= 100 {
			return structureError(knowledge.CodePayloadTooLarge)
		}
		seen[id] = true
		n, e := readStructureNode(ctx, tx, id, "")
		if e != nil {
			return e
		}
		if n.Content.SourceStatus == "superseded" {
			for _, next := range n.Content.ReplacedBy {
				if e = add(next, depth+1); e != nil {
					return e
				}
			}
			return nil
		}
		for _, source := range n.Content.Sources {
			allowed := false
			for _, entry := range scope.Entries {
				if entry.CollectionID == source.CollectionID && entry.RevisionID == source.RevisionID && (entry.DocumentID == "" || entry.DocumentID == source.DocumentID) && (entry.NodeID == "" || entry.NodeID == source.NodeID) {
					allowed = true
				}
			}
			if !allowed {
				return structureError(knowledge.CodeInvalidRequest)
			}
		}
		ids = append(ids, n.RevisionID)
		return nil
	}
	for _, n := range old.Concepts {
		previous = append(previous, n.RevisionID)
		if err = add(n.ConceptID, 0); err != nil {
			return "", err
		}
	}
	if slices.Equal(previous, ids) {
		return base, nil
	}
	version, err := structureVersion(ctx, tx)
	if err != nil {
		return "", err
	}
	id := uuid.NewString()
	_, err = tx.Exec(ctx, `INSERT INTO knowledge_context_revisions(id,policy_id,scope_snapshot_id,run_id,previous_revision_id,concept_revision_ids,reference_selection,reference_base_entries,structure_base_id,structure_version)
 SELECT $1,policy_id,scope_snapshot_id,$2,id,$3,reference_selection,reference_base_entries,id,$4 FROM knowledge_context_revisions WHERE id=$5`, id, uuid.NewString(), ids, version, base)
	return id, err
}

// ReviewedStructureContextMatchesTx 同时核对原参考政策和结构版本；排队期间新维护使候选失效。
func (s *Store) ReviewedStructureContextMatchesTx(ctx context.Context, tx pgx.Tx, expected, candidate string) (bool, error) {
	if candidate == "" {
		return expected == "", nil
	}
	var base *string
	var version *int64
	err := tx.QueryRow(ctx, `SELECT c.structure_base_id::text,c.structure_version FROM knowledge_context_revisions c JOIN knowledge_policies p ON p.id=c.policy_id WHERE c.id=$1 AND p.space_id=$2`, candidate, learningspace.Scope(ctx)).Scan(&base, &version)
	if err != nil {
		return false, err
	}
	if base == nil {
		return expected == "" || expected == candidate, nil
	}
	current, err := structureVersion(ctx, tx)
	return (expected == "" || *base == expected) && version != nil && *version == current, err
}
