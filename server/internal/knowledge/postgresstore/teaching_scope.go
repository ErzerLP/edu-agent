package postgresstore

import (
	"context"
	"crypto/sha256"
	"encoding/json"

	"github.com/edu-agent/edu-agent/server/internal/knowledge"
	"github.com/edu-agent/edu-agent/server/internal/privacy"
	"github.com/jackc/pgx/v5"
)

// EnsureTeachingScopeWith 仅物化不可变关系，使旧教学外键继续校验正文归属。
// 读取仍经 readScope/loadScopedRevision，章节限制不会因外键锚点而扩大。
func (s *Store) EnsureTeachingScopeWith(ctx context.Context, tx pgx.Tx, id, device string) error {
	if _, err := privacy.LockOwnerRead(ctx, tx, privacy.OwnerKnowledge); err != nil {
		return err
	}
	if err := lockSpaceWrite(ctx, tx); err != nil {
		return err
	}
	revision, err := loadScopedRevision(ctx, tx, id)
	if err != nil {
		return err
	}
	snapshot, err := readScope(ctx, tx, id)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(snapshot.Entries)
	if err != nil {
		return err
	}
	hash := sha256.Sum256(raw)
	if _, err = tx.Exec(ctx, `INSERT INTO knowledge_revisions(id,revision_no,manifest_hash,source,created_by_device_id,created_at,canonicalizer_version,parser_version,indexer_version,identity_policy_version,collection_id,scope_snapshot_id)
 VALUES($1,1,$2,'teaching_scope',$3,clock_timestamp(),'scope-v1','scope-v1','scope-v1','scope-v1',NULL,$1) ON CONFLICT(id) DO NOTHING`, id, hash[:], device); err != nil {
		return err
	}
	seen := map[string]string{}
	for _, doc := range revision.Documents {
		if old, ok := seen[doc.Revision.DocumentID]; ok {
			if old != doc.Revision.ID {
				return &knowledge.Error{Code: knowledge.CodeInvalidRequest}
			}
			continue
		}
		seen[doc.Revision.DocumentID] = doc.Revision.ID
		if _, err = tx.Exec(ctx, `INSERT INTO knowledge_snapshot_documents(knowledge_revision_id,canonical_path,folded_path,document_id,document_revision_id) VALUES($1,$2,$2,$3,$4) ON CONFLICT DO NOTHING`, id, doc.Revision.ID, doc.Revision.DocumentID, doc.Revision.ID); err != nil {
			return err
		}
	}
	return nil
}

// 确认期间锁住范围所依赖的集合，避免检查后 head 变化使旧计划覆盖新事实。
func (s *Store) LockPlanningScopeWith(ctx context.Context, tx pgx.Tx, id string) error {
	if err := s.ValidateGoalScopeWith(ctx, tx, id); err != nil {
		return err
	}
	scope, err := readScope(ctx, tx, id)
	if err != nil {
		return err
	}
	for _, entry := range scope.Entries {
		var head *string
		if err = tx.QueryRow(ctx, `SELECT head_revision_id FROM knowledge_collections WHERE id=$1 FOR SHARE`, entry.CollectionID).Scan(&head); err != nil {
			return err
		}
	}
	return nil
}
