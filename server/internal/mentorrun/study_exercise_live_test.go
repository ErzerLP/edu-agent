package mentorrun

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/knowledge"
	"github.com/edu-agent/edu-agent/server/internal/learning"
	"github.com/edu-agent/edu-agent/server/internal/learningcontent"
	"github.com/edu-agent/edu-agent/server/internal/learningspace"
	"github.com/edu-agent/edu-agent/server/internal/settings"
	"github.com/edu-agent/edu-agent/server/internal/tutoring"
	"github.com/google/uuid"
)

// Explicit opt-in: at most two generation calls plus two assessment calls.
// The shared adapters log usage only; credentials are never included in output.
func TestPostgreSQLStudyExerciseLiveResponses(t *testing.T) {
	if os.Getenv("C3B_LIVE_SMOKE") != "1" {
		t.Skip("explicit live model opt-in required")
	}
	endpoint, key := os.Getenv("EDU_ACC_MODEL_BASE_URL"), os.Getenv("EDU_ACC_MODEL_API_KEY")
	if endpoint == "" || key == "" {
		t.Fatal("acceptance model environment missing")
	}
	f := newExerciseFixture(t)
	limits := settings.DefaultLimits()
	limits.OutputTokens = 8192
	limits.ResearchRequests = 2
	limits.ResearchTokens = 50000
	limits.IdleTimeoutSeconds = 180
	configuration, err := settings.Open(settings.Options{Path: filepath.Join(t.TempDir(), "private", "settings.json"), Teaching: settings.Connection{Enabled: true, Provider: "openai_compatible", Endpoint: endpoint, Model: "deepseek-v4-flash", AuthMode: "bearer", APIStyle: "responses"}, TeachingKey: key, Limits: &limits})
	if err != nil {
		t.Fatal("live settings initialization failed")
	}
	useTeaching := true
	if _, err = configuration.Update(settings.Update{ExpectedRevision: configuration.View().Revision, Target: settings.Mentor, MentorUsesTeaching: &useTeaching}); err != nil {
		t.Fatal("mentor teaching reuse failed")
	}
	f.settings = configuration
	f.service, err = New(f.pool, configuration, bytes.Repeat([]byte{42}, 32))
	if err != nil {
		t.Fatal("live service initialization failed")
	}
	f.service.ConfigureStart(f.start)
	id, _ := studyExerciseConversation(t, f)
	source := selectedDocument(t, f.runtimeFixture, "偶数是可以被 2 整除的整数，例如 2、4、6。整数 n 是偶数，当且仅当存在整数 k，使 n=2k。")
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	started := time.Now()
	r, err := f.service.Submit(ctx, f.actor, learningspace.DefaultID, id, SubmitTurn{OperationID: uuid.NewString(), ExpectedVersion: 1, DirectionVersion: 1, Prompt: "出一道题：只问偶数的定义，并请我举一个例子。不要在讲解中给出答案。", SourceSelection: &knowledge.SourceSelection{Mode: "items", Items: []knowledge.SourceIdentity{source}}, RequestBudget: 2, TokenBudget: 50000})
	if err != nil {
		t.Fatal("live question submission failed")
	}
	if _, err = f.service.RunOnce(ctx); err != nil {
		t.Fatal("live question worker failed")
	}
	generated := f.snapshot(t, r.RunID)
	page := historyPage(t, f.runtimeFixture, id, 0, 10)
	if generated.Status != "succeeded" || len(page.Items) != 1 || page.Items[0].ExerciseID == "" {
		t.Fatalf("live question not published: status=%s reason=%s requests=%d", generated.Status, generated.Reason, generated.RequestsUsed)
	}
	card := readStudyExercise(t, f, page.Items[0].ExerciseID)
	e := learningcontent.Exercise{ID: card.ID, SessionID: card.SessionID, ActivityID: card.ActivityID, ContentArtifactID: card.ArtifactID, ContentVersion: card.ContentVersion}
	help := learning.HelpNone
	if len(card.AllowedHelp) > 0 {
		help = card.AllowedHelp[0]
	}
	command := f.command(t, e, tutoring.ActionSubmitAttempt, help)
	command.Answer = "偶数是能被 2 整除的整数，即可以写成 2k（k 为整数）。例如 4=2×2，所以 4 是偶数。"
	if _, err = f.learn.ApplyAction(f.start.Content.WithAnswer(ctx, f.actor, e.ContentArtifactID, e.ContentVersion), f.actor.Device.ID, e.SessionID, command); err != nil {
		t.Fatal("live answer submission failed")
	}
	if _, err = f.service.RunOnce(ctx); err != nil {
		t.Fatal("live assessment worker failed")
	}
	card = readStudyExercise(t, f, e.ID)
	if card.Feedback == nil || card.Feedback.Assessment == nil || card.Assessment == nil || card.Assessment.Status != "succeeded" {
		t.Fatalf("live feedback missing: status=%+v", card.Assessment)
	}
	for _, item := range card.Feedback.Assessment.Items {
		if item.KnowledgeReferenceID != "" {
			if err = f.service.ReadStudyExerciseCitation(ctx, f.actor, e.ID, item.KnowledgeReferenceID, func(SourceCitation) error { return nil }); err != nil {
				t.Fatal("live feedback citation could not be resolved")
			}
		}
	}
	state, err := f.service.AssessmentStatus(ctx, f.actor, card.AttemptID)
	if err != nil {
		t.Fatal("live usage unavailable")
	}
	requests := generated.RequestsUsed + state.Run.RequestsUsed
	if requests > 4 {
		t.Fatal("live call bound exceeded")
	}
	t.Logf("responses deepseek-v4-flash output_budget=8192 elapsed_ms=%d generation_requests=%d assessment_requests=%d total_requests=%d generation_tokens=%d assessment_tokens=%d disposition=%s citations=%d", time.Since(started).Milliseconds(), generated.RequestsUsed, state.Run.RequestsUsed, requests, 50000-generated.TokensLeft, 50000-state.Run.TokensLeft, card.Feedback.Decisions[len(card.Feedback.Decisions)-1].Disposition, len(card.Feedback.Assessment.Items))
}
