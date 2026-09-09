package postgresstore_test

import (
	"context"
	"encoding/json"
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/integrations/learningknowledge"
	"github.com/edu-agent/edu-agent/server/internal/knowledge"
	knowledgedb "github.com/edu-agent/edu-agent/server/internal/knowledge/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/learning"
	learningdb "github.com/edu-agent/edu-agent/server/internal/learning/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/learningspace"
	spacedb "github.com/edu-agent/edu-agent/server/internal/learningspace/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/tutoring"
	tutoringdb "github.com/edu-agent/edu-agent/server/internal/tutoring/postgresstore"
	"github.com/google/uuid"
)

func TestPostgreSQLExplicitSessionsRemainIndependent(t *testing.T) {
	pool := learningIntegrationPool(t)
	ctx := context.Background()
	actor := uuid.NewString()
	if _, err := pool.Exec(ctx, `INSERT INTO devices(id,display_name,created_at) VALUES($1,'教学续学测试',now())`, actor); err != nil {
		t.Fatal(err)
	}
	store := learningdb.New(pool, tutoringdb.New(pool), knowledgedb.New(pool))
	if _, err := store.Rebuild(ctx); err != nil {
		t.Fatal(err)
	}
	service, err := learning.NewService(store, store, integrationKnowledgeResolver{}, learning.ServiceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	spaces := spacedb.New(pool)
	sp, err := spaces.Mutate(ctx, actor, "", learningspace.Command{OperationID: uuid.NewString(), Name: "英语", Status: "active"})
	if err != nil {
		t.Fatal(err)
	}
	other, _ := learningspace.WithScope(ctx, sp.ID)
	create := func(c context.Context, name string) (learning.GoalRevision, string) {
		t.Helper()
		result, err := service.CreateGoal(c, actor, goalCommand(learning.GoalRevision{}, name))
		if err != nil {
			t.Fatal(err)
		}
		var g learning.GoalRevision
		if err := json.Unmarshal(result.Result, &g); err != nil {
			t.Fatal(err)
		}
		id := uuid.NewString()
		cmd := learning.SessionCommand{GoalRevisionID: g.ID, Operation: learning.OperationEnvelope{OperationID: uuid.NewString(), PayloadSchemaVersion: 1, AggregateType: "session", AggregateID: id, Payload: json.RawMessage(`{}`)}}
		if _, err := service.CreateSession(c, actor, cmd); err != nil {
			t.Fatal(err)
		}
		if _, err := service.CreateSession(c, actor, cmd); err != nil {
			t.Fatal(err)
		}
		return g, id
	}
	ga, a := create(ctx, "Go 并发")
	_, b := create(other, "英语口语")
	gc, c := create(ctx, "Go 面试")
	for _, id := range []string{a, c} {
		view, err := service.Session(ctx, id)
		if err != nil || view.Session.ID != id || view.Session.State != tutoring.StateGoalReady {
			t.Fatalf("指定续学失败：%+v %v", view, err)
		}
	}
	if _, err := service.Session(ctx, b); learning.ErrorCode(err) != learning.CodeNotFound {
		t.Fatalf("跨区读取未拒绝：%v", err)
	}
	page, err := service.ListSessions(ctx, learning.SessionQuery{Limit: 100})
	if err != nil || len(page.Items) != 2 {
		t.Fatalf("默认区列表=%+v %v", page, err)
	}
	page, err = service.ListSessions(other, learning.SessionQuery{Limit: 100})
	if err != nil || len(page.Items) != 1 || page.Items[0].SessionID != b {
		t.Fatalf("英语列表=%+v %v", page, err)
	}
	page, err = service.ListSessions(ctx, learning.SessionQuery{GoalID: ga.GoalID, Limit: 100})
	if err != nil || len(page.Items) != 1 || page.Items[0].SessionID != a {
		t.Fatalf("目标列表=%+v %v", page, err)
	}
	if _, err := service.ListSessions(other, learning.SessionQuery{GoalID: ga.GoalID, Limit: 100}); learning.ErrorCode(err) != learning.CodeNotFound {
		t.Fatalf("跨区目标筛选未拒绝：%v", err)
	}
	view, err := service.CurrentSession(ctx)
	if err != nil || view.Session.ID != c {
		t.Fatalf("默认入口跨区：%s %v", view.Session.ID, err)
	}
	selected, err := service.Session(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	action := learning.ActionCommand{Operation: learning.OperationEnvelope{OperationID: uuid.NewString(), PayloadSchemaVersion: 1, AggregateType: "session", AggregateID: a, ExpectedVersion: selected.Session.AggregateVer, Payload: json.RawMessage(`{}`)}, Action: tutoring.ActionStartDiagnostic}
	if _, err := service.ApplyAction(other, actor, a, action); learning.ErrorCode(err) != learning.CodeNotFound {
		t.Fatalf("跨区写入未拒绝：%v", err)
	}
	if _, err := service.ApplyAction(ctx, actor, a, action); err != nil {
		t.Fatal(err)
	}
	if _, err := service.ApplyAction(ctx, actor, a, action); err != nil {
		t.Fatal(err)
	}
	action.Operation.OperationID = uuid.NewString()
	if _, err := service.ApplyAction(ctx, actor, a, action); learning.ErrorCode(err) != learning.CodeVersionConflict {
		t.Fatalf("同会话竞争未拒绝：%v", err)
	}
	currentA, err := service.Session(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	replacement := learning.ActionCommand{Operation: learning.OperationEnvelope{OperationID: uuid.NewString(), PayloadSchemaVersion: 1, AggregateType: "session", AggregateID: a, ExpectedVersion: currentA.Session.AggregateVer, Payload: json.RawMessage(`{}`)}, Action: tutoring.ActionSwitchGoal, GoalRevisionID: gc.ID}
	if _, err := service.ApplyAction(ctx, actor, a, replacement); learning.ErrorCode(err) != learning.CodeInvalidTransition {
		t.Fatalf("跨目标替换旧会话未拒绝：%v", err)
	}
	if _, err := store.Rebuild(ctx); err != nil {
		t.Fatal(err)
	}
	view, err = service.Session(ctx, a)
	if err != nil || view.Session.State != tutoring.StateDiagnostic {
		t.Fatalf("重放改变会话：%+v %v", view, err)
	}
}

type sessionFixtureModel struct{}

func (sessionFixtureModel) Generate(_ context.Context, r learning.ProposalRequest) (json.RawMessage, error) {
	if r.Type == learning.ProposalRoute {
		return json.Marshal(map[string]any{"route": []learning.RouteProposalStep{{NodeRevisionID: r.NodeRevisionIDs[0], TeachingIntent: "理解资料", CompletionCondition: "解释概念"}}})
	}
	if r.Type == learning.ProposalFreeAnswer {
		return json.Marshal(map[string]any{"text": learning.TextProposal{Text: "依据当前资料继续解释", References: []learning.KnowledgeReference{{NodeRevisionID: r.NodeRevisionIDs[0]}}}})
	}
	if r.Type == learning.ProposalAssessment {
		return json.Marshal(map[string]any{"assessment": map[string]any{"items": []learning.AssessmentItem{{RubricItemID: "concept", Conclusion: learning.ConclusionUnassessed}}, "rubric_complete": true, "confidence": 500, "risk_flags": []string{}}})
	}
	return json.Marshal(map[string]any{"activity": learning.ActivityProposal{Prompt: "解释所选资料中的概念", Type: learning.ActivityOpen, Rubric: learning.Rubric{Revision: "fixture-v1", Items: []learning.RubricItem{{ID: "concept", Criterion: "准确解释"}}}, Difficulty: 2, AllowedHelp: []learning.HelpLevel{learning.HelpNone}, References: []learning.KnowledgeReference{{NodeRevisionID: r.NodeRevisionIDs[0]}}}})
}

func TestPostgreSQLScopedSessionMaterialsAndFocusSurviveSwitch(t *testing.T) {
	pool := learningIntegrationPool(t)
	ctx := context.Background()
	actor := uuid.NewString()
	if _, err := pool.Exec(ctx, `INSERT INTO devices(id,display_name,created_at) VALUES($1,'多方向教学',now())`, actor); err != nil {
		t.Fatal(err)
	}
	kstore := knowledgedb.New(pool)
	ks, err := knowledge.NewService(kstore, knowledge.NewCanonicalizer(), knowledge.ServiceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	store := learningdb.New(pool, tutoringdb.New(pool), kstore)
	if _, err := store.Rebuild(ctx); err != nil {
		t.Fatal(err)
	}
	service, err := learning.NewService(store, store, learningknowledge.New(ks), learning.ServiceOptions{Model: sessionFixtureModel{}, ModelID: "fixture", PromptRevision: "fixture-v1"})
	if err != nil {
		t.Fatal(err)
	}
	spaces := spacedb.New(pool)
	type direction struct {
		ctx   context.Context
		view  learning.SessionView
		goal  learning.GoalRevision
		node  string
		scope string
	}
	var directions []direction
	for _, name := range []string{"Go", "英语"} {
		sp, err := spaces.Mutate(ctx, actor, "", learningspace.Command{OperationID: uuid.NewString(), Name: name, Status: "active"})
		if err != nil {
			t.Fatal(err)
		}
		scoped, _ := learningspace.WithScope(ctx, sp.ID)
		collection := uuid.NewString()
		if _, err := ks.ChangeCollection(scoped, knowledge.CollectionCommand{Action: "create", ID: collection, Name: name, Source: "fixture"}); err != nil {
			t.Fatal(err)
		}
		withCollection, _ := knowledge.WithCollection(scoped, collection)
		imported, err := ks.Import(withCollection, knowledge.ImportCommand{OperationID: uuid.NewString(), ActorDeviceID: actor, Source: "fixture", ExpectedParentProvided: true, Documents: []knowledge.ImportDocument{{Path: "README.md", Markdown: "# " + name + "\n独立方向的学习资料。\n"}}})
		if err != nil {
			t.Fatal(err)
		}
		scope, err := ks.FreezeScope(scoped, knowledge.ScopeSnapshot{ID: uuid.NewString(), Entries: []knowledge.ScopeEntry{{CollectionID: collection, RevisionID: imported.Revision.ID}}})
		if err != nil {
			t.Fatal(err)
		}
		command := goalCommand(learning.GoalRevision{}, name)
		command.Details = &learning.GoalDetails{Name: name, ScopeSnapshotID: scope.ID}
		result, err := service.CreateGoal(scoped, actor, command)
		if err != nil {
			t.Fatal(err)
		}
		var goal learning.GoalRevision
		if err := json.Unmarshal(result.Result, &goal); err != nil {
			t.Fatal(err)
		}
		id := uuid.NewString()
		if _, err := service.CreateSession(scoped, actor, learning.SessionCommand{GoalRevisionID: goal.ID, Operation: learning.OperationEnvelope{OperationID: uuid.NewString(), PayloadSchemaVersion: 1, AggregateType: "session", AggregateID: id, Payload: json.RawMessage(`{}`)}}); err != nil {
			t.Fatal(err)
		}
		view, err := service.Session(scoped, id)
		if err != nil {
			t.Fatal(err)
		}
		directions = append(directions, direction{ctx: scoped, view: view, goal: goal, node: imported.Revision.Documents[0].Revision.Nodes[0].ID, scope: scope.ID})
	}
	apply := func(d *direction, action tutoring.Action, proposal string) {
		t.Helper()
		_, err := service.ApplyAction(d.ctx, actor, d.view.Session.ID, learning.ActionCommand{Operation: learning.OperationEnvelope{OperationID: uuid.NewString(), PayloadSchemaVersion: 1, AggregateType: "session", AggregateID: d.view.Session.ID, ExpectedVersion: d.view.Session.AggregateVer, Payload: json.RawMessage(`{}`)}, Action: action, ProposalID: proposal})
		if err != nil {
			t.Fatalf("动作 %s：%v", action, err)
		}
		d.view, err = service.Session(d.ctx, d.view.Session.ID)
		if err != nil {
			t.Fatal(err)
		}
	}
	for i := range directions {
		d := &directions[i]
		apply(d, tutoring.ActionStartDiagnostic, "")
		for _, kind := range []learning.ProposalType{learning.ProposalRoute, learning.ProposalActivity} {
			proposal, err := service.Propose(d.ctx, actor, learning.ProposalRequest{RequestID: uuid.NewString(), Type: kind, AggregateType: "session", AggregateID: d.view.Session.ID, AggregateVersion: d.view.Session.AggregateVer, KnowledgeRevisionID: d.scope, NodeRevisionIDs: []string{d.node}, Input: json.RawMessage(`{}`)})
			if err != nil {
				t.Fatalf("提案 %s：%v", kind, err)
			}
			action := tutoring.ActionApplyRoute
			if kind == learning.ProposalActivity {
				action = tutoring.ActionIssueActivity
			}
			apply(d, action, proposal.ID)
		}
		apply(d, tutoring.ActionPresentActivity, "")
	}
	for _, d := range directions {
		fresh, err := service.Session(d.ctx, d.view.Session.ID)
		if err != nil || !reflect.DeepEqual(fresh.WorkItem, d.view.WorkItem) || fresh.Session.State != tutoring.StateAwaitingResponse {
			t.Fatalf("切换丢失题目：%v", err)
		}
		if fresh.WorkItem.Activity.KnowledgeRevisionID != d.scope || fresh.WorkItem.GoalRevision.ID != d.goal.ID {
			t.Fatal("题目资料或目标发生串用")
		}
	}
	if _, err := service.Session(directions[1].ctx, directions[0].view.Session.ID); learning.ErrorCode(err) != learning.CodeNotFound {
		t.Fatalf("跨区会话未拒绝：%v", err)
	}
	d := &directions[0]
	signer := offlineIntegrationSigner(t)
	offline, err := learning.NewOfflineService(store, signer, signer.Origin(), time.Now)
	if err != nil {
		t.Fatal(err)
	}
	prepare := learning.OfflinePrepareRequest{OperationID: uuid.NewString(), SessionID: d.view.Session.ID, PayloadSchemaVersion: 1, ExpectedSessionVersion: strconv.FormatInt(d.view.Session.AggregateVer, 10), TrustedManifestRevision: "0", TrustedManifestDigest: learning.OfflineZeroDigest}
	prepared, err := offline.Prepare(d.ctx, actor, prepare)
	if err != nil {
		t.Fatalf("非默认区冻结资料离线签发：%v", err)
	}
	var pack learning.OfflinePackPayloadV1
	if err := json.Unmarshal(prepared.Pack.Payload, &pack); err != nil || pack.ParentSessionID != d.view.Session.ID || len(pack.Items) != 1 {
		t.Fatalf("冻结范围签发错误：%+v %v", pack, err)
	}
	if _, err := offline.Prepare(directions[1].ctx, actor, prepare); learning.ErrorCode(err) != learning.CodeNotFound {
		t.Fatalf("签发重放跨区未拒绝：%v", err)
	}
	operation := func() learning.OperationEnvelope {
		return learning.OperationEnvelope{OperationID: uuid.NewString(), PayloadSchemaVersion: 1, AggregateType: "session", AggregateID: d.view.Session.ID, ExpectedVersion: d.view.Session.AggregateVer, Payload: json.RawMessage(`{}`)}
	}
	if _, err := service.ApplyAction(d.ctx, actor, d.view.Session.ID, learning.ActionCommand{Operation: operation(), Action: tutoring.ActionAskFreeQuestion, Question: "能再解释吗？"}); err != nil {
		t.Fatal(err)
	}
	d.view, err = service.Session(d.ctx, d.view.Session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if d.view.Session.ActiveFrame == nil || d.view.Session.ActiveFrame.SavedState != tutoring.StateAwaitingResponse {
		t.Fatal("自由问答未保存原题")
	}
	questionView := d.view
	if _, err := service.Session(directions[1].ctx, directions[1].view.Session.ID); err != nil {
		t.Fatal(err)
	}
	d.view, err = service.Session(d.ctx, d.view.Session.ID)
	if err != nil || !reflect.DeepEqual(questionView.WorkItem, d.view.WorkItem) {
		t.Fatalf("自由问答切换丢失上下文：%v", err)
	}
	proposal, err := service.Propose(d.ctx, actor, learning.ProposalRequest{RequestID: uuid.NewString(), Type: learning.ProposalFreeAnswer, AggregateType: "session", AggregateID: d.view.Session.ID, AggregateVersion: d.view.Session.AggregateVer, KnowledgeRevisionID: d.scope, NodeRevisionIDs: []string{d.node}, Input: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	apply(d, tutoring.ActionRecordFreeAnswer, proposal.ID)
	apply(d, tutoring.ActionResumeFocus, "")
	if d.view.Session.State != tutoring.StateAwaitingResponse {
		t.Fatal("返回原题失败")
	}
	answer := learning.ActionCommand{Operation: operation(), Action: tutoring.ActionSubmitAttempt, Answer: "我的解释", Help: learning.HelpNone}
	if _, err := service.ApplyAction(d.ctx, actor, d.view.Session.ID, answer); err != nil {
		t.Fatal(err)
	}
	if _, err := service.ApplyAction(d.ctx, actor, d.view.Session.ID, answer); err != nil {
		t.Fatal(err)
	}
	d.view, err = service.Session(d.ctx, d.view.Session.ID)
	if err != nil {
		t.Fatal(err)
	}
	spaceID := learningspace.Scope(d.ctx)
	space, err := spaces.Get(ctx, spaceID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := spaces.Mutate(ctx, actor, spaceID, learningspace.Command{OperationID: uuid.NewString(), ExpectedVersion: space.Version, Name: space.Name, Status: "archived"}); err != nil {
		t.Fatal(err)
	}
	proposal, err = service.Propose(d.ctx, actor, learning.ProposalRequest{RequestID: uuid.NewString(), Type: learning.ProposalAssessment, AggregateType: "session", AggregateID: d.view.Session.ID, AggregateVersion: d.view.Session.AggregateVer, KnowledgeRevisionID: d.scope, NodeRevisionIDs: []string{d.node}, Input: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	apply(d, tutoring.ActionRecordAssessment, proposal.ID)
	if d.view.Session.State != tutoring.StateFeedback || d.view.WorkItem.Attempt.Answer != "我的解释" {
		t.Fatal("归档丢失已接收作答或迟到评估")
	}
	var authorization learning.OfflineAuthorizationPayloadV1
	item := pack.Items[0]
	if err := json.Unmarshal(item.Authorization.Payload, &authorization); err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(learning.OfflineAttemptPayload{Answer: "离线解释", AnswerSHA256: learning.SHA256([]byte("离线解释")), Help: learning.HelpNone, Observations: []learning.OfflineObservation{{Kind: "activity_presented"}, {Kind: "answer_recorded"}}})
	wire, _ := json.Marshal(learning.OfflineOperationWireV1{OperationID: authorization.OperationID, DeviceID: actor, DeviceSequence: authorization.DeviceSequence, SubmissionID: authorization.SubmissionID, PayloadSchemaVersion: 1, AggregateType: "offline_attempt", AggregateID: authorization.SubmissionID, ExpectedVersion: authorization.ExpectedVersion, OfflineActivityID: authorization.OfflineActivityID, ActivityRevision: authorization.ActivityRevision, Authorization: item.Authorization.Payload, Signature: item.Authorization.Signature, OperationType: learning.OfflineAttemptCompleted, Payload: payload})
	syncRequest := learning.OfflineSyncRequest{SyncRequestID: uuid.NewString(), PayloadSchemaVersion: 1, Operations: []json.RawMessage{wire}}
	for i := 0; i < 2; i++ {
		result, err := offline.Sync(directions[1].ctx, actor, syncRequest)
		if err != nil || len(result.Results) != 1 || result.Results[0].ArchiveStatus != learning.OfflineArchivedSucceeded || result.Results[0].Replayed != (i == 1) {
			t.Fatalf("切到英语后同步归档的 Go 授权失败：%+v %v", result, err)
		}
	}
	var originalSession, originalScope string
	if err := pool.QueryRow(ctx, `SELECT parent_session_id,knowledge_revision_id FROM offline_activities WHERE id=$1`, authorization.OfflineActivityID).Scan(&originalSession, &originalScope); err != nil || originalSession != d.view.Session.ID || originalScope != d.scope {
		t.Fatalf("冻结范围离线归属改变：%s %s %v", originalSession, originalScope, err)
	}
	if _, err := store.Rebuild(ctx); err != nil {
		t.Fatal(err)
	}
	for _, d := range directions {
		fresh, err := service.Session(d.ctx, d.view.Session.ID)
		if err != nil || !reflect.DeepEqual(fresh.WorkItem, d.view.WorkItem) {
			t.Fatalf("重放丢失冻结资料：%v", err)
		}
	}
}
