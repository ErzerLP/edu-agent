package httpapi

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/identity"
	identitydb "github.com/edu-agent/edu-agent/server/internal/identity/postgresstore"
	knowledgedb "github.com/edu-agent/edu-agent/server/internal/knowledge/postgresstore"
	learningdb "github.com/edu-agent/edu-agent/server/internal/learning/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/learningcontent"
	"github.com/edu-agent/edu-agent/server/internal/learningspace"
	spacedb "github.com/edu-agent/edu-agent/server/internal/learningspace/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/learningstart"
	"github.com/edu-agent/edu-agent/server/internal/mentorrun"
	"github.com/edu-agent/edu-agent/server/internal/research"
	"github.com/edu-agent/edu-agent/server/internal/settings"
	tutoringdb "github.com/edu-agent/edu-agent/server/internal/tutoring/postgresstore"
	"github.com/google/uuid"
)

func TestPostgreSQLResearchCookieSourcesAndDecisions(t *testing.T) {
	pool := webTestPool(t)
	ctx := context.Background()
	ids, err := identity.NewService(identitydb.New(pool), identity.Options{PairingCodeTTL: time.Minute, PairingCodeMaxAttempts: 5, LastUsedTouchInterval: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	code, _, err := ids.CreatePairingCodeForProfile(ctx, identity.PairingProfileResearch)
	if err != nil {
		t.Fatal(err)
	}
	cookie, principal, err := ids.ExchangeWebPairing(ctx, code, "来源浏览器验收")
	if err != nil {
		t.Fatal(err)
	}
	configuration, err := settings.Open(settings.Options{Path: filepath.Join(t.TempDir(), "private", "settings.json"), ModelEndpoints: []string{"https://model.example/v1"}})
	if err != nil {
		t.Fatal(err)
	}
	key := "fixture-only-key"
	if _, err = configuration.Update(settings.Update{Target: settings.Mentor, Connection: &settings.Connection{Enabled: true, Provider: "openai_compatible", Endpoint: "https://model.example/v1", Model: "fixture", AuthMode: "bearer"}, NewKey: &key}); err != nil {
		t.Fatal(err)
	}
	if _, err = configuration.Update(settings.Update{ExpectedRevision: 1, Target: settings.Search, Connection: &settings.Connection{Enabled: true, Provider: "brave", Endpoint: "https://api.search.brave.com/res/v1/web/search", AuthMode: "bearer"}, NewKey: &key}); err != nil {
		t.Fatal(err)
	}
	encryptionKey := bytes.Repeat([]byte{42}, 32)
	runtime, err := mentorrun.New(pool, configuration, encryptionKey)
	if err != nil {
		t.Fatal(err)
	}
	knowledgeStore := knowledgedb.New(pool)
	learningStore := learningdb.New(pool, tutoringdb.New(pool), knowledgeStore)
	contentStore, err := learningcontent.New(pool, learningStore, encryptionKey)
	if err != nil {
		t.Fatal(err)
	}
	learningStore.ConfigureContent(contentStore)
	runtime.ConfigureStart(&learningstart.Service{Learning: learningStore, Knowledge: knowledgeStore, Content: contentStore})
	goal := uuid.NewString()
	if _, err = pool.Exec(ctx, `INSERT INTO learning_aggregate_heads(aggregate_type,aggregate_id,aggregate_version,last_event_seq,updated_at) VALUES('goal',$1,1,0,now())`, goal); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO learning_goal_revisions(id,goal_id,revision,goal_text,source,actor_device_id,created_at) VALUES($1,$2,1,'私人目标','验收',$3,now())`, uuid.NewString(), goal, principal.Device.ID); err != nil {
		t.Fatal(err)
	}
	request := research.Request{Topic: "公开主题", ExternalConsent: true, Policy: research.Policy{Mode: "supplement", Domains: []string{}}}
	receipt, err := runtime.Create(ctx, principal.Credential, learningspace.DefaultID, goal, mentorrun.Create{OperationID: uuid.NewString(), SessionID: uuid.NewString(), ExpectedVersion: 1, Prompt: request.Topic, Research: &request, Save: true, RequestBudget: 5, TokenBudget: 10000})
	if err != nil {
		t.Fatal(err)
	}
	// transport 夹具注入已读取的加密 checkpoint；搜索/解析生产路径另由运行与网络测试覆盖。
	now := time.Now().UTC()
	text := "真实来源正文，必须能够按字节定位。"
	source := research.Source{SpaceID: learningspace.DefaultID, GoalID: goal, Purpose: "goal_reference", ID: uuid.NewString(), RevisionID: uuid.NewString(), Locator: "https://example.org/source", FinalURL: "https://example.org/source", Title: "来源标题", Kind: "text/plain", Status: "parsed", FetchedAt: &now, Fingerprint: strings.Repeat("a", 64), Parser: "safe-text-v1", Coverage: "complete_text", StorageAllowed: true, Text: text, Fragments: []research.Fragment{{ID: uuid.NewString(), Start: 0, End: len(text), Text: text}}}
	raw, _ := json.Marshal(mentorrun.Body{Research: &research.State{Request: request, Discovered: true, Sources: []research.Source{source}}})
	block, _ := aes.NewCipher(encryptionKey)
	aead, _ := cipher.NewGCM(block)
	nonce := bytes.Repeat([]byte{17}, aead.NonceSize())
	checkpoint := aead.Seal(nonce, nonce, raw, []byte(receipt.RunID))
	if _, err = pool.Exec(ctx, `UPDATE learning_mentor_runs SET checkpoint=$2,state=state||'{"status":"succeeded","stage":"completed"}'::jsonb WHERE id=$1`, receipt.RunID, checkpoint); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(nil)
	origin := "http://" + server.Listener.Addr().String()
	base, _ := url.Parse(origin)
	handler, err := New(Options{Identity: ids, LearningSpaces: spacedb.New(pool), Settings: configuration, MentorRuns: runtime, Readiness: fakeReadiness{}, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), PairLimiter: NewFixedWindowLimiter(1000, time.Minute), AuthLimiter: NewFixedWindowLimiter(1000, time.Minute), DeviceLimiter: NewFixedWindowLimiter(1000, time.Minute), WebUI: WebUIOptions{Enabled: true, AllowLoopbackHTTP: true, PublicBaseURL: base, Identity: ids, Assets: fstest.MapFS{"index.html": {Data: []byte(`<script type="module" src="/app/assets/main.js"></script>`)}, "assets/main.js": {Data: []byte(`export {}`)}}}})
	if err != nil {
		t.Fatal(err)
	}
	server.Config.Handler = handler
	server.Start()
	defer server.Close()
	call := func(method, path, space string, body any, csrf bool) (int, []byte) {
		t.Helper()
		raw, _ := json.Marshal(body)
		r, _ := http.NewRequest(method, origin+path, bytes.NewReader(raw))
		r.AddCookie(&http.Cookie{Name: "edu_web_dev", Value: cookie})
		r.Header.Set("Origin", origin)
		r.Header.Set("Content-Type", "application/json")
		if space != "" {
			r.Header.Set(learningspace.Header, space)
		}
		if csrf {
			r.Header.Set("X-CSRF-Token", webCSRF(cookie))
		}
		response, err := server.Client().Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		data, _ := io.ReadAll(response.Body)
		return response.StatusCode, data
	}
	path := "/v1/learning/runs/" + receipt.RunID + "/sources"
	status, data := call("GET", path+"?limit=1&q=来源", learningspace.DefaultID, nil, true)
	if status != 200 || !bytes.Contains(data, []byte(source.ID)) || bytes.Contains(data, []byte(text)) {
		t.Fatalf("来源列表没有分页或泄露正文：%d %s", status, data)
	}
	status, data = call("GET", path+"/"+source.ID, learningspace.DefaultID, nil, true)
	if status != 200 || !bytes.Contains(data, []byte(text)) {
		t.Fatalf("来源原文不可读：%d %s", status, data)
	}
	status, _ = call("GET", path+"/"+uuid.NewString(), learningspace.DefaultID, nil, true)
	if status != 404 {
		t.Fatal("伪造来源 ID 成功")
	}
	decision := mentorrun.SourceDecision{OperationID: uuid.NewString(), ExpectedVersion: receipt.Version, Kind: "adopt"}
	status, _ = call("POST", path+"/"+source.ID+"/decisions", learningspace.DefaultID, decision, false)
	if status != 403 {
		t.Fatal("来源采纳绕过 CSRF")
	}
	status, data = call("POST", path+"/"+source.ID+"/decisions", learningspace.DefaultID, decision, true)
	if status != 202 {
		t.Fatalf("来源采纳失败：%d %s", status, data)
	}
	status, replayed := call("POST", path+"/"+source.ID+"/decisions", learningspace.DefaultID, decision, true)
	if status != 202 || !bytes.Equal(data, replayed) {
		t.Fatal("采纳操作重试不是原回执")
	}
	status, data = call("GET", path+"/"+source.ID, learningspace.DefaultID, nil, true)
	if status != 200 || !bytes.Contains(data, []byte(`"status":"adopted"`)) {
		t.Fatal("真实采纳结果未显示")
	}
	status, data = call("GET", "/v1/learning/start/capabilities", "", nil, true)
	if status != 200 || !bytes.Contains(data, []byte(`"available":true`)) || !bytes.Contains(data, []byte(`"legacy_projection":"open_activity"`)) {
		t.Fatalf("开学能力没有同步配置和权限：%d %s", status, data)
	}
	request.AutoAdopt = true
	opening := mentorrun.Create{OperationID: uuid.NewString(), SessionID: uuid.NewString(), ExpectedVersion: 1, Prompt: request.Topic, Research: &request, Save: true, RequestBudget: 8, TokenBudget: 30000, StartLearning: &learningstart.Request{NewSession: true, ModelConsent: true}}
	goalPath := "/v1/learning/goals/" + goal
	for _, denied := range []struct {
		space  string
		csrf   bool
		status int
	}{{learningspace.DefaultID, false, 403}, {"", true, 400}} {
		status, data = call("POST", goalPath+"/runs", denied.space, opening, denied.csrf)
		if status != denied.status {
			t.Fatalf("开学绕过作用域或 CSRF：%d %s", status, data)
		}
	}
	opening.StartLearning.ModelConsent = false
	status, data = call("POST", goalPath+"/runs", learningspace.DefaultID, opening, true)
	if status != 400 {
		t.Fatalf("缺少模型授权仍受理：%d %s", status, data)
	}
	opening.StartLearning.ModelConsent = true
	status, data = call("POST", goalPath+"/runs", learningspace.DefaultID, opening, true)
	if status != 202 {
		t.Fatalf("新开学协议未受理：%d %s", status, data)
	}
	var startReceipt mentorrun.Receipt
	if err = json.Unmarshal(data, &startReceipt); err != nil {
		t.Fatal(err)
	}
	status, data = call("GET", goalPath+"/start", learningspace.DefaultID, nil, true)
	if status != 200 || !bytes.Contains(data, []byte(startReceipt.RunID)) || !bytes.Contains(data, []byte(`"kind":"start_learning"`)) {
		t.Fatalf("刷新未恢复开学运行：%d %s", status, data)
	}
	status, data = call("GET", goalPath+"/research", learningspace.DefaultID, nil, true)
	if status != 200 || !bytes.Contains(data, []byte(receipt.RunID)) || bytes.Contains(data, []byte(startReceipt.RunID)) {
		t.Fatalf("开学与普通研究运行串线：%d %s", status, data)
	}
	status, data = call("GET", "/v1/tutoring/sessions/"+uuid.NewString()+"/knowledge-context", learningspace.DefaultID, nil, true)
	if status != 404 {
		t.Fatalf("不存在的教学上下文可见：%d %s", status, data)
	}
}
