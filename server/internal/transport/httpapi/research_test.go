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
	"github.com/edu-agent/edu-agent/server/internal/learningspace"
	spacedb "github.com/edu-agent/edu-agent/server/internal/learningspace/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/mentorrun"
	"github.com/edu-agent/edu-agent/server/internal/research"
	"github.com/edu-agent/edu-agent/server/internal/settings"
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
		r.Header.Set(learningspace.Header, space)
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
}
