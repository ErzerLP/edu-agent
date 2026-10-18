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
	"github.com/edu-agent/edu-agent/server/internal/settings"
	"github.com/edu-agent/edu-agent/server/internal/tutoring"
	"github.com/google/uuid"
)

func budgetReply(w http.ResponseWriter, content string, tokens int) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"choices": []any{map[string]any{"finish_reason": "stop", "message": map[string]string{"content": content}}},
		"usage":   map[string]int{"prompt_tokens": tokens / 2, "completion_tokens": tokens - tokens/2, "total_tokens": tokens},
	})
}

func assessmentBudgetLimits(t *testing.T, f *exerciseFixture, requests, tokens int) {
	t.Helper()
	l := f.settings.View().Limits
	l.OutputTokens, l.ResearchRequests, l.ResearchTokens = 8192, requests, tokens
	if _, err := f.settings.Update(settings.Update{ExpectedRevision: f.settings.View().Revision, Target: settings.Mentor, Limits: &l}); err != nil {
		t.Fatal(err)
	}
}

func budgetQuiz(t *testing.T, f *exerciseFixture) (string, learningcontent.StudioRevision) {
	t.Helper()
	f.service.ConfigureStudio(f.start.Content)
	ctx := context.Background()
	direction := "理解偶数"
	if _, err := spacepg.New(f.pool, f.service.directions).Mutate(ctx, f.actor.Device.ID, learningspace.DefaultID, learningspace.Command{OperationID: uuid.NewString(), ExpectedVersion: 1, Name: "数学", Direction: &direction, Status: "active"}); err != nil {
		t.Fatal(err)
	}
	var candidate map[string]any
	_ = json.Unmarshal([]byte(quizCandidate), &candidate)
	questions := candidate["questions"].([]any)
	candidate["questions"] = append(questions, questions[0])
	raw, _ := json.Marshal(candidate)
	f.modelOverride = func(w http.ResponseWriter, r *http.Request) { budgetReply(w, string(raw), 9000) }
	c := directionQuizRequest(t, knowledge.SourceSelection{Mode: "items", Items: []knowledge.SourceIdentity{f.request.Question.Manifest.Entries[0].SourceIdentity}})
	c.RequestBudget, c.TokenBudget = 1, 12000
	receipt, err := f.service.CreateStudio(ctx, f.actor, learningspace.DefaultID, c)
	if err != nil {
		t.Fatal(err)
	}
	f.work(t)
	generated := f.snapshot(t, receipt.RunID)
	if generated.Studio == nil || generated.Studio.Result == nil || generated.RequestsLeft != 0 || generated.TokensLeft != 0 {
		t.Fatalf("generation did not consume its own budget: status=%s reason=%s requests=%d tokens=%d", generated.Status, generated.Reason, generated.RequestsLeft, generated.TokensLeft)
	}
	r, err := f.start.Content.StudioGet(ctx, f.actor, generated.Studio.Result.ArtifactID, 1)
	if err != nil {
		t.Fatal(err)
	}
	return receipt.RunID, r
}

func submitBudgetAnswer(t *testing.T, f *exerciseFixture, card PracticeView) learningcontent.Exercise {
	t.Helper()
	e := learningcontent.Exercise{ID: card.ID, SessionID: card.SessionID, ActivityID: card.ActivityID, ContentArtifactID: card.ArtifactID, ContentVersion: card.ContentVersion}
	if _, err := f.learn.ApplyAction(f.start.Content.WithAnswer(context.Background(), f.actor, e.ContentArtifactID, e.ContentVersion), f.actor.Device.ID, e.SessionID, f.command(t, e, tutoring.ActionSubmitAttempt, card.AllowedHelp[0])); err != nil {
		t.Fatal(err)
	}
	return e
}

func retryBudgetAssessment(t *testing.T, f *exerciseFixture, e learningcontent.Exercise) {
	t.Helper()
	valid := assessmentJSON(t, f, e, 1000)
	var value map[string]any
	_ = json.Unmarshal(valid, &value)
	view, err := f.start.Learning.Session(context.Background(), e.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	item := value["assessment"].(map[string]any)["items"].([]any)[0].(map[string]any)
	item["rubric_item_id"] = view.WorkItem.Activity.Rubric.Items[0].ID
	valid, _ = json.Marshal(value)
	item["knowledge_quote_sha256"] = learning.SHA256([]byte("wrong quote"))
	invalid, _ := json.Marshal(value)
	calls := 0
	f.modelOverride = func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			budgetReply(w, string(invalid), 6000)
		} else {
			budgetReply(w, string(valid), 6500)
		}
	}
}

// Copying the generation's one-request/12000-token budget prevents the repair
// and later questions from completing despite adequate configured limits.
func TestPostgreSQLStudioAssessmentBudgetIndependentPerQuestion(t *testing.T) {
	f := newExerciseFixture(t)
	assessmentBudgetLimits(t, f, 20, 100000)
	generationRun, quiz := budgetQuiz(t, f)
	runs := map[string]bool{generationRun: true}
	for question := 1; question <= 3; question++ {
		state, err := f.start.StartStudioQuiz(context.Background(), f.actor, quiz.ArtifactID, learningcontent.StudioQuizStart{OperationID: uuid.NewString(), Version: 1, Generation: 1, Question: question, GoalVersion: quiz.Body.Quiz.GoalVersion})
		if err != nil {
			t.Fatal(err)
		}
		card := readStudioExercise(t, f, state.Questions[question-1].ExerciseID)
		e := submitBudgetAnswer(t, f, card.PracticeView)
		retryBudgetAssessment(t, f, e)
		f.work(t)
		card = readStudioExercise(t, f, e.ID)
		if card.Assessment == nil || card.Assessment.Status != "succeeded" || card.Feedback.Assessment == nil {
			t.Fatalf("question %d did not assess automatically: %+v", question, card.Assessment)
		}
		usage, err := f.service.AssessmentStatus(context.Background(), f.actor, card.AttemptID)
		if err != nil || usage.Run == nil || usage.Run.RequestsUsed != 2 || usage.Run.TokensLeft != 0 || usage.Run.CostUnknown || runs[usage.Task.RunID] {
			t.Fatalf("question %d budget/accounting not independent: %+v %v", question, usage.Run, err)
		}
		runs[usage.Task.RunID] = true
		if _, err := f.learn.ApplyAction(context.Background(), f.actor.Device.ID, e.SessionID, f.command(t, e, tutoring.ActionAcknowledgeFeedback, "")); err != nil {
			t.Fatal(err)
		}
	}
	if generated := f.snapshot(t, generationRun); generated.RequestsUsed != 1 || generated.TokensLeft != 0 {
		t.Fatal("assessment spent generation budget")
	}
}

func TestPostgreSQLStudyAssessmentBudgetIndependent(t *testing.T) {
	f := newExerciseFixture(t)
	assessmentBudgetLimits(t, f, 20, 100000)
	id, selection := studyExerciseConversation(t, f)
	for range 2 {
		f.modelOverride = func(w http.ResponseWriter, r *http.Request) { budgetReply(w, studyExerciseCandidate, 9000) }
		conversation := historyPage(t, f.runtimeFixture, id, 0, 50).Conversation
		receipt, err := f.service.Submit(context.Background(), f.actor, learningspace.DefaultID, id, SubmitTurn{OperationID: uuid.NewString(), ExpectedVersion: conversation.Version, DirectionVersion: 1, SourceSelection: &selection, Prompt: "出一道题", RequestBudget: 1, TokenBudget: 12000})
		if err != nil {
			t.Fatal(err)
		}
		f.work(t)
		page := historyPage(t, f.runtimeFixture, id, 0, 50)
		card := readStudyExercise(t, f, page.Items[len(page.Items)-1].ExerciseID)
		e := submitBudgetAnswer(t, f, card.PracticeView)
		retryBudgetAssessment(t, f, e)
		f.work(t)
		card = readStudyExercise(t, f, e.ID)
		if card.Assessment == nil || card.Assessment.Status != "succeeded" || card.Feedback.Assessment == nil {
			t.Fatalf("study answer did not assess automatically: %+v", card.Assessment)
		}
		usage, err := f.service.AssessmentStatus(context.Background(), f.actor, card.AttemptID)
		if err != nil || usage.Run == nil || usage.Run.RequestsUsed != 2 || usage.Run.TokensLeft != 0 || usage.Run.CostUnknown || usage.Task.RunID == receipt.RunID {
			t.Fatalf("study budget/accounting not independent: %+v %v", usage.Run, err)
		}
		if generated := f.snapshot(t, receipt.RunID); generated.RequestsLeft != 0 || generated.TokensLeft != 0 {
			t.Fatal("assessment spent conversation budget")
		}
		if _, err := f.learn.ApplyAction(context.Background(), f.actor.Device.ID, e.SessionID, f.command(t, e, tutoring.ActionAcknowledgeFeedback, "")); err != nil {
			t.Fatal(err)
		}
	}
}

// Schema rejection still has paid usage; retaining both full reservations
// incorrectly exhausts a budget that covers the two known responses.
func TestPostgreSQLAssessmentRejectedResponseUsage(t *testing.T) {
	f := newExerciseFixture(t)
	f.request.Assessment.Tokens = 16000
	e, attempt := f.submitted(t)
	valid := assessmentJSON(t, f, e, 1000)
	calls := 0
	f.modelOverride = func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			budgetReply(w, `{"invalid":"assessment"}`, 1000)
		} else {
			budgetReply(w, string(valid), 1500)
		}
	}
	f.work(t)
	state, err := f.service.AssessmentStatus(context.Background(), f.actor, attempt.ID)
	if err != nil || state.Run == nil || state.Run.Status != "succeeded" || state.Run.RequestsUsed != 2 || state.Run.TokensLeft != 0 || state.Run.CostUnknown {
		t.Fatalf("rejected response usage was not settled: %+v %v", state.Run, err)
	}
}

func TestPostgreSQLAutomaticAssessmentIgnoresLegacySettings(t *testing.T) {
	for _, tc := range []struct {
		name     string
		requests int
		tokens   int
		used     int
	}{
		{"requests", 1, 100000, 1},
		{"tokens", 20, 12000, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newExerciseFixture(t)
			assessmentBudgetLimits(t, f, tc.requests, tc.tokens)
			_, quiz := budgetQuiz(t, f)
			state, err := f.start.StartStudioQuiz(context.Background(), f.actor, quiz.ArtifactID, learningcontent.StudioQuizStart{OperationID: uuid.NewString(), Version: 1, Generation: 1, Question: 1, GoalVersion: quiz.Body.Quiz.GoalVersion})
			if err != nil {
				t.Fatal(err)
			}
			card := readStudioExercise(t, f, state.Questions[0].ExerciseID)
			e := submitBudgetAnswer(t, f, card.PracticeView)
			retryBudgetAssessment(t, f, e)
			f.work(t)
			card = readStudioExercise(t, f, e.ID)
			usage, err := f.service.AssessmentStatus(context.Background(), f.actor, card.AttemptID)
			if err != nil || usage.Run == nil || usage.Run.Status != "succeeded" || usage.Run.RequestsUsed != 2 || card.Feedback.Assessment == nil {
				t.Fatalf("legacy settings blocked assessment: %+v %v", usage.Run, err)
			}
		})
	}
}
