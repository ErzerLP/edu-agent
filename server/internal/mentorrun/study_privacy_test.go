package mentorrun

import (
	"context"
	"io"
	"net/http"
	"testing"
	"time"

	"encoding/json"
	identitydb "github.com/edu-agent/edu-agent/server/internal/identity/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/knowledge"
	knowledgedb "github.com/edu-agent/edu-agent/server/internal/knowledge/postgresstore"
	learningdb "github.com/edu-agent/edu-agent/server/internal/learning/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/learningspace"
	spacepg "github.com/edu-agent/edu-agent/server/internal/learningspace/postgresstore"
	memorydb "github.com/edu-agent/edu-agent/server/internal/memory/postgresstore"
	outboxdb "github.com/edu-agent/edu-agent/server/internal/platform/outbox/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/privacy"
	privacydb "github.com/edu-agent/edu-agent/server/internal/privacy/postgresstore"
	tutoringdb "github.com/edu-agent/edu-agent/server/internal/tutoring/postgresstore"
	"github.com/google/uuid"
)

func TestPostgreSQLStudyDirectionErasureCannotRevive(t *testing.T) {
	started := make(chan struct{})
	lifetime, stop := context.WithCancel(context.Background())
	f := fixture(t, func(w http.ResponseWriter, r *http.Request, n int) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		close(started)
		select {
		case <-r.Context().Done():
		case <-lifetime.Done():
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"finish_reason": "stop", "message": map[string]string{"content": `{"answer":"不能复活 [1]","citations":[{"source":1,"fragment":1}]}`}}}})
	})
	t.Cleanup(stop)
	permits := privacy.NewReadPermitManager()
	f.service.ConfigureMemory(nil, permits)
	a := selectedDocument(t, f, "ERASE_SELECTED_SOURCE")
	direction := "ERASE_DIRECTION"
	spaces := spacepg.New(f.pool, f.service.directions)
	_, err := spaces.Mutate(context.Background(), f.actor.Device.ID, learningspace.DefaultID, learningspace.Command{OperationID: uuid.NewString(), ExpectedVersion: 1, Name: "方向清除", Direction: &direction, Status: "active"})
	if err != nil {
		t.Fatal(err)
	}
	id, err := f.service.NewConversation(context.Background(), f.actor, learningspace.DefaultID, NewConversation{ID: uuid.NewString(), Mode: "study"})
	if err != nil {
		t.Fatal(err)
	}
	req := SubmitTurn{OperationID: uuid.NewString(), ExpectedVersion: 1, DirectionVersion: 1, Prompt: "清除选中资料", RequestBudget: 2, TokenBudget: 30000, SourceSelection: &knowledge.SourceSelection{Mode: "items", Items: []knowledge.SourceIdentity{a}}}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	receipt, err := f.service.Submit(ctx, f.actor, learningspace.DefaultID, id, req)
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
	if err = f.service.ReadCitation(ctx, f.actor, learningspace.DefaultID, receipt.RunID, "1", func(SourceCitation) error { t.Error("erased citation sent"); return nil }); err == nil {
		t.Fatal("erased citation readable")
	}
	if _, err = f.service.Submit(ctx, f.actor, learningspace.DefaultID, id, req); err == nil {
		t.Fatal("old turn revived cleared direction")
	}
	var remaining int
	if err = f.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM learning_space_directions)+(SELECT count(*) FROM learning_direction_revisions)+(SELECT count(*) FROM learning_direction_goals)+(SELECT count(*) FROM learning_mentor_runs WHERE checkpoint IS NOT NULL)+(SELECT count(*) FROM learning_tutor_turns)+(SELECT count(*) FROM knowledge_document_payloads WHERE canonical_markdown LIKE '%ERASE_SELECTED_SOURCE%')`).Scan(&remaining); err != nil || remaining != 0 {
		t.Fatal("late content survived erasure", remaining, err)
	}
}
