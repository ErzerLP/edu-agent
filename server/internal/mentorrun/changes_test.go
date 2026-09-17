package mentorrun

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/edu-agent/edu-agent/packages/agentcore/modelclient"
	identitydb "github.com/edu-agent/edu-agent/server/internal/identity/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/integrations/learningknowledge"
	"github.com/edu-agent/edu-agent/server/internal/knowledge"
	"github.com/edu-agent/edu-agent/server/internal/learning"
	"github.com/edu-agent/edu-agent/server/internal/learningchange"
	"github.com/edu-agent/edu-agent/server/internal/learningspace"
	memorydb "github.com/edu-agent/edu-agent/server/internal/memory/postgresstore"
	outboxdb "github.com/edu-agent/edu-agent/server/internal/platform/outbox/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/privacy"
	privacydb "github.com/edu-agent/edu-agent/server/internal/privacy/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/tutoring"
	tutoringdb "github.com/edu-agent/edu-agent/server/internal/tutoring/postgresstore"
	"github.com/google/uuid"
	"net/http"
	"reflect"
	"sync"
	"testing"
	"time"
)

type changeFixture struct {
	*runtimeFixture
	changes  *learningchange.Service
	teaching *learning.Service
	session  string
}

func adaptiveFixture(t *testing.T) *changeFixture {
	t.Helper()
	f := startFixture(t, "Go 并发")
	run := f.accept(t)
	f.work(t)
	result := f.snapshot(t, run.RunID)
	if result.StartLearning == nil || result.StartLearning.Result == nil {
		t.Fatalf("空资料开学失败：%+v", result)
	}
	owners := f.service.starter
	c, err := learningchange.New(f.pool, owners.Learning, owners.Knowledge, owners.Content, bytes.Repeat([]byte{42}, 32))
	if err != nil {
		t.Fatal(err)
	}
	f.service.ConfigureChanges(c)
	ks, err := knowledge.NewService(owners.Knowledge, knowledge.NewCanonicalizer(), knowledge.ServiceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	ls, err := learning.NewService(owners.Learning, owners.Learning, learningknowledge.New(ks), learning.ServiceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return &changeFixture{f, c, ls, result.StartLearning.Result.SessionID}
}
func (f *changeFixture) context(t *testing.T) learningchange.Snapshot {
	t.Helper()
	s, e := f.changes.Snapshot(context.Background(), f.actor, f.goal, f.session)
	if e != nil {
		t.Fatal(e)
	}
	return s
}
func (f *changeFixture) act(t *testing.T, action tutoring.Action, answer string) {
	t.Helper()
	s := f.context(t)
	_, err := f.teaching.ApplyAction(context.Background(), f.actor.Device.ID, f.session, learning.ActionCommand{Operation: learning.OperationEnvelope{OperationID: uuid.NewString(), PayloadSchemaVersion: 1, AggregateType: "session", AggregateID: f.session, ExpectedVersion: s.Base.SessionVersion, Payload: json.RawMessage(`{}`)}, Action: action, Answer: answer, Help: learning.HelpNone})
	if err != nil {
		t.Fatal(err)
	}
}
func (f *changeFixture) propose(t *testing.T, kind string) learningchange.Change {
	t.Helper()
	snap := f.context(t)
	c := learningchange.Candidate{Kind: kind, Trigger: "user_request", Reason: "先补前置概念", EvidenceIDs: []string{}, Steps: []learningchange.Step{}}
	switch kind {
	case "route":
		c.Steps = []learningchange.Step{{NodeRevisionID: snap.Sources[0].NodeRevisionID, Name: "新的前置练习", Prompt: "请解释并发安全的必要条件", Criterion: "解释条件和原因", Difficulty: 2, Prerequisites: []int{}}}
	case "explanation":
		c.Explanation = "补充一个不同的例子"
	case "goal":
		g := snap.Goal.GoalManagement().Details
		g.Scope = "理解并发与通道"
		g.CompletionCriteria = "能独立解释示例"
		c.Goal = &g
	}
	id := uuid.NewString()
	v, err := f.changes.Change(context.Background(), f.actor, f.goal, f.session, id, learningchange.Command{OperationID: id, Action: "propose", Base: &snap.Base, Candidate: &c})
	if err != nil {
		t.Fatal(err)
	}
	return v
}
func changeCommand(c learningchange.Change, action string) learningchange.Command {
	return learningchange.Command{OperationID: uuid.NewString(), Action: action, ExpectedRevision: c.Revision, Hash: c.Hash, InteractionID: c.InteractionID}
}

func TestPostgreSQLAdaptiveQueueImmediateAndCompensation(t *testing.T) {
	f := adaptiveFixture(t)
	ctx := context.Background()
	f.act(t, tutoring.ActionPresentActivity, "")
	before := f.context(t)
	c := f.propose(t, "route")
	if c.Status != "queued_for_boundary" {
		t.Fatalf("作答中没有排队：%+v", c)
	}
	if got := f.context(t); !reflect.DeepEqual(got.Base, before.Base) {
		t.Fatal("排队覆盖当前题")
	}
	command := changeCommand(c, "apply_now")
	applied, err := f.changes.Change(ctx, f.actor, f.goal, f.session, c.ID, command)
	if err != nil {
		t.Fatal(err)
	}
	if applied.Status != "applied" || applied.FrameID == "" || applied.Applied.ActivityID == before.Base.ActivityID {
		t.Fatal("立即切换没有新题和原现场", applied)
	}
	if _, err = f.teaching.Session(ctx, f.session); err != nil {
		t.Fatal("立即切换后的正式课堂读取失败", err)
	}
	if current := f.context(t); !reflect.DeepEqual(current.Steps, c.Candidate.Steps) {
		t.Fatal("当前路线没有保留已应用候选的完整步骤", current.Steps)
	}
	retry, err := f.changes.Change(ctx, f.actor, f.goal, f.session, c.ID, command)
	if err != nil || retry.Applied.ActivityID != applied.Applied.ActivityID {
		t.Fatal("重试重复应用", err)
	}
	if _, err = f.changes.Change(ctx, f.actor, f.goal, f.session, c.ID, changeCommand(applied, "restore_focus")); err != nil {
		t.Fatal(err)
	}
	after := f.context(t)
	if after.Base.ActivityID != before.Base.ActivityID || after.Session.State != before.Session.State || !reflect.DeepEqual(after.Content.Body, before.Content.Body) {
		t.Fatal("原题现场未恢复")
	}
	// 同一会话处理原题后，已排队的新路线自动接入。
	next := f.propose(t, "route")
	f.act(t, tutoring.ActionEndActivity, "")
	if _, err = f.changes.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	next, err = f.changes.Read(ctx, f.actor, f.goal, next.ID, 0)
	if err != nil || next.Status != "applied" {
		t.Fatal("边界队列没有应用", next.Status, err)
	}
	undo, err := f.changes.Change(ctx, f.actor, f.goal, f.session, next.ID, changeCommand(next, "compensate"))
	if err != nil || undo.Compensates != next.ID || undo.Status != "waiting_approval" {
		t.Fatal("没有生成补偿候选", err)
	}
	undo, err = f.changes.Change(ctx, f.actor, f.goal, f.session, undo.ID, changeCommand(undo, "approve"))
	if err != nil || undo.Status != "queued_for_boundary" {
		t.Fatal("补偿未等待当前题", err)
	}
	f.act(t, tutoring.ActionEndActivity, "")
	if _, err = f.changes.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	undo, err = f.changes.Read(ctx, f.actor, f.goal, undo.ID, 0)
	if err != nil || undo.Status != "applied" || undo.Applied.RouteRevisionID == before.Base.RouteRevisionID {
		t.Fatal("补偿没有新版本", err)
	}
	old, err := f.service.starter.Content.Get(ctx, f.actor, before.Base.ArtifactID, 1)
	if err != nil || !reflect.DeepEqual(old.Body, before.Content.Body) {
		t.Fatal("历史正文被覆盖", err)
	}
}

func TestPostgreSQLAdaptiveApprovalVersionsAndExplanation(t *testing.T) {
	f := adaptiveFixture(t)
	ctx := context.Background()
	before := f.context(t)
	e := f.propose(t, "explanation")
	if e.Status != "applied" || e.Applied.ArtifactVersion != 2 || e.Applied.ActivityID != before.Base.ActivityID {
		t.Fatal("解释没有直接追加正文", e)
	}
	c := f.propose(t, "goal")
	if c.Status != "waiting_approval" || f.context(t).Goal.Revision != before.Goal.Revision {
		t.Fatal("未确认已改目标")
	}
	bad := changeCommand(c, "approve")
	bad.Hash = "错误摘要"
	if _, err := f.changes.Change(ctx, f.actor, f.goal, f.session, c.ID, bad); !errors.Is(err, learningchange.ErrConflict) {
		t.Fatal("接受错误批准摘要", err)
	}
	foreign, _ := learningspace.WithScope(ctx, uuid.NewString())
	if _, err := f.changes.Read(foreign, f.actor, f.goal, c.ID, 0); err == nil {
		t.Fatal("跨区可读候选")
	}
	snap := f.context(t)
	revised := c.Candidate
	revised.Goal.Scope = "更明确的目标范围"
	cmd := changeCommand(c, "revise")
	cmd.Candidate = &revised
	cmd.Base = &snap.Base
	v2, err := f.changes.Change(ctx, f.actor, f.goal, f.session, c.ID, cmd)
	if err != nil || v2.Revision != 2 {
		t.Fatal(err)
	}
	if _, err = f.changes.Change(ctx, f.actor, f.goal, f.session, c.ID, changeCommand(c, "approve")); !errors.Is(err, learningchange.ErrConflict) {
		t.Fatal("旧候选批准了新版本", err)
	}
	old, err := f.changes.Read(ctx, f.actor, f.goal, c.ID, 1)
	if err != nil || old.Hash != c.Hash {
		t.Fatal("旧差异丢失", err)
	}
	v2, err = f.changes.Change(ctx, f.actor, f.goal, f.session, c.ID, changeCommand(v2, "approve"))
	if err != nil || v2.Status != "applied" || f.context(t).Goal.GoalManagement().Details.Scope != "更明确的目标范围" {
		t.Fatal("批准未应用", err)
	}
	if _, err = f.changes.Mode(ctx, f.actor, f.goal, &learningchange.Mode{Mode: "cautious", Version: 0}); err != nil {
		t.Fatal(err)
	}
	route := f.propose(t, "route")
	if route.Status != "waiting_approval" {
		t.Fatal("谨慎模式自动应用")
	}
	f.act(t, tutoring.ActionPresentActivity, "")
	route, err = f.changes.Change(ctx, f.actor, f.goal, f.session, route.ID, changeCommand(route, "approve"))
	if err != nil || route.Status != "stale" {
		t.Fatal("版本变化后仍批准", err)
	}
}

func TestPostgreSQLAdaptiveAtomicConflictAndLifecycle(t *testing.T) {
	f := adaptiveFixture(t)
	ctx := context.Background()
	before := f.context(t)
	c := f.propose(t, "route")
	if _, err := f.pool.Exec(ctx, `CREATE FUNCTION fail_change() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION '变更回滚夹具'; END $$; CREATE TRIGGER fail_change BEFORE INSERT ON learning_content_revisions FOR EACH ROW EXECUTE FUNCTION fail_change()`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.changes.Change(ctx, f.actor, f.goal, f.session, c.ID, changeCommand(c, "apply_now")); err == nil {
		t.Fatal("故障被假报成功")
	}
	if got := f.context(t); !reflect.DeepEqual(got.Base, before.Base) {
		t.Fatal("路线或焦点半提交")
	}
	if _, err := f.pool.Exec(ctx, `DROP TRIGGER fail_change ON learning_content_revisions`); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, e := f.changes.Change(ctx, f.actor, f.goal, f.session, c.ID, changeCommand(c, "apply_now"))
			errs <- e
		}()
	}
	wg.Wait()
	close(errs)
	success := 0
	for e := range errs {
		if e == nil {
			success++
		} else if !errors.Is(e, learningchange.ErrConflict) {
			t.Fatal(e)
		}
	}
	if success != 1 {
		t.Fatal("并发不是唯一成功", success)
	}
	queued := f.propose(t, "route")
	if _, err := f.pool.Exec(ctx, `UPDATE device_tokens SET revoked_at=now() WHERE id=$1`, f.actor.TokenID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.changes.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	var status string
	if err := f.pool.QueryRow(ctx, `SELECT status FROM learning_changes WHERE id=$1`, queued.ID).Scan(&status); err != nil || status != "cancelled" {
		t.Fatal("撤销后队列未停止", status, err)
	}
}

func TestPostgreSQLAdaptiveRealMentorToolAndSubmittedAnswer(t *testing.T) {
	f := adaptiveFixture(t)
	ctx := context.Background()
	f.act(t, tutoring.ActionPresentActivity, "")
	f.act(t, tutoring.ActionSubmitAttempt, "我认为需要避免并发写入竞争")
	before := f.context(t)
	attemptID := *before.Session.Context.AttemptID
	baseCalls := int(f.calls.Load())
	candidate := learningchange.Candidate{Kind: "route", Trigger: "user_request", Reason: "先补前置概念", EvidenceIDs: []string{}, Steps: []learningchange.Step{{NodeRevisionID: before.Sources[0].NodeRevisionID, Name: "前置概念", Prompt: "说明并发安全", Criterion: "说明原因", Difficulty: 1, Prerequisites: []int{}}}}
	raw, _ := json.Marshal(candidate)
	f.modelReply = func(w http.ResponseWriter, r *http.Request, n int) bool {
		switch n - baseCalls {
		case 1:
			stream(w, modelclient.Message{Role: "assistant", ToolCalls: []modelclient.ToolCall{{ID: "read", Type: "function", Function: modelclient.ToolFunction{Name: "read_learning_context", Arguments: `{}`}}}})
		case 2:
			stream(w, modelclient.Message{Role: "assistant", ToolCalls: []modelclient.ToolCall{{ID: "change", Type: "function", Function: modelclient.ToolFunction{Name: "propose_learning_change", Arguments: string(raw)}}}})
		default:
			stream(w, modelclient.Message{Role: "assistant", Content: "新的前置练习已排队，本题处理后应用。"})
		}
		return true
	}
	f.create = Create{OperationID: uuid.NewString(), SessionID: uuid.NewString(), TeachingSessionID: f.session, ExpectedVersion: before.Goal.Revision, Prompt: "先补一个前置概念", Save: true, RequestBudget: 3, TokenBudget: 50000}
	r := f.accept(t)
	f.work(t)
	run := f.snapshot(t, r.RunID)
	if run.Status != "succeeded" {
		t.Fatalf("真实导师工具链失败：%+v", run)
	}
	items, err := f.changes.List(ctx, f.actor, f.goal)
	if err != nil || len(items) != 1 || items[0].Status != "queued_for_boundary" {
		t.Fatal("导师没有真正调用变更服务", items, err)
	}
	c, err := f.changes.Change(ctx, f.actor, f.goal, f.session, items[0].ID, changeCommand(items[0], "apply_now"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.changes.Change(ctx, f.actor, f.goal, f.session, c.ID, changeCommand(c, "restore_focus")); err != nil {
		t.Fatal(err)
	}
	after := f.context(t)
	if after.Session.State != tutoring.StateEvaluating || after.Session.Context.AttemptID == nil || *after.Session.Context.AttemptID != attemptID {
		t.Fatal("已提交答案没有恢复原结算合同")
	}
	attempt, err := f.service.starter.Learning.LoadAttempt(ctx, attemptID)
	if err != nil || attempt.ActivityID != before.Base.ActivityID || attempt.Answer != "我认为需要避免并发写入竞争" {
		t.Fatal("答案事实被改写", err)
	}
}

func TestPostgreSQLAdaptiveGoalRevisionAndQueueFences(t *testing.T) {
	for _, action := range []string{"goal_revision", "pause", "archive", "clear"} {
		t.Run(action, func(t *testing.T) {
			f := adaptiveFixture(t)
			ctx := context.Background()
			before := f.context(t)
			if action == "goal_revision" {
				goal := f.propose(t, "goal")
				if _, err := f.changes.Change(ctx, f.actor, f.goal, f.session, goal.ID, changeCommand(goal, "approve")); err != nil {
					t.Fatal(err)
				}
				route := f.propose(t, "route")
				route, err := f.changes.Change(ctx, f.actor, f.goal, f.session, route.ID, changeCommand(route, "apply_now"))
				if err != nil {
					t.Fatal("目标新标准后的路线不能接入", err)
				}
				if route.Applied.ContextID == before.Base.ContextID {
					t.Fatal("新目标复用了旧知识政策")
				}
				if _, err = f.changes.Change(ctx, f.actor, f.goal, f.session, route.ID, changeCommand(route, "restore_focus")); err != nil {
					t.Fatal("新目标标准覆盖旧题返回合同", err)
				}
				if got := f.context(t); got.Base.ActivityID != before.Base.ActivityID {
					t.Fatal("原题丢失")
				}
				return
			}
			if action == "pause" {
				fixtureGoalAction(t, f.runtimeFixture, "start")
			}
			c := f.propose(t, "route")
			if action == "pause" || action == "archive" {
				fixtureGoalAction(t, f.runtimeFixture, action)
				if _, err := f.changes.RunOnce(ctx); err != nil {
					t.Fatal(err)
				}
				read, err := f.changes.Read(ctx, f.actor, f.goal, c.ID, 0)
				if err != nil || read.Status != "cancelled" {
					t.Fatal("生命周期未停止队列", read.Status, err)
				}
				return
			}
			tutor := tutoringdb.New(f.pool)
			p := privacydb.New(f.pool, privacydb.WithReadPermits(privacy.NewReadPermitManager()), privacydb.WithLocalOwner(identitydb.New(f.pool)), privacydb.WithLocalOwner(f.service.starter.Knowledge), privacydb.WithLocalOwner(f.service.starter.Learning), privacydb.WithLocalOwner(tutor), privacydb.WithLocalOwner(memorydb.New(f.pool)), privacydb.WithLocalOwner(outboxdb.New(f.pool)))
			grants, err := privacy.NewErasureGrantService(privacydb.NewGrantStore(f.pool), privacy.ErasureGrantOptions{})
			if err != nil {
				t.Fatal(err)
			}
			grant, err := grants.Issue(ctx, f.actor.Device.ID, "教学变更隐私验收")
			if err != nil {
				t.Fatal(err)
			}
			now := time.Now().UTC()
			barrier, err := p.CommitBarrierAuthorized(ctx, privacy.ErasureRequest{DeviceID: f.actor.Device.ID, OperationID: uuid.NewString(), ActorDeviceID: f.actor.Device.ID, ReasonCode: string(privacy.ReasonLearnerRequest), RequestedAt: now, ManagedBackupUnrecoverableAfter: now.Add(24 * time.Hour), ExpectedCurrentLearnerGeneration: 1}, privacy.NewErasureGrantAuthorization(f.actor.Device.ID, grant.Token))
			if err != nil {
				t.Fatal(err)
			}
			if _, err = f.changes.Read(ctx, f.actor, f.goal, c.ID, 0); err == nil {
				t.Fatal("清除屏障后仍可读候选")
			}
			if _, err = p.RunLocalScrub(ctx, barrier.ErasureID); err != nil {
				t.Fatal(err)
			}
			var count int
			if err = f.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM learning_changes WHERE ciphertext IS NOT NULL OR status<>'cancelled')+(SELECT count(*) FROM learning_change_revisions WHERE ciphertext IS NOT NULL)+(SELECT count(*) FROM learning_change_operations WHERE request_hash<>'')+(SELECT count(*) FROM learning_change_events)`).Scan(&count); err != nil || count != 0 {
				t.Fatal("清除仍有候选、差异或回执正文", count, err)
			}
			if n, err := f.changes.RunOnce(ctx); err != nil || n != 0 {
				t.Fatal("清除后队列复活", n, err)
			}
		})
	}
}
