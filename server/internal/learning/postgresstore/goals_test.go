package postgresstore_test

import (
	"context"
	"encoding/json"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/knowledge"
	knowledgedb "github.com/edu-agent/edu-agent/server/internal/knowledge/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/learning"
	learningdb "github.com/edu-agent/edu-agent/server/internal/learning/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/learningspace"
	spacedb "github.com/edu-agent/edu-agent/server/internal/learningspace/postgresstore"
	tutoringdb "github.com/edu-agent/edu-agent/server/internal/tutoring/postgresstore"
	"github.com/google/uuid"
)

func goalCommand(g learning.GoalRevision, text string) learning.GoalCommand {
	id := g.GoalID
	if id == "" {
		id = uuid.NewString()
	}
	c := learning.GoalCommand{Text: text, Source: "goal-management-test", Operation: learning.OperationEnvelope{OperationID: uuid.NewString(), PayloadSchemaVersion: 1, AggregateType: "goal", AggregateID: id, ExpectedVersion: g.Revision, Payload: json.RawMessage(`{}`)}}
	if g.ID != "" {
		c.PreviousRevisionID = &g.ID
	}
	return c
}

func TestPostgreSQLGoalManagementLifecycleScopeRetriesAndHistory(t *testing.T) {
	pool := learningIntegrationPool(t)
	ctx := context.Background()
	actor := uuid.NewString()
	if _, err := pool.Exec(ctx, `INSERT INTO devices(id,display_name,created_at) VALUES($1,'目标测试',now())`, actor); err != nil {
		t.Fatal(err)
	}
	kstore := knowledgedb.New(pool)
	store := learningdb.New(pool, tutoringdb.New(pool), kstore)
	if _, err := store.Rebuild(ctx); err != nil {
		t.Fatal(err)
	}
	service, err := learning.NewService(store, store, integrationKnowledgeResolver{}, learning.ServiceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	spaces := spacedb.New(pool)
	space, err := spaces.Mutate(ctx, actor, "", learningspace.Command{OperationID: uuid.NewString(), Name: "Go 后端", Status: "active"})
	if err != nil {
		t.Fatal(err)
	}
	scoped, _ := learningspace.WithScope(ctx, space.ID)
	save := func(ctx context.Context, c learning.GoalCommand) learning.GoalRevision {
		t.Helper()
		result, err := service.CreateGoal(ctx, actor, c)
		if err != nil {
			t.Fatal(err)
		}
		var g learning.GoalRevision
		if err = json.Unmarshal(result.Result, &g); err != nil {
			t.Fatal(err)
		}
		return g
	}
	// 默认区教学会话与非默认区目标管理保持独立。
	legacy := save(ctx, goalCommand(learning.GoalRevision{}, "继续当前教学"))
	sessionID := uuid.NewString()
	if _, err := service.CreateSession(ctx, actor, learning.SessionCommand{Operation: learning.OperationEnvelope{OperationID: uuid.NewString(), PayloadSchemaVersion: 1, AggregateType: "session", AggregateID: sessionID, Payload: json.RawMessage(`{}`)}, GoalRevisionID: legacy.ID}); err != nil {
		t.Fatal(err)
	}
	before, err := store.LoadSessionAuthority(ctx, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	create := goalCommand(learning.GoalRevision{}, "掌握并发编程")
	a := save(scoped, create)
	b := save(scoped, goalCommand(learning.GoalRevision{}, "准备后端面试"))
	if a.Management.Status != "draft" || a.Management.Details.ScopeSnapshotID != "" || a.Management.Details.Deadline != nil {
		t.Fatalf("空资料草稿不正确：%+v", a)
	}
	replay, err := service.CreateGoal(scoped, actor, create)
	if err != nil || !replay.Replayed {
		t.Fatalf("创建重试未去重：%+v %v", replay, err)
	}
	var replayGoal learning.GoalRevision
	_ = json.Unmarshal(replay.Result, &replayGoal)
	if replayGoal.ID != a.ID {
		t.Fatal("重试创建了第二个目标")
	}
	if _, err = service.CreateGoal(ctx, actor, create); learning.ErrorCode(err) != learning.CodeIdempotencyConflict {
		t.Fatalf("跨区重放未拒绝：%v", err)
	}
	if _, err = service.GetGoal(ctx, a.GoalID); learning.ErrorCode(err) != learning.CodeNotFound {
		t.Fatalf("跨区目标读取未拒绝：%v", err)
	}
	if _, err = store.LoadGoalRevision(ctx, a.ID); learning.ErrorCode(err) != learning.CodeNotFound {
		t.Fatalf("跨区修订读取未拒绝：%v", err)
	}
	act := func(g learning.GoalRevision, action string) learning.GoalRevision {
		c := goalCommand(g, g.Text)
		c.Action = action
		if action == "complete" {
			c.CompletionReason = "我已完成本次面试准备"
		}
		return save(scoped, c)
	}
	a = act(a, "start")
	b = act(b, "start")
	page, err := service.ListGoals(scoped, learning.GoalQuery{Status: "active", Limit: 1})
	if err != nil || len(page.Items) != 1 || page.NextCursor == "" {
		t.Fatalf("多 active 或分页失败：%+v %v", page, err)
	}
	next, err := service.ListGoals(scoped, learning.GoalQuery{Status: "active", Limit: 1, Cursor: page.NextCursor})
	if err != nil || len(next.Items) != 1 || next.Items[0].GoalID == page.Items[0].GoalID {
		t.Fatalf("分页重复或缺失：%+v %v", next, err)
	}
	if _, err = service.ListGoals(ctx, learning.GoalQuery{Status: "active", Limit: 1, Cursor: page.NextCursor}); err == nil {
		t.Fatal("跨区游标被接受")
	}
	page, err = service.ListGoals(scoped, learning.GoalQuery{Search: "面试", Limit: 10})
	if err != nil || len(page.Items) != 1 || page.Items[0].GoalID != b.GoalID {
		t.Fatalf("搜索错误：%+v %v", page, err)
	}
	a = act(a, "pause")
	if allowed, err := service.CanStartGoal(scoped, a.GoalID); err != nil || allowed {
		t.Fatalf("暂停目标允许开始：%v %v", allowed, err)
	}
	a = act(a, "archive")
	a = act(a, "restore")
	if a.Management.Status != "paused" {
		t.Fatal("恢复归档丢失原状态")
	}
	a = act(a, "resume")
	b = act(b, "complete")
	if b.Management.Completion == nil || b.Management.CriteriaVerification != "unverified" {
		t.Fatal("手动完成虚构了验证结论")
	}
	assertCount(t, pool, `SELECT count(*) FROM learning_evidence`, 0)
	// 内容修订追加版本，不改变旧修订；并发只有一个成功。
	original := a
	d := a.Management.Details
	d.Name = "Go 并发"
	d.Scope = "Channel\nContext"
	d.CompletionCriteria = "解释取消传播"
	d.SelfAssessment = "我认为已经熟悉"
	deadline, _ := time.Parse(time.RFC3339, "2026-10-01T12:00:00+08:00")
	minutes := 120
	d.Timezone = "Asia/Shanghai"
	d.Deadline = &deadline
	d.WeeklyMinutes = &minutes
	one, two := goalCommand(a, a.Text), goalCommand(a, a.Text)
	one.Details = &d
	two.Details = &d
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, c := range []learning.GoalCommand{one, two} {
		wg.Add(1)
		go func(c learning.GoalCommand) { defer wg.Done(); _, e := service.CreateGoal(scoped, actor, c); errs <- e }(c)
	}
	wg.Wait()
	close(errs)
	success, conflict := 0, 0
	for e := range errs {
		if e == nil {
			success++
		} else if learning.ErrorCode(e) == learning.CodeVersionConflict {
			conflict++
		} else {
			t.Fatal(e)
		}
	}
	if success != 1 || conflict != 1 {
		t.Fatalf("并发结果 %d/%d", success, conflict)
	}
	old, err := store.LoadGoalRevision(scoped, original.ID)
	oldHash, _ := learning.HashJSON(old)
	originalHash, _ := learning.HashJSON(original)
	if err != nil || oldHash != originalHash {
		t.Fatalf("旧修订被改写：%+v %v", old, err)
	}
	restarted := learningdb.New(pool, tutoringdb.New(pool), kstore)
	latest, err := restarted.GetGoal(scoped, a.GoalID)
	if err != nil || latest.Management.Details.Scope != d.Scope || !latest.Management.RouteAdjustmentNeeded || latest.Management.Details.Deadline == nil || !latest.Management.Details.Deadline.Equal(deadline) {
		t.Fatalf("重启后结构信息丢失：%+v %v", latest, err)
	}
	history, err := service.GoalHistory(scoped, a.GoalID, learning.GoalQuery{Limit: 100})
	if err != nil || len(history.Items) != int(latest.Revision) {
		t.Fatalf("历史不完整：%+v %v", history, err)
	}
	after, err := store.LoadSessionAuthority(ctx, sessionID)
	if err != nil || !reflect.DeepEqual(before.Session, after.Session) {
		t.Fatalf("目标操作改变了教学会话：%v", err)
	}
	timeline, err := store.Timeline(ctx, learning.TimelineQuery{Page: learning.CursorPageRequest{Limit: 100}})
	if err != nil {
		t.Fatal(err)
	}
	if len(timeline.Items) < 2 {
		t.Fatal("默认时间线遗漏原有事件")
	}
	for _, item := range timeline.Items {
		if item.AggregateID == a.GoalID || item.AggregateID == b.GoalID {
			t.Fatal("默认时间线混入其他区目标")
		}
	}
	// 学习区归档阻止所有入口写入；归档默认区不妨碍其他区管理。
	defaultSpace, err := spaces.Get(ctx, learningspace.DefaultID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = spaces.Mutate(ctx, actor, defaultSpace.ID, learningspace.Command{OperationID: uuid.NewString(), ExpectedVersion: defaultSpace.Version, Name: defaultSpace.Name, Status: "archived"}); err != nil {
		t.Fatal(err)
	}
	save(scoped, goalCommand(learning.GoalRevision{}, "默认区归档后仍可保存"))
	if _, err = spaces.Mutate(ctx, actor, space.ID, learningspace.Command{OperationID: uuid.NewString(), ExpectedVersion: space.Version, Name: space.Name, Status: "archived"}); err != nil {
		t.Fatal(err)
	}
	if _, err = service.CreateGoal(scoped, actor, goalCommand(learning.GoalRevision{}, "归档区不得新增")); err == nil {
		t.Fatal("归档区允许新增")
	}
}

func TestPostgreSQLGoalFrozenMaterialsValidation(t *testing.T) {
	pool := learningIntegrationPool(t)
	ctx := context.Background()
	actor := uuid.NewString()
	if _, err := pool.Exec(ctx, `INSERT INTO devices(id,display_name,created_at) VALUES($1,'资料目标测试',now())`, actor); err != nil {
		t.Fatal(err)
	}
	kstore := knowledgedb.New(pool)
	ks, err := knowledge.NewService(kstore, knowledge.NewCanonicalizer(), knowledge.ServiceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	store := learningdb.New(pool, tutoringdb.New(pool), kstore)
	if _, err = store.Rebuild(ctx); err != nil {
		t.Fatal(err)
	}
	service, err := learning.NewService(store, store, integrationKnowledgeResolver{}, learning.ServiceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	imported, err := ks.Import(ctx, knowledge.ImportCommand{OperationID: uuid.NewString(), ActorDeviceID: actor, Source: "go-notes", ExpectedParentProvided: true, Documents: []knowledge.ImportDocument{{Path: "go.md", Markdown: "# Channel\n通道是并发通信方式。\n"}}})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := ks.FreezeScope(ctx, knowledge.ScopeSnapshot{ID: uuid.NewString(), Entries: []knowledge.ScopeEntry{{CollectionID: knowledge.DefaultCollectionID, RevisionID: imported.Revision.ID, DocumentID: imported.Revision.Documents[0].Revision.DocumentID}}})
	if err != nil {
		t.Fatal(err)
	}
	c := goalCommand(learning.GoalRevision{}, "学习通道")
	c.Details = &learning.GoalDetails{Name: "通道", ScopeSnapshotID: snapshot.ID}
	result, err := service.CreateGoal(ctx, actor, c)
	if err != nil {
		t.Fatal(err)
	}
	var g learning.GoalRevision
	_ = json.Unmarshal(result.Result, &g)
	for _, scopeID := range []string{uuid.NewString(), "forged"} {
		bad := goalCommand(g, g.Text)
		d := g.Management.Details
		d.ScopeSnapshotID = scopeID
		bad.Details = &d
		if _, err = service.CreateGoal(ctx, actor, bad); learning.ErrorCode(err) != learning.CodeKnowledgeReferenceInvalid {
			t.Fatalf("非法资料未拒绝：%v", err)
		}
	}
	other := uuid.NewString()
	if _, err = pool.Exec(ctx, `INSERT INTO learning_spaces(id,name,status,version) VALUES($1,'英语','active',1)`, other); err != nil {
		t.Fatal(err)
	}
	scoped, _ := learningspace.WithScope(ctx, other)
	c = goalCommand(learning.GoalRevision{}, "跨区资料")
	c.Details = &learning.GoalDetails{Name: "错误资料", ScopeSnapshotID: snapshot.ID}
	if _, err = service.CreateGoal(scoped, actor, c); learning.ErrorCode(err) != learning.CodeKnowledgeReferenceInvalid {
		t.Fatalf("跨区资料未拒绝：%v", err)
	}
	paused := goalCommand(g, g.Text)
	paused.Action = "start"
	result, err = service.CreateGoal(ctx, actor, paused)
	if err != nil {
		t.Fatal(err)
	}
	_ = json.Unmarshal(result.Result, &g)
	paused = goalCommand(g, g.Text)
	paused.Action = "pause"
	if _, err = service.CreateGoal(ctx, actor, paused); err != nil {
		t.Fatal(err)
	}
	_, err = service.CreateSession(ctx, actor, learning.SessionCommand{Operation: learning.OperationEnvelope{OperationID: uuid.NewString(), PayloadSchemaVersion: 1, AggregateType: "session", AggregateID: uuid.NewString(), Payload: json.RawMessage(`{}`)}, GoalRevisionID: g.ID})
	if learning.ErrorCode(err) != learning.CodeInvalidTransition {
		t.Fatalf("教学入口未消费目标状态：%v", err)
	}
}

func TestPostgreSQLGoalRevisionsPreserveLearningFactsAndFocus(t *testing.T) {
	pool := learningIntegrationPool(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `INSERT INTO devices(id,display_name,created_at) VALUES($1,'目标历史验收',now())`, learningDeviceOne); err != nil {
		t.Fatal(err)
	}
	insertLearningKnowledgeFixture(t, pool)
	store := learningdb.New(pool, tutoringdb.New(pool))
	initial := goalCommit(t, learningDeviceOne, "20000000-0000-4000-8000-000000000001", "30000000-0000-4000-8000-000000000001", 0, 1, 1)
	if _, err := store.Commit(ctx, initial); err != nil {
		t.Fatal(err)
	}
	sessionID, _ := commitLearningAuthorityFixture(t, store, false)
	service, err := learning.NewService(store, store, integrationKnowledgeResolver{}, learning.ServiceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	facts := func() map[string]string {
		t.Helper()
		result := map[string]string{}
		for _, table := range []string{"learning_route_revisions", "learning_activities", "learning_attempts", "learning_assessments", "learning_evidence", "tutoring_sessions", "tutoring_focus_frames"} {
			var rows string
			if err := pool.QueryRow(ctx, "SELECT jsonb_agg(to_jsonb(t) ORDER BY id)::text FROM "+table+" t").Scan(&rows); err != nil {
				t.Fatal(table, err)
			}
			result[table] = rows
		}
		return result
	}
	before := facts()
	nodeBefore, err := store.Node(ctx, learningNodeRevisionID)
	if err != nil {
		t.Fatal(err)
	}
	g, err := store.GetGoal(ctx, learningGoalID)
	if err != nil {
		t.Fatal(err)
	}
	for _, action := range []string{"", "pause", "archive", "restore", "resume", "complete"} {
		command := goalCommand(g, g.Text)
		command.Action = action
		if action == "" {
			d := g.GoalManagement().Details
			d.Scope = "调整为并发基础"
			d.CompletionCriteria = "能够解释取消传播"
			command.Details = &d
		}
		if action == "complete" {
			command.CompletionReason = "手动结束，不新增成绩"
		}
		result, err := service.CreateGoal(ctx, learningDeviceOne, command)
		if err != nil {
			t.Fatal(action, err)
		}
		if err = json.Unmarshal(result.Result, &g); err != nil {
			t.Fatal(err)
		}
	}
	if !reflect.DeepEqual(before, facts()) {
		t.Fatal("修订或生命周期操作改写了学习事实、冻结引用或焦点")
	}
	nodeAfter, err := store.Node(ctx, learningNodeRevisionID)
	if err != nil || !reflect.DeepEqual(nodeBefore.Node.Mastery, nodeAfter.Node.Mastery) {
		t.Fatalf("手动完成改变了掌握度：%v", err)
	}
	current, err := store.CurrentSession(ctx)
	if err != nil || current.Session.ID != sessionID || current.Session.ActiveFrame == nil {
		t.Fatalf("当前焦点丢失：%+v %v", current, err)
	}
}
