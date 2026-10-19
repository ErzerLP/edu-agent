package mentorrun

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/edu-agent/edu-agent/server/internal/learning"
	"github.com/edu-agent/edu-agent/server/internal/learningcontent"
	"github.com/edu-agent/edu-agent/server/internal/tutoring"
	"github.com/google/uuid"
)

func wrongStudyExercise(t *testing.T) (*exerciseFixture, learningcontent.Exercise) {
	t.Helper()
	f := newExerciseFixture(t)
	f.modelOverride = func(w http.ResponseWriter, _ *http.Request) { studioReply(w, studyExerciseCandidate, "stop") }
	conversation, selection := studyExerciseConversation(t, f)
	studyExerciseTurn(t, f, conversation, selection)
	card := readStudyExercise(t, f, historyPage(t, f.runtimeFixture, conversation, 0, 10).Items[0].ExerciseID)
	e := learningcontent.Exercise{ID: card.ID, SessionID: card.SessionID, ActivityID: card.ActivityID, ContentArtifactID: card.ArtifactID, ContentVersion: card.ContentVersion}
	command := f.command(t, e, tutoring.ActionSubmitAttempt, learning.HelpNone)
	command.Answer = "偶数不能被 2 整除。"
	ctx := f.start.Content.WithAnswer(context.Background(), f.actor, e.ContentArtifactID, e.ContentVersion)
	if _, err := f.learn.ApplyAction(ctx, f.actor.Device.ID, e.SessionID, command); err != nil {
		t.Fatal(err)
	}
	var output map[string]any
	if err := json.Unmarshal(assessmentJSON(t, f, e, 1000), &output); err != nil {
		t.Fatal(err)
	}
	item := output["assessment"].(map[string]any)["items"].([]any)[0].(map[string]any)
	item["rubric_item_id"] = "divisible"
	item["conclusion"] = "fail"
	item["misconception_candidate"] = "认为偶数不能被 2 整除"
	raw, err := json.Marshal(output)
	if err != nil {
		t.Fatal(err)
	}
	f.modelOverride = func(w http.ResponseWriter, _ *http.Request) { studioReply(w, string(raw), "stop") }
	return f, e
}

func assertWrongStudyEvidence(t *testing.T, f *exerciseFixture, e learningcontent.Exercise) {
	t.Helper()
	card := readStudyExercise(t, f, e.ID)
	if card.Assessment == nil || card.Assessment.Status != "succeeded" || card.Assessment.ResultUnknown || card.Assessment.CanResume {
		t.Fatalf("wrong answer assessment did not settle: %+v", card.Assessment)
	}
	if card.Feedback == nil || card.Feedback.Assessment == nil || len(card.Feedback.Decisions) != 1 || len(card.Feedback.Evidence) != 1 {
		t.Fatal("wrong answer did not produce one formal assessment and evidence")
	}
	if card.Feedback.Decisions[0].Disposition != learning.DispositionAccepted || card.Feedback.Decisions[0].Items[0].Conclusion != learning.ConclusionFail || card.Feedback.Evidence[0].Outcome != learning.OutcomeFail {
		t.Fatal("wrong answer was not recorded as a failure")
	}
	var sources, counters int
	var status string
	err := f.pool.QueryRow(context.Background(), `SELECT cardinality(source_evidence_ids),cardinality(counter_evidence_ids),status FROM learning_misconception_revisions WHERE caused_by_evidence_id=$1`, card.Feedback.Evidence[0].ID).Scan(&sources, &counters, &status)
	if err != nil || sources != 1 || counters != 0 || status != "proposed" {
		t.Fatalf("first misconception must have one source and an empty counter array: %d/%d/%s %v", sources, counters, status, err)
	}
}

// A first wrong answer has no counter-evidence. A nil Go slice must not turn
// into SQL NULL and roll back the assessment, evidence and misconception.
func TestPostgreSQLStudyExerciseWrongAnswerCreatesEvidence(t *testing.T) {
	f, e := wrongStudyExercise(t)
	f.work(t)
	f.work(t)
	assertWrongStudyEvidence(t, f, e)
	if f.calls.Load() != 2 {
		t.Fatal("assessment called the model again", f.calls.Load())
	}
}

// A durable ready proposal proves the response is known even when an older
// worker left call_started/result_unknown set after a failed database commit.
func TestPostgreSQLStudyExerciseAssessmentCommitFailureRecovery(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		name := "settled_failure"
		if legacy {
			name = "legacy_unknown"
		}
		t.Run(name, func(t *testing.T) {
			f, e := wrongStudyExercise(t)
			ctx := context.Background()
			if _, err := f.pool.Exec(ctx, `CREATE FUNCTION reject_assessment_commit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'assessment storage unavailable'; END $$;
CREATE TRIGGER reject_assessment_commit BEFORE INSERT ON learning_assessments FOR EACH ROW EXECUTE FUNCTION reject_assessment_commit()`); err != nil {
				t.Fatal(err)
			}
			f.work(t)
			card := readStudyExercise(t, f, e.ID)
			if card.Feedback == nil || card.Feedback.Assessment != nil || len(card.Feedback.Evidence) != 0 {
				t.Fatal("failed commit published learning facts or lost the answer")
			}
			state, err := f.service.AssessmentStatus(ctx, f.actor, card.AttemptID)
			if err != nil || state.Run == nil {
				t.Fatal("assessment run missing", err)
			}
			if legacy {
				if _, err = f.pool.Exec(ctx, `UPDATE learning_mentor_runs SET call_started=true,state=state||'{"reason":"model_failed","result_unknown":true}'::jsonb WHERE id=$1`, state.Task.RunID); err != nil {
					t.Fatal(err)
				}
			} else if state.Run.Reason != "assessment_commit_failed" || state.Run.ResultUnknown {
				t.Fatalf("durable proposal mislabeled as a model failure: %+v", state.Run)
			}
			card = readStudyExercise(t, f, e.ID)
			if card.Assessment.Status != "failed" || card.Assessment.ResultUnknown || !card.Assessment.CanResume {
				t.Fatalf("ready assessment cannot be recovered: %+v", card.Assessment)
			}
			if _, err = f.pool.Exec(ctx, `DROP TRIGGER reject_assessment_commit ON learning_assessments`); err != nil {
				t.Fatal(err)
			}
			command := StudyAssessmentResume{OperationID: uuid.NewString(), ExpectedVersion: card.Assessment.Version, Configuration: card.Assessment.Configuration}
			first, err := f.service.ResumeStudyAssessment(ctx, f.actor, e.ID, command)
			if err != nil {
				t.Fatal(err)
			}
			if replay, err := f.service.ResumeStudyAssessment(ctx, f.actor, e.ID, command); err != nil || replay != first {
				t.Fatal("resume replay changed the run", err)
			}
			f.work(t)
			f.work(t)
			assertWrongStudyEvidence(t, f, e)
			if f.calls.Load() != 2 {
				t.Fatal("recovery repeated the model call", f.calls.Load())
			}
		})
	}
}
