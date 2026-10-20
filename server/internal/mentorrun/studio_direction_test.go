package mentorrun

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/edu-agent/edu-agent/server/internal/knowledge"
	"github.com/edu-agent/edu-agent/server/internal/learning"
	"github.com/edu-agent/edu-agent/server/internal/learningcontent"
	"github.com/edu-agent/edu-agent/server/internal/learningspace"
	spacepg "github.com/edu-agent/edu-agent/server/internal/learningspace/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/tutoring"
	"github.com/google/uuid"
)

func directionQuizRequest(t *testing.T, selection knowledge.SourceSelection) StudioGenerate {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"operation_id": uuid.NewString(), "privacy_generation": 1, "kind": "quiz",
		"goal_id": nil, "goal_version": 0, "mode": "direction", "direction_version": 1,
		"artifact_id": nil, "expected_version": 0, "source_selection": selection,
	})
	if err != nil {
		t.Fatal(err)
	}
	var c StudioGenerate
	if err = json.Unmarshal(raw, &c); err != nil {
		t.Fatal(err)
	}
	return c
}

// The new mode must be explicit: a goal-less legacy quiz remains invalid.
func TestStudioDirectionModeValidation(t *testing.T) {
	id := uuid.NewString()
	selection := knowledge.SourceSelection{Mode: "items", Items: []knowledge.SourceIdentity{{CollectionID: id, RevisionID: id, DocumentID: id, DocumentRevisionID: id}}}
	c := directionQuizRequest(t, selection)
	if err := c.Validate(); err != nil {
		t.Fatalf("direction quiz rejected: %v", err)
	}
	for _, change := range []map[string]any{
		{"goal_id": id, "goal_version": 1}, {"goal_version": 1}, {"direction_version": 0},
		{"mode": "unknown"}, {"mode": ""}, {"kind": "guide"}, {"kind": "brief"},
	} {
		raw, _ := json.Marshal(c)
		var fields map[string]any
		_ = json.Unmarshal(raw, &fields)
		for k, v := range change {
			fields[k] = v
		}
		raw, _ = json.Marshal(fields)
		var bad StudioGenerate
		_ = json.Unmarshal(raw, &bad)
		if bad.Validate() == nil {
			t.Fatalf("mixed/invalid mode admitted: %v", change)
		}
	}
}

func TestPostgreSQLStudioDirectionFrozenQuizAndAutomaticFeedback(t *testing.T) {
	f := newExerciseFixture(t)
	f.service.ConfigureStudio(f.start.Content)
	ctx := context.Background()
	direction := "理解偶数"
	spaces := spacepg.New(f.pool, f.service.directions)
	if _, err := spaces.Mutate(ctx, f.actor.Device.ID, learningspace.DefaultID, learningspace.Command{OperationID: uuid.NewString(), ExpectedVersion: 1, Name: "数学", Direction: &direction, Status: "active"}); err != nil {
		t.Fatal(err)
	}
	selection := knowledge.SourceSelection{Mode: "items", Items: []knowledge.SourceIdentity{f.request.Question.Manifest.Entries[0].SourceIdentity}}
	f.modelOverride = func(w http.ResponseWriter, r *http.Request) { studioReply(w, quizCandidate, "stop") }
	c := directionQuizRequest(t, selection)
	receipt, err := f.service.CreateStudio(ctx, f.actor, learningspace.DefaultID, c)
	if err != nil {
		t.Fatal(err)
	}
	// Change direction before the worker runs. Accepted generation uses its snapshot.
	direction = "新方向不用于旧题"
	if _, err = spaces.Mutate(ctx, f.actor.Device.ID, learningspace.DefaultID, learningspace.Command{OperationID: uuid.NewString(), ExpectedVersion: 2, Name: "数学", Direction: &direction, Status: "active"}); err != nil {
		t.Fatal(err)
	}
	f.work(t)
	snapshot := f.snapshot(t, receipt.RunID)
	if snapshot.Studio == nil || snapshot.Studio.Result == nil {
		t.Fatalf("direction generation failed: %s/%s", snapshot.Status, snapshot.Reason)
	}
	if replay, err := f.service.CreateStudio(ctx, f.actor, learningspace.DefaultID, c); err != nil || replay.RunID != receipt.RunID {
		t.Fatal("direction edit broke generation replay", err)
	}
	id := snapshot.Studio.Result.ArtifactID
	r, err := f.start.Content.StudioGet(ctx, f.actor, id, 1)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(r.Body.Quiz)
	var frozen struct {
		Direction struct {
			ID             string
			Version        int64
			GoalRevisionID string `json:"goal_revision_id"`
		}
	}
	if err = json.Unmarshal(raw, &frozen); err != nil || frozen.Direction.ID == "" || frozen.Direction.Version != 1 || frozen.Direction.GoalRevisionID == "" {
		t.Fatalf("missing exact direction snapshot: %s", raw)
	}
	if r.GoalID == nil || *r.GoalID == f.goal {
		t.Fatal("quiz bound to manual goal")
	}
	// Ensure revision 2 before starting revision 1's first question.
	next := c
	raw, _ = json.Marshal(next)
	var fields map[string]any
	_ = json.Unmarshal(raw, &fields)
	fields["direction_version"], fields["operation_id"] = 2, uuid.NewString()
	raw, _ = json.Marshal(fields)
	_ = json.Unmarshal(raw, &next)
	if _, err = f.service.CreateStudio(ctx, f.actor, learningspace.DefaultID, next); err != nil {
		t.Fatal(err)
	}
	f.work(t)
	for question := 1; question <= 2; question++ {
		cmd := learningcontent.StudioQuizStart{OperationID: uuid.NewString(), Version: 1, Generation: 1, GoalVersion: r.Body.Quiz.GoalVersion, Question: question}
		state, err := f.start.StartStudioQuiz(ctx, f.actor, id, cmd)
		if err != nil {
			t.Fatal("old direction question cannot start", err)
		}
		q := state.Questions[question-1]
		if q.ExerciseID == "" {
			t.Fatal("direction carrier lacks exercise mapping")
		}
		card := readStudioExercise(t, f, q.ExerciseID)
		if card.StudioArtifactID != id || card.StudioVersion != 1 || card.Question != question || card.SessionID != q.SessionID {
			t.Fatal("Studio card binding changed")
		}
		hint := StudioHelpRequest{OperationID: uuid.NewString()}
		h, err := f.service.PublishStudioHelp(ctx, f.actor, q.ExerciseID, hint)
		if err != nil || h.Level != learning.HelpHint {
			t.Fatal("Studio hint not delivered", err)
		}
		if replay, err := f.service.StudioHelpOperation(ctx, f.actor, q.ExerciseID, hint.OperationID); err != nil || replay != h {
			t.Fatal("Studio hint receipt lost", err)
		}
		if card = readStudioExercise(t, f, q.ExerciseID); card.HelpFloor != learning.HelpHint {
			t.Fatal("Studio help floor lost")
		}
		if replay, err := f.start.StartStudioQuiz(ctx, f.actor, id, cmd); err != nil || replay.Questions[question-1].SessionID != q.SessionID {
			t.Fatal("start not idempotent", err)
		}
		view, err := f.start.Learning.Session(ctx, q.SessionID)
		if err != nil || view.Session.Context.GoalRevisionID != frozen.Direction.GoalRevisionID || view.Session.State != tutoring.StateAwaitingResponse {
			t.Fatal("question rebound or not presented", err)
		}
		if question == 1 {
			bad := cmd
			bad.OperationID = uuid.NewString()
			bad.Question = 2
			if _, err = f.start.StartStudioQuiz(ctx, f.actor, id, bad); err == nil {
				t.Fatal("unfinished predecessor bypassed")
			}
		}
		e := learningcontent.Exercise{SessionID: q.SessionID, ActivityID: q.ActivityID, ContentArtifactID: q.ContentArtifactID, ContentVersion: 1}
		answer := f.command(t, e, tutoring.ActionSubmitAttempt, learning.HelpHint)
		answerCtx := f.start.Content.WithAnswer(ctx, f.actor, e.ContentArtifactID, 1)
		if question == 2 {
			independent := f.command(t, e, tutoring.ActionSubmitAttempt, learning.HelpNone)
			if _, err = f.learn.ApplyAction(answerCtx, f.actor.Device.ID, e.SessionID, independent); err == nil {
				t.Fatal("Studio hint floor bypassed via content answers")
			}
		}
		if _, err = f.learn.ApplyAction(answerCtx, f.actor.Device.ID, e.SessionID, answer); err != nil {
			t.Fatal(err)
		}
		if result, err := f.learn.ApplyAction(answerCtx, f.actor.Device.ID, e.SessionID, answer); err != nil || !result.Replayed {
			t.Fatal("answer not idempotent", err)
		}
		assessment := assessmentJSON(t, f, e, 1000)
		f.modelOverride = func(w http.ResponseWriter, r *http.Request) { studioReply(w, string(assessment), "stop") }
		f.work(t)
		view, err = f.start.Learning.Session(ctx, q.SessionID)
		if err != nil {
			t.Fatal(err)
		}
		feedback, err := f.learn.Feedback(ctx, view.WorkItem.Attempt.ID, false)
		if err != nil || feedback.Assessment == nil || len(feedback.Evidence) != 1 {
			t.Fatal("automatic feedback/evidence missing", err)
		}
		card = readStudioExercise(t, f, q.ExerciseID)
		if card.Feedback == nil || card.Feedback.Attempt.ID != feedback.Attempt.ID || !card.CanContinue || card.Assessment == nil || card.Assessment.Status != "succeeded" {
			t.Fatal("shared Studio feedback differs")
		}
		if _, err = f.learn.ApplyAction(ctx, f.actor.Device.ID, e.SessionID, f.command(t, e, tutoring.ActionAcknowledgeFeedback, "")); err != nil {
			t.Fatal(err)
		}
		readback, err := f.start.StudioQuiz(ctx, f.actor, id, 1)
		if err != nil || !readback.Questions[question-1].Completed || readback.Questions[question-1].AttemptID != feedback.Attempt.ID {
			t.Fatal("completed feedback lost", err)
		}
		card = readStudioExercise(t, f, q.ExerciseID)
		if card.Feedback == nil || card.CanContinue || card.AttemptID != feedback.Attempt.ID {
			t.Fatal("ack erased original feedback")
		}
	}
	progress, err := f.start.Learning.Progress(ctx, learning.ProgressQuery{GoalID: *r.GoalID, Status: "all", Limit: 100})
	if err != nil || len(progress.Items) != 1 || progress.Items[0].EvidenceCount != 2 {
		t.Fatalf("progress duplicates learning facts: %+v %v", progress, err)
	}
}
