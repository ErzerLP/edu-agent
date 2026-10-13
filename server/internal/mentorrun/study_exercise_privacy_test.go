package mentorrun

import (
	"context"
	"net/http"
	"testing"
	"time"

	identitypg "github.com/edu-agent/edu-agent/server/internal/identity/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/learning"
	"github.com/edu-agent/edu-agent/server/internal/learningcontent"
	"github.com/edu-agent/edu-agent/server/internal/learningspace"
	memorypg "github.com/edu-agent/edu-agent/server/internal/memory/postgresstore"
	outboxpg "github.com/edu-agent/edu-agent/server/internal/platform/outbox/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/privacy"
	privacypg "github.com/edu-agent/edu-agent/server/internal/privacy/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/tutoring"
	tutoringpg "github.com/edu-agent/edu-agent/server/internal/tutoring/postgresstore"
	"github.com/google/uuid"
)

func TestPostgreSQLStudyExerciseHelpSubmissionConcurrency(t *testing.T) {
	f := newExerciseFixture(t)
	ctx := context.Background()
	f.modelOverride = func(w http.ResponseWriter, r *http.Request) { studioReply(w, studyExerciseCandidate, "stop") }
	id, selection := studyExerciseConversation(t, f)
	studyExerciseTurn(t, f, id, selection)
	card := readStudyExercise(t, f, historyPage(t, f.runtimeFixture, id, 0, 10).Items[0].ExerciseID)
	e := learningcontent.Exercise{ID: card.ID, SessionID: card.SessionID, ActivityID: card.ActivityID, ContentArtifactID: card.ArtifactID, ContentVersion: card.ContentVersion}
	command := f.command(t, e, tutoring.ActionSubmitAttempt, learning.HelpNone)
	start := make(chan struct{})
	helpDone, answerDone := make(chan error, 1), make(chan error, 1)
	go func() {
		<-start
		_, err := f.service.PublishStudyHelp(ctx, f.actor, card.ID, StudyHelpRequest{OperationID: uuid.NewString(), SourceSelection: selection})
		helpDone <- err
	}()
	go func() {
		<-start
		_, err := f.learn.ApplyAction(f.start.Content.WithAnswer(ctx, f.actor, e.ContentArtifactID, e.ContentVersion), f.actor.Device.ID, e.SessionID, command)
		answerDone <- err
	}()
	close(start)
	helpErr, answerErr := <-helpDone, <-answerDone
	if (helpErr == nil) == (answerErr == nil) {
		t.Fatalf("concurrent hint/independent answer must have exactly one winner: %v / %v", helpErr, answerErr)
	}
	if helpErr == nil {
		if _, err := f.learn.ApplyAction(f.start.Content.WithAnswer(ctx, f.actor, e.ContentArtifactID, e.ContentVersion), f.actor.Device.ID, e.SessionID, f.command(t, e, tutoring.ActionSubmitAttempt, learning.HelpHint)); err != nil {
			t.Fatal(err)
		}
	}
	card = readStudyExercise(t, f, card.ID)
	if card.Feedback == nil || helpErr == nil && card.Feedback.Attempt.Help != learning.HelpHint || helpErr != nil && card.HelpVersion != 0 {
		t.Fatal("help race changed submitted declaration")
	}
}

func TestPostgreSQLStudyExerciseClearCannotRevive(t *testing.T) {
	f := newExerciseFixture(t)
	ctx := context.Background()
	f.modelOverride = func(w http.ResponseWriter, r *http.Request) { studioReply(w, studyExerciseCandidate, "stop") }
	id, selection := studyExerciseConversation(t, f)
	studyExerciseTurn(t, f, id, selection)
	card := readStudyExercise(t, f, historyPage(t, f.runtimeFixture, id, 0, 10).Items[0].ExerciseID)
	help := StudyHelpRequest{OperationID: uuid.NewString(), SourceSelection: selection}
	if _, err := f.service.PublishStudyHelp(ctx, f.actor, card.ID, help); err != nil {
		t.Fatal(err)
	}
	tutor := tutoringpg.New(f.pool)
	store := privacypg.New(f.pool, privacypg.WithLocalOwner(identitypg.New(f.pool)), privacypg.WithLocalOwner(f.start.Knowledge), privacypg.WithLocalOwner(f.start.Learning), privacypg.WithLocalOwner(tutor), privacypg.WithLocalOwner(memorypg.New(f.pool)), privacypg.WithLocalOwner(outboxpg.New(f.pool)))
	grants, err := privacy.NewErasureGrantService(privacypg.NewGrantStore(f.pool), privacy.ErasureGrantOptions{})
	if err != nil {
		t.Fatal(err)
	}
	grant, err := grants.Issue(ctx, f.actor.Device.ID, "对话练习清除验收")
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
	if err = f.service.ReadStudyExercise(ctx, f.actor, card.ID, func(StudyExerciseView) error { t.Error("cleared card sent"); return nil }); err == nil {
		t.Fatal("cleared card revived")
	}
	if _, err = f.service.PublishStudyHelp(ctx, f.actor, card.ID, help); err == nil {
		t.Fatal("cleared help replay revived")
	}
	if err = f.service.ReadConversation(ctx, f.actor, learningspace.DefaultID, id, 0, 10, func(TurnPage) error { t.Error("cleared history sent"); return nil }); err == nil {
		t.Fatal("cleared history revived")
	}
	var count int
	if err = f.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM learning_tutor_turns)+(SELECT count(*) FROM learning_exercises)+(SELECT count(*) FROM learning_exercise_help)`).Scan(&count); err != nil || count != 0 {
		t.Fatal("card mapping/help survived clear", count, err)
	}
}
