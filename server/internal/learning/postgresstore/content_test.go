package postgresstore_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/identity"
	identitydb "github.com/edu-agent/edu-agent/server/internal/identity/postgresstore"
	knowledgedb "github.com/edu-agent/edu-agent/server/internal/knowledge/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/learning"
	"github.com/edu-agent/edu-agent/server/internal/learningcontent"
	"github.com/edu-agent/edu-agent/server/internal/learningspace"
	memorydb "github.com/edu-agent/edu-agent/server/internal/memory/postgresstore"
	outboxdb "github.com/edu-agent/edu-agent/server/internal/platform/outbox/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/privacy"
	privacydb "github.com/edu-agent/edu-agent/server/internal/privacy/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/tutoring"
	tutoringdb "github.com/edu-agent/edu-agent/server/internal/tutoring/postgresstore"
	"github.com/google/uuid"
)

func TestPostgreSQLLearningContentVersionsAnswersAndErasure(t *testing.T) {
	pool, authority := newA101WorkItemStore(t)
	sessionID, _ := commitLearningAuthorityFixture(t, authority, false, true)
	ctx := context.Background()
	actor := identity.Credential{Device: identity.Device{ID: learningDeviceOne}, TokenID: uuid.NewString()}
	if _, err := pool.Exec(ctx, `INSERT INTO device_tokens(id,device_id,token_hash,scopes,created_at) VALUES($1,$2,decode(repeat('31',32),'hex'),ARRAY['learning:read','learning:write'],now())`, actor.TokenID, actor.Device.ID); err != nil {
		t.Fatal(err)
	}
	key := bytes.Repeat([]byte{42}, 32)
	content, err := learningcontent.New(pool, authority, key)
	if err != nil {
		t.Fatal(err)
	}
	authority.ConfigureContent(content)
	view, err := authority.Session(ctx, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	original := *view.WorkItem.Activity
	var eventCount int64
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM learning_events`).Scan(&eventCount); err != nil {
		t.Fatal(err)
	}
	r, err := content.Ensure(ctx, actor, sessionID, "{"+original.ID+"}")
	if err != nil {
		t.Fatal(err)
	}
	if r.ActivityID != original.ID || r.Version != 1 || len(r.Body.Blocks) < 3 || r.Body.Blocks[0].Text != original.Prompt {
		t.Fatalf("适配失败：%+v", r)
	}
	assertCount(t, pool, `SELECT count(*) FROM learning_events`, eventCount)
	assertCount(t, pool, `SELECT count(*) FROM learning_content_revisions`, 1)
	restarted, _ := learningcontent.New(pool, authority, key)
	read, err := restarted.Get(ctx, actor, r.ArtifactID, 1)
	if err != nil || !reflect.DeepEqual(read, r) {
		t.Fatalf("重启未得到同一正式版本：%v", err)
	}
	other, _ := learningspace.WithScope(ctx, uuid.NewString())
	if _, err = content.Get(other, actor, r.ArtifactID, 1); !errors.Is(err, learningcontent.ErrNotFound) {
		t.Fatalf("跨区泄漏：%v", err)
	}
	if _, err = content.Ensure(other, actor, sessionID, original.ID); !errors.Is(err, learningcontent.ErrNotFound) {
		t.Fatalf("跨区可以适配旧活动：%v", err)
	}
	concurrent := make(chan error, 2)
	for range 2 {
		go func() {
			value, e := content.Ensure(ctx, actor, sessionID, original.ID)
			if e == nil && value.ArtifactID != r.ArtifactID {
				e = errors.New("标签页生成了不同的正文身份")
			}
			concurrent <- e
		}()
	}
	for range 2 {
		if e := <-concurrent; e != nil {
			t.Fatal(e)
		}
	}
	assertCount(t, pool, `SELECT count(*) FROM learning_content_revisions`, 1)
	if _, err = pool.Exec(ctx, `UPDATE device_tokens SET scopes=ARRAY['learning:read'] WHERE id=$1`, actor.TokenID); err != nil {
		t.Fatal(err)
	}
	if _, err = content.Ensure(ctx, actor, sessionID, original.ID); !errors.Is(err, learningcontent.ErrForbidden) {
		t.Fatalf("收回写权限后仍能物化正文：%v", err)
	}
	if _, err = content.Get(ctx, actor, r.ArtifactID, 1); err != nil {
		t.Fatalf("只读权限不能读取正文：%v", err)
	}
	if _, err = pool.Exec(ctx, `UPDATE device_tokens SET revoked_at=now() WHERE id=$1`, actor.TokenID); err != nil {
		t.Fatal(err)
	}
	if _, err = content.Get(ctx, actor, r.ArtifactID, 1); !errors.Is(err, learningcontent.ErrForbidden) {
		t.Fatalf("撤销凭据仍可读取正文：%v", err)
	}
	if _, err = pool.Exec(ctx, `UPDATE device_tokens SET revoked_at=NULL,scopes=ARRAY['learning:read','learning:write'] WHERE id=$1`, actor.TokenID); err != nil {
		t.Fatal(err)
	}
	if _, err = content.Ensure(ctx, actor, uuid.NewString(), original.ID); !errors.Is(err, learningcontent.ErrNotFound) {
		t.Fatalf("错会话可适配：%v", err)
	}
	var raw []byte
	if err = pool.QueryRow(ctx, `SELECT ciphertext FROM learning_content_revisions WHERE artifact_id=$1`, r.ArtifactID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte(original.Prompt)) || bytes.Contains(raw, []byte("slice_sha256")) {
		t.Fatal("正文或引用未加密")
	}
	if _, err = pool.Exec(ctx, `UPDATE learning_content_revisions SET status='failed' WHERE artifact_id=$1`, r.ArtifactID); err == nil {
		t.Fatal("历史可覆盖")
	}
	cmd := learningcontent.Commit{ProtocolVersion: 1, OperationID: uuid.NewString(), ExpectedVersion: 1, Status: "draft", Blocks: r.Body.Blocks[:1], Interaction: r.Body.Interaction}
	draft, err := content.Commit(ctx, actor, r.ArtifactID, cmd)
	if err != nil {
		t.Fatal(err)
	}
	current, err := content.Ensure(ctx, actor, sessionID, original.ID)
	if err != nil || current.Version != 1 {
		t.Fatalf("草稿替换正式版：%v", err)
	}
	cmd.OperationID = uuid.NewString()
	cmd.ExpectedVersion = 2
	cmd.Status = "committed"
	if _, err = content.Commit(ctx, actor, r.ArtifactID, cmd); !errors.Is(err, learningcontent.ErrInvalid) {
		t.Fatalf("未完成题目被提交：%v", err)
	}
	cmd.Blocks = r.Body.Blocks
	cmd.Interaction = learningcontent.Interaction{Kind: "future_input"}
	future, err := content.Commit(ctx, actor, r.ArtifactID, cmd)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := content.Commit(ctx, actor, r.ArtifactID, cmd)
	if err != nil || replay.Version != future.Version {
		t.Fatalf("版本操作不幂等：%v", err)
	}
	cmd.Blocks = append([]learningcontent.Block{{ID: uuid.NewString(), Kind: "callout", Text: "额外提示", Fallback: "额外提示"}}, cmd.Blocks...)
	if _, err = content.Commit(ctx, actor, r.ArtifactID, cmd); !errors.Is(err, learningcontent.ErrConflict) {
		t.Fatalf("操作身份复用未冲突：%v", err)
	}
	service, err := learning.NewService(authority, authority, integrationKnowledgeResolver{}, learning.ServiceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	apply := func(c context.Context, action tutoring.Action, answer string) (learning.OperationResult, error) {
		v, e := service.Session(ctx, sessionID)
		if e != nil {
			t.Fatal(e)
		}
		payload, _ := json.Marshal(map[string]any{"action": action, "answer": answer})
		return service.ApplyAction(c, actor.Device.ID, sessionID, learning.ActionCommand{Operation: learning.OperationEnvelope{OperationID: uuid.NewString(), PayloadSchemaVersion: 1, AggregateType: "session", AggregateID: sessionID, ExpectedVersion: v.Session.AggregateVer, Payload: payload}, Action: action, Answer: answer, Help: learning.HelpNone})
	}
	if _, err = apply(ctx, tutoring.ActionPresentActivity, ""); err != nil {
		t.Fatal(err)
	}
	for _, v := range []int64{1, draft.Version, future.Version} {
		_, err = apply(content.WithAnswer(ctx, actor, r.ArtifactID, v), tutoring.ActionSubmitAttempt, "ok")
		if err == nil {
			t.Fatalf("旧版本/草稿/未知交互仍能正式作答，version=%d", v)
		}
	}
	assertCount(t, pool, `SELECT count(*) FROM learning_attempts`, 0)
	if _, err = apply(ctx, tutoring.ActionSubmitAttempt, "ok"); !errors.Is(err, learningcontent.ErrUnsupported) {
		t.Fatalf("旧入口必须拒绝未知作答规则，实际：%v", err)
	}
	assertCount(t, pool, `SELECT count(*) FROM learning_attempts`, 0)
	cmd.OperationID = uuid.NewString()
	cmd.ExpectedVersion = future.Version
	cmd.Interaction = r.Body.Interaction
	committed, err := content.Commit(ctx, actor, r.ArtifactID, cmd)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = apply(content.WithAnswer(ctx, actor, r.ArtifactID, committed.Version), tutoring.ActionSubmitAttempt, "ok"); err != nil {
		t.Fatal(err)
	}
	accepted, err := service.Session(ctx, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	attemptID := accepted.WorkItem.Attempt.ID
	feedback, err := service.Feedback(ctx, attemptID, false)
	if err != nil || feedback.Status != "received" || feedback.Content == nil || feedback.Content.Version != committed.Version || feedback.Receipt.OperationID == "" {
		t.Fatalf("答案接收后缺少冻结版本或回执：%+v %v", feedback, err)
	}
	request := learning.ProposalRequest{RequestID: uuid.NewString(), Type: learning.ProposalAssessment,
		AggregateType: "session", AggregateID: sessionID, AggregateVersion: accepted.Session.AggregateVer,
		GoalRevisionID: original.GoalRevisionID, ActivityID: original.ID, AttemptID: attemptID,
		KnowledgeRevisionID: original.KnowledgeRevisionID, Input: json.RawMessage(`{}`)}
	requestHash, err := learning.HashJSON(request)
	if err != nil {
		t.Fatal(err)
	}
	claim, err := authority.ClaimProposal(ctx, actor.Device.ID, request, requestHash, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	checkPhase := func(want string) {
		t.Helper()
		detail, e := service.Feedback(ctx, attemptID, false)
		page, pe := service.ListFeedback(ctx, learning.FeedbackQuery{Status: "pending", SessionID: sessionID, Page: learning.CursorPageRequest{Limit: 10}})
		if e != nil || pe != nil || detail.Status != want || len(page.Items) != 1 || page.Items[0].Status != want || detail.Receipt != feedback.Receipt {
			t.Fatalf("原答案阶段或回执不一致：want=%s detail=%+v page=%+v errors=%v/%v", want, detail, page, e, pe)
		}
	}
	checkPhase("processing")
	if _, err = pool.Exec(ctx, `UPDATE tutoring_proposal_requests SET lease_expires_at=clock_timestamp()-interval '1 second' WHERE request_id=$1`, request.RequestID); err != nil {
		t.Fatal(err)
	}
	checkPhase("unknown")
	claim, err = authority.ClaimProposal(ctx, actor.Device.ID, request, requestHash, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if err = authority.FailProposal(ctx, actor.Device.ID, claim.LeaseToken, []string{"fixture_failure"}, "fixture_failure", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	checkPhase("failed")
	if _, err = apply(ctx, tutoring.ActionRecordAssessment, ""); err != nil {
		t.Fatal(err)
	}
	view, err = service.Session(ctx, sessionID)
	if err != nil || view.Session.State != tutoring.StateFeedback || view.WorkItem.AssessmentDecision.Disposition != learning.DispositionAccepted {
		t.Fatalf("正式答案未生成真实反馈：%+v %v", view, err)
	}
	if !reflect.DeepEqual(*view.WorkItem.Activity, original) {
		t.Fatal("内容改样式改变了原活动")
	}
	history, err := content.History(ctx, actor, r.ArtifactID)
	if err != nil || len(history) != 4 {
		t.Fatalf("版本历史缺失：%v", err)
	}
	var operationID string
	if err = pool.QueryRow(ctx, `SELECT operation_id FROM learning_events WHERE event_type='AttemptSubmitted' LIMIT 1`).Scan(&operationID); err != nil {
		t.Fatal(err)
	}
	receipt, err := service.SessionOperation(ctx, actor.Device.ID, sessionID, operationID)
	if err != nil || receipt.Status != "succeeded" {
		t.Fatalf("丢响应操作不能核对：%v", err)
	}
	if _, err = service.SessionOperation(ctx, uuid.NewString(), sessionID, operationID); learning.ErrorCode(err) != learning.CodeNotFound {
		t.Fatalf("其他设备能读原操作：%v", err)
	}
	if _, err = apply(ctx, tutoring.ActionAcknowledgeFeedback, ""); err != nil {
		t.Fatal(err)
	}
	feedback, err = service.Feedback(ctx, attemptID, false)
	if err != nil || feedback.Status != "settled" || len(feedback.Evidence) != 1 || len(feedback.Decisions) != 1 {
		t.Fatalf("离开活动后无法查证原评估：%+v %v", feedback, err)
	}
	if _, err = service.Feedback(other, attemptID, false); learning.ErrorCode(err) != learning.CodeNotFound {
		t.Fatalf("跨区泄漏评估：%v", err)
	}
	page, err := service.ListFeedback(ctx, learning.FeedbackQuery{Status: "all", Page: learning.CursorPageRequest{Limit: 1}})
	if err != nil || len(page.Items) != 1 || page.Items[0].AttemptID != attemptID {
		t.Fatalf("缺少历史列表：%+v %v", page, err)
	}
	decisionCommand := learning.AssessmentDecisionCommand{
		Operation: learning.OperationEnvelope{OperationID: uuid.NewString(), PayloadSchemaVersion: 1, AggregateType: "session", AggregateID: sessionID, ExpectedVersion: feedback.SessionVersion, Payload: json.RawMessage(`{"kind":"void"}`)},
		Kind:      "void", ExpectedDispositionVersion: 1, Reason: "历史复核发现原结论有误",
	}
	if _, err = service.Decide(ctx, actor.Device.ID, feedback.Assessment.ID, decisionCommand); err != nil {
		t.Fatal(err)
	}
	decisionReplay, err := service.Decide(ctx, actor.Device.ID, feedback.Assessment.ID, decisionCommand)
	if err != nil || !decisionReplay.Replayed {
		t.Fatalf("历史作废重放失败：%+v %v", decisionReplay, err)
	}
	if _, err = service.Decide(ctx, actor.Device.ID, uuid.NewString(), decisionCommand); learning.ErrorCode(err) != learning.CodeOperationConflict {
		t.Fatalf("操作重放越过原评估：%v", err)
	}
	feedback, err = service.Feedback(ctx, attemptID, false)
	if err != nil || len(feedback.Evidence) != 0 || len(feedback.Decisions) != 2 || feedback.Decisions[0].Disposition != learning.DispositionAccepted || feedback.Decisions[1].Disposition != learning.DispositionVoided || feedback.Attempt.Answer != "ok" {
		t.Fatalf("作废未追加历史或改写了原答案：%+v %v", feedback, err)
	}
	if _, err = authority.Rebuild(ctx); err != nil {
		t.Fatal(err)
	}
	if restored, e := service.Feedback(ctx, attemptID, false); e != nil || !reflect.DeepEqual(restored, feedback) {
		t.Fatalf("重放改变了冻结反馈：%v", e)
	}
	for range 2 {
		go func() {
			c := cmd
			c.OperationID, c.ExpectedVersion, c.Status = uuid.NewString(), committed.Version, "failed"
			_, e := content.Commit(ctx, actor, r.ArtifactID, c)
			concurrent <- e
		}()
	}
	succeeded, conflicted := 0, 0
	for range 2 {
		e := <-concurrent
		switch {
		case e == nil:
			succeeded++
		case errors.Is(e, learningcontent.ErrConflict):
			conflicted++
		default:
			t.Fatal(e)
		}
	}
	if succeeded != 1 || conflicted != 1 {
		t.Fatalf("并发版本未正确比较：成功 %d，冲突 %d", succeeded, conflicted)
	}
	if old, e := service.Feedback(ctx, attemptID, false); e != nil || old.Content.Version != committed.Version {
		t.Fatalf("内容更新重解释旧答案：%v", e)
	}
	// 全局隐私流程必须委派到正文 owner，并阻止旧版本与迟到提交复活。
	pinned := int64(1)
	if _, err = content.Preference(ctx, actor, r.ArtifactID, &learningcontent.Preference{Favorite: true, PinnedVersion: &pinned}); err != nil {
		t.Fatal("无法保存清除验收的阅读偏好", err)
	}
	privacyStore := privacydb.New(pool, privacydb.WithLocalOwner(identitydb.New(pool)), privacydb.WithLocalOwner(knowledgedb.New(pool)), privacydb.WithLocalOwner(authority), privacydb.WithLocalOwner(tutoringdb.New(pool)), privacydb.WithLocalOwner(memorydb.New(pool)), privacydb.WithLocalOwner(outboxdb.New(pool)))
	now := time.Now().UTC()
	barrier, err := privacyStore.CommitBarrier(ctx, privacy.ErasureRequest{DeviceID: actor.Device.ID, ActorDeviceID: actor.Device.ID, OperationID: uuid.NewString(), ReasonCode: "learner_request", RequestedAt: now, ManagedBackupUnrecoverableAfter: now.Add(24 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = privacyStore.RunLocalScrub(ctx, barrier.ErasureID); err != nil {
		t.Fatal(err)
	}
	if _, err = service.Feedback(ctx, attemptID, false); err == nil {
		t.Fatal("清除后仍可读取评估正文")
	}
	assertCount(t, pool, `SELECT count(*) FROM learning_attempts WHERE artifact_id IS NOT NULL OR artifact_version IS NOT NULL`, 0)
	remaining, err := learningcontent.Remaining(ctx, pool)
	if err != nil || remaining != 0 {
		t.Fatalf("内容清除有残留：%d %v", remaining, err)
	}
	if _, err = content.Get(ctx, actor, r.ArtifactID, 1); err == nil {
		t.Fatal("旧版本复活")
	}
	if _, err = content.Commit(ctx, actor, r.ArtifactID, cmd); err == nil {
		t.Fatal("迟到提交复活")
	}
	if _, err = content.Ensure(ctx, actor, sessionID, original.ID); err == nil {
		t.Fatal("清除后旧活动重新物化")
	}
}
