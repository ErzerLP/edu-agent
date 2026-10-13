package mentorrun

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/knowledge"
	"github.com/edu-agent/edu-agent/server/internal/learning"
	"github.com/edu-agent/edu-agent/server/internal/learningcontent"
	"github.com/edu-agent/edu-agent/server/internal/learningspace"
	spacepg "github.com/edu-agent/edu-agent/server/internal/learningspace/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/settings"
	"github.com/edu-agent/edu-agent/server/internal/tutoring"
	"github.com/google/uuid"
)

const studyExerciseCandidate = `{"answer":"试着用自己的话解释。[1]","citations":[{"source":1,"fragment":1}],"exercise":{"prompt":"偶数有什么特点？","rubric":[{"id":"divisible","criterion":"说明偶数能被 2 整除"}],"allowed_help":["none","hint","scaffold","answer_revealed"],"citations":[{"source":1,"fragment":1}]}}`

func TestStudyExerciseSchemaStrictRequired(t *testing.T) {
	var schema struct {
		Properties map[string]json.RawMessage `json:"properties"`
		Required   []string                   `json:"required"`
	}
	if err := json.Unmarshal(studyExerciseSchema(), &schema); err != nil {
		t.Fatal(err)
	}
	for name := range schema.Properties {
		found := false
		for _, required := range schema.Required {
			if name == required {
				found = true
			}
		}
		if !found {
			t.Fatalf("strict Responses schema has an optional property: %s", name)
		}
	}
}

func TestPostgreSQLStudyExerciseAssessmentResumeProjection(t *testing.T) {
	for _, scenario := range []string{"resume", "expired"} {
		t.Run(scenario, func(t *testing.T) {
			f := newExerciseFixture(t)
			ctx := context.Background()
			f.modelOverride = func(w http.ResponseWriter, r *http.Request) { studioReply(w, studyExerciseCandidate, "stop") }
			id, selection := studyExerciseConversation(t, f)
			studyExerciseTurn(t, f, id, selection)
			card := readStudyExercise(t, f, historyPage(t, f.runtimeFixture, id, 0, 10).Items[0].ExerciseID)
			e := learningcontent.Exercise{ID: card.ID, SessionID: card.SessionID, ActivityID: card.ActivityID, ContentArtifactID: card.ArtifactID, ContentVersion: card.ContentVersion}
			if _, err := f.learn.ApplyAction(f.start.Content.WithAnswer(ctx, f.actor, e.ContentArtifactID, e.ContentVersion), f.actor.Device.ID, e.SessionID, f.command(t, e, tutoring.ActionSubmitAttempt, learning.HelpNone)); err != nil {
				t.Fatal(err)
			}
			limits := f.settings.View().Limits
			limits.ResearchRequests = 1
			if _, err := f.settings.Update(settings.Update{ExpectedRevision: f.settings.View().Revision, Target: settings.Mentor, Limits: &limits}); err != nil {
				t.Fatal(err)
			}
			f.work(t)
			card = readStudyExercise(t, f, card.ID)
			if card.Assessment == nil || !card.Assessment.CanResume || card.Assessment.Reason != "context_changed" || f.calls.Load() != 1 {
				t.Fatal("changed destination was not paused")
			}
			state, err := f.service.AssessmentStatus(ctx, f.actor, card.AttemptID)
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "expired" {
				if _, err = f.pool.Exec(ctx, `UPDATE learning_mentor_runs SET expires_at=clock_timestamp()-interval '1 hour',state=jsonb_set(state,'{expires_at}',to_jsonb(clock_timestamp()-interval '1 hour')) WHERE id=$1`, state.Task.RunID); err != nil {
					t.Fatal(err)
				}
				if readStudyExercise(t, f, card.ID).Assessment.CanResume {
					t.Fatal("expired assessment advertised as resumable")
				}
				return
			}
			c := StudyAssessmentResume{OperationID: uuid.NewString(), ExpectedVersion: card.Assessment.Version, Configuration: card.Assessment.Configuration, RequestBudget: 1, TokenBudget: 50000}
			first, err := f.service.ResumeStudyAssessment(ctx, f.actor, card.ID, c)
			if err != nil {
				t.Fatal(err)
			}
			if replay, err := f.service.ResumeStudyAssessment(ctx, f.actor, card.ID, c); err != nil || replay != first {
				t.Fatal("resume replay lost task identity", err)
			}
			if recovered, err := f.service.Operation(ctx, f.actor, learningspace.DefaultID, c.OperationID); err != nil || recovered != first {
				t.Fatal("resume receipt inaccessible", err)
			}
			raw := bytes.ReplaceAll(assessmentJSON(t, f, e, 1000), []byte(`"definition"`), []byte(`"divisible"`))
			f.modelOverride = func(w http.ResponseWriter, r *http.Request) { studioReply(w, string(raw), "stop") }
			f.work(t)
			card = readStudyExercise(t, f, card.ID)
			if card.Feedback == nil || len(card.Feedback.Evidence) != 1 || f.calls.Load() != 2 {
				t.Fatal("resumed assessment did not settle original answer")
			}
		})
	}
}

func studyExerciseConversation(t *testing.T, f *exerciseFixture) (string, knowledge.SourceSelection) {
	t.Helper()
	ctx := context.Background()
	direction := "理解偶数"
	_, err := spacepg.New(f.pool, f.service.directions).Mutate(ctx, f.actor.Device.ID, learningspace.DefaultID, learningspace.Command{OperationID: uuid.NewString(), ExpectedVersion: 1, Name: "数学", Direction: &direction, Status: "active"})
	if err != nil {
		t.Fatal(err)
	}
	id, err := f.service.NewConversation(ctx, f.actor, learningspace.DefaultID, NewConversation{ID: uuid.NewString(), Mode: "study"})
	if err != nil {
		t.Fatal(err)
	}
	entry := f.request.Question.Manifest.Entries[0]
	return id, knowledge.SourceSelection{Mode: "items", Items: []knowledge.SourceIdentity{entry.SourceIdentity}}
}

func readStudyExercise(t *testing.T, f *exerciseFixture, id string) StudyExerciseView {
	t.Helper()
	var view StudyExerciseView
	if err := f.service.ReadStudyExercise(context.Background(), f.actor, id, func(v StudyExerciseView) error { view = v; return nil }); err != nil {
		t.Fatal(err)
	}
	return view
}

// Defer is presentation state, while the answer, help floor, assessment and
// evidence must remain the same learning facts after response loss/restart.
func TestPostgreSQLStudyExerciseDeferSubmitFeedback(t *testing.T) {
	f := newExerciseFixture(t)
	f.modelOverride = func(w http.ResponseWriter, r *http.Request) { studioReply(w, studyExerciseCandidate, "stop") }
	id, selection := studyExerciseConversation(t, f)
	studyExerciseTurn(t, f, id, selection)
	card := readStudyExercise(t, f, historyPage(t, f.runtimeFixture, id, 0, 50).Items[0].ExerciseID)
	deferCommand := StudyExerciseCommand{OperationID: uuid.NewString(), Kind: "defer"}
	for range 2 {
		if _, err := f.service.ChangeStudyExercise(context.Background(), f.actor, card.ID, deferCommand); err != nil {
			t.Fatal(err)
		}
	}
	if !readStudyExercise(t, f, card.ID).Deferred {
		t.Fatal("deferred card did not recover")
	}
	var attempts int
	if err := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM learning_attempts`).Scan(&attempts); err != nil || attempts != 0 {
		t.Fatal("defer recorded an answer", err)
	}
	if _, err := f.service.ChangeStudyExercise(context.Background(), f.actor, card.ID, StudyExerciseCommand{OperationID: uuid.NewString(), Kind: "resume"}); err != nil {
		t.Fatal(err)
	}
	help := StudyHelpRequest{OperationID: uuid.NewString(), SourceSelection: selection}
	h, err := f.service.PublishStudyHelp(context.Background(), f.actor, card.ID, help)
	if err != nil || h.Level != learning.HelpHint {
		t.Fatal("hint not disclosed", err)
	}
	if replay, err := f.service.StudyHelpOperation(context.Background(), f.actor, card.ID, help.OperationID); err != nil || replay != h {
		t.Fatal("lost help response", err)
	}
	card = readStudyExercise(t, f, card.ID)
	if card.HelpFloor != learning.HelpHint {
		t.Fatal("hint floor missing")
	}
	e := learningcontent.Exercise{ID: card.ID, SessionID: card.SessionID, ActivityID: card.ActivityID, ContentArtifactID: card.ArtifactID, ContentVersion: card.ContentVersion}
	command := f.command(t, e, tutoring.ActionSubmitAttempt, learning.HelpNone)
	ctx := f.start.Content.WithAnswer(context.Background(), f.actor, e.ContentArtifactID, e.ContentVersion)
	if _, err = f.learn.ApplyAction(ctx, f.actor.Device.ID, e.SessionID, command); err == nil {
		t.Fatal("help floor bypassed")
	}
	command = f.command(t, e, tutoring.ActionSubmitAttempt, learning.HelpHint)
	if _, err = f.learn.ApplyAction(ctx, f.actor.Device.ID, e.SessionID, command); err != nil {
		t.Fatal(err)
	}
	if result, err := f.learn.ApplyAction(ctx, f.actor.Device.ID, e.SessionID, command); err != nil || !result.Replayed {
		t.Fatal("duplicate submission", err)
	}
	// The fixture quotes the actual old activity/answer, including rubric identity.
	raw := assessmentJSON(t, f, e, 1000)
	var value map[string]any
	_ = json.Unmarshal(raw, &value)
	value["assessment"].(map[string]any)["items"].([]any)[0].(map[string]any)["rubric_item_id"] = "divisible"
	raw, _ = json.Marshal(value)
	f.modelOverride = func(w http.ResponseWriter, r *http.Request) { studioReply(w, string(raw), "stop") }
	f.work(t)
	card = readStudyExercise(t, f, card.ID)
	if card.Feedback == nil || len(card.Feedback.Evidence) != 1 || card.Assessment == nil || card.Assessment.Status != "succeeded" {
		t.Fatalf("feedback projection missing: %+v", card)
	}
	ref := card.Feedback.Assessment.Items[0].KnowledgeReferenceID
	if err = f.service.ReadStudyExerciseCitation(context.Background(), f.actor, card.ID, ref, func(c SourceCitation) error {
		if !strings.Contains(c.Fragment.Text, "SELECTED_EVEN_SOURCE") {
			t.Fatal("citation moved to other material")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err = f.learn.ApplyAction(context.Background(), f.actor.Device.ID, e.SessionID, f.command(t, e, tutoring.ActionAcknowledgeFeedback, "")); err != nil {
		t.Fatal(err)
	}
	completed := readStudyExercise(t, f, e.ID)
	if completed.Feedback == nil || completed.AttemptID != card.AttemptID {
		t.Fatal("acknowledging feedback removed the conversation result")
	}
}

func TestPostgreSQLStudyExerciseAtomicFailureAndZeroSources(t *testing.T) {
	for _, scenario := range []string{"mapping_failure", "no_sources"} {
		t.Run(scenario, func(t *testing.T) {
			f := newExerciseFixture(t)
			f.modelOverride = func(w http.ResponseWriter, r *http.Request) { studioReply(w, studyExerciseCandidate, "stop") }
			id, selection := studyExerciseConversation(t, f)
			if scenario == "no_sources" {
				selection = knowledge.SourceSelection{Mode: "none", Items: []knowledge.SourceIdentity{}}
			} else {
				if _, err := f.pool.Exec(context.Background(), `CREATE FUNCTION fail_card_mapping() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.exercise_id IS NOT NULL THEN RAISE EXCEPTION 'mapping fault'; END IF; RETURN NEW; END $$; CREATE TRIGGER fail_card_mapping BEFORE UPDATE ON learning_tutor_turns FOR EACH ROW EXECUTE FUNCTION fail_card_mapping()`); err != nil {
					t.Fatal(err)
				}
			}
			studyExerciseTurn(t, f, id, selection)
			var count int
			if err := f.pool.QueryRow(context.Background(), `SELECT (SELECT count(*) FROM learning_exercises)+(SELECT count(*) FROM learning_activities)+(SELECT count(*) FROM tutoring_sessions)+(SELECT count(*) FROM learning_content_artifacts)`).Scan(&count); err != nil || count != 0 {
				t.Fatal("orphan question after failure/empty selection", count, err)
			}
			page := historyPage(t, f.runtimeFixture, id, 0, 10)
			if page.Items[0].ExerciseID != "" {
				t.Fatal("orphan conversation mapping")
			}
			if scenario == "no_sources" && (f.calls.Load() != 0 || page.Items[0].Output != "先添加或选择来源，再试一道题。") {
				t.Fatal("zero-source request generated a question")
			}
		})
	}
}

func TestPostgreSQLStudyExerciseOldContextAndRestart(t *testing.T) {
	f := newExerciseFixture(t)
	ctx := context.Background()
	f.modelOverride = func(w http.ResponseWriter, r *http.Request) { studioReply(w, studyExerciseCandidate, "stop") }
	id, selection := studyExerciseConversation(t, f)
	studyExerciseTurn(t, f, id, selection)
	card := readStudyExercise(t, f, historyPage(t, f.runtimeFixture, id, 0, 10).Items[0].ExerciseID)
	b := selectedDocument(t, f.runtimeFixture, "SOURCE_B_NEVER_ASSESS")
	direction := "DIRECTION_B_NEVER_ASSESS"
	if _, err := spacepg.New(f.pool, f.service.directions).Mutate(ctx, f.actor.Device.ID, learningspace.DefaultID, learningspace.Command{OperationID: uuid.NewString(), ExpectedVersion: 2, Name: "数学", Direction: &direction, Status: "active"}); err != nil {
		t.Fatal(err)
	}
	f.modelOverride = func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		if bytes.Contains(raw, []byte("SELECTED_EVEN_SOURCE")) || bytes.Contains(raw, []byte("divisible")) {
			t.Error("old question leaked into new context")
		}
		studioReply(w, `{"answer":"新资料 [1]","citations":[{"source":1,"fragment":1}],"exercise":null}`, "stop")
	}
	page := historyPage(t, f.runtimeFixture, id, 0, 10)
	newSelection := knowledge.SourceSelection{Mode: "items", Items: []knowledge.SourceIdentity{b}}
	req := SubmitTurn{OperationID: uuid.NewString(), ExpectedVersion: page.Conversation.Version, DirectionVersion: 2, SourceSelection: &newSelection, Prompt: "按新方向讲解", RequestBudget: 2, TokenBudget: 50000}
	r, err := f.service.Submit(ctx, f.actor, learningspace.DefaultID, id, req)
	if err != nil {
		t.Fatal(err)
	}
	f.work(t)
	if replay, err := f.service.Submit(ctx, f.actor, learningspace.DefaultID, id, req); err != nil || replay.RunID != r.RunID {
		t.Fatal("lost generation response was not replayed", err)
	}
	if _, err = f.service.PublishStudyHelp(ctx, f.actor, card.ID, StudyHelpRequest{OperationID: uuid.NewString(), SourceSelection: newSelection}); err == nil {
		t.Fatal("new hint silently used old selection")
	}
	e := learningcontent.Exercise{ID: card.ID, SessionID: card.SessionID, ActivityID: card.ActivityID, ContentArtifactID: card.ArtifactID, ContentVersion: card.ContentVersion}
	if _, err = f.learn.ApplyAction(f.start.Content.WithAnswer(ctx, f.actor, e.ContentArtifactID, e.ContentVersion), f.actor.Device.ID, e.SessionID, f.command(t, e, tutoring.ActionSubmitAttempt, learning.HelpNone)); err != nil {
		t.Fatal(err)
	}
	raw := bytes.ReplaceAll(assessmentJSON(t, f, e, 1000), []byte(`"definition"`), []byte(`"divisible"`))
	f.modelOverride = func(w http.ResponseWriter, r *http.Request) {
		input, _ := io.ReadAll(r.Body)
		if bytes.Contains(input, []byte("SOURCE_B_NEVER_ASSESS")) || bytes.Contains(input, []byte(direction)) || !bytes.Contains(input, []byte("SELECTED_EVEN_SOURCE")) {
			t.Error("old question assessment changed material")
		}
		studioReply(w, string(raw), "stop")
	}
	f.service, err = New(f.pool, f.settings, bytes.Repeat([]byte{42}, 32))
	if err != nil {
		t.Fatal(err)
	}
	f.service.ConfigureStart(f.start)
	f.work(t)
	card = readStudyExercise(t, f, card.ID)
	if card.Feedback == nil || card.Feedback.Goal.Text != "理解偶数" || len(card.Feedback.Evidence) != 1 {
		t.Fatal("old direction/result lost after restart")
	}
	due := time.Now().Add(90 * 24 * time.Hour)
	reviews, err := f.start.Learning.Reviews(ctx, learning.ReviewQuery{Status: "all", DueBefore: &due, Page: learning.CursorPageRequest{Limit: 10}})
	if err != nil || len(reviews.Items) != 1 || reviews.Items[0].AttemptID != card.AttemptID {
		t.Fatal("review does not share original answer", err)
	}
	if _, err = f.pool.Exec(ctx, `UPDATE learning_mentor_runs SET expires_at=clock_timestamp()-interval '1 hour',state=jsonb_set(state,'{expires_at}',to_jsonb(clock_timestamp()-interval '1 hour')) WHERE id=$1`, card.RunID); err != nil {
		t.Fatal(err)
	}
	if historyPage(t, f.runtimeFixture, id, 0, 10).Items[0].ExerciseID != card.ID {
		t.Fatal("expired run lost saved card")
	}
}

func TestPostgreSQLStudyExerciseResumeConflictsAndRevocation(t *testing.T) {
	f := newExerciseFixture(t)
	ctx := context.Background()
	f.modelOverride = func(w http.ResponseWriter, r *http.Request) { studioReply(w, studyExerciseCandidate, "stop") }
	id, selection := studyExerciseConversation(t, f)
	studyExerciseTurn(t, f, id, selection)
	first := historyPage(t, f.runtimeFixture, id, 0, 10).Items[0].ExerciseID
	if _, err := f.service.ChangeStudyExercise(ctx, f.actor, first, StudyExerciseCommand{OperationID: uuid.NewString(), Kind: "defer"}); err != nil {
		t.Fatal(err)
	}
	studyExerciseTurn(t, f, id, selection)
	if _, err := f.service.ChangeStudyExercise(ctx, f.actor, first, StudyExerciseCommand{OperationID: uuid.NewString(), Kind: "resume"}); err == nil {
		t.Fatal("two active cards resumed")
	}
	if _, err := f.pool.Exec(ctx, `DELETE FROM knowledge_collection_links WHERE space_id=$1`, learningspace.DefaultID); err != nil {
		t.Fatal(err)
	}
	if err := f.service.ReadStudyExercise(ctx, f.actor, first, func(StudyExerciseView) error { t.Error("revoked card sent"); return nil }); err == nil {
		t.Fatal("revoked card readable")
	}
	if _, err := f.service.PublishStudyHelp(ctx, f.actor, first, StudyHelpRequest{OperationID: uuid.NewString(), SourceSelection: selection}); err == nil {
		t.Fatal("revoked help generated")
	}
	for _, turn := range historyPage(t, f.runtimeFixture, id, 0, 10).Items {
		if turn.BodyAvailable || turn.ExerciseID != "" {
			t.Fatal("revoked card revived in history")
		}
	}
}

func studyExerciseTurn(t *testing.T, f *exerciseFixture, conversation string, selection knowledge.SourceSelection) Receipt {
	t.Helper()
	c := historyPage(t, f.runtimeFixture, conversation, 0, 50).Conversation
	r, err := f.service.Submit(context.Background(), f.actor, learningspace.DefaultID, conversation, SubmitTurn{OperationID: uuid.NewString(), ExpectedVersion: c.Version, DirectionVersion: 1, SourceSelection: &selection, Prompt: "出一道题", RequestBudget: 2, TokenBudget: 50000})
	if err != nil {
		t.Fatal(err)
	}
	f.work(t)
	return r
}

// Without atomic candidate publication the conversation has no answerable card.
func TestPostgreSQLStudyExercisePublishesOneCard(t *testing.T) {
	f := newExerciseFixture(t)
	f.modelOverride = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"finish_reason": "stop", "message": map[string]string{"content": studyExerciseCandidate}}}})
	}
	id, selection := studyExerciseConversation(t, f)
	r := studyExerciseTurn(t, f, id, selection)
	page := historyPage(t, f.runtimeFixture, id, 0, 50)
	raw, _ := json.Marshal(page.Items[0])
	var turn map[string]any
	_ = json.Unmarshal(raw, &turn)
	if turn["exercise_id"] == nil {
		t.Fatalf("published answer has no exercise mapping: status=%s turn=%s", f.snapshot(t, r.RunID).Status, raw)
	}
	var count int
	if err := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM learning_exercises`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("exercise count=%d err=%v", count, err)
	}
	studyExerciseTurn(t, f, id, selection)
	if err := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM learning_exercises`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("second unanswered card published: %d %v", count, err)
	}
}
