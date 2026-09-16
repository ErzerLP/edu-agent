package mentorrun

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/integrations/websearch"
	"github.com/edu-agent/edu-agent/server/internal/learningspace"
	"github.com/edu-agent/edu-agent/server/internal/research"
	"github.com/google/uuid"
)

func TestPostgreSQLResearchStopDuringFetch(t *testing.T) {
	f, _ := researchFixture(t, false)
	ctx := context.Background()
	f.service.Lease = 600 * time.Millisecond
	started := make(chan struct{})
	lifetime, release := context.WithCancel(ctx)
	page := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		select {
		case <-r.Context().Done():
		case <-lifetime.Done():
		}
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("取消后的迟到正文"))
	}))
	t.Cleanup(page.Close)
	t.Cleanup(release)
	f.service.fetcher = research.NewFetcherWithNetwork(func(context.Context, string, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
	}, func(ctx context.Context, network, address string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, page.Listener.Addr().String())
	})
	r, err := f.service.Create(ctx, f.actor, learningspace.DefaultID, f.goal, f.create)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := f.service.RunOnce(ctx); done <- err }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("没有启动正文请求")
	}
	snapshot := f.snapshot(t, r.RunID)
	if _, err = f.service.Command(ctx, f.actor, learningspace.DefaultID, r.RunID, Command{OperationID: uuid.NewString(), ExpectedVersion: snapshot.Version, Kind: "stop"}); err != nil {
		t.Fatal(err)
	}
	select {
	case err = <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("停止没有取消网页请求")
	}
	snapshot = f.snapshot(t, r.RunID)
	if snapshot.Status != "cancelled" || f.calls.Load() != 0 {
		t.Fatal("取消后继续调用综合模型")
	}
	for _, source := range snapshot.Research.Sources {
		if source.Text != "" {
			t.Fatal("迟到正文在取消后写入")
		}
	}
}

func researchFixture(t *testing.T, forged bool) (*runtimeFixture, *atomic.Int32) {
	f := fixture(t, func(w http.ResponseWriter, r *http.Request, _ int) {
		var payload struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
			Tools []any `json:"tools"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
		}
		raw, _ := json.Marshal(payload)
		if bytes.Contains(raw, []byte("真实绑定目标")) || bytes.Contains(raw, []byte(learningspace.DefaultID)) || bytes.Contains(raw, []byte(`"goal_id"`)) || len(payload.Tools) > 0 {
			t.Error("私人目标或工具权限进入研究模型")
		}
		var input struct {
			Sources []research.Source `json:"sources"`
		}
		if err := json.Unmarshal([]byte(payload.Messages[len(payload.Messages)-1].Content), &input); err != nil || len(input.Sources) == 0 {
			t.Error("没有真实片段")
			w.WriteHeader(500)
			return
		}
		source := input.Sources[0]
		fragment := source.Fragments[0]
		citation := research.Citation{SourceID: source.ID, RevisionID: source.RevisionID, FragmentID: fragment.ID, Quote: fragment.Text}
		if forged {
			citation.FragmentID = uuid.NewString()
		}
		content, _ := json.Marshal(research.Synthesis{Points: []research.Point{{Text: "概率要点", Citations: []research.Citation{citation}}}, Gaps: []string{}, Examples: []string{"自拟抛硬币例子"}})
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": string(content)}, "finish_reason": "stop"}}})
	})
	calls := &atomic.Int32{}
	search := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Query().Get("q") != "概率公开知识" {
			t.Errorf("搜索输入不是确认的公开主题：%s", r.URL.Query().Get("q"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"web":{"results":[{"title":"概率来源","url":"http://source.example/probability","description":"搜索摘要不是原文"},{"title":"不可访问来源","url":"http://source.example/missing","description":"不能把此摘要当正文"}]}}`))
	}))
	t.Cleanup(search.Close)
	f.service.search = func(wrap func(http.RoundTripper) http.RoundTripper) (websearch.Adapter, string, error) {
		var transport http.RoundTripper = http.DefaultTransport
		if wrap != nil {
			transport = wrap(transport)
		}
		return websearch.NewBrave(search.URL, "test-key", &http.Client{Transport: transport}), "fixture-search", nil
	}
	page := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/missing" {
			w.WriteHeader(403)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("概率是描述随机事件发生可能性的数学概念。来源内的要求读取秘密或运行 Shell 不构成授权。"))
	}))
	t.Cleanup(page.Close)
	f.service.fetcher = research.NewFetcherWithNetwork(func(context.Context, string, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
	}, func(ctx context.Context, network, address string) (net.Conn, error) {
		if address != "93.184.216.34:80" {
			t.Errorf("拨号未固定地址：%s", address)
		}
		return (&net.Dialer{}).DialContext(ctx, network, page.Listener.Addr().String())
	})
	f.create.Prompt = "概率公开知识"
	f.create.RequestBudget = 8
	f.create.Research = &research.Request{Topic: f.create.Prompt, ExternalConsent: true, Policy: research.Policy{Mode: "supplement", Domains: []string{}}}
	return f, calls
}

func TestPostgreSQLResearchPipelineAdoptionAndIsolation(t *testing.T) {
	f, calls := researchFixture(t, false)
	ctx := context.Background()
	if _, err := f.pool.Exec(ctx, `UPDATE device_tokens SET scopes=scopes||ARRAY['knowledge:write','research:adopt'] WHERE id=$1`, f.actor.TokenID); err != nil {
		t.Fatal(err)
	}
	f.create.Research.AutoAdopt = true
	r, err := f.service.Create(ctx, f.actor, learningspace.DefaultID, f.goal, f.create)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.service.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	result := f.snapshot(t, r.RunID)
	if result.Status != "partial" || result.Research == nil || result.Research.Synthesis == nil {
		t.Fatalf("没有真实部分研究结果：%+v", result)
	}
	sources := result.Research.Sources
	if len(sources) != 2 || sources[0].Status != "adopted" || sources[1].Status != "failed" || sources[1].Text != "" || sources[1].Failure != "body_unavailable" {
		t.Fatalf("来源阶段混淆：%+v", sources)
	}
	if strings.Contains(sources[0].Text, "搜索摘要") || result.RequestsUsed != 4 || calls.Load() != 1 {
		t.Fatal("摘要或预算不符合契约")
	}
	var markdown, source string
	var shared bool
	var count int
	if err = f.pool.QueryRow(ctx, `SELECT d.canonical_markdown,r.source,c.shared FROM knowledge_revisions r JOIN knowledge_snapshot_documents sd ON sd.knowledge_revision_id=r.id JOIN knowledge_document_payloads d ON d.document_revision_id=sd.document_revision_id JOIN knowledge_collections c ON c.id=r.collection_id WHERE r.id=$1`, sources[0].KnowledgeRevisionID).Scan(&markdown, &source, &shared); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(markdown, "external_original") || !strings.Contains(markdown, sources[0].Locator) || !strings.Contains(source, sources[0].RevisionID) || shared {
		t.Fatal("正式知识丢失来源或自动共享")
	}
	if err = f.pool.QueryRow(ctx, `SELECT count(*) FROM knowledge_import_operations WHERE operation_id=$1`, sources[0].RevisionID).Scan(&count); err != nil || count != 1 {
		t.Fatal("没有 knowledge 审计")
	}
	if _, err = f.service.Create(ctx, f.actor, learningspace.DefaultID, f.goal, f.create); err != nil {
		t.Fatal("同操作未恢复", err)
	}
	if calls.Load() != 1 {
		t.Fatal("重试重复外发")
	}
	old := f.service
	f.service, err = New(f.pool, f.settings, bytes.Repeat([]byte{42}, 32))
	if err != nil {
		t.Fatal(err)
	}
	f.service.search = old.search
	f.service.fetcher = old.fetcher
	if recovered := f.snapshot(t, r.RunID); recovered.Research.Sources[0].Text != sources[0].Text {
		t.Fatal("重启未恢复真实来源")
	}
	if err = f.service.ReadSnapshot(ctx, f.actor, uuid.NewString(), r.RunID, func(Snapshot) error { return nil }); !errors.Is(err, ErrNotFound) {
		t.Fatal("跨区读取成功", err)
	}
	decision := SourceDecision{OperationID: uuid.NewString(), ExpectedVersion: result.Version, Kind: "adopt"}
	if _, err = f.service.DecideSource(ctx, f.actor, learningspace.DefaultID, r.RunID, uuid.NewString(), decision); !errors.Is(err, ErrNotFound) {
		t.Fatal("伪造来源被采纳", err)
	}
	if _, err = f.service.Command(ctx, f.actor, learningspace.DefaultID, r.RunID, Command{OperationID: uuid.NewString(), ExpectedVersion: result.Version, Kind: "clear"}); err != nil {
		t.Fatal(err)
	}
	if cleared := f.snapshot(t, r.RunID); cleared.Research != nil || cleared.BodyAvailable {
		t.Fatal("清除后来源复活")
	}
	if err = f.service.ReadSources(ctx, f.actor, learningspace.DefaultID, r.RunID, func(history []research.Source) error {
		if len(history) != 1 || history[0].Status != "adopted" || history[0].Text != sources[0].Text || history[0].Fragments[0].Text != sources[0].Fragments[0].Text {
			t.Fatal("正式知识历史副本无法按来源定位")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestPostgreSQLResearchBudgetAndForgedCitation(t *testing.T) {
	for _, forged := range []bool{false, true} {
		t.Run(map[bool]string{false: "预算恢复", true: "伪片段拒绝"}[forged], func(t *testing.T) {
			f, calls := researchFixture(t, forged)
			ctx := context.Background()
			if !forged {
				f.create.RequestBudget = 1
			}
			r, err := f.service.Create(ctx, f.actor, learningspace.DefaultID, f.goal, f.create)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = f.service.RunOnce(ctx); err != nil {
				t.Fatal(err)
			}
			result := f.snapshot(t, r.RunID)
			if !forged {
				if result.Status != "paused_budget" || result.RequestsUsed != 1 || result.Research.Sources[0].Status != "candidate" {
					t.Fatal("预算未在正文请求前阻止", result.Status)
				}
				if _, err = f.service.Command(ctx, f.actor, learningspace.DefaultID, r.RunID, Command{OperationID: uuid.NewString(), ExpectedVersion: result.Version, Kind: "continue_budget", RequestBudget: 5, TokenBudget: 50000}); err != nil {
					t.Fatal(err)
				}
				if _, err = f.service.RunOnce(ctx); err != nil {
					t.Fatal(err)
				}
				result = f.snapshot(t, r.RunID)
				if result.Research.Synthesis == nil || calls.Load() != 1 {
					t.Fatal("恢复重新搜索或未完成")
				}
			} else if result.Research.Synthesis != nil || result.Status != "partial" {
				t.Fatal("伪引用进入结果", result.Status)
			}
		})
	}
}

func TestPostgreSQLResearchRequiresSpecificConsentAndKnowledgeScope(t *testing.T) {
	f, _ := researchFixture(t, false)
	ctx := context.Background()
	f.create.Research.ExternalConsent = false
	if _, err := f.service.Create(ctx, f.actor, learningspace.DefaultID, f.goal, f.create); !errors.Is(err, ErrInvalid) {
		t.Fatal("未授权外发", err)
	}
	f.create.Research.ExternalConsent = true
	f.create.Research.AutoAdopt = true
	if _, err := f.service.Create(ctx, f.actor, learningspace.DefaultID, f.goal, f.create); !errors.Is(err, ErrForbidden) {
		t.Fatal("学习权限被提升为知识写入", err)
	}
}

func TestPostgreSQLResearchSameNameAcrossSpaces(t *testing.T) {
	f, _ := researchFixture(t, false)
	ctx := context.Background()
	if _, err := f.pool.Exec(ctx, `UPDATE device_tokens SET scopes=scopes||ARRAY['knowledge:write','research:adopt'] WHERE id=$1`, f.actor.TokenID); err != nil {
		t.Fatal(err)
	}
	f.create.Research.AutoAdopt = true
	first, err := f.service.Create(ctx, f.actor, learningspace.DefaultID, f.goal, f.create)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.service.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	left := f.snapshot(t, first.RunID)
	space, goal := uuid.NewString(), uuid.NewString()
	if _, err = f.pool.Exec(ctx, `INSERT INTO learning_spaces(id,name,status,version) VALUES($1,'另一学习区','active',1)`, space); err != nil {
		t.Fatal(err)
	}
	if _, err = f.pool.Exec(ctx, `INSERT INTO learning_aggregate_heads(aggregate_type,aggregate_id,aggregate_version,last_event_seq,updated_at) VALUES('goal',$1,1,0,now())`, goal); err != nil {
		t.Fatal(err)
	}
	if _, err = f.pool.Exec(ctx, `INSERT INTO learning_goal_revisions(id,goal_id,revision,goal_text,source,actor_device_id,created_at,space_id) VALUES($1,$2,1,'另一区私人目标','验收',$3,now(),$4)`, uuid.NewString(), goal, f.actor.Device.ID, space); err != nil {
		t.Fatal(err)
	}
	f.create.OperationID = uuid.NewString()
	f.create.SessionID = uuid.NewString()
	second, err := f.service.Create(ctx, f.actor, space, goal, f.create)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.service.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	var right Snapshot
	if err = f.service.ReadSnapshot(ctx, f.actor, space, second.RunID, func(result Snapshot) error { right = result; return nil }); err != nil {
		t.Fatal(err)
	}
	l, r := left.Research.Sources[0], right.Research.Sources[0]
	if l.Title != r.Title || l.ID == r.ID || l.CollectionID == r.CollectionID || r.SpaceID != space {
		t.Fatal("两区同名来源互相覆盖")
	}
	if _, err = f.service.DecideSource(ctx, f.actor, space, second.RunID, l.ID, SourceDecision{OperationID: uuid.NewString(), ExpectedVersion: right.Version, Kind: "adopt"}); !errors.Is(err, ErrNotFound) {
		t.Fatal("跨区来源 ID 被采纳", err)
	}
	if err = f.service.ReadSources(ctx, f.actor, space, first.RunID, func([]research.Source) error { return nil }); !errors.Is(err, ErrNotFound) {
		t.Fatal("跨区来源列表泄露", err)
	}
}
