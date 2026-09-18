package postgresstore_test

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	identitydb "github.com/edu-agent/edu-agent/server/internal/identity/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/integrations/learningknowledge"
	"github.com/edu-agent/edu-agent/server/internal/knowledge"
	knowledgedb "github.com/edu-agent/edu-agent/server/internal/knowledge/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/learning"
	"github.com/edu-agent/edu-agent/server/internal/learningspace"
	memorydb "github.com/edu-agent/edu-agent/server/internal/memory/postgresstore"
	outboxdb "github.com/edu-agent/edu-agent/server/internal/platform/outbox/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/privacy"
	privacydb "github.com/edu-agent/edu-agent/server/internal/privacy/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/tutoring"
	tutoringdb "github.com/edu-agent/edu-agent/server/internal/tutoring/postgresstore"
	"github.com/google/uuid"
)

func TestPostgreSQLReviewCarrierPreservesSourceAndReplay(t *testing.T) {
	pool, store := newA101WorkItemStore(t)
	originalID, _ := commitLearningAuthorityFixture(t, store, false, true)
	ctx := context.Background()
	service, err := learning.NewService(store, store, integrationKnowledgeResolver{}, learning.ServiceOptions{Now: func() time.Time { return time.Now().Add(-72 * time.Hour) }})
	if err != nil {
		t.Fatal(err)
	}
	for _, action := range []tutoring.Action{tutoring.ActionPresentActivity, tutoring.ActionSubmitAttempt, tutoring.ActionRecordAssessment, tutoring.ActionAcknowledgeFeedback} {
		view, e := store.Session(ctx, originalID)
		if e != nil {
			t.Fatal(e)
		}
		_, e = service.ApplyAction(ctx, learningDeviceOne, originalID, learning.ActionCommand{Operation: learning.OperationEnvelope{OperationID: uuid.NewString(), PayloadSchemaVersion: 1, AggregateType: "session", AggregateID: originalID, ExpectedVersion: view.Session.AggregateVer, Payload: json.RawMessage(`{}`)}, Action: action, Answer: "ok", Help: learning.HelpNone})
		if e != nil {
			t.Fatal(action, e)
		}
	}
	page, err := store.Reviews(ctx, learning.ReviewQuery{Page: learning.CursorPageRequest{Limit: 10}})
	if err != nil || len(page.Items) != 1 {
		t.Fatalf("没有真实到期复习：%+v %v", page, err)
	}
	review := page.Items[0]
	before, err := store.LoadSessionAuthority(ctx, originalID)
	if err != nil {
		t.Fatal(err)
	}
	command := learning.SessionCommand{Operation: learning.OperationEnvelope{OperationID: uuid.NewString(), PayloadSchemaVersion: 1, AggregateType: "session", AggregateID: uuid.NewString(), Payload: json.RawMessage(`{}`)}, GoalRevisionID: review.GoalRevisionID, ReviewSource: &learning.ReviewSessionSource{TaskID: review.TaskID, EvidenceID: review.EvidenceID, AttemptID: review.AttemptID}}
	other, _ := learningspace.WithScope(ctx, uuid.NewString())
	if _, err = service.CreateSession(other, learningDeviceOne, command); learning.ErrorCode(err) != learning.CodeNotFound {
		t.Fatalf("跨区创建未拒绝：%v", err)
	}
	result, err := service.CreateSession(ctx, learningDeviceOne, command)
	if err != nil {
		t.Fatal(err)
	}
	if replay, e := service.CreateSession(ctx, learningDeviceOne, command); e != nil || !replay.Replayed || replay.AggregateID != result.AggregateID {
		t.Fatalf("丢响应重试未幂等：%+v %v", replay, e)
	}
	after, err := store.LoadSessionAuthority(ctx, originalID)
	if err != nil || !reflect.DeepEqual(before.Session, after.Session) {
		t.Fatalf("改写了原会话：%v", err)
	}
	carrier, err := store.Session(ctx, command.Operation.AggregateID)
	if err != nil || carrier.Session.State != tutoring.StateRouteActive || carrier.Session.Context.RouteRevisionID != review.RouteRevisionID || carrier.Session.Context.FocusNodeRevisionID != review.NodeRevisionID {
		t.Fatalf("承载未保持原版本：%+v %v", carrier, err)
	}
	assertCount(t, pool, `SELECT count(*) FROM learning_evidence`, 1)
	assertCount(t, pool, `SELECT count(*) FROM learning_review_sessions`, 1)
	duplicate := command
	duplicate.Operation.OperationID, duplicate.Operation.AggregateID = uuid.NewString(), uuid.NewString()
	if _, err = service.CreateSession(ctx, learningDeviceOne, duplicate); learning.ErrorCode(err) != learning.CodeInvalidTransition {
		t.Fatalf("重复进入创建多个承载：%v", err)
	}
	finishedCarrier := carrier.Session.ID
	if _, err = service.ApplyAction(ctx, learningDeviceOne, finishedCarrier, learning.ActionCommand{Operation: learning.OperationEnvelope{OperationID: uuid.NewString(), PayloadSchemaVersion: 1, AggregateType: "session", AggregateID: finishedCarrier, ExpectedVersion: carrier.Session.AggregateVer, Payload: json.RawMessage(`{}`)}, Action: tutoring.ActionCompleteSession}); err != nil {
		t.Fatal(err)
	}
	finishedPage, err := store.Reviews(ctx, learning.ReviewQuery{Page: learning.CursorPageRequest{Limit: 10}})
	if err != nil || len(finishedPage.Items) != 1 || finishedPage.Items[0].CarrierSessionID != "" {
		t.Fatalf("已完成承载仍伪装成继续入口：%+v %v", finishedPage, err)
	}
	duplicate.Operation.OperationID, duplicate.Operation.AggregateID = uuid.NewString(), uuid.NewString()
	if _, err = service.CreateSession(ctx, learningDeviceOne, duplicate); err != nil {
		t.Fatalf("旧承载完成后不能明确新建：%v", err)
	}
	carrier, err = store.Session(ctx, duplicate.Operation.AggregateID)
	if err != nil {
		t.Fatal(err)
	}
	assertCount(t, pool, `SELECT count(*) FROM learning_review_sessions`, 2)
	assertCount(t, pool, `SELECT count(*) FROM learning_evidence`, 1)
	read := func() learning.ReviewsPage {
		p, e := store.Reviews(ctx, learning.ReviewQuery{Page: learning.CursorPageRequest{Limit: 10}})
		if e != nil || len(p.Items) != 1 || p.Items[0].SessionID != originalID || p.Items[0].CarrierSessionID != carrier.Session.ID || !p.Items[0].DueAt.Equal(review.DueAt) {
			t.Fatalf("新承载改变原任务或调度：%+v %v", p, e)
		}
		return p
	}
	incremental := read()
	if _, err = store.Rebuild(ctx); err != nil {
		t.Fatal(err)
	}
	replayed := read()
	if !reflect.DeepEqual(incremental.Items, replayed.Items) {
		t.Fatal("承载重放与增量不一致")
	}
	goal, err := store.LoadGoalRevision(ctx, review.GoalRevisionID)
	if err != nil {
		t.Fatal(err)
	}
	pause := goalCommand(goal, goal.Text)
	pause.Action = "pause"
	if _, err = service.CreateGoal(ctx, learningDeviceOne, pause); err != nil {
		t.Fatal(err)
	}
	hidden, err := store.Reviews(ctx, learning.ReviewQuery{Page: learning.CursorPageRequest{Limit: 10}})
	if err != nil || hidden.Total != 0 {
		t.Fatalf("暂停未隐藏：%+v %v", hidden, err)
	}
	paused, err := store.Reviews(ctx, learning.ReviewQuery{Status: "paused", Page: learning.CursorPageRequest{Limit: 10}})
	if err != nil || paused.Total != 1 || paused.Items[0].Startable || !paused.Items[0].DueAt.Equal(review.DueAt) {
		t.Fatalf("暂停删除或重排任务：%+v %v", paused, err)
	}
	duplicate.Operation.OperationID, duplicate.Operation.AggregateID = uuid.NewString(), uuid.NewString()
	if _, err = service.CreateSession(ctx, learningDeviceOne, duplicate); learning.ErrorCode(err) != learning.CodeInvalidTransition {
		t.Fatalf("暂停后仍创建任务：%v", err)
	}
	pausedGoal, err := store.GetGoal(ctx, goal.GoalID)
	if err != nil {
		t.Fatal(err)
	}
	resume := goalCommand(pausedGoal, pausedGoal.Text)
	resume.Action = "resume"
	if _, err = service.CreateGoal(ctx, learningDeviceOne, resume); err != nil {
		t.Fatal(err)
	}
	knowledgeService, err := knowledge.NewService(knowledgedb.New(pool), knowledge.NewCanonicalizer(), knowledge.ServiceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	writer, err := learning.NewService(store, store, learningknowledge.New(knowledgeService), learning.ServiceOptions{Model: reviewObjectiveModel{}, ModelID: "复习验收模型", PromptRevision: "review-test-v1"})
	if err != nil {
		t.Fatal(err)
	}
	proposal, err := writer.Propose(ctx, learningDeviceOne, learning.ProposalRequest{RequestID: uuid.NewString(), Type: learning.ProposalActivity, AggregateType: "session", AggregateID: carrier.Session.ID, AggregateVersion: carrier.Session.AggregateVer, KnowledgeRevisionID: review.KnowledgeRevisionID, NodeRevisionIDs: []string{review.NodeRevisionID}, Input: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	for _, action := range []tutoring.Action{tutoring.ActionPresentReview, tutoring.ActionPresentActivity, tutoring.ActionSubmitAttempt, tutoring.ActionRecordAssessment} {
		current, e := store.Session(ctx, carrier.Session.ID)
		if e != nil {
			t.Fatal(e)
		}
		cmd := learning.ActionCommand{Operation: learning.OperationEnvelope{OperationID: uuid.NewString(), PayloadSchemaVersion: 1, AggregateType: "session", AggregateID: carrier.Session.ID, ExpectedVersion: current.Session.AggregateVer, Payload: json.RawMessage(`{}`)}, Action: action, Answer: "复习正确", Help: learning.HelpNone}
		if action == tutoring.ActionPresentReview {
			cmd.ProposalID = proposal.ID
		}
		if _, e = writer.ApplyAction(ctx, learningDeviceOne, carrier.Session.ID, cmd); e != nil {
			t.Fatal(action, e)
		}
	}
	answered, err := store.Session(ctx, carrier.Session.ID)
	if err != nil {
		t.Fatal(err)
	}
	feedback, err := writer.Feedback(ctx, answered.WorkItem.Attempt.ID, false)
	if err != nil || len(feedback.Evidence) != 1 || feedback.Evidence[0].Kind != learning.EvidenceReviewRecall {
		t.Fatalf("未形成独立复习证据：%+v %v", feedback, err)
	}
	originalFeedback, err := writer.Feedback(ctx, review.AttemptID, false)
	if err != nil || originalFeedback.Activity.ID == feedback.Activity.ID || originalFeedback.Attempt.Answer != "ok" {
		t.Fatalf("复习复用了旧活动或改写了旧答案：%v", err)
	}
	if feedback.Activity.KnowledgeRevisionID != review.KnowledgeRevisionID || feedback.Activity.RouteRevisionID != review.RouteRevisionID {
		t.Fatal("复习使用了其他来源版本")
	}
	assertCount(t, pool, `SELECT count(*) FROM learning_evidence`, 2)
	privacyStore := privacydb.New(pool, privacydb.WithLocalOwner(identitydb.New(pool)), privacydb.WithLocalOwner(knowledgedb.New(pool)), privacydb.WithLocalOwner(store), privacydb.WithLocalOwner(tutoringdb.New(pool)), privacydb.WithLocalOwner(memorydb.New(pool)), privacydb.WithLocalOwner(outboxdb.New(pool)))
	now := time.Now().UTC()
	barrier, err := privacyStore.CommitBarrier(ctx, privacy.ErasureRequest{DeviceID: learningDeviceOne, ActorDeviceID: learningDeviceOne, OperationID: uuid.NewString(), ReasonCode: "learner_request", RequestedAt: now, ManagedBackupUnrecoverableAfter: now.Add(24 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = privacyStore.RunLocalScrub(ctx, barrier.ErasureID); err != nil {
		t.Fatal(err)
	}
	assertCount(t, pool, `SELECT count(*) FROM learning_review_sessions`, 0)
	clearedProgress, err := store.Progress(ctx, learning.ProgressQuery{Global: true, Status: "all", Limit: 10})
	if err != nil || !clearedProgress.DataCleared || clearedProgress.Total != 0 {
		t.Fatalf("清除后的进度被误报为普通空结果：%+v %v", clearedProgress, err)
	}
	clearedReviews, err := store.Reviews(ctx, learning.ReviewQuery{Global: true, Status: "all", Page: learning.CursorPageRequest{Limit: 10}})
	if err != nil || !clearedReviews.DataCleared || clearedReviews.Total != 0 {
		t.Fatalf("清除后的复习被误报为普通空结果：%+v %v", clearedReviews, err)
	}
	if _, err = service.Feedback(ctx, review.AttemptID, false); err == nil {
		t.Fatal("清除后恢复了原答案")
	}
	if _, err = service.CreateSession(ctx, learningDeviceOne, command); err == nil {
		t.Fatal("清除后旧承载操作复活")
	}
}

type reviewObjectiveModel struct{}

func (reviewObjectiveModel) Generate(_ context.Context, request learning.ProposalRequest) (json.RawMessage, error) {
	return json.Marshal(map[string]any{"activity": learning.ActivityProposal{Prompt: "请再次辨认 topic，并按要求回答复习正确。", Type: learning.ActivityObjective, Rubric: learning.Rubric{Revision: "review-test-v1", Items: []learning.RubricItem{{ID: "review", Criterion: "独立回忆概念"}}, ObjectiveRule: &learning.ObjectiveRule{AcceptedAnswers: []string{"复习正确"}, TrimSpace: true}}, Difficulty: 1, AllowedHelp: []learning.HelpLevel{learning.HelpNone}, References: []learning.KnowledgeReference{{NodeRevisionID: request.NodeRevisionIDs[0]}}}})
}
