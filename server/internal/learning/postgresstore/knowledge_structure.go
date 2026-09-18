package postgresstore

import (
	"context"

	"github.com/edu-agent/edu-agent/server/internal/knowledge"
	"github.com/edu-agent/edu-agent/server/internal/learningspace"
	"github.com/edu-agent/edu-agent/server/internal/privacy"
	"github.com/jackc/pgx/v5"
)

// StructureLearningImpactTx 只读取真实依赖，不生成 Evidence 或继承链接。
func (s *Store) StructureLearningImpactTx(ctx context.Context, tx pgx.Tx, contexts []string) (knowledge.StructureImpact, error) {
	result := knowledge.StructureImpact{GoalIDs: []string{}}
	if _, err := privacy.LockOwnerRead(ctx, tx, privacy.OwnerLearning); err != nil {
		return result, err
	}
	var fingerprint string
	err := tx.QueryRow(ctx, `WITH affected AS (
 SELECT a.id,a.artifact_id,a.artifact_version FROM learning_activities a JOIN learning_goal_revisions g ON g.id=a.goal_revision_id
 WHERE g.space_id=$1 AND a.knowledge_context_revision_id=ANY($2::uuid[])),
 evidence AS (SELECT e.id FROM learning_evidence e JOIN affected a ON a.id=e.activity_id WHERE e.accepted_event_seq IS NOT NULL AND NOT EXISTS(SELECT 1 FROM learning_evidence_invalidations i WHERE i.evidence_id=e.id)),
 contents AS (SELECT c.id,c.latest_version FROM learning_content_artifacts c JOIN affected a ON a.id=c.activity_id)
 SELECT (SELECT count(*) FROM affected),(SELECT count(*) FROM contents),(SELECT count(*) FROM evidence),
 COALESCE((SELECT string_agg(id::text||':'||COALESCE(artifact_version::text,''),',' ORDER BY id) FROM affected),'')||'/'||
 COALESCE((SELECT string_agg(id::text||':'||latest_version::text,',' ORDER BY id) FROM contents),'')||'/'||
 COALESCE((SELECT string_agg(id::text,',' ORDER BY id) FROM evidence),'')`, learningspace.Scope(ctx), contexts).Scan(&result.Activities, &result.Contents, &result.Evidence, &fingerprint)
	result.Fingerprint = knowledge.StructureDigest(fingerprint)
	return result, err
}

// 证据必须同时匹配确切概念修订的上下文和明确章节，仍不声明掌握或语义等价。
func (s *Store) ConceptLearningStateTx(ctx context.Context, tx pgx.Tx, nodes, contexts []string) (string, error) {
	if _, err := privacy.LockOwnerRead(ctx, tx, privacy.OwnerLearning); err != nil {
		return "", err
	}
	var activities, attempts, evidence int64
	err := tx.QueryRow(ctx, `SELECT count(DISTINCT a.id),count(DISTINCT t.id),count(DISTINCT e.id) FROM learning_activities a
 JOIN learning_goal_revisions g ON g.id=a.goal_revision_id LEFT JOIN learning_attempts t ON t.activity_id=a.id
 LEFT JOIN learning_evidence e ON e.activity_id=a.id AND e.node_id=ANY($3::uuid[]) AND e.accepted_event_seq IS NOT NULL
 AND NOT EXISTS(SELECT 1 FROM learning_evidence_invalidations i WHERE i.evidence_id=e.id)
 WHERE g.space_id=$1 AND a.knowledge_context_revision_id=ANY($2::uuid[])`, learningspace.Scope(ctx), contexts, nodes).Scan(&activities, &attempts, &evidence)
	if err != nil {
		return "", err
	}
	if activities == 0 {
		return "unseen", nil
	}
	if evidence > 0 {
		return "evidenced", nil
	}
	if attempts == 0 {
		return "needs_practice", nil
	}
	return "insufficient_evidence", nil
}
