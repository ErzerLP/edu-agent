package migrations

import (
	"context"
	"testing"

	"github.com/edu-agent/edu-agent/server/internal/learningspace"
	"github.com/google/uuid"
)

func TestIssue30MigrationPreservesLatestConceptRevision(t *testing.T) {
	pool := migrationPoolThrough(t, 28)
	ctx := context.Background()
	concept, goal, latest, old := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	if _, err := pool.Exec(ctx, `INSERT INTO knowledge_concepts(id,space_id,goal_id,semantic_key) VALUES($1,$2,$3,'旧语义身份')`, concept, learningspace.DefaultID, goal); err != nil {
		t.Fatal(err)
	}
	// 故意先写新版本再写旧版本，回填不得依赖物理行顺序。
	if _, err := pool.Exec(ctx, `INSERT INTO knowledge_concept_revisions(id,concept_id,name,support,created_at) VALUES($1,$3,'当前版本','[]','2026-09-02'),($2,$3,'历史版本','[]','2026-09-01')`, latest, old, concept); err != nil {
		t.Fatal(err)
	}
	if err := Run(ctx, pool); err != nil {
		t.Fatal(err)
	}
	var head string
	var version, revisions int64
	if err := pool.QueryRow(ctx, `SELECT revision_id FROM knowledge_concept_heads WHERE concept_id=$1`, concept).Scan(&head); err != nil || head != latest {
		t.Fatalf("迁移选错当前版本: got=%s want=%s err=%v", head, latest, err)
	}
	if err := pool.QueryRow(ctx, `SELECT version FROM knowledge_structure_heads WHERE space_id=$1`, learningspace.DefaultID).Scan(&version); err != nil || version != 2 {
		t.Fatalf("迁移版本计数错误: %d %v", version, err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM knowledge_concept_revisions WHERE concept_id=$1`, concept).Scan(&revisions); err != nil || revisions != 2 {
		t.Fatalf("迁移丢失旧修订: %d %v", revisions, err)
	}
	added := uuid.NewString()
	if _, err := pool.Exec(ctx, `INSERT INTO knowledge_concept_revisions(id,concept_id,name,support,structure) VALUES($1,$2,'审阅后的新版本','[]','{"source_status":"candidate"}')`, added, concept); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT revision_id FROM knowledge_concept_heads WHERE concept_id=$1`, concept).Scan(&head); err != nil || head != added {
		t.Fatalf("新修订没有推进当前身份: %s %v", head, err)
	}
	if err := pool.QueryRow(ctx, `SELECT version FROM knowledge_structure_heads WHERE space_id=$1`, learningspace.DefaultID).Scan(&version); err != nil || version != 3 {
		t.Fatalf("新修订没有推进版本: %d %v", version, err)
	}
}
