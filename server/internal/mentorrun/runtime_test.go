package mentorrun

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/edu-agent/edu-agent/packages/agentcore/modelclient"
	"github.com/edu-agent/edu-agent/server/internal/identity"
	"github.com/edu-agent/edu-agent/server/internal/learningspace"
	"github.com/edu-agent/edu-agent/server/internal/settings"
	"github.com/edu-agent/edu-agent/server/migrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestCheckpointEncryptionAndToolBoundary(t *testing.T) {
	key := bytes.Repeat([]byte{42}, 32)
	a, err := newCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	body := Body{Output: "私人导师正文"}
	raw, err := seal(a, "run-one", body)
	if err != nil || bytes.Contains(raw, []byte(body.Output)) {
		t.Fatal("正文加密失败")
	}
	if recovered, err := unseal(a, "run-one", raw); err != nil || recovered.Output != body.Output {
		t.Fatal("无法恢复加密正文")
	}
	if _, err = unseal(a, "run-two", raw); err == nil {
		t.Fatal("密文可跨运行置换")
	}
	raw[len(raw)-1] ^= 1
	if _, err = unseal(a, "run-one", raw); err == nil {
		t.Fatal("篡改密文被接受")
	}
	path := filepath.Join(t.TempDir(), "key")
	if err = os.WriteFile(path, key, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err = LoadKey(path); err == nil {
		t.Fatal("接受公开权限密钥")
	}
	if err = os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	if actual, err := LoadKey(path); err != nil || !bytes.Equal(actual, key) {
		t.Fatal("安全密钥不可读取")
	}
	for _, name := range []string{"shell", "sql", "cli", "research", "rewrite"} {
		host := executionHost{}
		_, err := host.Execute(context.Background(), []modelclient.ToolCall{{ID: "test", Function: modelclient.ToolFunction{Name: name, Arguments: `{}`}}})
		if !errors.Is(err, ErrInvalid) {
			t.Fatalf("未知工具 %s 未被拒绝", name)
		}
	}
}

func runtimePool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("未配置真实 PostgreSQL，租约和恢复验收未运行")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	schema := "mentor_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err = admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = admin.Exec(ctx, "DROP SCHEMA "+schema+" CASCADE"); admin.Close() })
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	config.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err = migrations.Run(ctx, pool); err != nil {
		t.Fatal(err)
	}
	return pool
}

type runtimeFixture struct {
	modelOverride func(http.ResponseWriter, *http.Request)
	modelHook     func(int)
	searchCalls   *atomic.Int32
	pool          *pgxpool.Pool
	service       *Service
	actor         identity.Credential
	goal          string
	create        Create
	settings      *settings.Service
	calls         atomic.Int32
}

func fixture(t *testing.T, handler func(http.ResponseWriter, *http.Request, int)) *runtimeFixture {
	t.Helper()
	f := &runtimeFixture{pool: runtimePool(t), goal: uuid.NewString()}
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call := int(f.calls.Add(1))
		if f.modelHook != nil {
			f.modelHook(call)
		}
		if f.modelOverride != nil {
			f.modelOverride(w, r)
			return
		}
		handler(w, r, call)
	}))
	t.Cleanup(provider.Close)
	configuration, err := settings.Open(settings.Options{Path: filepath.Join(t.TempDir(), "private", "settings.json"), ModelEndpoints: []string{provider.URL}})
	if err != nil {
		t.Fatal(err)
	}
	limits := settings.DefaultLimits()
	limits.OutputTokens = 64
	_, err = configuration.Update(settings.Update{ExpectedRevision: 0, Target: settings.Mentor, Connection: &settings.Connection{Enabled: true, Provider: "openai_compatible", Endpoint: provider.URL, Model: "mentor-fixture", AuthMode: "none"}, Limits: &limits})
	if err != nil {
		t.Fatal(err)
	}
	f.settings = configuration
	f.service, err = New(f.pool, configuration, bytes.Repeat([]byte{42}, 32))
	if err != nil {
		t.Fatal(err)
	}
	f.actor = identity.Credential{Device: identity.Device{ID: uuid.NewString()}, TokenID: uuid.NewString(), Scopes: []string{"learning:read", "learning:write"}}
	ctx := context.Background()
	if _, err = f.pool.Exec(ctx, `INSERT INTO devices(id,display_name,created_at) VALUES($1,'导师验收',now())`, f.actor.Device.ID); err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256([]byte(f.actor.TokenID))
	if _, err = f.pool.Exec(ctx, `INSERT INTO device_tokens(id,device_id,token_hash,scopes,created_at) VALUES($1,$2,$3,$4,now())`, f.actor.TokenID, f.actor.Device.ID, hash[:], f.actor.Scopes); err != nil {
		t.Fatal(err)
	}
	if _, err = f.pool.Exec(ctx, `INSERT INTO learning_aggregate_heads(aggregate_type,aggregate_id,aggregate_version,last_event_seq,updated_at) VALUES('goal',$1,1,0,now())`, f.goal); err != nil {
		t.Fatal(err)
	}
	if _, err = f.pool.Exec(ctx, `INSERT INTO learning_goal_revisions(id,goal_id,revision,goal_text,source,actor_device_id,created_at) VALUES($1,$2,1,'真实绑定目标：学习概率','导师夹具',$3,now())`, uuid.NewString(), f.goal, f.actor.Device.ID); err != nil {
		t.Fatal(err)
	}
	f.create = Create{OperationID: uuid.NewString(), SessionID: uuid.NewString(), ExpectedVersion: 1, Prompt: "请解释目标", Save: true, RequestBudget: 4, TokenBudget: 50000}
	return f
}

func stream(w http.ResponseWriter, message modelclient.Message) {
	w.Header().Set("Content-Type", "text/event-stream")
	delta := map[string]any{"role": "assistant", "content": message.Content}
	finish := "stop"
	if len(message.ToolCalls) > 0 {
		finish = "tool_calls"
		calls := []map[string]any{}
		for i, call := range message.ToolCalls {
			calls = append(calls, map[string]any{"index": i, "id": call.ID, "type": "function", "function": call.Function})
		}
		delta["tool_calls"] = calls
	}
	raw, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}})
	fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", raw)
}

func (f *runtimeFixture) accept(t *testing.T) Receipt {
	t.Helper()
	r, err := f.service.Create(context.Background(), f.actor, learningspace.DefaultID, f.goal, f.create)
	if err != nil {
		t.Fatal(err)
	}
	return r
}
func (f *runtimeFixture) snapshot(t *testing.T, id string) Snapshot {
	t.Helper()
	var value Snapshot
	err := f.service.ReadSnapshot(context.Background(), f.actor, learningspace.DefaultID, id, func(s Snapshot) error { value = s; return nil })
	if err != nil {
		t.Fatal(err)
	}
	return value
}
func (f *runtimeFixture) work(t *testing.T) {
	t.Helper()
	if _, err := f.service.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestPostgreSQLMentorRealCoreRecoveryAndIdempotency(t *testing.T) {
	f := fixture(t, func(w http.ResponseWriter, r *http.Request, n int) {
		raw, _ := io.ReadAll(r.Body)
		if !bytes.Contains(raw, []byte("真实绑定目标")) {
			t.Error("模型未读取绑定目标")
		}
		if n == 1 {
			stream(w, modelclient.Message{Role: "assistant", ToolCalls: []modelclient.ToolCall{{ID: "goal-read", Type: "function", Function: modelclient.ToolFunction{Name: "read_goal", Arguments: `{}`}}}})
			return
		}
		if !bytes.Contains(raw, []byte(`"tool_call_id":"goal-read"`)) {
			t.Error("未经过正式工具适配器")
		}
		stream(w, modelclient.Message{Role: "assistant", Content: "目标内导师回答"})
	})
	r := f.accept(t)
	if retry := f.accept(t); retry != r {
		t.Fatal("重复操作再次受理")
	}
	f.create.Prompt = "修改后的内容"
	if _, err := f.service.Create(context.Background(), f.actor, learningspace.DefaultID, f.goal, f.create); !errors.Is(err, ErrOperation) {
		t.Fatal("未拒绝同 ID 改载荷")
	}
	before := f.snapshot(t, r.RunID)
	f.work(t)
	result := f.snapshot(t, r.RunID)
	if result.Status != "succeeded" || result.Output != "目标内导师回答" || f.calls.Load() != 2 {
		t.Fatalf("真实核心运行失败：%+v，调用 %d", result, f.calls.Load())
	}
	var events []Event
	if err := f.service.Events(context.Background(), f.actor, learningspace.DefaultID, r.RunID, before.Watermark, func(e []Event) error { events = e; return nil }); err != nil {
		t.Fatal(err)
	}
	for i, event := range events {
		if event.Seq != before.Watermark+int64(i)+1 {
			t.Fatal("快照到事件流存在缺口")
		}
	}
	if len(events) == 0 || events[len(events)-1].Seq != result.Watermark {
		t.Fatal("事件未到达结果 watermark")
	}
	restarted, err := New(f.pool, f.settings, bytes.Repeat([]byte{42}, 32))
	if err != nil {
		t.Fatal(err)
	}
	f.service = restarted
	if recovered := f.snapshot(t, r.RunID); recovered.Output != result.Output || recovered.Watermark != result.Watermark {
		t.Fatal("重启未恢复原结果")
	}
	f.work(t)
	if f.calls.Load() != 2 {
		t.Fatal("查询或恢复重放了模型")
	}
	var plaintext string
	if err = f.pool.QueryRow(context.Background(), `SELECT state::text||encode(checkpoint,'escape') FROM learning_mentor_runs WHERE id=$1`, r.RunID).Scan(&plaintext); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(plaintext, "目标内导师回答") || strings.Contains(plaintext, "请解释目标") {
		t.Fatal("数据库含运行明文")
	}
	if receipt, err := f.service.Operation(context.Background(), f.actor, learningspace.DefaultID, r.OperationID); err != nil || receipt != r {
		t.Fatal("原操作回执变化")
	}
}

func TestPostgreSQLMentorLeaseCompetitionAndUnknownResult(t *testing.T) {
	f := fixture(t, func(w http.ResponseWriter, r *http.Request, _ int) {
		stream(w, modelclient.Message{Role: "assistant", Content: "完成"})
	})
	r := f.accept(t)
	second, err := New(f.pool, f.settings, bytes.Repeat([]byte{42}, 32))
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	claims := make(chan *row, 2)
	failures := make(chan error, 2)
	for _, service := range []*Service{f.service, second} {
		wg.Go(func() { claimed, err := service.claim(context.Background()); claims <- claimed; failures <- err })
	}
	wg.Wait()
	close(claims)
	close(failures)
	for err := range failures {
		if err != nil {
			t.Fatal(err)
		}
	}
	var winner *row
	for claimed := range claims {
		if claimed != nil {
			if winner != nil {
				t.Fatal("有效租约被重复承领")
			}
			winner = claimed
		}
	}
	if winner == nil {
		t.Fatal("无人承领队列")
	}
	if _, err = f.pool.Exec(context.Background(), `UPDATE learning_mentor_runs SET lease_until=clock_timestamp()-interval '1 second' WHERE id=$1`, r.RunID); err != nil {
		t.Fatal(err)
	}
	reclaimed, err := second.claim(context.Background())
	if err != nil || reclaimed == nil || *reclaimed.lease == *winner.lease {
		t.Fatal("未安全承领过期租约")
	}
	if err = f.service.mutate(context.Background(), winner, "late", func(*row) error { return nil }); !errors.Is(err, ErrLease) {
		t.Fatal("旧 worker 可提交")
	}
	if _, err = f.pool.Exec(context.Background(), `UPDATE learning_mentor_runs SET call_started=TRUE,lease_until=clock_timestamp()-interval '1 second' WHERE id=$1`, r.RunID); err != nil {
		t.Fatal(err)
	}
	if _, err = second.claim(context.Background()); err != nil {
		t.Fatal(err)
	}
	result := f.snapshot(t, r.RunID)
	if result.Status != "failed" || !result.ResultUnknown || !result.CostUnknown || f.calls.Load() != 0 {
		t.Fatalf("未知调用被错误重试：%+v", result)
	}
}

func TestPostgreSQLMentorInteractionBudgetAndStaleApproval(t *testing.T) {
	f := fixture(t, func(w http.ResponseWriter, r *http.Request, n int) {
		if n < 3 {
			stream(w, modelclient.Message{Role: "assistant", ToolCalls: []modelclient.ToolCall{{ID: fmt.Sprint("confirm-", n), Type: "function", Function: modelclient.ToolFunction{Name: "confirm_focus", Arguments: `{"focus":"先讨论概率直觉？"}`}}}})
			return
		}
		stream(w, modelclient.Message{Role: "assistant", Content: "已按确认的方向回答"})
	})
	f.create.RequestBudget = 1
	r := f.accept(t)
	f.work(t)
	first := f.snapshot(t, r.RunID)
	if first.Status != "waiting_approval" || first.Interaction == nil {
		t.Fatal("未进入结构化批准等待")
	}
	c := Command{OperationID: uuid.NewString(), ExpectedVersion: first.Version, Kind: "respond", InteractionID: first.Interaction.ID, Answer: "同意"}
	if _, err := f.service.Command(context.Background(), f.actor, learningspace.DefaultID, r.RunID, c); err != nil {
		t.Fatal(err)
	}
	f.work(t)
	paused := f.snapshot(t, r.RunID)
	if paused.Status != "paused_budget" || f.calls.Load() != 1 {
		t.Fatal("预算耗尽仍启动新调用")
	}
	if _, err := f.service.Command(context.Background(), f.actor, learningspace.DefaultID, r.RunID, Command{OperationID: uuid.NewString(), ExpectedVersion: paused.Version, Kind: "continue_budget", RequestBudget: 2, TokenBudget: 20000}); err != nil {
		t.Fatal(err)
	}
	f.work(t)
	second := f.snapshot(t, r.RunID)
	if second.Interaction == nil || second.Interaction.ID == first.Interaction.ID {
		t.Fatal("新候选复用旧交互")
	}
	c.OperationID = uuid.NewString()
	c.ExpectedVersion = second.Version
	if _, err := f.service.Command(context.Background(), f.actor, learningspace.DefaultID, r.RunID, c); !errors.Is(err, ErrConflict) {
		t.Fatal("旧回答可批准新候选")
	}
	c.InteractionID = second.Interaction.ID
	if _, err := f.service.Command(context.Background(), f.actor, learningspace.DefaultID, r.RunID, c); err != nil {
		t.Fatal(err)
	}
	f.work(t)
	if f.snapshot(t, r.RunID).Status != "succeeded" {
		t.Fatal("批准后未完成")
	}
}

func TestPostgreSQLMentorCompatibilityFallbackCannotBypassBudget(t *testing.T) {
	f := fixture(t, func(w http.ResponseWriter, r *http.Request, _ int) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(400)
		io.WriteString(w, `{"error":{"code":"unsupported_parameter","param":"stream_options","message":"unsupported"}}`)
	})
	f.create.RequestBudget = 1
	r := f.accept(t)
	f.work(t)
	if snapshot := f.snapshot(t, r.RunID); snapshot.Status != "paused_budget" || snapshot.RequestsUsed != 1 || f.calls.Load() != 1 {
		t.Fatalf("协议回退绕过预算：%+v", snapshot)
	}
}

func TestPostgreSQLMentorTemporaryClearAndCursorExpiry(t *testing.T) {
	f := fixture(t, func(w http.ResponseWriter, r *http.Request, _ int) {
		stream(w, modelclient.Message{Role: "assistant", Content: "临时正文"})
	})
	f.create.Save = false
	r := f.accept(t)
	f.work(t)
	var isNull bool
	if err := f.pool.QueryRow(context.Background(), `SELECT checkpoint IS NULL FROM learning_mentor_runs WHERE id=$1`, r.RunID).Scan(&isNull); err != nil || !isNull {
		t.Fatal("临时正文写入数据库")
	}
	restarted, err := New(f.pool, f.settings, bytes.Repeat([]byte{42}, 32))
	if err != nil {
		t.Fatal(err)
	}
	f.service = restarted
	if snapshot := f.snapshot(t, r.RunID); snapshot.BodyAvailable || snapshot.Output != "" || snapshot.Reason != "temporary_unavailable" {
		t.Fatal("临时模式错误承诺跨进程恢复")
	}
	if _, err = f.service.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err = f.service.Events(context.Background(), f.actor, learningspace.DefaultID, r.RunID, 1000000, func([]Event) error { return nil }); !errors.Is(err, ErrResync) {
		t.Fatal("未来游标未要求恢复")
	}
	snapshot := f.snapshot(t, r.RunID)
	if _, err = f.service.Command(context.Background(), f.actor, learningspace.DefaultID, r.RunID, Command{OperationID: uuid.NewString(), ExpectedVersion: snapshot.Version, Kind: "clear"}); err != nil {
		t.Fatal(err)
	}
	if result := f.snapshot(t, r.RunID); result.BodyAvailable || result.Output != "" {
		t.Fatal("清除后正文仍可读")
	}
}

func TestPostgreSQLMentorInFlightCancellationAndLifecycleFences(t *testing.T) {
	for _, action := range []string{"stop", "pause", "archive", "revoke", "scope"} {
		t.Run(action, func(t *testing.T) {
			started := make(chan struct{})
			lifetime, releaseFixture := context.WithCancel(context.Background())
			f := fixture(t, func(w http.ResponseWriter, r *http.Request, _ int) {
				_, _ = io.Copy(io.Discard, r.Body)
				w.Header().Set("Content-Type", "text/event-stream")
				raw, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"role": "assistant", "content": strings.Repeat("已有部分回答", 100)}}}})
				fmt.Fprintf(w, "data: %s\n\n", raw)
				w.(http.Flusher).Flush()
				close(started)
				select {
				case <-r.Context().Done():
				case <-lifetime.Done():
				}
			})
			t.Cleanup(releaseFixture)
			f.service.Lease = 600 * time.Millisecond
			r := f.accept(t)
			done := make(chan error, 1)
			go func() { _, err := f.service.RunOnce(context.Background()); done <- err }()
			select {
			case <-started:
			case <-time.After(5 * time.Second):
				t.Fatal("未发出模型请求")
			}
			deadline := time.Now().Add(5 * time.Second)
			for f.snapshot(t, r.RunID).Stage != "answering" {
				if time.Now().After(deadline) {
					t.Fatal("未保存增量")
				}
				time.Sleep(10 * time.Millisecond)
			}
			ctx := context.Background()
			var err error
			switch action {
			case "stop":
				snapshot := f.snapshot(t, r.RunID)
				_, err = f.service.Command(ctx, f.actor, learningspace.DefaultID, r.RunID, Command{OperationID: uuid.NewString(), ExpectedVersion: snapshot.Version, Kind: "stop"})
				if err == nil && f.snapshot(t, r.RunID).Status != "cancelling" {
					t.Fatal("未显示取消中")
				}
			case "pause", "archive":
				status := "paused"
				if action == "archive" {
					status = "archived"
				}
				tx, beginErr := f.pool.Begin(ctx)
				if beginErr != nil {
					t.Fatal(beginErr)
				}
				_, err = tx.Exec(ctx, `UPDATE learning_aggregate_heads SET aggregate_version=2 WHERE aggregate_type='goal' AND aggregate_id=$1`, f.goal)
				if err == nil {
					_, err = tx.Exec(ctx, `INSERT INTO learning_goal_revisions(id,goal_id,revision,goal_text,source,actor_device_id,created_at,previous_revision_id,previous_goal_id,previous_revision,management) SELECT $1,goal_id,2,goal_text,source,actor_device_id,now(),id,goal_id,1,jsonb_build_object('status',$3::text) FROM learning_goal_revisions WHERE goal_id=$2 AND revision=1`, uuid.NewString(), f.goal, status)
				}
				if err == nil {
					err = tx.Commit(ctx)
				} else {
					_ = tx.Rollback(ctx)
				}
			case "revoke":
				_, err = f.pool.Exec(ctx, `UPDATE devices SET revoked_at=now() WHERE id=$1`, f.actor.Device.ID)
			case "scope":
				_, err = f.pool.Exec(ctx, `UPDATE device_tokens SET scopes=ARRAY['learning:read'] WHERE id=$1`, f.actor.TokenID)
			}
			if err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("在途调用未取消")
			}
			var raw []byte
			if err = f.pool.QueryRow(ctx, `SELECT state FROM learning_mentor_runs WHERE id=$1`, r.RunID).Scan(&raw); err != nil {
				t.Fatal(err)
			}
			var meta Meta
			if json.Unmarshal(raw, &meta) != nil || meta.Status != "cancelled" {
				t.Fatalf("失效上下文未终止：%s", raw)
			}
			if f.calls.Load() != 1 {
				t.Fatal("取消后又调用模型")
			}
		})
	}
}

func TestPostgreSQLMentorExpiredCursorAndSnapshotCommitGate(t *testing.T) {
	f := fixture(t, func(w http.ResponseWriter, r *http.Request, _ int) {
		stream(w, modelclient.Message{Role: "assistant", Content: "正文"})
	})
	r := f.accept(t)
	owned, err := f.service.claim(context.Background())
	if err != nil || owned == nil {
		t.Fatal(err)
	}
	for range EventWindow + 2 {
		if err = f.service.mutate(context.Background(), owned, "output", func(*row) error { return nil }); err != nil {
			t.Fatal(err)
		}
	}
	if err = f.service.Events(context.Background(), f.actor, learningspace.DefaultID, r.RunID, 1, func([]Event) error { return nil }); !errors.Is(err, ErrResync) {
		t.Fatal("过期游标未返回 resync_required")
	}
	sending := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- f.service.ReadSnapshot(context.Background(), f.actor, learningspace.DefaultID, r.RunID, func(Snapshot) error { close(sending); <-release; return nil })
	}()
	<-sending
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	_, err = f.pool.Exec(ctx, `UPDATE devices SET revoked_at=now() WHERE id=$1`, f.actor.Device.ID)
	cancel()
	if err == nil {
		t.Fatal("发送期间撤销越过了设备锁")
	}
	close(release)
	if err = <-done; err != nil {
		t.Fatal(err)
	}
	if _, err = f.pool.Exec(context.Background(), `UPDATE devices SET revoked_at=now() WHERE id=$1`, f.actor.Device.ID); err != nil {
		t.Fatal(err)
	}
	if err = f.service.ReadSnapshot(context.Background(), f.actor, learningspace.DefaultID, r.RunID, func(Snapshot) error { t.Error("撤销后仍发送正文"); return nil }); !errors.Is(err, ErrForbidden) {
		t.Fatal("撤销未生效")
	}
}
