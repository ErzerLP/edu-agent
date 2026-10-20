package mentorrun

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/integrations/learningknowledge"
	"github.com/edu-agent/edu-agent/server/internal/knowledge"
	knowledgepg "github.com/edu-agent/edu-agent/server/internal/knowledge/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/learning"
	learningpg "github.com/edu-agent/edu-agent/server/internal/learning/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/learningcontent"
	"github.com/edu-agent/edu-agent/server/internal/learningspace"
	spacepg "github.com/edu-agent/edu-agent/server/internal/learningspace/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/learningstart"
	"github.com/edu-agent/edu-agent/server/internal/tutoring"
	tutoringpg "github.com/edu-agent/edu-agent/server/internal/tutoring/postgresstore"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type exerciseFixture struct {
	*runtimeFixture
	start   *learningstart.Service
	learn   *learning.Service
	request learningstart.ExerciseRequest
}

func newExerciseFixture(t *testing.T, modelAddress ...string) *exerciseFixture {
	t.Helper()
	f := fixture(t, func(w http.ResponseWriter, r *http.Request, n int) {
		t.Error("unexpected model call")
		http.Error(w, "unexpected", 500)
	}, modelAddress...)
	selected := selectedDocument(t, f, "SELECTED_EVEN_SOURCE")
	k := knowledgepg.New(f.pool)
	store := learningpg.New(f.pool, tutoringpg.New(f.pool), k)
	content, err := learningcontent.New(f.pool, store, bytes.Repeat([]byte{42}, 32))
	if err != nil {
		t.Fatal(err)
	}
	content.ConfigureStudioSources(k)
	content.ConfigureReferences(k)
	store.ConfigureContent(content)
	start := &learningstart.Service{Learning: store, Knowledge: k, Content: content}
	f.service.ConfigureStart(start)
	ks, _ := knowledge.NewService(k, knowledge.NewCanonicalizer(), knowledge.ServiceOptions{})
	learn, err := learning.NewService(store, store, learningknowledge.New(ks), learning.ServiceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	f.goal = uuid.NewString()
	if _, err = learn.CreateGoal(ctx, f.actor.Device.ID, learning.GoalCommand{Operation: learning.OperationEnvelope{OperationID: uuid.NewString(), PayloadSchemaVersion: 1, AggregateType: "goal", AggregateID: f.goal, Payload: json.RawMessage(`{}`)}, Text: "单题正式目标 A", Source: "fixture"}); err != nil {
		t.Fatal(err)
	}
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	manifest, err := k.FreezeSourceSelectionTx(ctx, tx, knowledge.SourceSelection{Mode: "items", Items: []knowledge.SourceIdentity{selected}})
	if err != nil {
		t.Fatal(err)
	}
	sources, err := k.ReadSourceSelectionTx(ctx, tx, *manifest)
	if err != nil {
		t.Fatal(err)
	}
	body, err := learningcontent.DecodeStudioCandidate("quiz", []byte(quizCandidate), manifest, sources)
	if err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	goal, err := learn.GetGoal(ctx, f.goal)
	if err != nil {
		t.Fatal(err)
	}
	_, _, configuration, err := f.settings.MentorClient()
	if err != nil {
		t.Fatal(err)
	}
	question := body.Quiz.Questions[0]
	question.AllowedHelp = []string{"none", "hint", "scaffold", "answer_revealed"}
	return &exerciseFixture{runtimeFixture: f, start: start, learn: learn, request: learningstart.ExerciseRequest{
		Question:   learningstart.SingleQuestion{OperationID: uuid.NewString(), GoalRevisionID: goal.ID, Generation: 1, Label: "单题 A", Model: "fixture", Manifest: *manifest, Question: question, Citations: body.Citations},
		Assessment: learningcontent.AssessmentPolicy{Configuration: configuration},
	}}
}

func (f *exerciseFixture) publish(t *testing.T) learningcontent.Exercise {
	t.Helper()
	var exercise learningcontent.Exercise
	err := f.start.Content.ExerciseTransaction(context.Background(), f.actor, func(tx pgx.Tx) error {
		var e error
		exercise, e = f.start.PublishExerciseTx(context.Background(), tx, f.actor, f.request)
		return e
	})
	if err != nil {
		t.Fatal(err)
	}
	return exercise
}

func (f *exerciseFixture) command(t *testing.T, e learningcontent.Exercise, action tutoring.Action, help learning.HelpLevel) learning.ActionCommand {
	t.Helper()
	view, err := f.start.Learning.Session(context.Background(), e.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	return learning.ActionCommand{Operation: learning.OperationEnvelope{OperationID: uuid.NewString(), PayloadSchemaVersion: 1, AggregateType: "session", AggregateID: e.SessionID, ExpectedVersion: view.Session.AggregateVer, Payload: json.RawMessage(`{}`)}, Action: action, Answer: "偶数可被 2 整除。", Help: help}
}

func (f *exerciseFixture) present(t *testing.T, e learningcontent.Exercise) {
	t.Helper()
	_, err := f.learn.ApplyAction(context.Background(), f.actor.Device.ID, e.SessionID, f.command(t, e, tutoring.ActionPresentActivity, ""))
	if err != nil {
		t.Fatal(err)
	}
}

// Losing the common Commit guard would allow either legacy entry point to
// under-report disclosed help or commit an Attempt without its recovery task.
func TestPostgreSQLExerciseSubmissionAndHelp(t *testing.T) {
	f := newExerciseFixture(t)
	ctx := context.Background()
	e := f.publish(t)
	if replay := f.publish(t); replay.ID != e.ID || replay.SessionID != e.SessionID {
		t.Fatal("publication replay duplicated question")
	}
	f.present(t, e)
	h := learningcontent.ExerciseHelp{OperationID: uuid.NewString(), Level: learning.HelpScaffold, Text: "先判断是否能被 2 整除。"}
	if _, err := f.start.Content.PublishHelp(ctx, f.actor, e.ID, h); err != nil {
		t.Fatal(err)
	}
	if _, err := f.start.Content.PublishHelp(ctx, f.actor, e.ID, learningcontent.ExerciseHelp{OperationID: uuid.NewString(), Level: learning.HelpHint, Text: "想一想整除。"}); err != nil {
		t.Fatal(err)
	}
	submit := f.command(t, e, tutoring.ActionSubmitAttempt, learning.HelpNone)
	actorCtx := learning.WithAssessmentActor(ctx, f.actor)
	if _, err := f.learn.ApplyAction(actorCtx, f.actor.Device.ID, e.SessionID, submit); err == nil {
		t.Fatal("session action bypassed help floor")
	}
	answerCtx := f.start.Content.WithAnswer(ctx, f.actor, e.ContentArtifactID, 1)
	submit = f.command(t, e, tutoring.ActionSubmitAttempt, learning.HelpNone)
	if _, err := f.learn.ApplyAction(answerCtx, f.actor.Device.ID, e.SessionID, submit); err == nil {
		t.Fatal("content answer bypassed help floor")
	}
	submit = f.command(t, e, tutoring.ActionSubmitAttempt, learning.HelpScaffold)
	if _, err := f.learn.ApplyAction(actorCtx, f.actor.Device.ID, e.SessionID, submit); err != nil {
		t.Fatal(err)
	}
	if r, err := f.learn.ApplyAction(actorCtx, f.actor.Device.ID, e.SessionID, submit); err != nil || !r.Replayed {
		t.Fatal("lost response replay", err)
	}
	var attempts, tasks int
	if err := f.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM learning_attempts WHERE activity_id=$1),(SELECT count(*) FROM learning_assessment_tasks WHERE exercise_id=$2)`, e.ActivityID, e.ID).Scan(&attempts, &tasks); err != nil || attempts != 1 || tasks != 1 {
		t.Fatalf("attempt/task atomicity %d/%d: %v", attempts, tasks, err)
	}
	if _, err := f.start.Content.PublishHelp(ctx, f.actor, e.ID, learningcontent.ExerciseHelp{OperationID: uuid.NewString(), Level: learning.HelpAnswerRevealed, Text: "完整答案"}); err == nil {
		t.Fatal("late hint changed submitted help")
	}
	view, err := f.start.Learning.Session(ctx, e.SessionID)
	if err != nil || view.WorkItem.Attempt.Help != learning.HelpScaffold {
		t.Fatal("submitted help changed", err)
	}
}

func assessmentJSON(t *testing.T, f *exerciseFixture, e learningcontent.Exercise, confidence int) json.RawMessage {
	t.Helper()
	view, err := f.start.Learning.Session(context.Background(), e.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	a, attempt := view.WorkItem.Activity, view.WorkItem.Attempt
	ref := a.References[0]
	item := learning.AssessmentItem{RubricItemID: "definition", Conclusion: learning.ConclusionPass, AnswerQuote: attempt.Answer, AnswerRange: learning.SourceRange{End: len(attempt.Answer)}, AnswerQuoteSHA256: learning.SHA256([]byte(attempt.Answer)), KnowledgeReferenceID: ref.NodeRevisionID, KnowledgeQuote: ref.Slice, KnowledgeRange: learning.SourceRange{End: len(ref.Slice)}, KnowledgeQuoteSHA256: learning.SHA256([]byte(ref.Slice))}
	encoded, _ := json.Marshal(item)
	var fields map[string]any
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatal(err)
	}
	fields["misconception_candidate"] = nil
	raw, _ := json.Marshal(map[string]any{"assessment": map[string]any{"items": []any{fields}, "rubric_complete": true, "confidence": confidence, "risk_flags": []string{}}})
	return raw
}

func assessmentReply(t *testing.T, f *exerciseFixture, e learningcontent.Exercise, confidence int) {
	raw := assessmentJSON(t, f, e, confidence)
	f.modelOverride = func(w http.ResponseWriter, r *http.Request) { studioReply(w, string(raw), "stop") }
}

func TestPostgreSQLExerciseAutomaticAssessment(t *testing.T) {
	f := newExerciseFixture(t)
	ctx := context.Background()
	e := f.publish(t)
	f.present(t, e)
	command := f.command(t, e, tutoring.ActionSubmitAttempt, learning.HelpNone)
	if _, err := f.learn.ApplyAction(f.start.Content.WithAnswer(ctx, f.actor, e.ContentArtifactID, 1), f.actor.Device.ID, e.SessionID, command); err != nil {
		t.Fatal(err)
	}
	assessmentReply(t, f, e, 1000)
	f.work(t)
	f.work(t)
	view, err := f.start.Learning.Session(ctx, e.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	feedback, err := f.learn.Feedback(ctx, view.WorkItem.Attempt.ID, false)
	if err != nil || feedback.Assessment == nil || len(feedback.Evidence) != 1 {
		t.Fatalf("automatic assessment missing: %+v %v", feedback, err)
	}
	if f.calls.Load() != 1 {
		t.Fatal("duplicate task repeated model", f.calls.Load())
	}
	assertTaskUsage(t, f.runtimeFixture, view.WorkItem.Attempt.ID, "assessment", "assessment", 1)
	due := time.Now().Add(90 * 24 * time.Hour)
	reviews, err := f.start.Learning.Reviews(ctx, learning.ReviewQuery{Status: "all", DueBefore: &due, Page: learning.CursorPageRequest{Limit: 10}})
	if err != nil || len(reviews.Items) != 1 || reviews.Items[0].AttemptID != view.WorkItem.Attempt.ID {
		t.Fatalf("assessment did not reach due review: %+v attempt=%s err=%v", reviews, view.WorkItem.Attempt.ID, err)
	}
}

func (f *exerciseFixture) submitted(t *testing.T) (learningcontent.Exercise, learning.Attempt) {
	t.Helper()
	e := f.publish(t)
	f.present(t, e)
	if _, err := f.learn.ApplyAction(f.start.Content.WithAnswer(context.Background(), f.actor, e.ContentArtifactID, 1), f.actor.Device.ID, e.SessionID, f.command(t, e, tutoring.ActionSubmitAttempt, learning.HelpNone)); err != nil {
		t.Fatal(err)
	}
	view, err := f.start.Learning.Session(context.Background(), e.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	return e, *view.WorkItem.Attempt
}

func TestPostgreSQLExerciseLeaseRecovery(t *testing.T) {
	for _, stage := range []string{"queued", "before_call", "proposal_ready", "operation_committed", "unknown", "proposal_busy"} {
		t.Run(stage, func(t *testing.T) {
			f := newExerciseFixture(t)
			ctx := context.Background()
			e, attempt := f.submitted(t)
			assessmentReply(t, f, e, 1000)
			if err := f.service.enqueueAssessment(ctx); err != nil {
				t.Fatal(err)
			}
			tx, err := f.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			task, err := f.start.Learning.AssessmentTaskTx(ctx, tx, attempt.ID)
			if err != nil {
				t.Fatal(err)
			}
			if err = tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			if stage != "queued" {
				owned, err := f.service.claim(ctx)
				if err != nil || owned == nil {
					t.Fatal("claim", err)
				}
				if stage != "before_call" {
					if err = f.service.mutate(ctx, owned, "model_started", func(r *row) error {
						r.callStarted = true
						r.CostUnknown = true
						r.RequestsUsed++
						return nil
					}); err != nil {
						t.Fatal(err)
					}
				}
				view, err := f.start.Learning.Session(ctx, e.SessionID)
				if err != nil {
					t.Fatal(err)
				}
				req := assessmentRequest(task, *view.WorkItem.Activity, *view.WorkItem.Attempt)
				if stage == "proposal_ready" || stage == "operation_committed" {
					ks, _ := knowledge.NewService(f.start.Knowledge, knowledge.NewCanonicalizer(), knowledge.ServiceOptions{})
					svc, err := learning.NewService(f.start.Learning, f.start.Learning, learningknowledge.New(ks), learning.ServiceOptions{Model: quizAssessmentModel(assessmentJSON(t, f, e, 1000)), ModelID: "fixture"})
					if err != nil {
						t.Fatal(err)
					}
					p, err := svc.Propose(ctx, f.actor.Device.ID, req)
					if err != nil {
						t.Fatal(err)
					}
					if stage == "operation_committed" {
						if _, err = svc.ApplyAction(ctx, f.actor.Device.ID, e.SessionID, assessmentCommand(task, p.ID)); err != nil {
							t.Fatal(err)
						}
					}
				} else {
					hash, err := learning.HashJSON(req)
					if err != nil {
						t.Fatal(err)
					}
					if _, err = f.start.Learning.ClaimProposal(ctx, task.DeviceID, req, hash, time.Now()); err != nil {
						t.Fatal(err)
					}
					if stage != "proposal_busy" {
						if _, err = f.pool.Exec(ctx, `UPDATE tutoring_proposal_requests SET lease_expires_at=clock_timestamp()-interval '1 second' WHERE request_id=$1`, task.RequestID); err != nil {
							t.Fatal(err)
						}
					}
				}
				if _, err = f.pool.Exec(ctx, `UPDATE learning_mentor_runs SET lease_until=clock_timestamp()-interval '1 second' WHERE id=$1`, task.RunID); err != nil {
					t.Fatal(err)
				}
			}
			restarted, err := New(f.pool, f.settings, bytes.Repeat([]byte{42}, 32))
			if err != nil {
				t.Fatal(err)
			}
			restarted.ConfigureStart(f.start)
			f.service = restarted
			f.work(t)
			status, err := f.service.AssessmentStatus(ctx, f.actor, task.AttemptID)
			if err != nil || status.Run == nil {
				t.Fatal("assessment status", err)
			}
			state := *status.Run
			if stage == "unknown" {
				if state.Status != "failed" || !state.ResultUnknown || f.calls.Load() != 0 {
					t.Fatalf("unknown call repeated: %+v", state)
				}
				return
			}
			if stage == "proposal_busy" {
				if state.Status != "running" || f.calls.Load() != 0 {
					t.Fatalf("live proposal not respected: %+v", state)
				}
				return
			}
			if state.Status != "succeeded" || state.ResultUnknown {
				t.Fatalf("durable assessment not reconciled: %+v", state)
			}
			want := int32(0)
			if stage == "queued" || stage == "before_call" {
				want = 1
			}
			if f.calls.Load() != want {
				t.Fatal("recovery repeated paid evaluation", f.calls.Load(), want)
			}
			var count int
			if err = f.pool.QueryRow(ctx, `SELECT count(*) FROM learning_assessments WHERE attempt_id=$1`, attempt.ID).Scan(&count); err != nil || count != 1 {
				t.Fatal("recovery duplicated assessment", count, err)
			}
		})
	}
}

func TestPostgreSQLExerciseOriginalDirectionAndSources(t *testing.T) {
	f := newExerciseFixture(t)
	ctx := context.Background()
	spaces := spacepg.New(f.pool, f.service.directions)
	setDirection := func(text string, version int64) {
		t.Helper()
		if _, err := spaces.Mutate(ctx, f.actor.Device.ID, learningspace.DefaultID, learningspace.Command{OperationID: uuid.NewString(), ExpectedVersion: version, Name: "练习方向", Direction: &text, Status: "active"}); err != nil {
			t.Fatal(err)
		}
	}
	setDirection("DIRECTION_A", 1)
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	d, err := f.service.directions.EnsureTx(ctx, tx, f.actor.Device.ID, learningspace.DefaultID, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	f.request.Question.GoalRevisionID = d.GoalRevisionID
	e := f.publish(t)
	setDirection("DIRECTION_B", 2)
	b := selectedDocument(t, f.runtimeFixture, "SOURCE_B_MUST_NOT_ENTER_ASSESSMENT")
	tx, err = f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.start.Knowledge.FreezeSourceSelectionTx(ctx, tx, knowledge.SourceSelection{Mode: "items", Items: []knowledge.SourceIdentity{b}}); err != nil {
		t.Fatal(err)
	}
	if _, err = f.service.directions.EnsureTx(ctx, tx, f.actor.Device.ID, learningspace.DefaultID, 1, 2); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	// Republishing an accepted old candidate must also use its exact revision.
	f.request.Question.OperationID = uuid.NewString()
	old := f.publish(t)
	authority, err := f.start.Learning.LoadSessionAuthority(ctx, old.SessionID)
	if err != nil || authority.Session.Context.GoalRevisionID != d.GoalRevisionID {
		t.Fatal("bridge followed latest direction", err)
	}
	f.present(t, e)
	if _, err = f.learn.ApplyAction(learning.WithAssessmentActor(ctx, f.actor), f.actor.Device.ID, e.SessionID, f.command(t, e, tutoring.ActionSubmitAttempt, learning.HelpNone)); err != nil {
		t.Fatal(err)
	}
	raw := assessmentJSON(t, f, e, 1000)
	f.modelOverride = func(w http.ResponseWriter, r *http.Request) {
		input, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
		}
		if !bytes.Contains(input, []byte("SELECTED_EVEN_SOURCE")) || !bytes.Contains(input, []byte(d.GoalRevisionID)) || bytes.Contains(input, []byte("SOURCE_B_MUST_NOT_ENTER_ASSESSMENT")) || bytes.Contains(input, []byte("DIRECTION_B")) {
			t.Errorf("assessment context flags: sourceA=%v directionA=%v sourceB=%v directionB=%v", bytes.Contains(input, []byte("SELECTED_EVEN_SOURCE")), bytes.Contains(input, []byte("DIRECTION_A")), bytes.Contains(input, []byte("SOURCE_B_MUST_NOT_ENTER_ASSESSMENT")), bytes.Contains(input, []byte("DIRECTION_B")))
		}
		studioReply(w, string(raw), "stop")
	}
	f.work(t)
	view, err := f.start.Learning.Session(ctx, e.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	feedback, err := f.learn.Feedback(ctx, view.WorkItem.Attempt.ID, false)
	if err != nil || len(feedback.Evidence) != 1 || feedback.Goal.ID != d.GoalRevisionID {
		t.Fatal("old A assessment not accepted under A", err)
	}
}

func TestPostgreSQLExerciseProvisionalAndAllowedHelp(t *testing.T) {
	f := newExerciseFixture(t)
	ctx := context.Background()
	f.request.Question.Question.AllowedHelp = []string{"hint", "answer_revealed"}
	e := f.publish(t)
	f.present(t, e)
	if _, err := f.start.Content.PublishHelp(ctx, f.actor, e.ID, learningcontent.ExerciseHelp{OperationID: uuid.NewString(), Level: learning.HelpScaffold, Text: "不允许的帮助"}); err == nil {
		t.Fatal("forbidden help defaulted to none")
	}
	if _, err := f.learn.ApplyAction(learning.WithAssessmentActor(ctx, f.actor), f.actor.Device.ID, e.SessionID, f.command(t, e, tutoring.ActionSubmitAttempt, learning.HelpNone)); err == nil {
		t.Fatal("none silently admitted")
	}
	if _, err := f.start.Content.PublishHelp(ctx, f.actor, e.ID, learningcontent.ExerciseHelp{OperationID: uuid.NewString(), Level: learning.HelpAnswerRevealed, Text: "完整答案"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.learn.ApplyAction(learning.WithAssessmentActor(ctx, f.actor), f.actor.Device.ID, e.SessionID, f.command(t, e, tutoring.ActionSubmitAttempt, learning.HelpAnswerRevealed)); err != nil {
		t.Fatal(err)
	}
	assessmentReply(t, f, e, 1000)
	f.work(t)
	view, err := f.start.Learning.Session(ctx, e.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	feedback, err := f.learn.Feedback(ctx, view.WorkItem.Attempt.ID, false)
	if err != nil || feedback.Assessment == nil || len(feedback.Evidence) != 0 || len(feedback.Decisions) != 1 || feedback.Decisions[0].Disposition != learning.DispositionProvisional {
		t.Fatal("revealed answer created accepted evidence", err)
	}
}

func TestPostgreSQLExerciseTaskFailureRollsBackAttempt(t *testing.T) {
	f := newExerciseFixture(t)
	ctx := context.Background()
	e := f.publish(t)
	f.present(t, e)
	if _, err := f.pool.Exec(ctx, `CREATE FUNCTION fail_exercise_task() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected task registration failure'; END $$; CREATE TRIGGER fail_exercise_task BEFORE INSERT ON learning_assessment_tasks FOR EACH ROW EXECUTE FUNCTION fail_exercise_task()`); err != nil {
		t.Fatal(err)
	}
	cmd := f.command(t, e, tutoring.ActionSubmitAttempt, learning.HelpNone)
	answerCtx := f.start.Content.WithAnswer(ctx, f.actor, e.ContentArtifactID, 1)
	if _, err := f.learn.ApplyAction(answerCtx, f.actor.Device.ID, e.SessionID, cmd); err == nil {
		t.Fatal("injected task failure ignored")
	}
	var attempts int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM learning_attempts WHERE activity_id=$1`, e.ActivityID).Scan(&attempts); err != nil || attempts != 0 {
		t.Fatal("attempt escaped registration transaction", attempts, err)
	}
	if _, err := f.pool.Exec(ctx, `DROP TRIGGER fail_exercise_task ON learning_assessment_tasks`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.learn.ApplyAction(answerCtx, f.actor.Device.ID, e.SessionID, cmd); err != nil {
		t.Fatal("failed transaction archived success", err)
	}
}

func TestPostgreSQLExerciseHelpCompetesWithSubmission(t *testing.T) {
	for _, helpFirst := range []bool{true, false} {
		t.Run(map[bool]string{true: "help_first", false: "answer_first"}[helpFirst], func(t *testing.T) {
			f := newExerciseFixture(t)
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			e := f.publish(t)
			f.present(t, e)
			cmd := f.command(t, e, tutoring.ActionSubmitAttempt, learning.HelpNone)
			help := learningcontent.ExerciseHelp{OperationID: uuid.NewString(), Level: learning.HelpScaffold, Text: "先检查整除。"}
			// Hold the shared lock, queue both real operations in a deterministic order,
			// and observe lock waiters instead of relying on scheduling or sleeps.
			lock, err := f.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer lock.Rollback(context.Background())
			if _, err = lock.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "learning-aggregate:session:"+e.SessionID); err != nil {
				t.Fatal(err)
			}
			first, second := make(chan error, 1), make(chan error, 1)
			answer := func(ch chan error) {
				_, err := f.learn.ApplyAction(learning.WithAssessmentActor(ctx, f.actor), f.actor.Device.ID, e.SessionID, cmd)
				ch <- err
			}
			hint := func(ch chan error) { _, err := f.start.Content.PublishHelp(ctx, f.actor, e.ID, help); ch <- err }
			if helpFirst {
				go hint(first)
			} else {
				go answer(first)
			}
			waitForExerciseLock(t, ctx, f, 1)
			if helpFirst {
				go answer(second)
			} else {
				go hint(second)
			}
			waitForExerciseLock(t, ctx, f, 2)
			if err = lock.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			if err = <-first; err != nil {
				t.Fatal("first operation lost", err)
			}
			if err = <-second; err == nil {
				t.Fatal("second operation ignored shared lock/floor")
			}
			if helpFirst {
				if _, err = f.learn.ApplyAction(learning.WithAssessmentActor(ctx, f.actor), f.actor.Device.ID, e.SessionID, f.command(t, e, tutoring.ActionSubmitAttempt, learning.HelpScaffold)); err != nil {
					t.Fatal(err)
				}
			}
			view, err := f.start.Learning.Session(ctx, e.SessionID)
			if err != nil {
				t.Fatal(err)
			}
			want := learning.HelpNone
			if helpFirst {
				want = learning.HelpScaffold
			}
			if view.WorkItem.Attempt.Help != want {
				t.Fatal("concurrency changed submitted help", view.WorkItem.Attempt.Help)
			}
		})
	}
}

func waitForExerciseLock(t *testing.T, ctx context.Context, f *exerciseFixture, want int) {
	t.Helper()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		var n int
		if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND wait_event='advisory' AND query LIKE '%pg_advisory_xact_lock%'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n >= want {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal("operations did not compete for aggregate lock", ctx.Err())
		case <-ticker.C:
		}
	}
}

func TestPostgreSQLExercisePublicationRollbackAndHelpState(t *testing.T) {
	f := newExerciseFixture(t)
	ctx := context.Background()
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	e, err := f.start.PublishExerciseTx(ctx, tx, f.actor, f.request)
	if err != nil {
		t.Fatal(err)
	}
	if err = tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	var count int
	if err = f.pool.QueryRow(ctx, `SELECT count(*) FROM tutoring_sessions WHERE id=$1`, e.SessionID).Scan(&count); err != nil || count != 0 {
		t.Fatal("publication escaped outer transaction", err)
	}
	e = f.publish(t)
	if e.Help.Version != 0 || e.Help.Level != "" {
		t.Fatal("no disclosure defaulted actual help")
	}
	h := learningcontent.ExerciseHelp{OperationID: uuid.NewString(), Level: learning.HelpScaffold, Text: "已披露帮助"}
	if _, err = f.start.Content.PublishHelp(ctx, f.actor, e.ID, h); err != nil {
		t.Fatal(err)
	}
	if _, err = f.start.Content.PublishHelp(ctx, f.actor, e.ID, h); err != nil {
		t.Fatal(err)
	}
	tx, err = f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	read, err := f.start.Content.ExerciseTx(ctx, tx, f.actor, e.ID)
	if err != nil || read.Help.Version != 1 || read.Help.Level != learning.HelpScaffold {
		t.Fatal("help version/floor lost", read.Help, err)
	}
	got, err := f.start.Content.HelpOperationTx(ctx, tx, f.actor, e.ID, h.OperationID)
	if err != nil || got != h {
		t.Fatal("lost help response cannot be read back", err)
	}
}

func TestPostgreSQLExerciseRecoveryChecksAssessmentOperation(t *testing.T) {
	f := newExerciseFixture(t)
	ctx := context.Background()
	e, attempt := f.submitted(t)
	if err := f.service.enqueueAssessment(ctx); err != nil {
		t.Fatal(err)
	}
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	task, err := f.start.Learning.AssessmentTaskTx(ctx, tx, attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	cmd := f.command(t, e, tutoring.ActionEndActivity, "")
	cmd.Operation.OperationID = task.OperationID
	if _, err = f.learn.ApplyAction(ctx, f.actor.Device.ID, e.SessionID, cmd); err != nil {
		t.Fatal(err)
	}
	f.work(t)
	state, err := f.service.AssessmentStatus(ctx, f.actor, attempt.ID)
	if err != nil || state.Run == nil || state.Run.Status != "failed" || f.calls.Load() != 0 {
		t.Fatalf("unrelated operation treated as recorded assessment: %+v %v", state, err)
	}
}

func TestPostgreSQLExerciseSessionActionRechecksSubmitter(t *testing.T) {
	f := newExerciseFixture(t)
	ctx := context.Background()
	e := f.publish(t)
	f.present(t, e)
	if _, err := f.pool.Exec(ctx, `UPDATE device_tokens SET scopes=array_remove(scopes,'learning:write') WHERE id=$1`, f.actor.TokenID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.learn.ApplyAction(learning.WithAssessmentActor(ctx, f.actor), f.actor.Device.ID, e.SessionID, f.command(t, e, tutoring.ActionSubmitAttempt, learning.HelpNone)); err == nil {
		t.Fatal("mapped legacy action accepted read-only token")
	}
}

func TestPostgreSQLExerciseKnownRejectionIsNotUnknown(t *testing.T) {
	f := newExerciseFixture(t)
	ctx := context.Background()
	e, attempt := f.submitted(t)
	f.modelOverride = func(w http.ResponseWriter, r *http.Request) { studioReply(w, `{"invalid":"assessment"}`, "stop") }
	f.work(t)
	state, err := f.service.AssessmentStatus(ctx, f.actor, attempt.ID)
	if err != nil || state.Run == nil || state.Run.Status != "failed" || state.Run.ResultUnknown || state.Run.RequestsUsed != 2 {
		t.Fatalf("known proposal failure mislabeled: %+v %v", state.Run, err)
	}
	policy, err := f.service.AssessmentPolicy()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.service.ResumeAssessment(ctx, f.actor, attempt.ID, AssessmentResume{OperationID: uuid.NewString(), ExpectedVersion: state.Run.Version, Policy: policy}); err != nil {
		t.Fatal("known rejected output cannot be retried", err)
	}
	assessmentReply(t, f, e, 1000)
	f.work(t)
	feedback, err := f.learn.Feedback(ctx, attempt.ID, false)
	if err != nil || feedback.Assessment == nil || len(feedback.Evidence) != 1 || f.calls.Load() != 3 {
		t.Fatal("retry did not assess the original answer once", err)
	}
}
