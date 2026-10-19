package mentorrun

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"reflect"
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

// This fails if staging loses the frozen quiz contract, publishes early, leaks a
// rubric into planning/summary/progress, or bypasses the existing single-question kernel.
func TestPostgreSQLStudioStagedQuizLearningPath(t *testing.T) {
	f := newExerciseFixture(t)
	f.service.ConfigureStudio(f.start.Content)
	ctx, _ := learningspace.WithScope(context.Background(), learningspace.DefaultID)
	spaces := spacepg.New(f.pool, f.service.directions)
	direction := "理解偶数"
	if _, err := spaces.Mutate(ctx, f.actor.Device.ID, learningspace.DefaultID, learningspace.Command{OperationID: uuid.NewString(), ExpectedVersion: 1, Name: "测验", Direction: &direction, Status: "active"}); err != nil {
		t.Fatal(err)
	}
	missingOrder := true
	f.modelOverride = func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var input struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.Unmarshal(raw, &input); err != nil {
			t.Error(err)
			return
		}
		material := input.Messages[1].Content
		if strings.Contains(material, `"directory"`) {
			if strings.Contains(material, "SECRET_RUBRIC") {
				t.Error("rubric leaked to plan")
			}
			difficulty := "medium"
			if strings.Contains(material, "更难") {
				difficulty = "hard"
			}
			sections := []map[string]any{}
			var options struct {
				QuestionCount int `json:"question_count"`
			}
			if err := json.Unmarshal([]byte(material), &options); err != nil {
				t.Error(err)
				return
			}
			for _, id := range []string{"q1", "q2", "q3"}[:options.QuestionCount] {
				sections = append(sections, map[string]any{"id": id, "topic": id + "偶数", "objective": "解释所选知识点", "question_type": "explanation", "difficulty": difficulty, "chapters": []int{1}})
			}
			result, _ := json.Marshal(map[string]any{"title": "分步测验", "coverage": "partial", "sections": sections, "uncovered": []string{}, "explanation": "已调整题目难度。"})
			studioReply(w, string(result), "stop")
		} else if strings.Contains(material, `"allowed_claims"`) {
			if strings.Contains(material, "SECRET_RUBRIC") || strings.Contains(material, `"prompt"`) || strings.Contains(material, `"sources"`) {
				t.Error("quiz summary exposed answer material")
			}
			var summary struct {
				Claims []learningcontent.StudioSummaryClaim `json:"allowed_claims"`
			}
			if err := json.Unmarshal([]byte(material), &summary); err != nil {
				t.Error(err)
				return
			}
			ids := []string{}
			for _, claim := range summary.Claims {
				ids = append(ids, claim.ID)
			}
			order := ids
			if missingOrder {
				order = []string{}
				missingOrder = false
			}
			result, _ := json.Marshal(map[string]any{"overview": ids, "transitions": order, "uncovered": []string{}})
			studioReply(w, string(result), "stop")
		} else {
			if !strings.Contains(input.Messages[0].Content, "rubric 只考查题干问到的内容") {
				t.Error("question request does not restrict rubric to explicit prompt questions")
			}
			var data struct {
				Topic   string                   `json:"topic"`
				Sources []knowledge.SourceDetail `json:"sources"`
			}
			_ = json.Unmarshal([]byte(material), &data)
			if len(data.Sources) != 1 || !strings.Contains(data.Sources[0].Fragments[0].Text, "SELECTED_EVEN_SOURCE") {
				t.Error("question source snapshot changed")
			}
			result, _ := json.Marshal(map[string]any{"title": data.Topic, "coverage": "partial", "questions": []any{map[string]any{"prompt": data.Topic + "：解释偶数并举例。", "rubric": []any{map[string]string{"id": "definition", "criterion": "SECRET_RUBRIC：说明偶数可被2整除"}}, "allowed_help": []string{"none", "hint", "scaffold", "answer_revealed"}, "citations": []any{map[string]int{"source": 1, "fragment": 1}}}}})
			studioReply(w, string(result), "stop")
		}
	}
	generationModel := f.modelOverride
	selection := knowledge.SourceSelection{Mode: "items", Items: []knowledge.SourceIdentity{f.request.Question.Manifest.Entries[0].SourceIdentity}}
	j := concurrentStudioQuizCreate(t, f, ctx, StudioJobCreate{OperationID: uuid.NewString(), Generation: 1, Kind: "quiz", SourceSelection: selection, DirectionVersion: 1})
	var err error
	f.work(t)
	j, _ = contentSnapshot(t, f.runtimeFixture, ctx, j.ID)
	if j.Status != "awaiting_confirmation" || len(j.Plan.Sections) != 3 {
		t.Fatalf("question plan: %+v", j)
	}
	j, err = f.service.ChangeStudioPlan(ctx, f.actor, j.ID, "message", StudioPlanCommand{OperationID: uuid.NewString(), ExpectedVersion: j.Version, PlanRevision: j.ActivePlanRevision, Text: "更难一点"})
	if err != nil {
		t.Fatal(err)
	}
	f.work(t)
	j, _ = contentSnapshot(t, f.runtimeFixture, ctx, j.ID)
	j, err = f.service.ChangeStudioPlan(ctx, f.actor, j.ID, "patch", StudioPlanCommand{OperationID: uuid.NewString(), ExpectedVersion: j.Version, PlanRevision: j.ActivePlanRevision, DeleteIDs: []string{"q2"}, OrderedIDs: []string{"q3", "q1"}})
	if err != nil {
		t.Fatal(err)
	}
	confirm := StudioPlanCommand{OperationID: uuid.NewString(), ExpectedVersion: j.Version, PlanRevision: j.ActivePlanRevision}
	j, err = f.service.ChangeStudioPlan(ctx, f.actor, j.ID, "confirm", confirm)
	if err != nil || j.TotalUnits != 2 {
		t.Fatalf("one unit per question: %+v %v", j, err)
	}
	if replay, e := f.service.ChangeStudioPlan(ctx, f.actor, j.ID, "confirm", confirm); e != nil || replay.RunID != j.RunID {
		t.Fatal("confirmation duplicated execution", e)
	}
	direction = "新方向不用于旧题"
	if _, err = spaces.Mutate(ctx, f.actor.Device.ID, learningspace.DefaultID, learningspace.Command{OperationID: uuid.NewString(), ExpectedVersion: 2, Name: "测验", Direction: &direction, Status: "active"}); err != nil {
		t.Fatal(err)
	}
	f.work(t)
	units, err := f.service.StudioUnits(ctx, f.actor, j.ID, "", 0, 32)
	if err != nil || len(units.Items) != 2 || units.Items[0].ID != "q3" || units.Items[0].Status != "succeeded" {
		t.Fatalf("question progress: %+v %v", units, err)
	}
	raw, _ := json.Marshal(units)
	if bytes.Contains(raw, []byte("SECRET_RUBRIC")) {
		t.Fatal("unpublished rubric exposed")
	}
	var count int
	if err = f.pool.QueryRow(ctx, `SELECT count(*) FROM learning_studio_artifacts`).Scan(&count); err != nil || count != 0 {
		t.Fatal("partial quiz published", err)
	}
	f.work(t)
	f.work(t)
	j, _ = contentSnapshot(t, f.runtimeFixture, ctx, j.ID)
	if j.Status != "completed" {
		t.Fatalf("quiz publication: %s/%s", j.Status, j.Reason)
	}
	r, err := f.start.Content.StudioGet(ctx, f.actor, j.Result.ArtifactID, 1)
	if err != nil || r.Body.Schema != 1 || len(r.Body.Quiz.Questions) != 2 || !strings.HasPrefix(r.Body.Quiz.Questions[0].Prompt, "q3") || r.Body.Quiz.Direction.Version != 1 {
		t.Fatalf("quiz read contract: %+v %v", r, err)
	}
	if r.Body.SourceManifest.Entries[0].SourceIdentity != selection.Items[0] {
		t.Fatal("published source version changed")
	}
	if len(r.Body.Summary.Transitions) != 2 || r.Body.Summary.Transitions[0].ID != "q3" || r.Body.Summary.Transitions[1].ID != "q1" {
		t.Fatal("quiz summary omitted confirmed answer order")
	}
	for index, unit := range []string{"q3", "q1"} {
		var question int
		if err = f.pool.QueryRow(ctx, `SELECT ordinal FROM learning_studio_section_revisions WHERE artifact_id=$1 AND version=1 AND section_id=$2`, r.ArtifactID, unit).Scan(&question); err != nil || question != index+1 {
			t.Fatal("immutable unit/question mapping", err)
		}
	}
	if err = f.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM learning_attempts)+(SELECT count(*) FROM learning_evidence)+(SELECT count(*) FROM learning_studio_quiz_questions)`).Scan(&count); err != nil || count != 0 {
		t.Fatal("generation wrote learning facts/session", count, err)
	}
	var roots int
	if err = f.pool.QueryRow(ctx, `SELECT count(DISTINCT task_id) FROM learning_model_calls WHERE stage IN ('studio_plan','studio_question','studio_summary')`).Scan(&roots); err != nil || roots != 1 {
		t.Fatal("split usage roots", roots, err)
	}
	// A newer managed goal must not rebind either question from this old version.
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.service.directions.EnsureTx(ctx, tx, f.actor.Device.ID, learningspace.DefaultID, 1, 2); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	previous := ""
	for question := 1; question <= 2; question++ {
		start := learningcontent.StudioQuizStart{OperationID: uuid.NewString(), Version: 1, Generation: 1, GoalVersion: r.Body.Quiz.GoalVersion, Question: question}
		state, e := f.start.StartStudioQuiz(ctx, f.actor, r.ArtifactID, start)
		if e != nil {
			t.Fatal("old quiz cannot start", e)
		}
		q := state.Questions[question-1]
		if q.SessionID == previous || q.ExerciseID == "" {
			t.Fatal("questions share session or bypass kernel")
		}
		previous = q.SessionID
		for i := 0; i < 2; i++ {
			replay, e := f.start.StartStudioQuiz(ctx, f.actor, r.ArtifactID, start)
			if e != nil || replay.Questions[question-1].SessionID != q.SessionID {
				t.Fatal("duplicate start", e)
			}
			start.OperationID = uuid.NewString()
		}
		view, e := f.start.Learning.Session(ctx, q.SessionID)
		if e != nil || view.Session.Context.GoalRevisionID != r.Body.Quiz.Direction.GoalRevisionID {
			t.Fatal("old goal revision changed", e)
		}
		if err = f.pool.QueryRow(ctx, `SELECT count(*) FROM learning_attempts WHERE activity_id=$1`, q.ActivityID).Scan(&count); err != nil || count != 0 {
			t.Fatal("start wrote attempt", err)
		}
		if _, e = f.service.PublishStudioHelp(ctx, f.actor, q.ExerciseID, StudioHelpRequest{OperationID: uuid.NewString()}); e != nil {
			t.Fatal(e)
		}
		ex := learningcontent.Exercise{ID: q.ExerciseID, SessionID: q.SessionID, ActivityID: q.ActivityID, ContentArtifactID: q.ContentArtifactID, ContentVersion: 1}
		answerCtx := f.start.Content.WithAnswer(ctx, f.actor, ex.ContentArtifactID, 1)
		if _, e = f.learn.ApplyAction(answerCtx, f.actor.Device.ID, ex.SessionID, f.command(t, ex, tutoring.ActionSubmitAttempt, learning.HelpNone)); e == nil {
			t.Fatal("help floor bypassed")
		}
		answer := f.command(t, ex, tutoring.ActionSubmitAttempt, learning.HelpHint)
		if _, e = f.learn.ApplyAction(answerCtx, f.actor.Device.ID, ex.SessionID, answer); e != nil {
			t.Fatal(e)
		}
		if replay, e := f.learn.ApplyAction(answerCtx, f.actor.Device.ID, ex.SessionID, answer); e != nil || !replay.Replayed {
			t.Fatal("duplicate answer", e)
		}
		if question == 1 {
			limits := f.settings.View().Limits
			limits.ResearchRequests = 1
			if _, e = f.settings.Update(settings.Update{ExpectedRevision: f.settings.View().Revision, Target: settings.Mentor, Limits: &limits}); e != nil {
				t.Fatal(e)
			}
		}
		{
			f.work(t)
			card := readStudioExercise(t, f, ex.ID)
			if card.Assessment == nil || !card.Assessment.CanResume {
				t.Fatal("assessment cannot recover")
			}
			resume := StudyAssessmentResume{OperationID: uuid.NewString(), ExpectedVersion: card.Assessment.Version, Configuration: card.Assessment.Configuration}
			first, e := f.service.ResumeStudioAssessment(ctx, f.actor, ex.ID, resume)
			if e != nil {
				t.Fatal(e)
			}
			if again, e := f.service.ResumeStudioAssessment(ctx, f.actor, ex.ID, resume); e != nil || again != first {
				t.Fatal("duplicate assessment recovery", e)
			}
		}
		assessmentReply(t, f, ex, 1000)
		f.work(t)
		card := readStudioExercise(t, f, ex.ID)
		if card.Feedback == nil || card.Feedback.Assessment == nil || len(card.Feedback.Evidence) != 1 {
			t.Fatalf("question %d feedback missing: assessment=%+v feedback=%+v", question, card.Assessment, card.Feedback)
		}
		if _, e = f.learn.ApplyAction(ctx, f.actor.Device.ID, ex.SessionID, f.command(t, ex, tutoring.ActionAcknowledgeFeedback, "")); e != nil {
			t.Fatal(e)
		}
	}
	if err = f.pool.QueryRow(ctx, `SELECT count(*) FROM learning_attempts`).Scan(&count); err != nil || count != 2 {
		t.Fatal("duplicate scoring", count, err)
	}
	// A new artifact version gets its own mapping and source/direction snapshot;
	// the old version and its already answered questions stay readable.
	f.modelOverride = generationModel
	newSource := selectedDocument(t, f.runtimeFixture, "SELECTED_EVEN_SOURCE_NEW_VERSION")
	questionCount := 1
	newJob, err := f.service.CreateStudioJob(ctx, f.actor, StudioJobCreate{OperationID: uuid.NewString(), Generation: 1, Kind: "quiz", DirectionVersion: 2, SourceSelection: knowledge.SourceSelection{Mode: "items", Items: []knowledge.SourceIdentity{newSource}}, QuestionCount: &questionCount, ArtifactID: r.ArtifactID, ExpectedArtifactVersion: 1})
	if err != nil {
		t.Fatal(err)
	}
	f.work(t)
	newJob, _ = contentSnapshot(t, f.runtimeFixture, ctx, newJob.ID)
	newJob, err = f.service.ChangeStudioPlan(ctx, f.actor, newJob.ID, "confirm", StudioPlanCommand{OperationID: uuid.NewString(), ExpectedVersion: newJob.Version, PlanRevision: newJob.ActivePlanRevision})
	if err != nil {
		t.Fatal(err)
	}
	f.work(t)
	f.work(t)
	newJob, _ = contentSnapshot(t, f.runtimeFixture, ctx, newJob.ID)
	if newJob.Status != "completed" || newJob.Result.ArtifactID != r.ArtifactID || newJob.Result.Version != 2 {
		t.Fatalf("regeneration failed: %+v", newJob)
	}
	latest, err := f.start.Content.StudioGet(ctx, f.actor, r.ArtifactID, 2)
	if err != nil || latest.Body.Quiz.Direction.Version != 2 || latest.Body.SourceManifest.Entries[0].SourceIdentity != newSource {
		t.Fatal("new snapshot not used", err)
	}
	old, err := f.start.Content.StudioGet(ctx, f.actor, r.ArtifactID, 1)
	if err != nil || !reflect.DeepEqual(old.Body, r.Body) {
		t.Fatal("old quiz mutated", err)
	}
	newState, err := f.start.StartStudioQuiz(ctx, f.actor, r.ArtifactID, learningcontent.StudioQuizStart{OperationID: uuid.NewString(), Version: 2, Generation: 1, GoalVersion: latest.Body.Quiz.GoalVersion, Question: 1})
	if err != nil || len(newState.Questions) != 1 || newState.Questions[0].SessionID == previous || newState.Questions[0].AttemptID != "" {
		t.Fatal("regeneration reused old answer/session", err)
	}
	contentCommand(t, f.runtimeFixture, ctx, newJob, "delete")
	if err = f.pool.QueryRow(ctx, `SELECT ordinal FROM learning_studio_section_revisions WHERE artifact_id=$1 AND version=2 AND section_id='q1'`, r.ArtifactID).Scan(&count); err != nil || count != 1 {
		t.Fatal("job deletion removed published mapping", err)
	}
	oldState, err := f.start.StudioQuiz(ctx, f.actor, r.ArtifactID, 1)
	if err != nil || len(oldState.Questions) != 2 || !oldState.Questions[1].Completed {
		t.Fatal("old quiz cannot continue/read", err)
	}
}

// Hold the space until both duplicate requests have reached its lock. A shared
// lock followed by EnsureTx's exclusive lock deadlocks even with operation receipts.
func concurrentStudioQuizCreate(t *testing.T, f *exerciseFixture, ctx context.Context, command StudioJobCreate) learningcontent.StudioJob {
	t.Helper()
	lock, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Rollback(context.Background())
	var blocker int
	if err = lock.QueryRow(ctx, `SELECT pg_backend_pid() FROM learning_spaces WHERE id=$1 FOR UPDATE`, learningspace.DefaultID).Scan(&blocker); err != nil {
		t.Fatal(err)
	}
	type result struct {
		job learningcontent.StudioJob
		err error
	}
	results := make(chan result, 2)
	for i := 0; i < 2; i++ {
		go func() { j, e := f.service.CreateStudioJob(ctx, f.actor, command); results <- result{j, e} }()
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		var blocked int
		if err = f.pool.QueryRow(ctx, `WITH RECURSIVE waiting(pid) AS (
 SELECT pid FROM pg_stat_activity WHERE $1=ANY(pg_blocking_pids(pid))
 UNION SELECT a.pid FROM pg_stat_activity a JOIN waiting w ON w.pid=ANY(pg_blocking_pids(a.pid))
) SELECT count(*) FROM waiting`, blocker).Scan(&blocked); err != nil {
			t.Fatal(err)
		}
		if blocked == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("duplicate requests did not reach space gate")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err = lock.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	first, second := <-results, <-results
	if first.err != nil || second.err != nil {
		t.Fatalf("concurrent quiz create: %v / %v", first.err, second.err)
	}
	if first.job.ID != second.job.ID || first.job.RunID != second.job.RunID {
		t.Fatal("duplicate create produced two jobs/runs")
	}
	return first.job
}
