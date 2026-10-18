package mentorrun

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"testing"
	"time"

	identitypg "github.com/edu-agent/edu-agent/server/internal/identity/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/learning"
	"github.com/edu-agent/edu-agent/server/internal/learningcontent"
	"github.com/edu-agent/edu-agent/server/internal/learningspace"
	memorypg "github.com/edu-agent/edu-agent/server/internal/memory/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/platform/keyrotation"
	outboxpg "github.com/edu-agent/edu-agent/server/internal/platform/outbox/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/privacy"
	privacypg "github.com/edu-agent/edu-agent/server/internal/privacy/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/settings"
	"github.com/edu-agent/edu-agent/server/internal/tutoring"
	tutoringpg "github.com/edu-agent/edu-agent/server/internal/tutoring/postgresstore"
	"github.com/google/uuid"
)

func TestPostgreSQLExerciseLegacyPauseAndInternalPorts(t *testing.T) {
	f := newExerciseFixture(t)
	ctx := context.Background()
	f.request.Assessment.Tokens = 1
	e, attempt := f.submitted(t)
	assessmentReply(t, f, e, 1000)
	if err := f.service.enqueueAssessment(ctx); err != nil {
		t.Fatal(err)
	}
	before, err := f.service.AssessmentStatus(ctx, f.actor, attempt.ID)
	if err != nil || before.Run == nil {
		t.Fatal(err)
	}
	legacyPause(t, f.runtimeFixture, before.Task.RunID)
	f.work(t)
	state, err := f.service.AssessmentStatus(ctx, f.actor, attempt.ID)
	if err != nil || state.Run == nil || state.Run.Status != "paused_budget" || f.calls.Load() != 0 {
		t.Fatalf("budget bypass: %+v %v", state, err)
	}
	err = f.service.ReadList(ctx, f.actor, learningspace.DefaultID, ListQuery{Limit: 100}, func(page Page) error {
		if len(page.Items) != 0 {
			t.Error("internal task leaked into public run contract")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = f.service.ReadSnapshot(ctx, f.actor, learningspace.DefaultID, state.Task.RunID, func(Snapshot) error { t.Error("internal task exposed by public reader"); return nil }); err == nil {
		t.Fatal("internal run publicly readable")
	}
	resume := AssessmentResume{OperationID: uuid.NewString(), ExpectedVersion: state.Run.Version, Policy: learningcontent.AssessmentPolicy{Configuration: f.request.Assessment.Configuration, Requests: 2, Tokens: 50000}}
	first, err := f.service.ResumeAssessment(ctx, f.actor, attempt.ID, resume)
	if err != nil {
		t.Fatal(err)
	}
	if again, err := f.service.ResumeAssessment(ctx, f.actor, attempt.ID, resume); err != nil || again.RunID != first.RunID {
		t.Fatal("resume replay", err)
	}
	f.work(t)
	state, err = f.service.AssessmentStatus(ctx, f.actor, attempt.ID)
	if err != nil || state.Run.Status != "succeeded" || f.calls.Load() != 1 {
		t.Fatalf("budget continuation lost attempt: %+v %v", state, err)
	}
}

func TestPostgreSQLExerciseRevocationAndRotation(t *testing.T) {
	f := newExerciseFixture(t)
	ctx := context.Background()
	e := f.publish(t)
	help := learningcontent.ExerciseHelp{OperationID: uuid.NewString(), Level: learning.HelpHint, Text: "PRIVATE_HELP_TO_ROTATE"}
	if _, err := f.start.Content.PublishHelp(ctx, f.actor, e.ID, help); err != nil {
		t.Fatal(err)
	}
	f.present(t, e)
	if _, err := f.learn.ApplyAction(learning.WithAssessmentActor(ctx, f.actor), f.actor.Device.ID, e.SessionID, f.command(t, e, tutoring.ActionSubmitAttempt, learning.HelpHint)); err != nil {
		t.Fatal(err)
	}
	if err := f.service.enqueueAssessment(ctx); err != nil {
		t.Fatal(err)
	}
	var raw []byte
	if err := f.pool.QueryRow(ctx, `SELECT ciphertext FROM learning_exercise_help WHERE exercise_id=$1`, e.ID).Scan(&raw); err != nil || bytes.Contains(raw, []byte(help.Text)) {
		t.Fatal("help stored in plaintext", err)
	}
	// Unprivileged mutation must not lower the disclosed floor or swap a mapping.
	if _, err := f.pool.Exec(ctx, `UPDATE learning_exercise_help SET level='answer_revealed' WHERE exercise_id=$1`, e.ID); err == nil {
		t.Fatal("immutable disclosure could be changed")
	}
	key := bytes.Repeat([]byte{43}, 32)
	rewrap, err := keyrotation.New(bytes.Repeat([]byte{42}, 32), key)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `LOCK TABLE learning_content_revisions IN ACCESS EXCLUSIVE MODE; SET LOCAL app.mentor_key_rotation='on'`); err != nil {
		t.Fatal(err)
	}
	if err = learningcontent.RotateKeyTx(ctx, tx, rewrap); err != nil {
		t.Fatal(err)
	}
	if err = RotateKeyTx(ctx, tx, rewrap); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	content, err := learningcontent.New(f.pool, f.start.Learning, key)
	if err != nil {
		t.Fatal(err)
	}
	content.ConfigureStudioSources(f.start.Knowledge)
	content.ConfigureReferences(f.start.Knowledge)
	f.start.Content = content
	f.start.Learning.ConfigureContent(content)
	restarted, err := New(f.pool, f.settings, key)
	if err != nil {
		t.Fatal(err)
	}
	restarted.ConfigureStart(f.start)
	f.service = restarted
	if got, err := content.PublishHelp(ctx, f.actor, e.ID, help); err != nil || got.Text != help.Text {
		t.Fatal("help rotation/replay failed", err)
	}
	// Revoke sources after enqueue. Restart may not assess or publish late output.
	if _, err = f.pool.Exec(ctx, `DELETE FROM knowledge_collection_links WHERE space_id=$1`, learningspace.DefaultID); err != nil {
		t.Fatal(err)
	}
	f.work(t)
	var count int
	if err = f.pool.QueryRow(ctx, `SELECT count(*) FROM learning_assessments`).Scan(&count); err != nil || count != 0 || f.calls.Load() != 0 {
		t.Fatal("revoked source revived evaluation", count, err)
	}
	if _, err = content.PublishHelp(ctx, f.actor, e.ID, help); err == nil {
		t.Fatal("revoked help replay readable")
	}
}

func TestPostgreSQLExerciseGlobalErasureCancelsInFlight(t *testing.T) {
	f := newExerciseFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	e := f.publish(t)
	help := learningcontent.ExerciseHelp{OperationID: uuid.NewString(), Level: learning.HelpHint, Text: "ERASE_DISCLOSED_HELP"}
	if _, err := f.start.Content.PublishHelp(ctx, f.actor, e.ID, help); err != nil {
		t.Fatal(err)
	}
	f.present(t, e)
	if _, err := f.learn.ApplyAction(learning.WithAssessmentActor(ctx, f.actor), f.actor.Device.ID, e.SessionID, f.command(t, e, tutoring.ActionSubmitAttempt, learning.HelpHint)); err != nil {
		t.Fatal(err)
	}
	view, err := f.start.Learning.Session(ctx, e.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	attempt := *view.WorkItem.Attempt
	raw := assessmentJSON(t, f, e, 1000)
	started := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	f.modelOverride = func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		close(started)
		select {
		case <-r.Context().Done():
		case <-release:
		}
		studioReply(w, string(raw), "stop")
	}
	permits := privacy.NewReadPermitManager()
	f.service.ConfigureMemory(nil, permits)
	done := make(chan error, 1)
	go func() { _, err := f.service.RunOnce(ctx); done <- err }()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	tutor := tutoringpg.New(f.pool)
	store := privacypg.New(f.pool, privacypg.WithReadPermits(permits), privacypg.WithLocalOwner(identitypg.New(f.pool)), privacypg.WithLocalOwner(f.start.Knowledge), privacypg.WithLocalOwner(f.start.Learning), privacypg.WithLocalOwner(tutor), privacypg.WithLocalOwner(memorypg.New(f.pool)), privacypg.WithLocalOwner(outboxpg.New(f.pool)))
	grants, err := privacy.NewErasureGrantService(privacypg.NewGrantStore(f.pool), privacy.ErasureGrantOptions{})
	if err != nil {
		t.Fatal(err)
	}
	grant, err := grants.Issue(ctx, f.actor.Device.ID, "单题内核清除验收")
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
		t.Fatal("assessment did not cancel for erasure")
	}
	if _, err = store.RunLocalScrub(ctx, barrier.ErasureID); err != nil {
		t.Fatal(err)
	}
	if remaining, err := learningcontent.Remaining(ctx, f.pool); err != nil || remaining != 0 {
		t.Fatal("exercise/help/task survived Remaining", remaining, err)
	}
	if _, err = f.service.AssessmentStatus(ctx, f.actor, attempt.ID); err == nil {
		t.Fatal("erased task readable")
	}
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err = f.start.PublishExerciseTx(ctx, tx, f.actor, f.request); err == nil {
		t.Fatal("old publication revived after erasure")
	}
	var remaining int
	if err = f.pool.QueryRow(ctx, `SELECT count(*) FROM learning_mentor_runs WHERE checkpoint IS NOT NULL`).Scan(&remaining); err != nil || remaining != 0 {
		t.Fatal("late assessment checkpoint revived", remaining, err)
	}
}

func TestPostgreSQLExerciseConfigurationChangeRequiresExplicitResume(t *testing.T) {
	f := newExerciseFixture(t)
	ctx := context.Background()
	e, attempt := f.submitted(t)
	assessmentReply(t, f, e, 1000)
	limits := f.settings.View().Limits
	limits.ResearchRequests = 1
	if _, err := f.settings.Update(settings.Update{ExpectedRevision: f.settings.View().Revision, Target: settings.Mentor, Limits: &limits}); err != nil {
		t.Fatal(err)
	}
	f.work(t)
	state, err := f.service.AssessmentStatus(ctx, f.actor, attempt.ID)
	if err != nil || state.Run == nil || state.Run.Status != "failed" || state.Run.Reason != "context_changed" || f.calls.Load() != 0 {
		t.Fatalf("configuration changed without confirmation: %+v %v", state, err)
	}
	policy, err := f.service.AssessmentPolicy(1, 50000)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.service.ResumeAssessment(ctx, f.actor, attempt.ID, AssessmentResume{OperationID: uuid.NewString(), ExpectedVersion: state.Run.Version, Policy: policy}); err != nil {
		t.Fatal(err)
	}
	f.work(t)
	state, err = f.service.AssessmentStatus(ctx, f.actor, attempt.ID)
	if err != nil || state.Run.Status != "succeeded" || f.calls.Load() != 1 {
		t.Fatal("explicit destination resume failed", err)
	}
}
