package mentorrun

import (
	"bytes"
	"context"
	"net/http"
	"testing"
	"time"

	identitydb "github.com/edu-agent/edu-agent/server/internal/identity/postgresstore"
	knowledgedb "github.com/edu-agent/edu-agent/server/internal/knowledge/postgresstore"
	learningdb "github.com/edu-agent/edu-agent/server/internal/learning/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/learningspace"
	memorydb "github.com/edu-agent/edu-agent/server/internal/memory/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/platform/keyrotation"
	outboxdb "github.com/edu-agent/edu-agent/server/internal/platform/outbox/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/privacy"
	privacydb "github.com/edu-agent/edu-agent/server/internal/privacy/postgresstore"
	tutoringdb "github.com/edu-agent/edu-agent/server/internal/tutoring/postgresstore"
	"github.com/google/uuid"
)

func TestPostgreSQLTopicsErasureRejectsLateResponse(t *testing.T) {
	f := topicFixture(t)
	ctx := context.Background()
	id := newHistory(t, f, true, "")
	sendHistory(t, f, id, "需要清除的话题")
	f.work(t)
	requestTopics(t, f, id)
	f.work(t)
	sendHistory(t, f, id, "未整理的新消息")
	f.work(t)
	started, release := make(chan struct{}), make(chan struct{})
	f.service.Lease = time.Second
	f.modelOverride = func(w http.ResponseWriter, r *http.Request) {
		close(started)
		select {
		case <-release:
		case <-r.Context().Done():
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"{\"topics\":[]}"}}]}`))
	}
	requestTopics(t, f, id)
	done := make(chan error, 1)
	go func() { _, err := f.service.RunOnce(ctx); done <- err }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("index did not start")
	}
	permits := privacy.NewReadPermitManager()
	tutoring := tutoringdb.New(f.pool)
	store := privacydb.New(f.pool, privacydb.WithReadPermits(permits), privacydb.WithLocalOwner(identitydb.New(f.pool)), privacydb.WithLocalOwner(knowledgedb.New(f.pool)), privacydb.WithLocalOwner(learningdb.New(f.pool, tutoring)), privacydb.WithLocalOwner(tutoring), privacydb.WithLocalOwner(memorydb.New(f.pool)), privacydb.WithLocalOwner(outboxdb.New(f.pool)))
	grants, err := privacy.NewErasureGrantService(privacydb.NewGrantStore(f.pool), privacy.ErasureGrantOptions{})
	if err != nil {
		t.Fatal(err)
	}
	grant, err := grants.Issue(ctx, f.actor.Device.ID, "话题清除验收")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	barrier, err := store.CommitBarrierAuthorized(ctx, privacy.ErasureRequest{DeviceID: f.actor.Device.ID, OperationID: uuid.NewString(), ActorDeviceID: f.actor.Device.ID, ReasonCode: string(privacy.ReasonLearnerRequest), RequestedAt: now, ManagedBackupUnrecoverableAfter: now.Add(24 * time.Hour), ExpectedCurrentLearnerGeneration: 1}, privacy.NewErasureGrantAuthorization(f.actor.Device.ID, grant.Token))
	close(release)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("late response remained active")
	}
	if _, err = store.RunLocalScrub(ctx, barrier.ErasureID); err != nil {
		t.Fatal(err)
	}
	var count int
	if err = f.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM learning_conversation_topics)+(SELECT count(*) FROM learning_conversation_messages)+(SELECT count(*) FROM learning_topic_anchors)+(SELECT count(*) FROM learning_topic_index_heads)+(SELECT count(*) FROM learning_topic_index_scopes)`).Scan(&count); err != nil || count != 0 {
		t.Fatal("derived data revived", count, err)
	}
	if err = f.service.ReadTopics(ctx, f.actor, learningspace.DefaultID, id, "", 20, func(TopicPage) error { t.Error("cleared topics sent"); return nil }); err == nil {
		t.Fatal("erasure gate missing")
	}
}

func TestPostgreSQLTopicsRotationAndConversationDeletion(t *testing.T) {
	f := topicFixture(t)
	ctx := context.Background()
	id := newHistory(t, f, true, "")
	sendHistory(t, f, id, "可定位的话题")
	f.work(t)
	requestTopics(t, f, id)
	f.work(t)
	old := readTopics(t, f, id)
	rewrap, err := keyrotation.New(bytes.Repeat([]byte{42}, 32), bytes.Repeat([]byte{43}, 32))
	if err != nil {
		t.Fatal(err)
	}
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if err = RotateKeyTx(ctx, tx, rewrap); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	f.service, err = New(f.pool, f.settings, bytes.Repeat([]byte{43}, 32))
	if err != nil {
		t.Fatal(err)
	}
	if p := readTopics(t, f, id); len(p.Items) != 2 || p.Items[0].ID != old.Items[0].ID || p.CoveredThrough != old.CoveredThrough {
		t.Fatal("rotation changed index", p)
	}
	c := historyPage(t, f, id, 0, 1).Conversation
	if err = f.service.ChangeConversation(ctx, f.actor, learningspace.DefaultID, id, c.Version, "", true); err != nil {
		t.Fatal(err)
	}
	var count int
	if err = f.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM learning_conversation_topics)+(SELECT count(*) FROM learning_conversation_messages)+(SELECT count(*) FROM learning_topic_anchors)+(SELECT count(*) FROM learning_topic_index_heads)+(SELECT count(*) FROM learning_topic_index_scopes)`).Scan(&count); err != nil || count != 0 {
		t.Fatal("delete left derived data", count, err)
	}
}
