package mentorrun

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/edu-agent/edu-agent/packages/agentcore/modelclient"
	"github.com/edu-agent/edu-agent/server/internal/learningspace"
	"github.com/edu-agent/edu-agent/server/internal/settings"
	"github.com/google/uuid"
)

func newHistory(t *testing.T, f *runtimeFixture, saved bool, goal string) string {
	t.Helper()
	id, err := f.service.NewConversation(context.Background(), f.actor, learningspace.DefaultID, NewConversation{ID: uuid.NewString(), GoalID: goal, Saved: saved})
	if err != nil {
		t.Fatal(err)
	}
	return id
}
func historyPage(t *testing.T, f *runtimeFixture, id string, after int64, limit int) TurnPage {
	t.Helper()
	var page TurnPage
	if err := f.service.ReadConversation(context.Background(), f.actor, learningspace.DefaultID, id, after, limit, func(p TurnPage) error { page = p; return nil }); err != nil {
		t.Fatal(err)
	}
	return page
}
func historyRequest(t *testing.T, f *runtimeFixture, id, prompt string) Create {
	t.Helper()
	c := historyPage(t, f, id, 0, 1).Conversation
	return Create{ConversationID: id, SessionID: id, ConversationVersion: c.Version, OperationID: uuid.NewString(), ExpectedVersion: c.GoalVersion, TeachingSessionID: c.TeachingSessionID, Prompt: prompt, Save: c.Saved, RequestBudget: 4, TokenBudget: 50000}
}
func sendHistory(t *testing.T, f *runtimeFixture, id, prompt string) Receipt {
	t.Helper()
	request := historyRequest(t, f, id, prompt)
	c := historyPage(t, f, id, 0, 1).Conversation
	r, err := f.service.Create(context.Background(), f.actor, learningspace.DefaultID, runtimeGoal(c.GoalID), request)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestPostgreSQLTutorHistoryMultiTurnRestartExpiryAndDelete(t *testing.T) {
	f := fixture(t, func(w http.ResponseWriter, r *http.Request, n int) {
		raw, _ := io.ReadAll(r.Body)
		if n == 2 && (!bytes.Contains(raw, []byte("首轮私密问题")) || !bytes.Contains(raw, []byte("首轮正式回答"))) {
			t.Error("下一轮丢失已提交历史")
		}
		stream(w, modelclient.Message{Role: "assistant", Content: "首轮正式回答"})
	})
	ctx := context.Background()
	id := newHistory(t, f, true, f.goal)
	first := sendHistory(t, f, id, "首轮私密问题")
	f.work(t)
	second := sendHistory(t, f, id, "再解释一次")
	f.work(t)
	page := historyPage(t, f, id, 0, 1)
	if len(page.Items) != 1 || page.NextCursor != 1 || page.Items[0].Output != "首轮正式回答" {
		t.Fatalf("轮次分页错误：%+v", page)
	}
	if next := historyPage(t, f, id, page.NextCursor, 1); len(next.Items) != 1 || next.Items[0].RunID != second.RunID || len(next.Items[0].Messages) != 2 {
		t.Fatalf("重复存入累积历史：%+v", next)
	}
	var stored []byte
	if err := f.pool.QueryRow(ctx, `SELECT ciphertext FROM learning_tutor_turns WHERE run_id=$1`, first.RunID).Scan(&stored); err != nil || bytes.Contains(stored, []byte("首轮")) {
		t.Fatal("轮次没有加密", err)
	}
	if _, err := f.pool.Exec(ctx, `UPDATE learning_mentor_runs SET expires_at=clock_timestamp()-interval '1 second',state=jsonb_set(state,'{expires_at}',to_jsonb(clock_timestamp()-interval '1 second'))`); err != nil {
		t.Fatal(err)
	}
	restarted, err := New(f.pool, f.settings, bytes.Repeat([]byte{42}, 32))
	if err != nil {
		t.Fatal(err)
	}
	f.service = restarted
	if _, err = f.service.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	page = historyPage(t, f, id, 0, 20)
	if len(page.Items) != 2 || !page.Items[0].BodyAvailable || page.Items[0].Output != "首轮正式回答" || f.calls.Load() != 2 {
		t.Fatal("重启/七日清理丢历史或重放模型")
	}
	other := newHistory(t, f, true, f.goal)
	if other == id {
		t.Fatal("新建复用了旧会话")
	}
	var list ConversationPage
	if err = f.service.ReadConversations(ctx, f.actor, learningspace.DefaultID, ConversationQuery{GoalID: f.goal, Limit: 1}, func(p ConversationPage) error { list = p; return nil }); err != nil || len(list.Items) != 1 || list.NextCursor == "" {
		t.Fatal("历史列表分页失败", err)
	}
	if err = f.service.ChangeConversation(ctx, f.actor, learningspace.DefaultID, id, page.Conversation.Version, "概率复习", false); err != nil {
		t.Fatal(err)
	}
	if err = f.service.ReadConversations(ctx, f.actor, learningspace.DefaultID, ConversationQuery{GoalID: f.goal, Search: "概率", Limit: 20}, func(p ConversationPage) error { list = p; return nil }); err != nil || len(list.Items) != 1 || list.Items[0].ID != id {
		t.Fatal("标题检索失败", err)
	}
	page = historyPage(t, f, id, 0, 20)
	if err = f.service.ChangeConversation(ctx, f.actor, learningspace.DefaultID, id, page.Conversation.Version, "", true); err != nil {
		t.Fatal(err)
	}
	if err = f.service.ReadConversation(ctx, f.actor, learningspace.DefaultID, id, 0, 20, func(TurnPage) error { t.Error("已删除历史仍发送"); return nil }); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	var remaining int
	if err = f.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM learning_tutor_turns WHERE conversation_id=$1)+(SELECT count(*) FROM learning_mentor_runs WHERE session_id=$1 AND checkpoint IS NOT NULL)+(SELECT count(*) FROM learning_tutor_conversations WHERE id=$1 AND (title IS NOT NULL OR NOT deleted))`, id).Scan(&remaining); err != nil || remaining != 0 {
		t.Fatal("删除存在正文残留", err)
	}
	var text string
	if err = f.pool.QueryRow(ctx, `SELECT goal_text FROM learning_goal_revisions WHERE goal_id=$1`, f.goal).Scan(&text); err != nil || !strings.Contains(text, "真实绑定目标") {
		t.Fatal("删除聊天误删业务事实", err)
	}
}

func TestPostgreSQLTutorHistoryTemporaryAndOptionalGoal(t *testing.T) {
	f := fixture(t, func(w http.ResponseWriter, r *http.Request, n int) {
		var request modelclient.Request
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		for _, tool := range request.Tools {
			if tool.Function.Name == "read_learning_progress" || tool.Function.Name == "open_references" || tool.Function.Name == "read_references" {
				t.Errorf("无目标对话不应提供目标绑定工具：%s", tool.Function.Name)
			}
		}
		stream(w, modelclient.Message{Role: "assistant", Content: "临时秘密回答"})
	})
	id := newHistory(t, f, false, "")
	sendHistory(t, f, id, "临时秘密正文")
	f.work(t)
	ctx := context.Background()
	page := historyPage(t, f, id, 0, 20)
	if !page.Items[0].BodyAvailable || page.Conversation.GoalID != "" {
		t.Fatal("无目标交流未运行")
	}
	var remaining int
	if err := f.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM learning_tutor_conversations WHERE id=$1 AND title IS NOT NULL)+(SELECT count(*) FROM learning_tutor_turns WHERE conversation_id=$1 AND ciphertext IS NOT NULL)+(SELECT count(*) FROM learning_mentor_runs WHERE session_id=$1 AND (checkpoint IS NOT NULL OR state::text LIKE '%秘密%'))`, id).Scan(&remaining); err != nil || remaining != 0 {
		t.Fatal("临时正文落库", err)
	}
	request := historyRequest(t, f, id, "下一轮")
	restarted, _ := New(f.pool, f.settings, bytes.Repeat([]byte{42}, 32))
	f.service = restarted
	page = historyPage(t, f, id, 0, 20)
	if page.Conversation.StorageState != "temporary_unavailable" || page.Items[0].BodyAvailable {
		t.Fatal("重启伪称可恢复临时正文")
	}
	if _, err := f.service.Create(ctx, f.actor, learningspace.DefaultID, uuid.Nil.String(), request); !errors.Is(err, ErrTemporary) {
		t.Fatal("丢失临时正文仍继续", err)
	}
	request.Save = true
	if _, err := f.service.Create(ctx, f.actor, learningspace.DefaultID, uuid.Nil.String(), request); !errors.Is(err, ErrInvalid) {
		t.Fatal("临时数据转为永久", err)
	}
}

func TestPostgreSQLTutorHistoryDestinationBindingAndNoReplay(t *testing.T) {
	f := fixture(t, func(w http.ResponseWriter, r *http.Request, n int) {
		stream(w, modelclient.Message{Role: "assistant", Content: "目的地验证"})
	})
	ctx := context.Background()
	id := newHistory(t, f, true, f.goal)
	sendHistory(t, f, id, "历史只能在同意后发送")
	f.work(t)
	// 模拟原会话绑定的历史端点；当前 fixture 端点是新的目的地。
	if _, err := f.pool.Exec(ctx, `UPDATE learning_tutor_conversations SET endpoint='https://old.example/v1' WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	page := historyPage(t, f, id, 0, 20)
	request := historyRequest(t, f, id, "新的问题")
	if !page.Conversation.ConfirmationRequired || f.calls.Load() != 1 {
		t.Fatal("恢复触发外发或未要求确认")
	}
	for _, confirmation := range []string{"", "https://old.example/v1", "不正确的目的地"} {
		request.ConfirmDestination = confirmation
		if _, err := f.service.Create(ctx, f.actor, learningspace.DefaultID, f.goal, request); !errors.Is(err, ErrDestination) {
			t.Fatal("未确认的历史外发被接受", err)
		}
	}
	request.ConfirmDestination = page.Conversation.Destination
	r, err := f.service.Create(ctx, f.actor, learningspace.DefaultID, f.goal, request)
	if err != nil {
		t.Fatal(err)
	}
	if repeat, e := f.service.Create(ctx, f.actor, learningspace.DefaultID, f.goal, request); e != nil || repeat != r {
		t.Fatal("确认重试重复创建", e)
	}
	f.work(t)
	if f.calls.Load() != 2 {
		t.Fatal("模型请求数量错误")
	}
	request = historyRequest(t, f, id, "非法重标记")
	request.TeachingSessionID = uuid.NewString()
	if _, err = f.service.Create(ctx, f.actor, learningspace.DefaultID, f.goal, request); err == nil {
		t.Fatal("允许重标记课堂")
	}
	if err = f.service.ReadConversation(ctx, f.actor, uuid.NewString(), id, 0, 20, func(TurnPage) error { t.Error("跨区泄漏"); return nil }); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
}

func TestPostgreSQLTutorHistoryStorageFailureAndCorruption(t *testing.T) {
	f := fixture(t, func(w http.ResponseWriter, r *http.Request, n int) {
		stream(w, modelclient.Message{Role: "assistant", Content: "已保存回答"})
	})
	ctx := context.Background()
	id := newHistory(t, f, true, f.goal)
	request := historyRequest(t, f, id, "不能静默丢失")
	if _, err := f.pool.Exec(ctx, `CREATE FUNCTION reject_tutor_turn() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION '存储故障夹具'; END $$; CREATE TRIGGER reject_tutor_turn BEFORE INSERT ON learning_tutor_turns FOR EACH ROW EXECUTE FUNCTION reject_tutor_turn()`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.Create(ctx, f.actor, learningspace.DefaultID, f.goal, request); err == nil {
		t.Fatal("存储失败仍报告成功")
	}
	if len(historyPage(t, f, id, 0, 20).Items) != 0 || f.calls.Load() != 0 {
		t.Fatal("故障留下部分提交或外发")
	}
	if _, err := f.pool.Exec(ctx, `DROP TRIGGER reject_tutor_turn ON learning_tutor_turns`); err != nil {
		t.Fatal(err)
	}
	r := sendHistory(t, f, id, "不能静默丢失")
	f.work(t)
	wrong, _ := New(f.pool, f.settings, bytes.Repeat([]byte{43}, 32))
	if err := wrong.ReadConversation(ctx, f.actor, learningspace.DefaultID, id, 0, 20, func(TurnPage) error { t.Error("错误密钥回退空历史"); return nil }); !errors.Is(err, ErrStorage) {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `UPDATE learning_tutor_turns SET ciphertext=decode('deadbeef','hex') WHERE run_id=$1`, r.RunID); err != nil {
		t.Fatal(err)
	}
	if err := f.service.ReadConversation(ctx, f.actor, learningspace.DefaultID, id, 0, 20, func(TurnPage) error { t.Error("损坏历史降级成功"); return nil }); !errors.Is(err, ErrStorage) {
		t.Fatal(err)
	}
	pageVersion := int64(2)
	if err := f.service.ChangeConversation(ctx, f.actor, learningspace.DefaultID, id, pageVersion, "", true); err != nil {
		t.Fatal("损坏历史不能明确删除", err)
	}
}

func TestHistorySchemaAndIncompleteToolGroups(t *testing.T) {
	a, _ := newCipher(bytes.Repeat([]byte{42}, 32))
	aad := historyAAD(uuid.NewString(), 1, "title")
	nonce := bytes.Repeat([]byte{1}, a.NonceSize())
	raw := a.Seal(nonce, nonce, []byte(`{"version":2,"value":{"text":"未来"}}`), []byte(aad))
	var title conversationTitle
	if err := openHistory(a, aad, raw, &title); !errors.Is(err, ErrHistorySchema) {
		t.Fatal("未来格式未拒绝", err)
	}
	messages := []modelclient.Message{{Role: "user", Content: "已提交"}, {Role: "assistant", ToolCalls: []modelclient.ToolCall{{ID: "pending"}}}}
	if got := committedMessages(messages); len(got) != 1 {
		t.Fatal("未完成工具组进入历史")
	}
	messages = append(messages, modelclient.Message{Role: "tool", ToolCallID: "pending", Content: "已提交来源"})
	if got := committedMessages(messages); len(got) != 3 {
		t.Fatal("已提交工具组丢失边界")
	}
	encoded, _ := json.Marshal(turnBody(row{body: Body{Messages: messages, HistoryCount: 1}}))
	if bytes.Contains(encoded, []byte(`"history_count"`)) {
		t.Fatal("轮次包含其他轮历史")
	}
}

func TestPostgreSQLTutorHistoryExpiredInteraction(t *testing.T) {
	f := fixture(t, func(w http.ResponseWriter, r *http.Request, n int) {
		stream(w, modelclient.Message{Role: "assistant", ToolCalls: []modelclient.ToolCall{{ID: "ask", Type: "function", Function: modelclient.ToolFunction{Name: "ask_user", Arguments: `{"question":"选择","choices":[]}`}}}})
	})
	id := newHistory(t, f, true, f.goal)
	receipt := sendHistory(t, f, id, "需要澄清")
	f.work(t)
	snapshot := f.snapshot(t, receipt.RunID)
	if snapshot.Interaction == nil {
		t.Fatal("没有待审交互")
	}
	if _, err := f.pool.Exec(context.Background(), `UPDATE learning_mentor_runs SET state=jsonb_set(state,'{expires_at}',to_jsonb($2::timestamptz)) WHERE id=$1`, receipt.RunID, time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	_, err := f.service.Command(context.Background(), f.actor, learningspace.DefaultID, receipt.RunID, Command{OperationID: uuid.NewString(), ExpectedVersion: snapshot.Version, Kind: "respond", InteractionID: snapshot.Interaction.ID, Answer: "已过期"})
	if !errors.Is(err, ErrInactive) || f.calls.Load() != 1 {
		t.Fatal("过期交互恢复执行", err)
	}
}

func TestPostgreSQLTutorHistoryConcurrentSubmissionAndLateOutput(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	f := fixture(t, func(w http.ResponseWriter, r *http.Request, n int) {
		close(started)
		select {
		case <-release:
		case <-r.Context().Done():
		}
		stream(w, modelclient.Message{Role: "assistant", Content: "不得复活的迟到输出"})
	})
	f.service.Lease = time.Second
	ctx := context.Background()
	id := newHistory(t, f, true, f.goal)
	request := SubmitTurn{OperationID: uuid.NewString(), ExpectedVersion: 1, Prompt: "并发问题", RequestBudget: 2, TokenBudget: 50000}
	var wait sync.WaitGroup
	receipts := make(chan Receipt, 2)
	failures := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			r, e := f.service.Submit(ctx, f.actor, learningspace.DefaultID, id, request)
			receipts <- r
			failures <- e
		}()
	}
	wait.Wait()
	first, second := <-receipts, <-receipts
	if e1, e2 := <-failures, <-failures; e1 != nil || e2 != nil || first != second {
		t.Fatal("同操作并发分叉", e1, e2)
	}
	work := make(chan error, 1)
	go func() { _, e := f.service.RunOnce(ctx); work <- e }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("模型没有启动")
	}
	page := historyPage(t, f, id, 0, 20)
	if err := f.service.ChangeConversation(ctx, f.actor, learningspace.DefaultID, id, page.Conversation.Version, "", true); err != nil {
		close(release)
		t.Fatal(err)
	}
	close(release)
	select {
	case err := <-work:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("删除后运行未退出")
	}
	if snapshot := f.snapshot(t, first.RunID); snapshot.BodyAvailable || snapshot.Output != "" || snapshot.Status != "cancelled" {
		t.Fatal("删除后迟到输出复活")
	}
	if f.calls.Load() != 1 {
		t.Fatal("并发导致双模型流")
	}
}

func TestPostgreSQLTutorHistoryQuotaAndOperationAfterGoalChange(t *testing.T) {
	f := fixture(t, func(w http.ResponseWriter, r *http.Request, n int) {
		stream(w, modelclient.Message{Role: "assistant", Content: "已提交"})
	})
	ctx := context.Background()
	id := newHistory(t, f, true, f.goal)
	request := SubmitTurn{OperationID: uuid.NewString(), ExpectedVersion: 1, Prompt: "原问题", RequestBudget: 1, TokenBudget: 50000}
	first, err := f.service.Submit(ctx, f.actor, learningspace.DefaultID, id, request)
	if err != nil {
		t.Fatal(err)
	}
	f.work(t)
	if _, err = f.pool.Exec(ctx, `UPDATE learning_aggregate_heads SET aggregate_version=2 WHERE aggregate_id=$1`, f.goal); err != nil {
		t.Fatal(err)
	}
	if replay, e := f.service.Submit(ctx, f.actor, learningspace.DefaultID, id, request); e != nil || replay != first {
		t.Fatal("目标变化后重试失去原回执", e)
	}
	// 存储配额使用真实既有运行预留；达到上限不会淘汰第一轮。
	limits := f.settings.View().Limits
	limits.StorageMiB = 16
	if _, err = f.settings.Update(settings.Update{ExpectedRevision: f.settings.View().Revision, Target: settings.Mentor, Limits: &limits}); err != nil {
		t.Fatal(err)
	}
	failed := false
	for i := 0; i < 40; i++ {
		c := historyRequest(t, f, id, "配额轮次")
		_, e := f.service.Create(ctx, f.actor, learningspace.DefaultID, f.goal, c)
		if errors.Is(e, ErrLimit) {
			failed = true
			break
		}
		if e != nil {
			t.Fatal(e)
		}
		f.work(t)
	}
	if !failed {
		t.Fatal("未执行存储硬上限")
	}
	page := historyPage(t, f, id, 0, 1)
	if page.Items[0].RunID != first.RunID || page.Items[0].Output != "已提交" {
		t.Fatal("配额满时删除旧历史")
	}
}
