package mentorrun

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/integrations/learningknowledge"
	"github.com/edu-agent/edu-agent/server/internal/integrations/websearch"
	"github.com/edu-agent/edu-agent/server/internal/knowledge"
	knowledgedb "github.com/edu-agent/edu-agent/server/internal/knowledge/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/learning"
	learningdb "github.com/edu-agent/edu-agent/server/internal/learning/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/learningcontent"
	"github.com/edu-agent/edu-agent/server/internal/learningspace"
	spacedb "github.com/edu-agent/edu-agent/server/internal/learningspace/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/learningstart"
	"github.com/edu-agent/edu-agent/server/internal/research"
	"github.com/edu-agent/edu-agent/server/internal/settings"
	tutoringdb "github.com/edu-agent/edu-agent/server/internal/tutoring/postgresstore"
	"github.com/google/uuid"
)

func TestPostgreSQLStartLearningFailuresRecover(t *testing.T) {
	for _, failure := range []string{"search", "sources", "model"} {
		t.Run(failure, func(t *testing.T) {
			f := startFixture(t, "Go 并发")
			search, fetcher := f.service.search, f.service.fetcher
			var failing atomic.Bool
			failing.Store(true)
			switch failure {
			case "model":
				f.modelHook = func(int) {
					if failing.Load() {
						panic(http.ErrAbortHandler)
					}
				}
			case "search":
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) }))
				t.Cleanup(upstream.Close)
				f.service.search = func(wrap func(http.RoundTripper) http.RoundTripper) (websearch.Adapter, string, error) {
					transport := http.DefaultTransport
					if wrap != nil {
						transport = wrap(transport)
					}
					return websearch.NewBrave(upstream.URL, "fixture", &http.Client{Transport: transport}), "fixture-search", nil
				}
			case "sources":
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusForbidden) }))
				t.Cleanup(upstream.Close)
				f.service.fetcher = research.NewFetcherWithNetwork(func(context.Context, string, string) ([]netip.Addr, error) {
					return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
				}, func(ctx context.Context, network, address string) (net.Conn, error) {
					return (&net.Dialer{}).DialContext(ctx, network, upstream.Listener.Addr().String())
				})
			}
			r := f.accept(t)
			f.work(t)
			s := f.snapshot(t, r.RunID)
			if !terminal(s.Status) || s.Status == "succeeded" || s.StartLearning.Result != nil || s.Reason == "" {
				t.Fatal("失败被冒充开学完成", s)
			}
			if failure == "sources" && (s.Reason != "sources_insufficient" || f.calls.Load() != 0) {
				t.Fatal("缺少原文仍调用模型或未解释缺口", s)
			}
			var count int
			if err := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM learning_activities`).Scan(&count); err != nil || count != 0 {
				t.Fatal("失败遗留活动", count, err)
			}
			failing.Store(false)
			f.service.search, f.service.fetcher = search, fetcher
			if _, err := f.service.Command(context.Background(), f.actor, learningspace.DefaultID, r.RunID, Command{OperationID: uuid.NewString(), ExpectedVersion: s.Version, Kind: "retry_start", RequestBudget: 8, TokenBudget: 50000}); err != nil {
				t.Fatal(err)
			}
			f.work(t)
			if s = f.snapshot(t, r.RunID); s.Status != "succeeded" || s.StartLearning.Result == nil {
				t.Fatal("显式重试不能恢复", s)
			}
		})
	}
}

func TestPostgreSQLStartLearningConcurrentGoals(t *testing.T) {
	f := startFixture(t, "Go 并发")
	ctx := context.Background()
	space, err := spacedb.New(f.pool).Mutate(ctx, f.actor.Device.ID, "", learningspace.Command{OperationID: uuid.NewString(), Name: "独立开学区", Status: "active"})
	if err != nil {
		t.Fatal(err)
	}
	ks, _ := knowledge.NewService(f.service.starter.Knowledge, knowledge.NewCanonicalizer(), knowledge.ServiceOptions{})
	ls, _ := learning.NewService(f.service.starter.Learning, f.service.starter.Learning, learningknowledge.New(ks), learning.ServiceOptions{})
	type target struct{ space, goal, run string }
	targets := []target{{space: learningspace.DefaultID, goal: f.goal}, {space: learningspace.DefaultID, goal: uuid.NewString()}, {space: space.ID, goal: uuid.NewString()}}
	for i := range targets {
		item := &targets[i]
		if i > 0 {
			scope, _ := learningspace.WithScope(ctx, item.space)
			_, err = ls.CreateGoal(scope, f.actor.Device.ID, learning.GoalCommand{Operation: learning.OperationEnvelope{OperationID: uuid.NewString(), PayloadSchemaVersion: 1, AggregateType: "goal", AggregateID: item.goal, Payload: json.RawMessage(`{}`)}, GoalID: item.goal, Text: fmt.Sprintf("Go 并发目标 %d", i), Source: "并发夹具", Details: &learning.GoalDetails{Name: "独立目标", SelfAssessment: "已有基础", Purpose: "用于实际练习"}})
			if err != nil {
				t.Fatal(err)
			}
		}
		create := f.create
		create.OperationID, create.SessionID = uuid.NewString(), uuid.NewString()
		r, e := f.service.Create(ctx, f.actor, item.space, item.goal, create)
		if e != nil {
			t.Fatal(e)
		}
		item.run = r.RunID
	}
	var wg sync.WaitGroup
	for range targets {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := f.service.RunOnce(ctx); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	// 并发配额可能只受理部分 worker；继续领取剩余队列而不是创建新运行。
	for range targets {
		f.work(t)
	}
	seen := map[string]bool{}
	for _, item := range targets {
		err = f.service.ReadSnapshot(ctx, f.actor, item.space, item.run, func(s Snapshot) error {
			if s.Status != "succeeded" || s.GoalID != item.goal || s.StartLearning.Result == nil {
				return fmt.Errorf("目标没有独立完成：%+v", s)
			}
			r := s.StartLearning.Result
			if seen[r.SessionID] {
				return errors.New("不同目标复用了教学现场")
			}
			seen[r.SessionID] = true
			for _, source := range s.Research.Sources {
				if source.GoalID != item.goal || source.SpaceID != item.space {
					return errors.New("来源串到其他目标或区")
				}
			}
			var actual string
			if err := f.pool.QueryRow(ctx, `SELECT g.goal_id::text FROM tutoring_sessions s JOIN learning_goal_revisions g ON g.id=s.goal_revision_id WHERE s.id=$1`, r.SessionID).Scan(&actual); err != nil {
				return err
			}
			if actual != item.goal {
				return errors.New("活动读取了全局 current 目标")
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestPostgreSQLStartLearningAtomicFailureAndRetry(t *testing.T) {
	for _, table := range []string{"knowledge_concept_revisions", "knowledge_context_revisions", "learning_content_revisions"} {
		t.Run(table, func(t *testing.T) {
			f := startFixture(t, "Go 并发")
			ctx := context.Background()
			if _, err := f.pool.Exec(ctx, `CREATE FUNCTION fail_start() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION '开学故障注入'; END $$; CREATE TRIGGER fail_start BEFORE INSERT ON `+table+` FOR EACH ROW EXECUTE FUNCTION fail_start()`); err != nil {
				t.Fatal(err)
			}
			r := f.accept(t)
			f.work(t)
			s := f.snapshot(t, r.RunID)
			if s.StartLearning.Result != nil || s.StartLearning.Prepared == nil || s.Status != "partial" {
				t.Fatal("失败没有保留准备结果", s)
			}
			var count int
			if err := f.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM tutoring_sessions)+(SELECT count(*) FROM learning_activities)+(SELECT count(*) FROM learning_content_artifacts)+(SELECT count(*) FROM knowledge_context_revisions)+(SELECT count(*) FROM knowledge_concepts)+(SELECT count(*) FROM knowledge_policies)`).Scan(&count); err != nil || count != 0 {
				t.Fatal("发布失败遗留半成功", count, err)
			}
			if _, err := f.pool.Exec(ctx, `DROP TRIGGER fail_start ON `+table); err != nil {
				t.Fatal(err)
			}
			before := f.calls.Load()
			command := Command{OperationID: uuid.NewString(), ExpectedVersion: s.Version, Kind: "retry_start", RequestBudget: 8, TokenBudget: 50000}
			if _, err := f.service.Command(ctx, f.actor, learningspace.DefaultID, r.RunID, command); err != nil {
				t.Fatal(err)
			}
			f.work(t)
			s = f.snapshot(t, r.RunID)
			if s.StartLearning.Result == nil || f.calls.Load() != before {
				t.Fatal("恢复未复用已准备活动", s)
			}
		})
	}
}

func TestPostgreSQLStartLearningBudgetRecovery(t *testing.T) {
	f := startFixture(t, "英语阅读")
	f.create.RequestBudget = 4
	r := f.accept(t)
	f.work(t)
	s := f.snapshot(t, r.RunID)
	if s.Status != "paused_budget" || s.StartLearning.Result != nil {
		t.Fatal("预算未阻止活动准备", s)
	}
	old := f.service
	restarted, err := New(f.pool, f.settings, bytes.Repeat([]byte{42}, 32))
	if err != nil {
		t.Fatal(err)
	}
	restarted.ConfigureStart(old.starter)
	restarted.search = old.search
	restarted.fetcher = old.fetcher
	f.service = restarted
	if _, err = f.service.Command(context.Background(), f.actor, learningspace.DefaultID, r.RunID, Command{OperationID: uuid.NewString(), ExpectedVersion: s.Version, Kind: "continue_budget", RequestBudget: 2, TokenBudget: 50000}); err != nil {
		t.Fatal(err)
	}
	f.work(t)
	s = f.snapshot(t, r.RunID)
	if s.StartLearning.Result == nil || s.RequestsUsed != 5 {
		t.Fatal("重启续预算重复搜索或未开学", s)
	}
}

func TestPostgreSQLStartLearningLifecycleRace(t *testing.T) {
	for _, action := range []string{"pause", "archive", "revoke", "stop", "clear"} {
		t.Run(action, func(t *testing.T) {
			f := startFixture(t, "数学概率")
			if action == "pause" {
				fixtureGoalAction(t, f, "start")
			}
			started := make(chan struct{})
			ctx, release := context.WithCancel(context.Background())
			t.Cleanup(release)
			f.modelHook = func(call int) {
				if call == 2 {
					close(started)
					<-ctx.Done()
				}
			}
			r := f.accept(t)
			done := make(chan error, 1)
			go func() { _, err := f.service.RunOnce(context.Background()); done <- err }()
			select {
			case <-started:
			case <-time.After(10 * time.Second):
				t.Fatal("未进入活动准备")
			}
			var err error
			switch action {
			case "pause":
				fixtureGoalAction(t, f, "pause")
			case "archive":
				_, err = f.pool.Exec(context.Background(), `UPDATE learning_spaces SET status='archived' WHERE id=$1`, learningspace.DefaultID)
			case "revoke":
				_, err = f.pool.Exec(context.Background(), `UPDATE device_tokens SET revoked_at=clock_timestamp() WHERE id=$1`, f.actor.TokenID)
			case "stop", "clear":
				s := f.snapshot(t, r.RunID)
				_, err = f.service.Command(context.Background(), f.actor, learningspace.DefaultID, r.RunID, Command{OperationID: uuid.NewString(), ExpectedVersion: s.Version, Kind: action})
			}
			if err != nil {
				t.Fatal(err)
			}
			release()
			select {
			case err = <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("开学未结束")
			}
			var count int
			if err = f.pool.QueryRow(context.Background(), `SELECT (SELECT count(*) FROM knowledge_context_revisions)+(SELECT count(*) FROM learning_activities)+(SELECT count(*) FROM learning_content_artifacts)`).Scan(&count); err != nil || count != 0 {
				t.Fatal("迟到结果发布了非法活动", count, err)
			}
		})
	}
}

func fixtureGoalAction(t *testing.T, f *runtimeFixture, action string) {
	t.Helper()
	ctx := context.Background()
	goal, err := f.service.starter.Learning.GetGoal(ctx, f.goal)
	if err != nil {
		t.Fatal(err)
	}
	ks, err := knowledge.NewService(f.service.starter.Knowledge, knowledge.NewCanonicalizer(), knowledge.ServiceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	ls, err := learning.NewService(f.service.starter.Learning, f.service.starter.Learning, learningknowledge.New(ks), learning.ServiceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = ls.CreateGoal(ctx, f.actor.Device.ID, learning.GoalCommand{Operation: learning.OperationEnvelope{OperationID: uuid.NewString(), PayloadSchemaVersion: 1, AggregateType: "goal", AggregateID: f.goal, ExpectedVersion: goal.Revision, Payload: json.RawMessage(`{}`)}, GoalID: f.goal, Text: goal.Text, Source: "生命周期验收", PreviousRevisionID: &goal.ID, Action: action})
	if err != nil {
		t.Fatal(err)
	}
	f.create.ExpectedVersion = goal.Revision + 1
}

func TestPostgreSQLStartLearningPermissionAndIsolation(t *testing.T) {
	f := startFixture(t, "Go 并发")
	ctx := context.Background()
	bad := f.create
	bad.StartLearning = &learningstart.Request{NewSession: true}
	if _, err := f.service.Create(ctx, f.actor, learningspace.DefaultID, f.goal, bad); !errors.Is(err, ErrInvalid) {
		t.Fatal("未确认模型外发却开学", err)
	}
	bad = f.create
	bad.Save = false
	if _, err := f.service.Create(ctx, f.actor, learningspace.DefaultID, f.goal, bad); !errors.Is(err, ErrInvalid) {
		t.Fatal("不可恢复运行被接受", err)
	}
	r := f.accept(t)
	f.work(t)
	result := f.snapshot(t, r.RunID).StartLearning.Result
	if result == nil {
		t.Fatal("没有活动")
	}
	var got *knowledge.KnowledgeContextRevision
	if err := f.service.ReadSessionContext(ctx, f.actor, learningspace.DefaultID, result.SessionID, func(k *knowledge.KnowledgeContextRevision) error { got = k; return nil }); err != nil || got == nil || got.ID != result.Context.ID {
		t.Fatal("上下文查询不一致", err)
	}
	if err := f.service.ReadSessionContext(ctx, f.actor, uuid.NewString(), result.SessionID, func(*knowledge.KnowledgeContextRevision) error { return fmt.Errorf("跨区发送内容") }); !errors.Is(err, ErrNotFound) {
		t.Fatal("跨区上下文可见", err)
	}
	if _, err := f.pool.Exec(ctx, `UPDATE device_tokens SET scopes=array_remove(scopes,'research:adopt') WHERE id=$1`, f.actor.TokenID); err != nil {
		t.Fatal(err)
	}
	f.create.OperationID = uuid.NewString()
	if _, err := f.service.Create(ctx, f.actor, learningspace.DefaultID, f.goal, f.create); !errors.Is(err, ErrForbidden) {
		t.Fatal("撤销采纳授权后仍可开始", err)
	}
}

func startFixture(t *testing.T, topic string) *runtimeFixture {
	t.Helper()
	f, _ := researchFixture(t, false, topic)
	limits := f.settings.View().Limits
	limits.OutputTokens = 2048
	connection := f.settings.View().EffectiveMentor.Connection
	connection.Model = map[string]string{"Go 并发": "go-fixture", "英语阅读": "english-fixture", "数学概率": "math-fixture"}[topic]
	if _, err := f.settings.Update(settings.Update{ExpectedRevision: f.settings.View().Revision, Limits: &limits, Target: settings.Mentor, Connection: &connection}); err != nil {
		t.Fatal(err)
	}
	k := knowledgedb.New(f.pool)
	l := learningdb.New(f.pool, tutoringdb.New(f.pool), k)
	content, err := learningcontent.New(f.pool, l, bytes.Repeat([]byte{42}, 32))
	if err != nil {
		t.Fatal(err)
	}
	l.ConfigureContent(content)
	if _, err = l.Rebuild(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.service.ConfigureStart(&learningstart.Service{Learning: l, Knowledge: k, Content: content})
	ks, err := knowledge.NewService(k, knowledge.NewCanonicalizer(), knowledge.ServiceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	ls, err := learning.NewService(l, l, learningknowledge.New(ks), learning.ServiceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	f.goal = uuid.NewString()
	if _, err = ls.CreateGoal(context.Background(), f.actor.Device.ID, learning.GoalCommand{Operation: learning.OperationEnvelope{OperationID: uuid.NewString(), PayloadSchemaVersion: 1, AggregateType: "goal", AggregateID: f.goal, Payload: json.RawMessage(`{}`)}, GoalID: f.goal, Text: topic, Source: "开学夹具", Details: &learning.GoalDetails{Name: topic, SelfAssessment: "已有基础", Purpose: "用于实际练习"}}); err != nil {
		t.Fatal(err)
	}
	f.create.StartLearning = &learningstart.Request{NewSession: true, ModelConsent: true}
	f.create.Research.AutoAdopt = true
	if _, err = f.pool.Exec(context.Background(), `UPDATE device_tokens SET scopes=scopes||ARRAY['knowledge:read','knowledge:write','research:adopt'] WHERE id=$1`, f.actor.TokenID); err != nil {
		t.Fatal(err)
	}
	return f
}

func TestPostgreSQLStartLearningEmptyLibrary(t *testing.T) {
	for _, topic := range []string{"Go 并发", "英语阅读", "数学概率"} {
		t.Run(topic, func(t *testing.T) {
			f := startFixture(t, topic)
			ctx := context.Background()
			var count int
			if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM knowledge_revisions`).Scan(&count); err != nil || count != 0 {
				t.Fatal("初始不是空知识库", count, err)
			}
			if err := f.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM tutoring_sessions)+(SELECT count(*) FROM learning_mentor_runs)`).Scan(&count); err != nil || count != 0 || f.calls.Load() != 0 || f.searchCalls.Load() != 0 {
				t.Fatal("仅保存目标产生教学或外部请求副作用", count, err)
			}
			r := f.accept(t)
			f.work(t)
			snapshot := f.snapshot(t, r.RunID)
			if snapshot.StartLearning == nil || snapshot.StartLearning.Result == nil {
				t.Fatalf("没有首项正式活动：%+v", snapshot)
			}
			result := snapshot.StartLearning.Result
			if snapshot.Status != "succeeded" || snapshot.RequestsUsed != 5 {
				t.Fatalf("运行未成功或预算失真：%+v", snapshot.Meta)
			}
			var oldActivity []byte
			if err := f.pool.QueryRow(ctx, `SELECT to_jsonb(a) FROM learning_activities a WHERE id=$1`, result.ActivityID).Scan(&oldActivity); err != nil {
				t.Fatal(err)
			}
			if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM learning_activities a JOIN tutoring_sessions s ON s.id=a.session_id JOIN learning_content_artifacts c ON c.activity_id=a.id JOIN learning_content_revisions r ON r.artifact_id=c.id AND r.version=a.artifact_version JOIN knowledge_context_revisions k ON k.id=a.knowledge_context_revision_id WHERE a.id=$1 AND s.knowledge_context_revision_id=k.id AND a.artifact_id=c.id`, result.ActivityID).Scan(&count); err != nil || count != 1 {
				t.Fatal("活动、上下文和正文没有一致发布", err)
			}
			if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM learning_evidence`).Scan(&count); err != nil || count != 0 {
				t.Fatal("开学写入了能力证据", err)
			}
			if _, err := f.service.Create(ctx, f.actor, learningspace.DefaultID, f.goal, f.create); err != nil {
				t.Fatal("操作未幂等", err)
			}
			restarted, err := New(f.pool, f.settings, bytes.Repeat([]byte{42}, 32))
			if err != nil {
				t.Fatal(err)
			}
			restarted.ConfigureStart(f.service.starter)
			err = restarted.ReadSnapshot(ctx, f.actor, learningspace.DefaultID, r.RunID, func(s Snapshot) error {
				if s.StartLearning.Result.ActivityID != result.ActivityID {
					t.Fatal("重启重复开课")
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			// 新知识在相同政策下推进上下文，旧题、目标及概念身份保留。
			f.create.OperationID = uuid.NewString()
			next := f.accept(t)
			f.work(t)
			newResult := f.snapshot(t, next.RunID).StartLearning.Result
			if newResult == nil || newResult.Context.PreviousRevisionID != result.Context.ID || newResult.Context.Policy.ID != result.Context.Policy.ID || newResult.Context.Concepts[0].ConceptID != result.Context.Concepts[0].ConceptID {
				t.Fatal("知识更新改变政策或概念身份", newResult)
			}
			if err = f.pool.QueryRow(ctx, `SELECT count(*) FROM learning_goal_revisions WHERE goal_id=$1`, f.goal).Scan(&count); err != nil || count != 1 {
				t.Fatal("开学擅改目标版本", err)
			}
			var unchanged []byte
			if err = f.pool.QueryRow(ctx, `SELECT to_jsonb(a) FROM learning_activities a WHERE id=$1`, result.ActivityID).Scan(&unchanged); err != nil || !bytes.Equal(oldActivity, unchanged) {
				t.Fatal("新上下文改写了旧题目、rubric 或内容版本", err)
			}
		})
	}
}
