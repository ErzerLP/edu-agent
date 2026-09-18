package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/identity"
	identitydb "github.com/edu-agent/edu-agent/server/internal/identity/postgresstore"
	knowledgedb "github.com/edu-agent/edu-agent/server/internal/knowledge/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/learning"
	learningdb "github.com/edu-agent/edu-agent/server/internal/learning/postgresstore"
	space "github.com/edu-agent/edu-agent/server/internal/learningspace"
	spacedb "github.com/edu-agent/edu-agent/server/internal/learningspace/postgresstore"
	tutoringdb "github.com/edu-agent/edu-agent/server/internal/tutoring/postgresstore"
	"github.com/edu-agent/edu-agent/server/migrations"
	"github.com/getkin/kin-openapi/openapi3"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func webTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("未配置真实 PostgreSQL，Web HTTP 集成未运行")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	schema := "web_http_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = admin.Exec(ctx, "DROP SCHEMA "+schema+" CASCADE"); admin.Close() })
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := migrations.Run(ctx, pool); err != nil {
		t.Fatal(err)
	}
	return pool
}

func TestPostgreSQLWebHTTPIdentityAndRealGoals(t *testing.T) {
	pool := webTestPool(t)
	ctx := context.Background()
	now := time.Now().UTC()
	store := learningdb.New(pool, tutoringdb.New(pool), knowledgedb.New(pool))
	if _, err := store.Rebuild(ctx); err != nil {
		t.Fatal(err)
	}
	learningService, err := learning.NewService(store, store, goalHTTPResolver{}, learning.ServiceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var ids *identity.Service
	var origin string
	var logs bytes.Buffer
	newHandler := func() http.Handler {
		ids, err = identity.NewService(identitydb.New(pool), identity.Options{PairingCodeTTL: 5 * time.Minute, PairingCodeMaxAttempts: 5, LastUsedTouchInterval: time.Minute, Now: func() time.Time { return now }})
		if err != nil {
			t.Fatal(err)
		}
		u, _ := url.Parse(origin)
		h, err := New(Options{Identity: ids, Learning: learningService, LearningSpaces: spacedb.New(pool), Readiness: fakeReadiness{}, Logger: slog.New(slog.NewTextHandler(&logs, nil)), PairLimiter: NewFixedWindowLimiter(1000, time.Minute), AuthLimiter: NewFixedWindowLimiter(1000, time.Minute), DeviceLimiter: NewFixedWindowLimiter(1000, time.Minute),
			WebUI: WebUIOptions{Enabled: true, AllowLoopbackHTTP: true, PublicBaseURL: u, Identity: ids, Assets: fstest.MapFS{"index.html": {Data: []byte(`<html><script type="module" src="/app/assets/main.js"></script></html>`)}, "assets/main.js": {Data: []byte(`export {}`)}}}})
		if err != nil {
			t.Fatal(err)
		}
		return h
	}
	server := httptest.NewUnstartedServer(nil)
	origin = "http://" + server.Listener.Addr().String()
	server.Config.Handler = newHandler()
	server.Start()
	defer server.Close()
	client := server.Client()
	var cookie *http.Cookie
	csrf := ""
	request := func(method, path, scope string, body any, alter func(*http.Request)) (int, []byte, *http.Response) {
		t.Helper()
		raw, _ := json.Marshal(body)
		r, err := http.NewRequest(method, origin+path, bytes.NewReader(raw))
		if err != nil {
			t.Fatal(err)
		}
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Origin", origin)
		if scope != "" {
			r.Header.Set(space.Header, scope)
		}
		if cookie != nil {
			r.AddCookie(cookie)
			r.Header.Set("X-CSRF-Token", csrf)
		}
		if alter != nil {
			alter(r)
		}
		response, err := client.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		data, err := io.ReadAll(response.Body)
		if err != nil {
			t.Fatal(err)
		}
		return response.StatusCode, data, response
	}
	loader := openapi3.NewLoader()
	doc, err := loader.LoadFromFile("../../../api/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var principalID string
	pair := func() {
		t.Helper()
		cookie = nil
		code, _, err := ids.CreatePairingCode(ctx)
		if err != nil {
			t.Fatal(err)
		}
		status, data, response := request("POST", "/v1/web/pairings", "", map[string]string{"code": code, "display_name": "浏览器学习"}, nil)
		if status != 201 {
			t.Fatalf("浏览器配对失败: %d %s", status, data)
		}
		var payload map[string]any
		if err := json.Unmarshal(data, &payload); err != nil {
			t.Fatal(err)
		}
		if err := doc.Components.Schemas["WebSession"].Value.VisitJSON(payload, openapi3.EnableJSONSchema2020()); err != nil {
			t.Fatal(err)
		}
		if _, exists := payload["token"]; exists {
			t.Fatal("响应泄漏长期 Token")
		}
		csrf = payload["csrf_token"].(string)
		principalID = payload["device"].(map[string]any)["id"].(string)
		cookies := response.Cookies()
		if len(cookies) != 1 {
			t.Fatal("缺少唯一会话 Cookie")
		}
		cookie = cookies[0]
		if !cookie.HttpOnly || cookie.Domain != "" || cookie.Path != "/" || cookie.SameSite != http.SameSiteStrictMode || cookie.Secure {
			t.Fatalf("开发 Cookie 属性不符: %+v", cookie)
		}
		if _, err := ids.Authenticate(ctx, cookie.Value, ""); err != identity.ErrUnauthenticated {
			t.Fatal("Cookie 被错误当作 Bearer")
		}
		if strings.Contains(logs.String(), cookie.Value) || strings.Contains(logs.String(), csrf) || strings.Contains(logs.String(), code) {
			t.Fatal("日志包含会话或配对秘密")
		}
	}
	pair()
	for _, path := range []string{"/app/", "/app/spaces/" + space.DefaultID, "/app/settings", "/app/runs", "/app/runs/import/" + uuid.NewString(), "/app/runs/run/" + uuid.NewString(), "/app/assets/main.js"} {
		status, _, response := request("GET", path, "", nil, nil)
		if status != 200 {
			t.Fatalf("学习入口不存在 %s: %d", path, status)
		}
		csp := response.Header.Get("Content-Security-Policy")
		if !strings.Contains(csp, "img-src 'self' data: blob:;") || !strings.Contains(csp, "script-src 'self';") || !strings.Contains(csp, "connect-src 'self';") {
			t.Fatal("安全页图的 CSP 扩展改变了脚本或网络边界", csp)
		}
	}
	for _, path := range []string{"/v1/no-such-api", "/app/assets/missing.js", "/app/missing.css"} {
		status, data, _ := request("GET", path, "", nil, nil)
		if status != 404 || strings.Contains(string(data), "<html>") {
			t.Fatalf("错误 SPA 回退 %s: %d %s", path, status, data)
		}
	}
	for _, test := range []struct {
		name, path string
		want       int
		alter      func(*http.Request)
	}{
		{"缺少 Origin", "/v1/learning/goals", 403, func(r *http.Request) { r.Header.Del("Origin") }},
		{"错误 Origin", "/v1/learning/goals", 403, func(r *http.Request) { r.Header.Set("Origin", "https://evil.example") }},
		{"缺少 CSRF", "/v1/learning/goals", 403, func(r *http.Request) { r.Header.Del("X-CSRF-Token") }},
		{"其他会话 CSRF", "/v1/learning/goals", 403, func(r *http.Request) { r.Header.Set("X-CSRF-Token", strings.Repeat("x", 43)) }},
		{"混用身份", "/v1/learning/goals", 400, func(r *http.Request) { r.Header.Set("Authorization", "Bearer test") }},
		{"重复 Cookie", "/v1/learning/goals", 400, func(r *http.Request) { r.AddCookie(cookie) }},
		{"旧页面身份", "/v1/learning/goals", 401, func(r *http.Request) { r.Header.Set("X-Web-Principal-ID", uuid.NewString()) }},
		{"旧页面代次", "/v1/learning/goals", 401, func(r *http.Request) { r.Header.Set("X-Web-Generation", "999") }},
		{"旧设备写路径 CSRF", "/v1/devices/" + principalID, 403, func(r *http.Request) { r.Header.Del("X-CSRF-Token") }},
		{"管理面", "/admin/api/pairing-codes", 403, nil}, {"内部面", "/internal/privacy/migrations/acquire", 403, nil}, {"MCP", "/mcp", 403, nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			status, data, _ := request("POST", test.path, "", map[string]any{}, test.alter)
			if status != test.want {
				t.Fatalf("边界失败 %d %s", status, data)
			}
		})
	}
	spaceID := ""
	status, data, _ := request("POST", "/v1/learning-spaces", "", space.Command{OperationID: uuid.NewString(), Name: "学习区甲", Status: "active"}, nil)
	if status != 200 {
		t.Fatalf("创建学习区 %d %s", status, data)
	}
	var sp space.Space
	_ = json.Unmarshal(data, &sp)
	spaceID = sp.ID
	goalID := uuid.NewString()
	body := map[string]any{"operation_id": uuid.NewString(), "payload_schema_version": 1, "aggregate_type": "goal", "aggregate_id": goalID, "expected_version": 0, "text": "没有模型也能保存的真实目标", "source": "web"}
	status, data, _ = request("POST", "/v1/learning/goals", spaceID, body, nil)
	if status != 201 {
		t.Fatalf("无模型目标保存失败 %d %s", status, data)
	}
	var result struct {
		Result learning.GoalRevision `json:"result"`
	}
	_ = json.Unmarshal(data, &result)
	// 重建身份与 HTTP 服务，验证 Cookie 和目标均来自 PostgreSQL。
	server.Close()
	server = httptest.NewUnstartedServer(nil)
	origin = "http://" + server.Listener.Addr().String()
	server.Config.Handler = newHandler()
	server.Start()
	defer server.Close()
	client = server.Client()
	status, data, _ = request("GET", "/v1/learning/goals/"+goalID, spaceID, nil, nil)
	if status != 200 {
		t.Fatalf("重启恢复失败 %d %s", status, data)
	}
	status, _, _ = request("GET", "/v1/learning/goals/"+goalID, space.DefaultID, nil, nil)
	if status != 404 {
		t.Fatalf("跨区实体未拒绝 %d", status)
	}
	for _, action := range []string{"start", "pause", "resume", "complete", "archive", "restore"} {
		body["operation_id"] = uuid.NewString()
		body["expected_version"] = result.Result.Revision
		body["action"] = action
		body["previous_revision_id"] = result.Result.ID
		if action == "complete" {
			status, _, _ = request("PUT", "/v1/learning/goals/"+goalID, spaceID, body, nil)
			if status != 400 {
				t.Fatalf("无依据完成未拒绝 %d", status)
			}
			body["completion_reason"] = "已独立完成练习"
			body["operation_id"] = uuid.NewString()
		} else {
			delete(body, "completion_reason")
		}
		status, data, _ = request("PUT", "/v1/learning/goals/"+goalID, spaceID, body, nil)
		if status != 201 && status != 200 {
			t.Fatalf("生命周期 %s: %d %s", action, status, data)
		}
		_ = json.Unmarshal(data, &result)
	}
	var sessions, evidence int
	if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM tutoring_sessions),(SELECT count(*) FROM learning_evidence)`).Scan(&sessions, &evidence); err != nil {
		t.Fatal(err)
	}
	if sessions != 0 || evidence != 0 {
		t.Fatalf("保存/手工状态改变了教学事实: %d/%d", sessions, evidence)
	}
	status, _, _ = request("POST", "/v1/web/logout", "", nil, nil)
	if status != 204 {
		t.Fatalf("退出失败 %d", status)
	}
	status, _, _ = request("POST", "/v1/learning/goals", spaceID, body, nil)
	if status != 401 {
		t.Fatal("退出后的会话仍可写入")
	}
	var revoked *time.Time
	if err := pool.QueryRow(ctx, `SELECT revoked_at FROM devices WHERE id=$1`, principalID).Scan(&revoked); err != nil || revoked != nil {
		t.Fatal("退出错误撤销了设备")
	}
	pair()
	now = now.Add(identity.WebSessionTTL)
	status, _, expiredResponse := request("POST", "/v1/learning/goals", spaceID, body, nil)
	if status != 401 {
		t.Fatal("过期会话未拒绝")
	}
	if len(expiredResponse.Cookies()) != 1 || expiredResponse.Cookies()[0].MaxAge >= 0 {
		t.Fatal("过期 Cookie 未清除，会阻止重新配对")
	}
	pair()
	if err := ids.RevokeDevice(ctx, principalID); err != nil {
		t.Fatal(err)
	}
	status, _, _ = request("POST", "/v1/learning/goals", spaceID, body, nil)
	if status != 401 {
		t.Fatal("撤销设备未拒绝")
	}
	pair()
	if _, err := pool.Exec(ctx, `UPDATE device_tokens SET scopes=ARRAY['learning:read'] WHERE device_id=$1`, principalID); err != nil {
		t.Fatal(err)
	}
	status, _, _ = request("POST", "/v1/learning/goals", spaceID, body, nil)
	if status != 403 {
		t.Fatal("收回 scope 未拒绝")
	}
}

func TestWebProductionCookieAndConfiguration(t *testing.T) {
	u, _ := url.Parse("https://learn.example")
	a := &API{webUI: WebUIOptions{PublicBaseURL: u}}
	w := httptest.NewRecorder()
	a.setWebCookie(w, "opaque", time.Now().Add(time.Hour))
	c := w.Result().Cookies()[0]
	if c.Name != "__Host-edu_web" || !c.Secure || !c.HttpOnly || c.Domain != "" || c.Path != "/" || c.SameSite != http.SameSiteStrictMode {
		t.Fatal("生产 Cookie 属性错误")
	}
	assets := fstest.MapFS{"index.html": {Data: []byte(`<script type="module" src="/app/assets/main.js"></script>`)}, "assets/main.js": {Data: []byte(`export {}`)}}
	opts := WebUIOptions{Enabled: true, PublicBaseURL: u, Identity: &identity.Service{}, Assets: assets}
	if err := validateWebUI(opts); err != nil {
		t.Fatalf("合法 HTTPS 配置未通过: %v", err)
	}
	for _, origin := range []string{"http://learn.example", "http://localhost:8080", "https://learn.example/path"} {
		u, _ := url.Parse(origin)
		opts.PublicBaseURL = u
		if validateWebUI(opts) == nil {
			t.Fatalf("错误配置被接受 %s", origin)
		}
	}
	opts.PublicBaseURL, _ = url.Parse("http://localhost:8080")
	opts.AllowLoopbackHTTP = true
	if err := validateWebUI(opts); err != nil {
		t.Fatal("显式 loopback 开发配置被拒绝")
	}
	delete(assets, "assets/main.js")
	if validateWebUI(opts) == nil {
		t.Fatal("缺少入口引用的真实资源未阻止启用 Web")
	}
}
