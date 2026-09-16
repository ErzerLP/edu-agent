package mentorrun

import (
	"context"
	"testing"
	"time"

	identitydb "github.com/edu-agent/edu-agent/server/internal/identity/postgresstore"
	knowledgedb "github.com/edu-agent/edu-agent/server/internal/knowledge/postgresstore"
	learningdb "github.com/edu-agent/edu-agent/server/internal/learning/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/learningspace"
	memorydb "github.com/edu-agent/edu-agent/server/internal/memory/postgresstore"
	outboxdb "github.com/edu-agent/edu-agent/server/internal/platform/outbox/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/privacy"
	privacydb "github.com/edu-agent/edu-agent/server/internal/privacy/postgresstore"
	tutoringdb "github.com/edu-agent/edu-agent/server/internal/tutoring/postgresstore"
	"github.com/google/uuid"
)

func TestPostgreSQLResearchGlobalErasureCannotReviveSources(t *testing.T) {
	f, calls := researchFixture(t, false)
	ctx := context.Background()
	if _, err := f.pool.Exec(ctx, `UPDATE device_tokens SET scopes=scopes||ARRAY['knowledge:write','research:adopt'] WHERE id=$1`, f.actor.TokenID); err != nil {
		t.Fatal(err)
	}
	f.create.Research.AutoAdopt = true
	r, err := f.service.Create(ctx, f.actor, learningspace.DefaultID, f.goal, f.create)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.service.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	snapshot := f.snapshot(t, r.RunID)
	if snapshot.Research.Sources[0].KnowledgeRevisionID == "" {
		t.Fatal("没有待清除的正式来源")
	}
	tutoringStore := tutoringdb.New(f.pool)
	store := privacydb.New(f.pool, privacydb.WithReadPermits(privacy.NewReadPermitManager()), privacydb.WithLocalOwner(identitydb.New(f.pool)), privacydb.WithLocalOwner(knowledgedb.New(f.pool)), privacydb.WithLocalOwner(learningdb.New(f.pool, tutoringStore)), privacydb.WithLocalOwner(tutoringStore), privacydb.WithLocalOwner(memorydb.New(f.pool)), privacydb.WithLocalOwner(outboxdb.New(f.pool)))
	grants, err := privacy.NewErasureGrantService(privacydb.NewGrantStore(f.pool), privacy.ErasureGrantOptions{})
	if err != nil {
		t.Fatal(err)
	}
	grant, err := grants.Issue(ctx, f.actor.Device.ID, "研究来源隐私验收")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	barrier, err := store.CommitBarrierAuthorized(ctx, privacy.ErasureRequest{DeviceID: f.actor.Device.ID, OperationID: uuid.NewString(), ActorDeviceID: f.actor.Device.ID, ReasonCode: string(privacy.ReasonLearnerRequest), RequestedAt: now, ManagedBackupUnrecoverableAfter: now.Add(24 * time.Hour), ExpectedCurrentLearnerGeneration: 1}, privacy.NewErasureGrantAuthorization(f.actor.Device.ID, grant.Token))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.RunLocalScrub(ctx, barrier.ErasureID); err != nil {
		t.Fatal(err)
	}
	_, _ = f.service.Sweep(ctx)
	var remaining int
	if err = f.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM learning_mentor_runs WHERE checkpoint IS NOT NULL OR (state->>'body_available')::boolean)+(SELECT count(*) FROM knowledge_document_payloads WHERE canonical_markdown LIKE '%概率%')+(SELECT count(*) FROM knowledge_revisions WHERE source<>'privacy_erasure')`).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != 0 {
		t.Fatal("清除后残留研究正文、正式知识或来源关系")
	}
	if err = f.service.ReadSnapshot(ctx, f.actor, learningspace.DefaultID, r.RunID, func(Snapshot) error { t.Error("发送了已清除来源"); return nil }); err == nil {
		t.Fatal("旧运行可恢复")
	}
	if _, err = f.service.Create(ctx, f.actor, learningspace.DefaultID, f.goal, f.create); err == nil {
		t.Fatal("旧操作复活来源")
	}
	if calls.Load() != 1 {
		t.Fatal("清除后重新发出搜索")
	}
	if err = f.pool.QueryRow(ctx, `SELECT count(*) FROM knowledge_source_revisions`).Scan(&remaining); err != nil || remaining != 0 {
		t.Fatal("隐私清除遗留来源索引", err)
	}
}
