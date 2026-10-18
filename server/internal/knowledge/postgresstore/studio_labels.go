package postgresstore

import (
	"context"

	"github.com/edu-agent/edu-agent/server/internal/learningspace"
	"github.com/edu-agent/edu-agent/server/internal/privacy"
	"github.com/jackc/pgx/v5"
)

// ReadLearningNodeLabelsTx supplies short concept labels for the at most 24
// node revisions selected by the formal learning-record reader. It does not
// return document bodies or expand the selected records. A missing or revoked
// collection link omits the label; callers retain only the learning metadata.
func ReadLearningNodeLabelsTx(ctx context.Context, tx pgx.Tx, nodeRevisions []string) (map[string]string, error) {
	labels := make(map[string]string, len(nodeRevisions))
	if len(nodeRevisions) == 0 {
		return labels, nil
	}
	if _, err := privacy.LockOwnerRead(ctx, tx, privacy.OwnerKnowledge); err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, `SELECT n.id,COALESCE(NULLIF(left(btrim(n.title),48),''),'知识点')
 FROM knowledge_node_revisions n JOIN LATERAL (
   SELECT l.collection_id FROM knowledge_snapshot_documents d
   JOIN knowledge_revisions r ON r.id=d.knowledge_revision_id
   JOIN knowledge_collection_links l ON l.collection_id=r.collection_id
   WHERE d.document_revision_id=n.document_revision_id AND l.space_id=$1 AND r.redacted_at IS NULL
   LIMIT 1 FOR SHARE OF l
 ) readable ON true
 WHERE n.id=ANY($2::uuid[]) ORDER BY n.id LIMIT 24`, learningspace.Scope(ctx), nodeRevisions)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id, label string
		if err = rows.Scan(&id, &label); err != nil {
			return nil, err
		}
		labels[id] = label
	}
	return labels, rows.Err()
}
