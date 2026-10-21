package postgresstore

import (
	"context"
	"errors"
	"github.com/edu-agent/edu-agent/server/internal/knowledge"
	"github.com/edu-agent/edu-agent/server/internal/learningspace"
	"github.com/edu-agent/edu-agent/server/internal/privacy"
	"github.com/jackc/pgx/v5"
)

// WebSourceIdentityTx reads only accessible identity/fingerprint metadata. It
// never loads private source text into a public research context.
func (s *Store) WebSourceIdentityTx(ctx context.Context, tx pgx.Tx, fingerprint string) (*knowledge.SourceIdentity, error) {
	if _, err := privacy.LockOwnerRead(ctx, tx, privacy.OwnerKnowledge); err != nil {
		return nil, err
	}
	var result knowledge.SourceIdentity
	err := tx.QueryRow(ctx, `SELECT c.id::text,r.id::text,sd.document_id::text,sd.document_revision_id::text
 FROM knowledge_collection_links l JOIN knowledge_collections c ON c.id=l.collection_id
 JOIN knowledge_revisions r ON r.id=c.head_revision_id AND r.redacted_at IS NULL
 JOIN knowledge_snapshot_documents sd ON sd.knowledge_revision_id=r.id
 JOIN knowledge_source_metadata m ON m.document_revision_id=sd.document_revision_id
 WHERE l.space_id=$1 AND m.metadata->>'fingerprint'=$2 AND m.metadata->>'kind' IN ('url','pdf') ORDER BY c.id,sd.document_id LIMIT 1 FOR SHARE OF l,c,r`, learningspace.Scope(ctx), fingerprint).Scan(&result.CollectionID, &result.RevisionID, &result.DocumentID, &result.DocumentRevisionID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &result, nil
}
