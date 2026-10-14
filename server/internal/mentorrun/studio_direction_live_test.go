package mentorrun

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
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
	"github.com/jackc/pgx/v5/pgxpool"
)

// Explicit opt-in; generation plus two answers allow at most five real requests. Never logs keys,
// model payloads or raw errors from the configured provider.
func TestPostgreSQLStudioDirectionLiveResponses(t *testing.T) {
	if os.Getenv("C4_LIVE_SMOKE") != "1" {
		t.Skip("explicit live model opt-in required")
	}
	endpoint, key := os.Getenv("EDU_ACC_MODEL_BASE_URL"), os.Getenv("EDU_ACC_MODEL_API_KEY")
	if endpoint == "" || key == "" {
		t.Fatal("acceptance model environment missing")
	}
	budgetLiveDatabase(t)
	f := newExerciseFixture(t, "127.0.0.1:33660")
	limits := settings.DefaultLimits()
	limits.OutputTokens = 8192
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
	f.service.ConfigureStudio(f.start.Content)
	direction := "理解偶数的定义，能解释一个整数为什么是偶数"
	if _, err = spacepg.New(f.pool, f.service.directions).Mutate(context.Background(), f.actor.Device.ID, learningspace.DefaultID, learningspace.Command{OperationID: uuid.NewString(), ExpectedVersion: 1, Name: "偶数", Direction: &direction, Status: "active"}); err != nil {
		t.Fatal("live direction setup failed")
	}
	source := selectedDocument(t, f.runtimeFixture, "偶数是可以被 2 整除的整数，例如 2、4、6。整数 n 是偶数，当且仅当存在整数 k，使 n=2k。0=2×0，所以 0 也是偶数。")
	c := directionQuizRequest(t, knowledge.SourceSelection{Mode: "items", Items: []knowledge.SourceIdentity{source}})
	c.RequestBudget = 1
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	started := time.Now()
	receipt, err := f.service.CreateStudio(ctx, f.actor, learningspace.DefaultID, c)
	if err != nil {
		t.Fatal("live direction quiz submission failed")
	}
	if _, err = f.service.RunOnce(ctx); err != nil {
		t.Fatal("live generation worker failed")
	}
	generated := f.snapshot(t, receipt.RunID)
	if generated.Studio == nil || generated.Studio.Result == nil {
		t.Fatalf("live quiz not published: status=%s reason=%s requests=%d", generated.Status, generated.Reason, generated.RequestsUsed)
	}
	id := generated.Studio.Result.ArtifactID
	r, err := f.start.Content.StudioGet(ctx, f.actor, id, 1)
	if err != nil || r.Body.Quiz.Direction == nil {
		t.Fatal("live direction snapshot missing")
	}
	if len(r.Body.Quiz.Questions) != 3 {
		t.Fatal("live quiz did not generate three questions")
	}
	requests := generated.RequestsUsed
	for question := 1; question <= 2; question++ {
		state, err := f.start.StartStudioQuiz(ctx, f.actor, id, learningcontent.StudioQuizStart{OperationID: uuid.NewString(), Version: 1, Generation: 1, Question: question, GoalVersion: r.Body.Quiz.GoalVersion})
		if err != nil {
			t.Fatal("live question cannot start")
		}
		card := readStudioExercise(t, f, state.Questions[question-1].ExerciseID)
		e := learningcontent.Exercise{ID: card.ID, SessionID: card.SessionID, ActivityID: card.ActivityID, ContentArtifactID: card.ArtifactID, ContentVersion: card.ContentVersion}
		answer := f.command(t, e, tutoring.ActionSubmitAttempt, card.AllowedHelp[0])
		answer.Answer = "偶数是可以被 2 整除的整数，即能写成 2k（k 为整数）。例如 4=2×2，因此 4 是偶数。0=2×0，所以 0 也是偶数。"
		if _, err = f.learn.ApplyAction(f.start.Content.WithAnswer(ctx, f.actor, card.ArtifactID, card.ContentVersion), f.actor.Device.ID, card.SessionID, answer); err != nil {
			t.Fatal("live answer submission failed")
		}
		if _, err = f.service.RunOnce(ctx); err != nil {
			t.Fatal("live assessment worker failed")
		}
		card = readStudioExercise(t, f, card.ID)
		if card.Feedback == nil || card.Feedback.Assessment == nil || card.Assessment == nil || card.Assessment.Status != "succeeded" {
			t.Fatalf("live question %d feedback missing: status=%+v", question, card.Assessment)
		}
		status, err := f.service.AssessmentStatus(ctx, f.actor, card.AttemptID)
		if err != nil || status.Run == nil {
			t.Fatal("live usage unavailable")
		}
		requests += status.Run.RequestsUsed
		if requests > 5 || status.Run.RequestsUsed > 2 || status.Task.RunID == receipt.RunID {
			t.Fatal("live independent request bound exceeded")
		}
		t.Logf("question=%d status=%s requests=%d tokens=%d disposition=%s", question, status.Run.Status, status.Run.RequestsUsed, 65536-status.Run.TokensLeft, card.Feedback.Decisions[len(card.Feedback.Decisions)-1].Disposition)
		if question == 1 {
			decision := card.Feedback.Decisions[len(card.Feedback.Decisions)-1]
			if decision.Disposition == learning.DispositionProvisional {
				// Budget completion does not imply accepted learning evidence.
				// Use the existing explicit void decision before advancing; never
				// change model output or bypass the provisional feedback guard.
				op := f.command(t, e, tutoring.ActionAcknowledgeFeedback, "").Operation
				if _, err = f.learn.Decide(ctx, f.actor.Device.ID, card.Feedback.Assessment.ID, learning.AssessmentDecisionCommand{Operation: op, Kind: "void", ExpectedDispositionVersion: decision.Version, Reason: "预算验收：保留待复核结论，作废后继续验收下一题"}); err != nil {
					t.Fatal("live provisional feedback decision failed")
				}
				t.Log("question=1 provisional feedback explicitly voided through existing decision flow")
			}
			if _, err = f.learn.ApplyAction(ctx, f.actor.Device.ID, e.SessionID, f.command(t, e, tutoring.ActionAcknowledgeFeedback, "")); err != nil {
				t.Fatal("live feedback acknowledgement failed")
			}
		}
	}
	t.Logf("responses deepseek-v4-flash output_budget=8192 idle_timeout=60 research_requests=20 research_tokens=100000 elapsed_ms=%d questions=%d assessed=2 generation_requests=%d total_requests=%d generation_tokens=%d", time.Since(started).Milliseconds(), len(r.Body.Quiz.Questions), generated.RequestsUsed, requests, 50000-generated.TokensLeft)
}

// Isolate paid acceptance from the normal integration database. Cleanup runs
// after fixture pools close; neither deployment data nor shared schemas change.
func budgetLiveDatabase(t *testing.T) {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Fatal("live PostgreSQL environment missing")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal("live database initialization failed")
	}
	name := "budget_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err = admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		admin.Close()
		t.Fatal("live database creation failed")
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(context.Background(), "DROP DATABASE "+name); err != nil {
			t.Error("live database cleanup failed")
		}
		admin.Close()
	})
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal("live database configuration failed")
	}
	config.ConnConfig.Database = name
	t.Setenv("TEST_DATABASE_URL", config.ConnConfig.ConnString())
}
