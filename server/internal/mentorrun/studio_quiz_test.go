package mentorrun

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	identitypg "github.com/edu-agent/edu-agent/server/internal/identity/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/integrations/learningknowledge"
	"github.com/edu-agent/edu-agent/server/internal/knowledge"
	knowledgepg "github.com/edu-agent/edu-agent/server/internal/knowledge/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/learning"
	learningpg "github.com/edu-agent/edu-agent/server/internal/learning/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/learningcontent"
	"github.com/edu-agent/edu-agent/server/internal/learningspace"
	"github.com/edu-agent/edu-agent/server/internal/learningstart"
	memorypg "github.com/edu-agent/edu-agent/server/internal/memory/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/platform/keyrotation"
	outboxpg "github.com/edu-agent/edu-agent/server/internal/platform/outbox/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/privacy"
	privacypg "github.com/edu-agent/edu-agent/server/internal/privacy/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/tutoring"
	tutoringpg "github.com/edu-agent/edu-agent/server/internal/tutoring/postgresstore"
	"github.com/google/uuid"
)

const quizCandidate = `{"title":"偶数测验","coverage":"complete","questions":[{"prompt":"解释偶数。","rubric":[{"id":"definition","criterion":"说明被 2 整除"}],"allowed_help":["hint","scaffold"],"citations":[{"source":1,"fragment":1}]},{"prompt":"判断 4 是否为偶数并解释。","rubric":[{"id":"definition","criterion":"说明 4 能被 2 整除"}],"allowed_help":["none","hint"],"citations":[{"source":1,"fragment":1}]}]}`

func TestPostgreSQLStudioQuizFormalSessions(t *testing.T) {
	f, request := studioFixture(t, func(w http.ResponseWriter, r *http.Request, _ int) { studioReply(w, quizCandidate, "stop") })
	ctx := context.Background()
	k := knowledgepg.New(f.pool)
	ks, _ := knowledge.NewService(k, knowledge.NewCanonicalizer(), knowledge.ServiceOptions{})
	store := learningpg.New(f.pool, tutoringpg.New(f.pool), k)
	content, err := learningcontent.New(f.pool, store, bytes.Repeat([]byte{42}, 32))
	if err != nil {
		t.Fatal(err)
	}
	content.ConfigureStudioSources(k)
	content.ConfigureReferences(k)
	store.ConfigureContent(content)
	f.service.ConfigureStudio(content)
	service, err := learning.NewService(store, store, learningknowledge.New(ks), learning.ServiceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	goal := uuid.NewString()
	envelope := func(id string, v int64) learning.OperationEnvelope {
		return learning.OperationEnvelope{OperationID: uuid.NewString(), PayloadSchemaVersion: 1, AggregateType: "session", AggregateID: id, ExpectedVersion: v, Payload: json.RawMessage(`{}`)}
	}
	op := envelope(goal, 0)
	op.AggregateType = "goal"
	if _, err = service.CreateGoal(ctx, f.actor.Device.ID, learning.GoalCommand{Operation: op, Text: "偶数学习", Source: "test"}); err != nil {
		t.Fatal(err)
	}
	g, err := service.GetGoal(ctx, goal)
	if err != nil {
		t.Fatal(err)
	}
	classroom := uuid.NewString()
	if _, err = service.CreateSession(ctx, f.actor.Device.ID, learning.SessionCommand{Operation: envelope(classroom, 0), GoalRevisionID: g.ID}); err != nil {
		t.Fatal(err)
	}
	classroomBefore, err := store.LoadSessionAuthority(ctx, classroom)
	if err != nil {
		t.Fatal(err)
	}
	request.Kind = "quiz"
	request.GoalID = &goal
	request.GoalVersion = 1
	receipt, err := f.service.CreateStudio(ctx, f.actor, learningspace.DefaultID, request)
	if err != nil {
		t.Fatal(err)
	}
	f.work(t)
	snapshot := f.snapshot(t, receipt.RunID)
	if snapshot.Studio == nil || snapshot.Studio.Result == nil {
		t.Fatalf("quiz not published: %+v", snapshot)
	}
	id := snapshot.Studio.Result.ArtifactID
	start := &learningstart.Service{Learning: store, Knowledge: k, Content: content}
	cmd := learningcontent.StudioQuizStart{OperationID: uuid.NewString(), Version: 1, Generation: 1, Question: 1, GoalVersion: 1}
	result, err := start.StartStudioQuiz(ctx, f.actor, id, cmd)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Questions) != 1 || result.Questions[0].SessionID == classroom {
		t.Fatal("not an isolated single question carrier")
	}
	replay, err := start.StartStudioQuiz(ctx, f.actor, id, cmd)
	if err != nil || replay.Questions[0].SessionID != result.Questions[0].SessionID {
		t.Fatal("start replay duplicated", err)
	}
	cmd.Question = 2
	if _, err = start.StartStudioQuiz(ctx, f.actor, id, cmd); err == nil {
		t.Fatal("operation payload conflict admitted")
	}
	cmd.OperationID = uuid.NewString()
	if _, err = start.StartStudioQuiz(ctx, f.actor, id, cmd); err == nil {
		t.Fatal("next question issued before previous completion")
	}
	current, err := store.CurrentSession(ctx)
	if err != nil || current.Session.ID != classroom {
		t.Fatal("quiz replaced current classroom", err)
	}
	sessions, err := store.ListSessions(ctx, learning.SessionQuery{GoalID: goal, Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, s := range sessions.Items {
		if s.Name == "偶数测验 · 第 1 题" {
			found = true
		}
	}
	if !found {
		t.Fatal("quiz session display label missing")
	}
	q := result.Questions[0]
	view, err := store.Session(ctx, q.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if view.WorkItem.Activity.Prompt != "解释偶数。" || len(view.WorkItem.Activity.AllowedHelp) != 2 {
		t.Fatal("frozen question changed")
	}
	action := func(kind tutoring.Action, help learning.HelpLevel) (learning.OperationResult, error) {
		a, _ := store.LoadSessionAuthority(ctx, q.SessionID)
		return service.ApplyAction(ctx, f.actor.Device.ID, q.SessionID, learning.ActionCommand{Operation: envelope(q.SessionID, a.Session.AggregateVer), Action: kind, Answer: "偶数可被 2 整除。", Help: help})
	}
	if _, err = action(tutoring.ActionPresentActivity, ""); err != nil {
		t.Fatal(err)
	}
	if _, err = action(tutoring.ActionSubmitAttempt, learning.HelpNone); err == nil {
		t.Fatal("none silently accepted")
	}
	a, _ := store.LoadSessionAuthority(ctx, q.SessionID)
	answer := learning.ActionCommand{Operation: envelope(q.SessionID, a.Session.AggregateVer), Action: tutoring.ActionSubmitAttempt, Answer: "偶数可被 2 整除。", Help: learning.HelpHint}
	answerCtx := content.WithAnswer(ctx, f.actor, q.ContentArtifactID, 1)
	if _, err = service.ApplyAction(answerCtx, f.actor.Device.ID, q.SessionID, answer); err != nil {
		t.Fatal(err)
	}
	if _, err = service.ApplyAction(answerCtx, f.actor.Device.ID, q.SessionID, answer); err != nil {
		t.Fatal(err)
	}
	var attempts int
	if err = f.pool.QueryRow(ctx, `SELECT count(*) FROM learning_attempts WHERE activity_id=$1`, q.ActivityID).Scan(&attempts); err != nil || attempts != 1 {
		t.Fatal("response loss duplicated attempt", attempts, err)
	}
	for question := 1; question <= 2; question++ {
		if question == 2 {
			result, err = start.StartStudioQuiz(ctx, f.actor, id, cmd)
			if err != nil {
				t.Fatal("second question", err)
			}
			q = result.Questions[1]
			if _, err = action(tutoring.ActionPresentActivity, ""); err != nil {
				t.Fatal(err)
			}
			a, _ = store.LoadSessionAuthority(ctx, q.SessionID)
			answer = learning.ActionCommand{Operation: envelope(q.SessionID, a.Session.AggregateVer), Action: tutoring.ActionSubmitAttempt, Answer: "4 能被 2 整除。", Help: learning.HelpNone}
			if _, err = service.ApplyAction(content.WithAnswer(ctx, f.actor, q.ContentArtifactID, 1), f.actor.Device.ID, q.SessionID, answer); err != nil {
				t.Fatal(err)
			}
		}
		view, err = store.Session(ctx, q.SessionID)
		if err != nil {
			t.Fatal(err)
		}
		activity, attempt := *view.WorkItem.Activity, *view.WorkItem.Attempt
		ref := activity.References[0]
		item := learning.AssessmentItem{RubricItemID: "definition", Conclusion: learning.ConclusionPass, AnswerQuote: attempt.Answer, AnswerRange: learning.SourceRange{Start: 0, End: len(attempt.Answer)}, AnswerQuoteSHA256: learning.SHA256([]byte(attempt.Answer)), KnowledgeReferenceID: ref.NodeRevisionID, KnowledgeQuote: ref.Slice, KnowledgeRange: learning.SourceRange{Start: 0, End: len(ref.Slice)}, KnowledgeQuoteSHA256: learning.SHA256([]byte(ref.Slice))}
		raw, _ := json.Marshal(map[string]any{"assessment": map[string]any{"items": []learning.AssessmentItem{item}, "rubric_complete": true, "confidence": 1000, "risk_flags": []string{}}})
		service, err = learning.NewService(store, store, learningknowledge.New(ks), learning.ServiceOptions{Model: quizAssessmentModel(raw), ModelID: "fixture"})
		if err != nil {
			t.Fatal(err)
		}
		proposal, err := service.Propose(ctx, f.actor.Device.ID, learning.ProposalRequest{RequestID: uuid.NewString(), Type: learning.ProposalAssessment, AggregateType: "session", AggregateID: q.SessionID, AggregateVersion: view.Session.AggregateVer, KnowledgeRevisionID: activity.KnowledgeRevisionID, NodeRevisionIDs: []string{ref.NodeRevisionID}, Input: json.RawMessage(`{}`)})
		if err != nil {
			t.Fatal(err)
		}
		if _, err = service.ApplyAction(ctx, f.actor.Device.ID, q.SessionID, learning.ActionCommand{Operation: envelope(q.SessionID, view.Session.AggregateVer), Action: tutoring.ActionRecordAssessment, ProposalID: proposal.ID}); err != nil {
			t.Fatal(err)
		}
		feedback, err := service.Feedback(ctx, attempt.ID, false)
		if err != nil || feedback.Assessment == nil || len(feedback.Evidence) != 1 {
			t.Fatal("formal feedback/evidence missing", err)
		}
		if _, err = action(tutoring.ActionAcknowledgeFeedback, ""); err != nil {
			t.Fatal(err)
		}
		state, err := start.StudioQuiz(ctx, f.actor, id, 1)
		if err != nil || !state.Questions[question-1].Completed || state.Questions[question-1].AttemptID != attempt.ID {
			t.Fatal("completion not read from actual results", err)
		}
	}
	progress, err := store.Progress(ctx, learning.ProgressQuery{GoalID: goal, Status: "all", Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if len(progress.Items) != 1 || progress.Items[0].EvidenceCount != 2 {
		t.Fatalf("progress evidence differs: %+v", progress)
	}
	if _, err = store.Rebuild(ctx); err != nil {
		t.Fatal(err)
	}
	progress, err = store.Progress(ctx, learning.ProgressQuery{GoalID: goal, Status: "all", Limit: 100})
	if err != nil || progress.Items[0].EvidenceCount != 2 {
		t.Fatal("rebuild changed evidence", err)
	}
	current, err = store.CurrentSession(ctx)
	if err != nil || current.Session.ID != classroom || current.Session.AggregateVer != classroomBefore.Session.AggregateVer {
		t.Fatal("classroom changed", err)
	}
	// Rotate both encrypted quiz revisions and formal content, preserving mappings.
	var automaticTasks int
	if err = f.pool.QueryRow(ctx, `SELECT count(*) FROM learning_assessment_tasks`).Scan(&automaticTasks); err != nil || automaticTasks != 0 {
		t.Fatal("legacy Studio silently opted into automatic assessment", automaticTasks, err)
	}
	key := bytes.Repeat([]byte{43}, 32)
	rewrap, _ := keyrotation.New(bytes.Repeat([]byte{42}, 32), key)
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, `LOCK TABLE learning_content_revisions IN ACCESS EXCLUSIVE MODE; SET LOCAL app.mentor_key_rotation='on'`); err != nil {
		t.Fatal(err)
	}
	if err = learningcontent.RotateKeyTx(ctx, tx, rewrap); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	rotated, _ := learningcontent.New(f.pool, store, key)
	rotated.ConfigureStudioSources(k)
	rotated.ConfigureReferences(k)
	start.Content = rotated
	if state, err := start.StudioQuiz(ctx, f.actor, id, 1); err != nil || len(state.Questions) != 2 {
		t.Fatal("rotation lost quiz", err)
	}
	if _, err = rotated.Get(ctx, f.actor, q.ContentArtifactID, 1); err != nil {
		t.Fatal("rotation lost formal content", err)
	}
	// Global scrub must erase associations as well as encrypted payloads.
	tutor := tutoringpg.New(f.pool)
	privacyStore := privacypg.New(f.pool, privacypg.WithLocalOwner(identitypg.New(f.pool)), privacypg.WithLocalOwner(k), privacypg.WithLocalOwner(store), privacypg.WithLocalOwner(tutor), privacypg.WithLocalOwner(memorypg.New(f.pool)), privacypg.WithLocalOwner(outboxpg.New(f.pool)))
	grants, _ := privacy.NewErasureGrantService(privacypg.NewGrantStore(f.pool), privacy.ErasureGrantOptions{})
	grant, err := grants.Issue(ctx, f.actor.Device.ID, "测验清除验收")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	barrier, err := privacyStore.CommitBarrierAuthorized(ctx, privacy.ErasureRequest{DeviceID: f.actor.Device.ID, OperationID: uuid.NewString(), ActorDeviceID: f.actor.Device.ID, ReasonCode: string(privacy.ReasonLearnerRequest), RequestedAt: now, ManagedBackupUnrecoverableAfter: now.Add(24 * time.Hour), ExpectedCurrentLearnerGeneration: 1}, privacy.NewErasureGrantAuthorization(f.actor.Device.ID, grant.Token))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = privacyStore.RunLocalScrub(ctx, barrier.ErasureID); err != nil {
		t.Fatal(err)
	}
	if _, err = start.StudioQuiz(ctx, f.actor, id, 1); err == nil {
		t.Fatal("erased quiz readable")
	}
	if _, err = start.StartStudioQuiz(ctx, f.actor, id, cmd); err == nil {
		t.Fatal("old start resurrected quiz")
	}
	var remaining int
	if err = f.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM learning_studio_quiz_questions)+(SELECT count(*) FROM learning_studio_quiz_operations)`).Scan(&remaining); err != nil || remaining != 0 {
		t.Fatal("mapping survived scrub", remaining, err)
	}
}

type quizAssessmentModel json.RawMessage

func (m quizAssessmentModel) Generate(context.Context, learning.ProposalRequest) (json.RawMessage, error) {
	return json.RawMessage(m), nil
}
