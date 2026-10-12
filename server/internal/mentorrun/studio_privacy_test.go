package mentorrun

import (
	"context"
	"io"
	"net/http"
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

func TestPostgreSQLStudioGenerationGlobalErasureCancelsInFlight(t *testing.T) {
	started := make(chan struct{})
	lifetime, stop := context.WithCancel(context.Background())
	f, req := studioFixture(t, func(w http.ResponseWriter, r *http.Request, n int) {
		if n == 1 {
			studioReply(w, studioCandidate("guide", "complete"), "stop")
			return
		}
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		close(started)
		select {
		case <-r.Context().Done():
		case <-lifetime.Done():
		}
		studioReply(w, studioCandidate("guide", "complete"), "stop")
	})
	t.Cleanup(stop)
	permits := privacy.NewReadPermitManager()
	f.service.ConfigureMemory(nil, permits)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	first, err := f.service.CreateStudio(ctx, f.actor, learningspace.DefaultID, req)
	if err != nil {
		t.Fatal(err)
	}
	f.work(t)
	artifact := f.snapshot(t, first.RunID).Studio.Result
	req.OperationID = uuid.NewString()
	receipt, err := f.service.CreateStudio(ctx, f.actor, learningspace.DefaultID, req)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := f.service.RunOnce(ctx); done <- err }()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	tutoring := tutoringdb.New(f.pool)
	store := privacydb.New(f.pool, privacydb.WithReadPermits(permits), privacydb.WithLocalOwner(identitydb.New(f.pool)), privacydb.WithLocalOwner(knowledgedb.New(f.pool)), privacydb.WithLocalOwner(learningdb.New(f.pool, tutoring)), privacydb.WithLocalOwner(tutoring), privacydb.WithLocalOwner(memorydb.New(f.pool)), privacydb.WithLocalOwner(outboxdb.New(f.pool)))
	grants, err := privacy.NewErasureGrantService(privacydb.NewGrantStore(f.pool), privacy.ErasureGrantOptions{})
	if err != nil {
		t.Fatal(err)
	}
	grant, err := grants.Issue(ctx, f.actor.Device.ID, "来源问答清除验收")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	barrier, err := store.CommitBarrierAuthorized(ctx, privacy.ErasureRequest{DeviceID: f.actor.Device.ID, OperationID: uuid.NewString(), ActorDeviceID: f.actor.Device.ID, ReasonCode: string(privacy.ReasonLearnerRequest), RequestedAt: now, ManagedBackupUnrecoverableAfter: now.Add(24 * time.Hour), ExpectedCurrentLearnerGeneration: 1}, privacy.NewErasureGrantAuthorization(f.actor.Device.ID, grant.Token))
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("source model did not cancel")
	}
	if _, err = store.RunLocalScrub(ctx, barrier.ErasureID); err != nil {
		t.Fatal(err)
	}
	if _, err = f.service.studio.StudioGet(ctx, f.actor, artifact.ArtifactID, 1); err == nil {
		t.Fatal("erased citation readable")
	}
	if _, err = f.service.CreateStudio(ctx, f.actor, learningspace.DefaultID, req); err == nil {
		t.Fatal("old generation resurrected")
	}
	_ = receipt
	var remaining int
	if err = f.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM learning_mentor_runs WHERE checkpoint IS NOT NULL)+(SELECT count(*) FROM learning_tutor_turns)+(SELECT count(*) FROM learning_studio_artifacts)+(SELECT count(*) FROM learning_studio_revisions)+(SELECT count(*) FROM learning_studio_operations)`).Scan(&remaining); err != nil || remaining != 0 {
		t.Fatal("late content survived erasure", remaining, err)
	}
}
