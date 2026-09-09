package postgresstore_test

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/edu-agent/edu-agent/server/internal/integrations/learningknowledge"
	"github.com/edu-agent/edu-agent/server/internal/knowledge"
	knowledgedb "github.com/edu-agent/edu-agent/server/internal/knowledge/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/learning"
	learningdb "github.com/edu-agent/edu-agent/server/internal/learning/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/tutoring"
	tutoringdb "github.com/edu-agent/edu-agent/server/internal/tutoring/postgresstore"
	"github.com/google/uuid"
)

type planningFixtureModel struct {
	calls   int
	invalid bool
}

func (m *planningFixtureModel) Generate(_ context.Context, r learning.ProposalRequest) (json.RawMessage, error) {
	m.calls++
	if m.invalid {
		return json.RawMessage(`{"mastery":"retained"}`), nil
	}
	var input struct {
		Draft   learning.PlanningContent  `json:"draft"`
		Sources []learning.PlanningSource `json:"sources"`
	}
	if err := json.Unmarshal(r.Input, &input); err != nil {
		return nil, err
	}
	c := input.Draft
	c.Details.ExpectedOutcome = "能够解释并发并完成练习"
	c.Steps = []learning.PlanningStep{{Name: "并发入门", Content: "阅读并发资料", Reason: "建立基础", Exercise: "说明并发与并行区别", Completion: "独立说明区别", Minutes: 30, Prerequisites: []int{}, NodeRevisionID: input.Sources[0].Reference.NodeRevisionID}}
	return json.Marshal(c)
}
func TestPostgreSQLPlanningGenerateEditConfirmAndRecovery(t *testing.T) {
	pool := learningIntegrationPool(t)
	ctx := context.Background()
	actor := uuid.NewString()
	if _, err := pool.Exec(ctx, `INSERT INTO devices(id,display_name,created_at) VALUES($1,'规划验收',now())`, actor); err != nil {
		t.Fatal(err)
	}
	kstore := knowledgedb.New(pool)
	ks, err := knowledge.NewService(kstore, knowledge.NewCanonicalizer(), knowledge.ServiceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	imported, err := ks.Import(ctx, knowledge.ImportCommand{OperationID: uuid.NewString(), ActorDeviceID: actor, Source: "fixture", ExpectedParentProvided: true, Documents: []knowledge.ImportDocument{{Path: "go.md", Markdown: "# Go 并发\n并发由多个执行流程协作完成。\n"}}})
	if err != nil {
		t.Fatal(err)
	}
	scope, err := ks.FreezeScope(ctx, knowledge.ScopeSnapshot{ID: uuid.NewString(), Entries: []knowledge.ScopeEntry{{CollectionID: knowledge.DefaultCollectionID, RevisionID: imported.Revision.ID}}})
	if err != nil {
		t.Fatal(err)
	}
	store := learningdb.New(pool, tutoringdb.New(pool), kstore)
	if _, err = store.Rebuild(ctx); err != nil {
		t.Fatal(err)
	}
	model := &planningFixtureModel{}
	service, err := learning.NewService(store, store, learningknowledge.New(ks), learning.ServiceOptions{Model: model, ModelID: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	gc := goalCommand(learning.GoalRevision{}, "学习 Go 并发")
	gc.Details = &learning.GoalDetails{Name: "Go 并发", ScopeSnapshotID: scope.ID, Priority: "normal", SelfAssessment: "刚开始学习"}
	result, err := service.CreateGoal(ctx, actor, gc)
	if err != nil {
		t.Fatal(err)
	}
	var goal learning.GoalRevision
	if err = json.Unmarshal(result.Result, &goal); err != nil {
		t.Fatal(err)
	}
	id := uuid.NewString()
	d, err := service.ChangePlanning(ctx, actor, goal.GoalID, id, learning.PlanningCommand{OperationID: uuid.NewString(), Action: "create"})
	if err != nil {
		t.Fatal(err)
	}
	assertCount(t, pool, `SELECT count(*) FROM tutoring_sessions`, 0)
	generated := learning.PlanningCommand{OperationID: uuid.NewString(), Action: "generate", ExpectedVersion: d.Version}
	d, err = service.ChangePlanning(ctx, actor, goal.GoalID, id, generated)
	if err != nil || d.Suggestion == nil || d.LastError != "" {
		t.Fatalf("生成失败：%+v %v", d, err)
	}
	if _, err = service.ChangePlanning(ctx, actor, goal.GoalID, id, generated); err != nil || model.calls != 1 {
		t.Fatalf("重试重复调用模型：%d %v", model.calls, err)
	}
	current, err := service.GetGoal(ctx, goal.GoalID)
	if err != nil || !planningJSONEqual(current, goal) || len(d.Content.Steps) != 0 {
		t.Fatalf("未确认改变正式目标或用户草稿：当前=%+v 原始=%+v 草稿=%+v 错误=%v", current, goal, d.Content, err)
	}
	content := *d.Suggestion
	content.Steps[0].Minutes = 45
	d, err = service.ChangePlanning(ctx, actor, goal.GoalID, id, learning.PlanningCommand{OperationID: uuid.NewString(), Action: "edit", ExpectedVersion: d.Version, Content: &content})
	if err != nil {
		t.Fatal(err)
	}
	confirm := learning.PlanningCommand{OperationID: uuid.NewString(), Action: "confirm", ExpectedVersion: d.Version, Target: "new_session", UpdateGoal: true}
	// 无效目标写入后的路线失败也必须回滚整个确认。
	broken := content
	broken.Steps = append([]learning.PlanningStep(nil), content.Steps...)
	broken.Steps[0].Content = strings.Repeat("过长", 600)
	d, err = service.ChangePlanning(ctx, actor, goal.GoalID, id, learning.PlanningCommand{OperationID: uuid.NewString(), Action: "edit", ExpectedVersion: d.Version, Content: &broken})
	if err != nil {
		t.Fatal(err)
	}
	confirm.ExpectedVersion = d.Version
	if _, err = service.ChangePlanning(ctx, actor, goal.GoalID, id, confirm); err == nil {
		t.Fatal("无效路线未拒绝")
	}
	assertCount(t, pool, `SELECT count(*) FROM tutoring_sessions`, 0)
	assertCount(t, pool, `SELECT count(*) FROM learning_goal_revisions`, 1)
	d, err = service.ChangePlanning(ctx, actor, goal.GoalID, id, learning.PlanningCommand{OperationID: uuid.NewString(), Action: "edit", ExpectedVersion: d.Version, Content: &content})
	if err != nil {
		t.Fatal(err)
	}
	confirm.OperationID = uuid.NewString()
	confirm.ExpectedVersion = d.Version
	applied, err := service.ChangePlanning(ctx, actor, goal.GoalID, id, confirm)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := service.ChangePlanning(ctx, actor, goal.GoalID, id, confirm)
	if err != nil || !planningJSONEqual(applied, replay) {
		t.Fatalf("回执不一致：%v", err)
	}
	confirmedView, err := service.Planning(ctx, goal.GoalID, id)
	if err != nil || !planningJSONEqual(confirmedView, applied) {
		t.Fatal("已确认结果被误报为过期")
	}
	assertCount(t, pool, `SELECT count(*) FROM tutoring_sessions`, 1)
	assertCount(t, pool, `SELECT count(*) FROM learning_route_revisions`, 1)
	assertCount(t, pool, `SELECT count(*) FROM learning_evidence`, 0)
	view, err := service.Session(ctx, applied.AppliedSessionID)
	if err != nil || view.Session.State != tutoring.StateRouteActive || view.WorkItem.RouteRevision.Steps[0].TeachingIntent != "并发入门：阅读并发资料" {
		t.Fatalf("未按目标应用：%+v %v", view, err)
	}
	old, err := store.LoadGoalRevision(ctx, goal.ID)
	if err != nil || !planningJSONEqual(old, goal) {
		t.Fatal("原目标版本被覆盖")
	}
	latest, err := service.GetGoal(ctx, goal.GoalID)
	if err != nil {
		t.Fatal(err)
	}
	// 从进行中的会话重新规划，新会话不能改写旧路线和状态。
	nextID := uuid.NewString()
	next, err := service.ChangePlanning(ctx, actor, goal.GoalID, nextID, learning.PlanningCommand{OperationID: uuid.NewString(), Action: "create", SessionID: view.Session.ID})
	if err != nil {
		t.Fatal(err)
	}
	next, err = service.ChangePlanning(ctx, actor, goal.GoalID, nextID, learning.PlanningCommand{OperationID: uuid.NewString(), Action: "edit", ExpectedVersion: next.Version, Content: &content})
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.ChangePlanning(ctx, actor, goal.GoalID, nextID, learning.PlanningCommand{OperationID: uuid.NewString(), Action: "confirm", ExpectedVersion: next.Version, Target: "new_session"})
	if err != nil || second.AppliedSessionID == view.Session.ID {
		t.Fatalf("新会话失败：%v", err)
	}
	unchanged, err := service.Session(ctx, view.Session.ID)
	if err != nil || !reflect.DeepEqual(unchanged.WorkItem, view.WorkItem) {
		t.Fatal("重新规划改写旧历史")
	}
	currentID := uuid.NewString()
	currentDraft, err := service.ChangePlanning(ctx, actor, goal.GoalID, currentID, learning.PlanningCommand{OperationID: uuid.NewString(), Action: "create", SessionID: view.Session.ID})
	if err != nil {
		t.Fatal(err)
	}
	currentDraft, err = service.ChangePlanning(ctx, actor, goal.GoalID, currentID, learning.PlanningCommand{OperationID: uuid.NewString(), Action: "edit", ExpectedVersion: currentDraft.Version, Content: &content})
	if err != nil {
		t.Fatal(err)
	}
	oldRoute := *view.WorkItem.RouteRevision
	currentApplied, err := service.ChangePlanning(ctx, actor, goal.GoalID, currentID, learning.PlanningCommand{OperationID: uuid.NewString(), Action: "confirm", ExpectedVersion: currentDraft.Version, Target: "current_session"})
	if err != nil || currentApplied.AppliedSessionID != view.Session.ID {
		t.Fatalf("未使用指定原会话：%v", err)
	}
	retained, err := store.LoadRouteRevision(ctx, oldRoute.ID)
	if err != nil || !planningJSONEqual(retained, oldRoute) {
		t.Fatal("原路线修订被改写")
	}
	assertCount(t, pool, `SELECT count(*) FROM tutoring_sessions`, 2)
	staleID := uuid.NewString()
	stale, err := service.ChangePlanning(ctx, actor, goal.GoalID, staleID, learning.PlanningCommand{OperationID: uuid.NewString(), Action: "create"})
	if err != nil {
		t.Fatal(err)
	}
	revision := goalCommand(latest, "新的学习目的")
	if _, err = service.CreateGoal(ctx, actor, revision); err != nil {
		t.Fatal(err)
	}
	fresh, err := service.Planning(ctx, goal.GoalID, staleID)
	if err != nil || len(fresh.StaleReasons) == 0 {
		t.Fatal("未展示过期")
	}
	if _, err = service.ChangePlanning(ctx, actor, goal.GoalID, staleID, learning.PlanningCommand{OperationID: uuid.NewString(), Action: "confirm", ExpectedVersion: stale.Version, Target: "goal", UpdateGoal: true}); learning.ErrorCode(err) != learning.CodeStaleProposal {
		t.Fatalf("未拒绝旧草稿：%v", err)
	}
	model.invalid = true
	badID := uuid.NewString()
	bad, err := service.ChangePlanning(ctx, actor, goal.GoalID, badID, learning.PlanningCommand{OperationID: uuid.NewString(), Action: "create"})
	if err != nil {
		t.Fatal(err)
	}
	bad, err = service.ChangePlanning(ctx, actor, goal.GoalID, badID, learning.PlanningCommand{OperationID: uuid.NewString(), Action: "generate", ExpectedVersion: bad.Version})
	if err != nil || bad.LastError == "" {
		t.Fatalf("错误未保存：%v", err)
	}
	restarted, _ := learning.NewService(learningdb.New(pool, tutoringdb.New(pool), kstore), store, learningknowledge.New(ks), learning.ServiceOptions{})
	recovered, err := restarted.Planning(ctx, goal.GoalID, badID)
	if err != nil || recovered.LastError != bad.LastError {
		t.Fatal("重启丢失草稿")
	}
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, e := service.ChangePlanning(ctx, actor, goal.GoalID, badID, learning.PlanningCommand{OperationID: uuid.NewString(), Action: "edit", ExpectedVersion: bad.Version, Content: &bad.Content})
			results <- e
		}()
	}
	wg.Wait()
	close(results)
	success, conflict := 0, 0
	for e := range results {
		if e == nil {
			success++
		} else if learning.ErrorCode(e) == learning.CodeVersionConflict {
			conflict++
		} else {
			t.Fatal(e)
		}
	}
	if success != 1 || conflict != 1 {
		t.Fatal("并发编辑未按版本拒绝")
	}
	parent := imported.Revision.ID
	if _, err = ks.Import(ctx, knowledge.ImportCommand{OperationID: uuid.NewString(), ActorDeviceID: actor, Source: "fixture", ExpectedParentProvided: true, ExpectedParentRevisionID: &parent, Documents: []knowledge.ImportDocument{{Path: "go.md", Markdown: "# Go 并发\n并发由多个执行流程协作完成。\n"}, {Path: "more.md", Markdown: "# 后续练习\n补充学习资料。\n"}}}); err != nil {
		t.Fatal(err)
	}
	staleMaterials, err := service.Planning(ctx, goal.GoalID, badID)
	if err != nil || len(staleMaterials.StaleReasons) == 0 {
		t.Fatal("资料更新未使草稿过期")
	}
	if _, err = service.ChangePlanning(ctx, actor, goal.GoalID, badID, learning.PlanningCommand{OperationID: uuid.NewString(), Action: "confirm", ExpectedVersion: staleMaterials.Version, Target: "goal", UpdateGoal: true}); learning.ErrorCode(err) != learning.CodeStaleProposal {
		t.Fatalf("旧资料仍被采用：%v", err)
	}
}

func planningJSONEqual(a, b any) bool {
	x, _ := learning.HashJSON(a)
	y, _ := learning.HashJSON(b)
	return x == y
}
