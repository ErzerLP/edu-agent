package mentorrun

import (
	"context"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/edu-agent/edu-agent/packages/agentcore/modelclient"
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

func TestPostgreSQLMentorPrivacyBarrierClearsSavedAndInFlightTemporary(t *testing.T) {
	started := make(chan struct{})
	lifetime, cancelFixture := context.WithCancel(context.Background())
	f := fixture(t, func(w http.ResponseWriter, r *http.Request, n int) {
		if n == 1 {
			stream(w, modelclient.Message{Role: "assistant", Content: "需要清除的持久正文"})
			return
		}
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, ": 等待\n\n")
		w.(http.Flusher).Flush()
		close(started)
		select {
		case <-r.Context().Done():
		case <-lifetime.Done():
		}
	})
	t.Cleanup(cancelFixture)
	f.service.Lease = time.Second
	f.accept(t)
	f.work(t)
	f.create.OperationID = uuid.NewString()
	f.create.Save = false
	current := f.accept(t)
	done := make(chan error, 1)
	go func() { _, err := f.service.RunOnce(context.Background()); done <- err }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("未启动在途请求")
	}
	ctx := context.Background()
	tutoringStore := tutoringdb.New(f.pool)
	privacyStore := privacydb.New(f.pool,
		privacydb.WithReadPermits(privacy.NewReadPermitManager()),
		privacydb.WithLocalOwner(identitydb.New(f.pool)), privacydb.WithLocalOwner(knowledgedb.New(f.pool)),
		privacydb.WithLocalOwner(learningdb.New(f.pool, tutoringStore)), privacydb.WithLocalOwner(tutoringStore),
		privacydb.WithLocalOwner(memorydb.New(f.pool)), privacydb.WithLocalOwner(outboxdb.New(f.pool)))
	grants, err := privacy.NewErasureGrantService(privacydb.NewGrantStore(f.pool), privacy.ErasureGrantOptions{})
	if err != nil {
		t.Fatal(err)
	}
	grant, err := grants.Issue(ctx, f.actor.Device.ID, "导师运行隐私验收")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	barrier, err := privacyStore.CommitBarrierAuthorized(ctx, privacy.ErasureRequest{DeviceID: f.actor.Device.ID, OperationID: uuid.NewString(), ActorDeviceID: f.actor.Device.ID, ReasonCode: string(privacy.ReasonLearnerRequest), RequestedAt: now, ManagedBackupUnrecoverableAfter: now.Add(24 * time.Hour), ExpectedCurrentLearnerGeneration: 1}, privacy.NewErasureGrantAuthorization(f.actor.Device.ID, grant.Token))
	if err != nil {
		t.Fatal(err)
	}
	if err = f.service.ReadSnapshot(ctx, f.actor, learningspace.DefaultID, current.RunID, func(Snapshot) error { t.Error("清除屏障后仍发送旧正文"); return nil }); err == nil {
		t.Fatal("隐私屏障未关闭读取")
	}
	if err = f.service.ReadList(ctx, f.actor, learningspace.DefaultID, ListQuery{Limit: 20}, func(Page) error { t.Error("隐私屏障后仍发送任务列表"); return nil }); err == nil {
		t.Fatal("隐私屏障未关闭任务列表")
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("隐私屏障未终止模型")
	}
	if _, err = privacyStore.RunLocalScrub(ctx, barrier.ErasureID); err != nil {
		t.Fatal(err)
	}
	_, _ = f.service.Sweep(ctx)
	var remaining int
	if err = f.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM learning_mentor_runs WHERE checkpoint IS NOT NULL OR (state->>'body_available')::boolean)+(SELECT count(*) FROM learning_mentor_events)+(SELECT count(*) FROM learning_mentor_operations WHERE request_hash<>decode(repeat('00',32),'hex'))`).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != 0 {
		t.Fatal("清除后存在运行正文、事件或载荷摘要")
	}
	f.service.mu.Lock()
	cached := len(f.service.temporary)
	f.service.mu.Unlock()
	if cached != 0 {
		t.Fatal("临时正文缓存未进入清除路径")
	}
	if f.calls.Load() != 2 {
		t.Fatal("隐私清除后重放了模型")
	}
}
