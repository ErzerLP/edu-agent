package mentorrun

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/identity"
	identitypg "github.com/edu-agent/edu-agent/server/internal/identity/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/knowledge"
	"github.com/edu-agent/edu-agent/server/internal/learning"
	"github.com/edu-agent/edu-agent/server/internal/learningcontent"
	"github.com/edu-agent/edu-agent/server/internal/learningspace"
	spacepg "github.com/edu-agent/edu-agent/server/internal/learningspace/postgresstore"
	memorypg "github.com/edu-agent/edu-agent/server/internal/memory/postgresstore"
	outboxpg "github.com/edu-agent/edu-agent/server/internal/platform/outbox/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/privacy"
	privacypg "github.com/edu-agent/edu-agent/server/internal/privacy/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/settings"
	"github.com/edu-agent/edu-agent/server/internal/tutoring"
	tutoringpg "github.com/edu-agent/edu-agent/server/internal/tutoring/postgresstore"
	"github.com/google/uuid"
)

func readStudioExercise(t *testing.T, f *exerciseFixture, id string) StudioExerciseView {
	t.Helper()
	var v StudioExerciseView
	if err := f.service.ReadStudioExercise(context.Background(), f.actor, id, func(view StudioExerciseView) error { v = view; return nil }); err != nil {
		t.Fatal(err)
	}
	return v
}

func studioQuestionFixture(t *testing.T) (*exerciseFixture, string, learningcontent.StudioQuizStart, StudioExerciseView) {
	t.Helper()
	f := newExerciseFixture(t)
	f.service.ConfigureStudio(f.start.Content)
	direction := "理解偶数"
	if _, err := spacepg.New(f.pool, f.service.directions).Mutate(context.Background(), f.actor.Device.ID, learningspace.DefaultID, learningspace.Command{OperationID: uuid.NewString(), ExpectedVersion: 1, Name: "测验", Direction: &direction, Status: "active"}); err != nil {
		t.Fatal(err)
	}
	f.modelOverride = func(w http.ResponseWriter, _ *http.Request) { studioReply(w, quizCandidate, "stop") }
	c := directionQuizRequest(t, knowledge.SourceSelection{Mode: "items", Items: []knowledge.SourceIdentity{f.request.Question.Manifest.Entries[0].SourceIdentity}})
	receipt, err := f.service.CreateStudio(context.Background(), f.actor, learningspace.DefaultID, c)
	if err != nil {
		t.Fatal(err)
	}
	f.work(t)
	snapshot := f.snapshot(t, receipt.RunID)
	if snapshot.Studio.Result == nil {
		t.Fatal("fixture quiz generation failed")
	}
	id := snapshot.Studio.Result.ArtifactID
	cmd := learningcontent.StudioQuizStart{OperationID: uuid.NewString(), Version: 1, Generation: 1, GoalVersion: 1, Question: 1}
	state, err := f.start.StartStudioQuiz(context.Background(), f.actor, id, cmd)
	if err != nil {
		t.Fatal(err)
	}
	return f, id, cmd, readStudioExercise(t, f, state.Questions[0].ExerciseID)
}

func TestPostgreSQLStudioExerciseAssessmentRecovery(t *testing.T) {
	for _, scenario := range []string{"resume", "expired", "unknown"} {
		t.Run(scenario, func(t *testing.T) {
			f, _, _, card := studioQuestionFixture(t)
			ctx := context.Background()
			e := learningcontent.Exercise{ID: card.ID, SessionID: card.SessionID, ActivityID: card.ActivityID, ContentArtifactID: card.ArtifactID, ContentVersion: card.ContentVersion}
			if _, err := f.learn.ApplyAction(f.start.Content.WithAnswer(ctx, f.actor, card.ArtifactID, card.ContentVersion), f.actor.Device.ID, card.SessionID, f.command(t, e, tutoring.ActionSubmitAttempt, learning.HelpHint)); err != nil {
				t.Fatal(err)
			}
			if scenario == "unknown" {
				if err := f.service.enqueueAssessment(ctx); err != nil {
					t.Fatal(err)
				}
				owned, err := f.service.claim(ctx)
				if err != nil || owned == nil {
					t.Fatal("cannot claim assessment", err)
				}
				if err = f.service.mutate(ctx, owned, "model_started", func(item *row) error { item.callStarted = true; return nil }); err != nil {
					t.Fatal(err)
				}
				if _, err = f.pool.Exec(ctx, `UPDATE learning_mentor_runs SET lease_until=clock_timestamp()-interval '1 second' WHERE id=$1`, owned.RunID); err != nil {
					t.Fatal(err)
				}
			} else {
				limits := f.settings.View().Limits
				limits.OutputTokens += 64
				if _, err := f.settings.Update(settings.Update{ExpectedRevision: f.settings.View().Revision, Target: settings.Mentor, Limits: &limits}); err != nil {
					t.Fatal(err)
				}
			}
			f.work(t)
			card = readStudioExercise(t, f, card.ID)
			if card.Feedback == nil || card.Feedback.Assessment != nil || card.Assessment == nil || f.calls.Load() != 1 {
				t.Fatal("answer lost or unexpected assessment call")
			}
			c := StudyAssessmentResume{OperationID: uuid.NewString(), ExpectedVersion: card.Assessment.Version, Configuration: card.Assessment.Configuration}
			if scenario == "unknown" {
				if !card.Assessment.ResultUnknown || card.Assessment.CanResume {
					t.Fatal("unknown assessment presented as retryable")
				}
				if _, err := f.service.ResumeStudioAssessment(ctx, f.actor, card.ID, c); err == nil {
					t.Fatal("unknown assessment resent")
				}
				return
			}
			if !card.Assessment.CanResume {
				t.Fatal("configuration pause cannot resume")
			}
			if scenario == "expired" {
				status, err := f.service.AssessmentStatus(ctx, f.actor, card.AttemptID)
				if err != nil {
					t.Fatal(err)
				}
				if _, err = f.pool.Exec(ctx, `UPDATE learning_mentor_runs SET expires_at=clock_timestamp()-interval '1 hour',state=jsonb_set(state,'{expires_at}',to_jsonb(clock_timestamp()-interval '1 hour')) WHERE id=$1`, status.Task.RunID); err != nil {
					t.Fatal(err)
				}
				if readStudioExercise(t, f, card.ID).Assessment.CanResume {
					t.Fatal("expired assessment resumable")
				}
				if _, err = f.service.ResumeStudioAssessment(ctx, f.actor, card.ID, c); err == nil {
					t.Fatal("expired task restarted")
				}
				return
			}
			first, err := f.service.ResumeStudioAssessment(ctx, f.actor, card.ID, c)
			if err != nil {
				t.Fatal(err)
			}
			if replay, err := f.service.ResumeStudioAssessment(ctx, f.actor, card.ID, c); err != nil || replay != first {
				t.Fatal("resume duplicated run", err)
			}
			assessmentReply(t, f, e, 1000)
			f.work(t)
			card = readStudioExercise(t, f, card.ID)
			if card.Feedback.Assessment == nil || len(card.Feedback.Evidence) != 1 || f.calls.Load() != 2 {
				t.Fatal("resumed feedback lost")
			}
		})
	}
}

func TestPostgreSQLStudioExerciseSharedReadRetainsAssessmentOwner(t *testing.T) {
	f, _, _, card := studioQuestionFixture(t)
	ctx := context.Background()
	e := learningcontent.Exercise{ID: card.ID, SessionID: card.SessionID, ActivityID: card.ActivityID, ContentArtifactID: card.ArtifactID, ContentVersion: card.ContentVersion}
	if _, err := f.learn.ApplyAction(f.start.Content.WithAnswer(ctx, f.actor, card.ArtifactID, card.ContentVersion), f.actor.Device.ID, card.SessionID, f.command(t, e, tutoring.ActionSubmitAttempt, learning.HelpHint)); err != nil {
		t.Fatal(err)
	}
	limits := f.settings.View().Limits
	limits.OutputTokens += 64
	if _, err := f.settings.Update(settings.Update{ExpectedRevision: f.settings.View().Revision, Target: settings.Mentor, Limits: &limits}); err != nil {
		t.Fatal(err)
	}
	f.work(t)
	card = readStudioExercise(t, f, card.ID)
	ids, err := identity.NewService(identitypg.New(f.pool), identity.Options{PairingCodeTTL: time.Minute, PairingCodeMaxAttempts: 5, LastUsedTouchInterval: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	code, _, err := ids.CreatePairingCodeForProfile(ctx, identity.PairingProfileLearner)
	if err != nil {
		t.Fatal(err)
	}
	issued, err := ids.ExchangePairingCode(ctx, code, "Studio 另一个读取设备")
	if err != nil {
		t.Fatal(err)
	}
	peer, err := ids.Authenticate(ctx, issued.Token, "learning:read")
	if err != nil {
		t.Fatal(err)
	}
	var read StudioExerciseView
	if err = f.service.ReadStudioExercise(ctx, peer, card.ID, func(v StudioExerciseView) error { read = v; return nil }); err != nil {
		t.Fatal("authorized Studio read lost after submission", err)
	}
	if read.AttemptID != card.AttemptID || read.Feedback == nil || read.Assessment == nil || read.Assessment.CanResume {
		t.Fatal("shared read lost feedback or transferred assessment ownership")
	}
	if _, err = f.service.ResumeStudioAssessment(ctx, peer, card.ID, StudyAssessmentResume{OperationID: uuid.NewString(), ExpectedVersion: card.Assessment.Version, Configuration: card.Assessment.Configuration}); err == nil {
		t.Fatal("peer resumed another device's model task")
	}
}

func TestPostgreSQLStudioExerciseScopeRevocationAndClear(t *testing.T) {
	f, id, start, card := studioQuestionFixture(t)
	ctx := context.Background()
	other, err := spacepg.New(f.pool, f.service.directions).Mutate(ctx, f.actor.Device.ID, "", learningspace.Command{OperationID: uuid.NewString(), ExpectedVersion: 0, Name: "另一区", Status: "active"})
	if err != nil {
		t.Fatal(err)
	}
	wrong, _ := learningspace.WithScope(ctx, other.ID)
	if err := f.service.ReadStudioExercise(wrong, f.actor, card.ID, func(StudioExerciseView) error { t.Error("cross-space card disclosed"); return nil }); err == nil {
		t.Fatal("cross-space read admitted")
	}
	hint := StudioHelpRequest{OperationID: uuid.NewString()}
	if _, err := f.service.PublishStudioHelp(ctx, f.actor, card.ID, hint); err != nil {
		t.Fatal(err)
	}
	var scopes []string
	if err := f.pool.QueryRow(ctx, `SELECT scopes FROM device_tokens WHERE id=$1`, f.actor.TokenID).Scan(&scopes); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `UPDATE device_tokens SET scopes=array_remove(scopes,'knowledge:read') WHERE id=$1`, f.actor.TokenID); err != nil {
		t.Fatal(err)
	}
	if err := f.service.ReadStudioExercise(ctx, f.actor, card.ID, func(StudioExerciseView) error { t.Error("revoked card disclosed"); return nil }); err == nil {
		t.Fatal("source revocation bypassed")
	}
	if _, err := f.service.StudioHelpOperation(ctx, f.actor, card.ID, hint.OperationID); err == nil {
		t.Fatal("revoked hint readable")
	}
	if _, err := f.pool.Exec(ctx, `UPDATE device_tokens SET scopes=$2 WHERE id=$1`, f.actor.TokenID, scopes); err != nil {
		t.Fatal(err)
	}
	tutor := tutoringpg.New(f.pool)
	store := privacypg.New(f.pool, privacypg.WithLocalOwner(identitypg.New(f.pool)), privacypg.WithLocalOwner(f.start.Knowledge), privacypg.WithLocalOwner(f.start.Learning), privacypg.WithLocalOwner(tutor), privacypg.WithLocalOwner(memorypg.New(f.pool)), privacypg.WithLocalOwner(outboxpg.New(f.pool)))
	grants, _ := privacy.NewErasureGrantService(privacypg.NewGrantStore(f.pool), privacy.ErasureGrantOptions{})
	grant, err := grants.Issue(ctx, f.actor.Device.ID, "Studio 清除验收")
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
	if err = f.service.ReadStudioExercise(ctx, f.actor, card.ID, func(StudioExerciseView) error { t.Error("cleared card disclosed"); return nil }); err == nil {
		t.Fatal("clear revived card")
	}
	if _, err = f.service.PublishStudioHelp(ctx, f.actor, card.ID, hint); err == nil {
		t.Fatal("clear revived hint")
	}
	if _, err = f.start.StartStudioQuiz(ctx, f.actor, id, start); err == nil {
		t.Fatal("clear revived quiz operation")
	}
	if remaining, err := learningcontent.Remaining(ctx, f.pool); err != nil || remaining != 0 {
		t.Fatal("Studio practice survived scrub", remaining, err)
	}
}

func TestPostgreSQLStudioLegacyFirstQuestionVersionConflict(t *testing.T) {
	f := newExerciseFixture(t)
	f.service.ConfigureStudio(f.start.Content)
	f.modelOverride = func(w http.ResponseWriter, _ *http.Request) { studioReply(w, quizCandidate, "stop") }
	c := StudioGenerate{StudioTarget: learningcontent.StudioTarget{Kind: "quiz", Generation: 1, GoalID: &f.goal}, OperationID: uuid.NewString(), GoalVersion: 1, SourceSelection: knowledge.SourceSelection{Mode: "items", Items: []knowledge.SourceIdentity{f.request.Question.Manifest.Entries[0].SourceIdentity}}}
	receipt, err := f.service.CreateStudio(context.Background(), f.actor, learningspace.DefaultID, c)
	if err != nil {
		t.Fatal(err)
	}
	f.work(t)
	id := f.snapshot(t, receipt.RunID).Studio.Result.ArtifactID
	goal, err := f.learn.GetGoal(context.Background(), f.goal)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.learn.CreateGoal(context.Background(), f.actor.Device.ID, learning.GoalCommand{Operation: learning.OperationEnvelope{OperationID: uuid.NewString(), AggregateType: "goal", AggregateID: f.goal, ExpectedVersion: 1, PayloadSchemaVersion: 1, Payload: json.RawMessage(`{}`)}, PreviousRevisionID: &goal.ID, Text: "新手选目标修订", Source: "fixture"}); err != nil {
		t.Fatal(err)
	}
	cmd := learningcontent.StudioQuizStart{OperationID: uuid.NewString(), Version: 1, Generation: 1, GoalVersion: 1, Question: 1}
	if _, err = f.start.StartStudioQuiz(context.Background(), f.actor, id, cmd); learning.ErrorCode(err) != learning.CodeVersionConflict {
		t.Fatal("legacy first question lost version conflict", err)
	}
	read, err := f.start.Content.StudioGet(context.Background(), f.actor, id, 1)
	if err != nil || read.Body.Quiz.Direction != nil || read.Body.Quiz.Assessment != nil {
		t.Fatal("legacy data silently upgraded", err)
	}
}
