package postgresstore_test

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/learning"
	learningdb "github.com/edu-agent/edu-agent/server/internal/learning/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/learningspace"
	spacedb "github.com/edu-agent/edu-agent/server/internal/learningspace/postgresstore"
	tutoringdb "github.com/edu-agent/edu-agent/server/internal/tutoring/postgresstore"
	"github.com/google/uuid"
)

func TestPostgreSQLProgressScopePaginationAndReplay(t *testing.T) {
	pool := learningIntegrationPool(t)
	ctx := context.Background()
	actor := uuid.NewString()
	if _, err := pool.Exec(ctx, `INSERT INTO devices(id,display_name,created_at) VALUES($1,'进度测试',now())`, actor); err != nil {
		t.Fatal(err)
	}
	store := learningdb.New(pool, tutoringdb.New(pool))
	if _, err := store.Rebuild(ctx); err != nil {
		t.Fatal(err)
	}
	svc, err := learning.NewService(store, store, integrationKnowledgeResolver{}, learning.ServiceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	spaces := spacedb.New(pool)
	sp, err := spaces.Mutate(ctx, actor, "", learningspace.Command{OperationID: uuid.NewString(), Name: "英语", Status: "active"})
	if err != nil {
		t.Fatal(err)
	}
	other, _ := learningspace.WithScope(ctx, sp.ID)
	create := func(c context.Context, name string) learning.GoalRevision {
		t.Helper()
		result, err := svc.CreateGoal(c, actor, goalCommand(learning.GoalRevision{}, name))
		if err != nil {
			t.Fatal(err)
		}
		var g learning.GoalRevision
		if err = json.Unmarshal(result.Result, &g); err != nil {
			t.Fatal(err)
		}
		cmd := goalCommand(g, name)
		cmd.Action = "start"
		result, err = svc.CreateGoal(c, actor, cmd)
		if err != nil {
			t.Fatal(err)
		}
		if err = json.Unmarshal(result.Result, &g); err != nil {
			t.Fatal(err)
		}
		return g
	}
	a := create(ctx, "Go 并发")
	create(ctx, "算法")
	create(other, "英语")
	page, err := svc.Progress(ctx, learning.ProgressQuery{Limit: 1})
	if err != nil || page.Total != 2 || len(page.Items) != 1 || page.NextCursor == "" {
		t.Fatalf("区内分页错误：%+v %v", page, err)
	}
	second, err := svc.Progress(ctx, learning.ProgressQuery{Limit: 1, Cursor: page.NextCursor})
	if err != nil || len(second.Items) != 1 || second.Items[0].Goal.GoalID == page.Items[0].Goal.GoalID || second.NextCursor != "" {
		t.Fatalf("分页重复或遗漏：%+v %v", second, err)
	}
	if _, err := svc.Progress(other, learning.ProgressQuery{Limit: 1, Cursor: page.NextCursor}); learning.ErrorCode(err) != learning.CodeStaleCursor {
		t.Fatalf("跨区游标未拒绝：%v", err)
	}
	global, err := svc.Progress(ctx, learning.ProgressQuery{Global: true, Limit: 50})
	if err != nil || global.Total != 3 {
		t.Fatalf("全局错误：%+v %v", global, err)
	}
	goal, err := svc.Progress(ctx, learning.ProgressQuery{GoalID: a.GoalID, Limit: 50})
	if err != nil || goal.Total != 1 || goal.Items[0].EvidenceCount != 0 || len(goal.Items[0].Routes) != 0 {
		t.Fatalf("无证据目标伪造进度：%+v %v", goal, err)
	}
	if _, err = store.Rebuild(ctx); err != nil {
		t.Fatal(err)
	}
	after, err := svc.Progress(ctx, learning.ProgressQuery{Global: true, Limit: 50})
	if err != nil || !reflect.DeepEqual(global.Items, after.Items) {
		t.Fatalf("重放不一致：%v", err)
	}
	globalCursor, err := svc.Progress(ctx, learning.ProgressQuery{Global: true, Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = spaces.Mutate(ctx, actor, sp.ID, learningspace.Command{OperationID: uuid.NewString(), ExpectedVersion: sp.Version, Name: sp.Name, Status: "archived"}); err != nil {
		t.Fatal(err)
	}
	if _, err = svc.Progress(ctx, learning.ProgressQuery{Global: true, Limit: 1, Cursor: globalCursor.NextCursor}); learning.ErrorCode(err) != learning.CodeStaleCursor {
		t.Fatalf("归档后旧游标未失效：%v", err)
	}
	visible, err := svc.Progress(ctx, learning.ProgressQuery{Global: true, Limit: 50})
	if err != nil || visible.Total != 2 {
		t.Fatalf("归档区仍在默认总览：%+v %v", visible, err)
	}
	history, err := svc.Progress(ctx, learning.ProgressQuery{Global: true, Status: "all", Limit: 50})
	if err != nil || history.Total != 3 {
		t.Fatalf("归档删除了历史：%+v %v", history, err)
	}
	pause := goalCommand(a, "Go 并发")
	pause.Action = "pause"
	if _, err = svc.CreateGoal(ctx, actor, pause); err != nil {
		t.Fatal(err)
	}
	paused, err := svc.Progress(ctx, learning.ProgressQuery{GoalID: a.GoalID, Status: "paused", Limit: 50})
	if err != nil || paused.Total != 1 {
		t.Fatalf("暂停过滤错误：%+v %v", paused, err)
	}
	if _, err = pool.Exec(ctx, `UPDATE learning_projection_generations SET progress_version=0 WHERE id=$1`, after.Metadata.GenerationID); err != nil {
		t.Fatal(err)
	}
	if _, err = svc.Progress(ctx, learning.ProgressQuery{Limit: 50}); learning.ErrorCode(err) != learning.CodeProjectionUnavailable {
		t.Fatalf("未重建应报告不可用：%v", err)
	}
	if count, err := store.EnsureProgressProjection(ctx); err != nil || count != 1 {
		t.Fatalf("升级未重建进度：%d %v", count, err)
	}
	if count, err := store.EnsureProgressProjection(ctx); err != nil || count != 0 {
		t.Fatalf("升级重复重建：%d %v", count, err)
	}
}

func TestPostgreSQLReviewTasksKeepIndependentGoalsAndReplay(t *testing.T) {
	pool, store, original := newOfflineIngestFixture(t)
	ctx := context.Background()
	goalID, revisionID, sessionID := uuid.NewString(), uuid.NewString(), uuid.NewString()
	if _, err := store.Commit(ctx, goalCommitFor(t, learningDeviceOne, uuid.NewString(), goalID, revisionID, 0, 1, 1)); err != nil {
		t.Fatal(err)
	}
	svc, err := learning.NewService(store, store, integrationKnowledgeResolver{}, learning.ServiceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CreateSession(ctx, learningDeviceOne, learning.SessionCommand{GoalRevisionID: revisionID, Operation: learning.OperationEnvelope{OperationID: uuid.NewString(), AggregateType: "session", AggregateID: sessionID, PayloadSchemaVersion: 1, Payload: json.RawMessage(`{}`)}}); err != nil {
		t.Fatal(err)
	}
	accepted := learning.AcceptedEvidence{ID: uuid.NewString(), GoalRevisionID: revisionID, NodeRevisionID: learningNodeRevisionID, ActivityID: uuid.NewString(), Kind: learning.EvidencePracticeRecall, Outcome: learning.OutcomePass, Help: learning.HelpNone, ReceivedAt: time.Now().Add(-72 * time.Hour)}
	_, version := appendLearningReplayEvent(t, pool, sessionID, learningReplayEventInput{ID: uuid.NewString(), PayloadID: uuid.NewString(), OperationID: uuid.NewString(), Type: learning.EventEvidenceAccepted, SchemaVersion: 1, Payload: accepted, ReceivedAt: accepted.ReceivedAt})
	if _, err := store.Commit(ctx, learning.CommitRequest{DeviceID: learningDeviceOne, Operation: learning.OperationEnvelope{OperationID: uuid.NewString(), PayloadSchemaVersion: 1, AggregateType: "session", AggregateID: sessionID, ExpectedVersion: version, Payload: json.RawMessage(`{}`)}, RequestHash: learning.SHA256([]byte("进度投影夹具")), Expectations: []learning.AggregateExpectation{{Type: "session", ID: sessionID, ExpectedVersion: version}}, Batch: learning.CommandBatch{Events: []learning.EventDraft{{Type: learning.EventActivityPresented, AggregateType: "session", AggregateID: sessionID, Payload: json.RawMessage(`{"kind":"projection_tick"}`)}}}, ReceivedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Rebuild(ctx); err != nil {
		t.Fatal(err)
	}
	q := learning.ReviewQuery{Global: true, Page: learning.CursorPageRequest{Limit: 1}}
	first, err := store.Reviews(ctx, q)
	if err != nil || first.Total != 2 || len(first.Items) != 1 || first.NextCursor == "" {
		t.Fatalf("全局独立任务计数错误：%+v %v", first, err)
	}
	q.Page.Cursor = first.NextCursor
	second, err := store.Reviews(ctx, q)
	if err != nil || len(second.Items) != 1 || second.Items[0].TaskID == first.Items[0].TaskID || second.NextCursor != "" || !first.DueBefore.Equal(second.DueBefore) {
		t.Fatalf("复习分页或冻结截止时间错误：%+v %v", second, err)
	}
	byGoal, err := store.Reviews(ctx, learning.ReviewQuery{GoalID: goalID, Page: learning.CursorPageRequest{Limit: 50}})
	if err != nil || byGoal.Total != 1 || byGoal.Items[0].EvidenceID != accepted.ID || byGoal.Items[0].SessionID != sessionID || byGoal.Items[0].NodeRevisionID != learningNodeRevisionID {
		t.Fatalf("来源错误：%+v %v", byGoal, err)
	}
	before, err := store.Progress(ctx, learning.ProgressQuery{Global: true, Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	for _, goal := range before.Items {
		if goal.Goal.GoalID == goalID && !reflect.DeepEqual(goal.Reviews, byGoal.Items) {
			t.Fatalf("目标详情和统一复习的任务语义不一致：%+v %+v", goal.Reviews, byGoal.Items)
		}
	}
	var facts int
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM learning_events`).Scan(&facts); err != nil {
		t.Fatal(err)
	}
	if _, err = store.Rebuild(ctx); err != nil {
		t.Fatal(err)
	}
	after, err := store.Progress(ctx, learning.ProgressQuery{Global: true, Limit: 50})
	if err != nil || !reflect.DeepEqual(before.Items, after.Items) {
		t.Fatalf("任务重放不一致：%v", err)
	}
	var factsAfter int
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM learning_events`).Scan(&factsAfter); err != nil || factsAfter != facts {
		t.Fatal("读操作或重放生成了额外事实")
	}
	if _, err = store.Session(ctx, original.Session.ID); err != nil {
		t.Fatalf("读取复习改变原会话：%v", err)
	}
}
