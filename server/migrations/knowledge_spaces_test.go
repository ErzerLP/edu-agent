package migrations

import (
	"context"
	"github.com/edu-agent/edu-agent/server/internal/knowledge"
	"github.com/google/uuid"
	"testing"
)

func TestKnowledgeSpaceUpgradePreservesLegacyRevision(t *testing.T) {
	pool := migrationPoolThrough(t, 12)
	ctx := context.Background()
	id := uuid.NewString()
	if _, err := pool.Exec(ctx, `INSERT INTO knowledge_revisions(id,revision_no,manifest_hash,source,created_by_device_id,created_at,canonicalizer_version,parser_version,indexer_version,identity_policy_version) VALUES($1,1,decode(repeat('ab',32),'hex'),'旧来源',$2,now(),'v1','v1','v1','v1')`, id, uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE knowledge_catalog SET head_revision_id=$1 WHERE singleton_id=1`, id); err != nil {
		t.Fatal(err)
	}
	var before, after string
	if err := pool.QueryRow(ctx, `SELECT to_jsonb(r)::text FROM knowledge_revisions r WHERE id=$1`, id).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if err := Run(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if err := Run(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT (to_jsonb(r)-'collection_id'-'scope_snapshot_id')::text FROM knowledge_revisions r WHERE id=$1 AND scope_snapshot_id IS NULL`, id).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Fatal("迁移修改了旧版本或引用")
	}
	var head string
	if err := pool.QueryRow(ctx, `SELECT head_revision_id FROM knowledge_collections WHERE id=$1`, knowledge.DefaultCollectionID).Scan(&head); err != nil || head != id {
		t.Fatalf("默认集合未保留旧 head: %s %v", head, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE knowledge_revisions SET collection_id=NULL WHERE id=$1`, id); err == nil {
		t.Fatal("资料版本不能同时缺少集合和冻结范围归属")
	}
}
