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
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/identity"
	identitydb "github.com/edu-agent/edu-agent/server/internal/identity/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/settings"
	"github.com/getkin/kin-openapi/openapi3"
)

func TestSettingsReadsAndLegacyCapabilityRemainAvailable(t *testing.T) {
	var logs bytes.Buffer
	id := &fakeIdentity{auth: identity.Credential{Device: identity.Device{ID: "test"}, Scopes: []string{"learning:read", "model:probe"}}}
	h := newTestAPI(t, id, 100, 100, &logs)
	for _, path := range []string{"/v1/settings", "/v1/capabilities", "/v1/model/capabilities"} {
		r := httptest.NewRequest("GET", path, nil)
		r.Header.Set("Authorization", "Bearer test")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 200 {
			t.Fatalf("能力入口 %s 返回 %d", path, w.Code)
		}
		if path != "/v1/model/capabilities" && w.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("设置可被缓存")
		}
	}
}

func TestPostgreSQLSettingsHTTPAuthorizationAndPersistence(t *testing.T) {
	pool := webTestPool(t)
	ctx := context.Background()
	ids, err := identity.NewService(identitydb.New(pool), identity.Options{PairingCodeTTL: time.Minute, PairingCodeMaxAttempts: 5, LastUsedTouchInterval: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	const secret = "http-settings-secret-sentinel"
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Authorization") != "Bearer "+secret {
			t.Error("探测未使用已保存 Key")
		}
		io.WriteString(w, `{"choices":[{"message":{"content":"{\"capability_probe\":true}"}}]}`)
	}))
	defer provider.Close()
	o := settings.Options{Path: filepath.Join(t.TempDir(), "private", "settings.json"), ModelEndpoints: []string{provider.URL}}
	service, err := settings.Open(o)
	if err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	server := httptest.NewUnstartedServer(nil)
	origin := "http://" + server.Listener.Addr().String()
	base, _ := url.Parse(origin)
	newHandler := func() http.Handler {
		h, err := New(Options{Identity: ids, Settings: service, Readiness: fakeReadiness{}, Logger: slog.New(slog.NewTextHandler(&logs, nil)), PairLimiter: NewFixedWindowLimiter(1000, time.Minute), AuthLimiter: NewFixedWindowLimiter(1000, time.Minute), DeviceLimiter: NewFixedWindowLimiter(1000, time.Minute),
			WebUI: WebUIOptions{Enabled: true, AllowLoopbackHTTP: true, PublicBaseURL: base, Identity: ids, Assets: fstest.MapFS{"index.html": {Data: []byte(`<script type="module" src="/app/assets/main.js"></script>`)}, "assets/main.js": {Data: []byte(`export {}`)}}}})
		if err != nil {
			t.Fatal(err)
		}
		return h
	}
	var handler atomic.Value
	handler.Store(newHandler())
	server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { handler.Load().(http.Handler).ServeHTTP(w, r) })
	server.Start()
	defer server.Close()
	var cookie *http.Cookie
	csrf := ""
	request := func(method, path string, body any, alter func(*http.Request)) (*http.Response, []byte) {
		t.Helper()
		raw, _ := json.Marshal(body)
		r, _ := http.NewRequest(method, origin+path, bytes.NewReader(raw))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Origin", origin)
		if cookie != nil {
			r.AddCookie(cookie)
			r.Header.Set("X-CSRF-Token", csrf)
		}
		if alter != nil {
			alter(r)
		}
		response, err := server.Client().Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		data, _ := io.ReadAll(response.Body)
		if strings.Contains(string(data), secret) {
			t.Fatal("API 返回秘密")
		}
		return response, data
	}
	pair := func(profile identity.PairingProfile) string {
		t.Helper()
		cookie = nil
		code, _, err := ids.CreatePairingCodeForProfile(ctx, profile)
		if err != nil {
			t.Fatal(err)
		}
		response, data := request("POST", "/v1/web/pairings", map[string]string{"code": code, "display_name": "设置验收浏览器"}, nil)
		if response.StatusCode != 201 {
			t.Fatalf("配对失败 %d", response.StatusCode)
		}
		cookie = response.Cookies()[0]
		var session struct {
			CSRF   string          `json:"csrf_token"`
			Device identity.Device `json:"device"`
		}
		if json.Unmarshal(data, &session) != nil {
			t.Fatal("会话响应无效")
		}
		csrf = session.CSRF
		return session.Device.ID
	}
	reader := pair(identity.PairingProfileUser)
	connection := settings.Connection{Enabled: true, Provider: "openai_compatible", Endpoint: provider.URL, Model: "settings-test", AuthMode: "bearer"}
	update := map[string]any{"expected_revision": 0, "target": "teaching", "connection": connection, "new_key": secret}
	for _, method := range []string{"PUT", "POST"} {
		path := "/v1/settings"
		body := any(update)
		if method == "POST" {
			path += "/probes"
			body = map[string]any{"target": "teaching", "expected_revision": 0, "consent": true}
		}
		response, _ := request(method, path, body, nil)
		if response.StatusCode != 403 {
			t.Fatal("普通学习设备可写或探测设置")
		}
	}
	if calls.Load() != 0 {
		t.Fatal("无权限请求触发提供商")
	}
	response, _ := request("GET", "/v1/settings", nil, nil)
	if response.StatusCode != 200 {
		t.Fatal("普通设备不能读取非敏感状态")
	}
	writer := pair(identity.PairingProfileSettings)
	for _, alter := range []func(*http.Request){func(r *http.Request) { r.Header.Del("Origin") }, func(r *http.Request) { r.Header.Set("Origin", "https://evil.example") }, func(r *http.Request) { r.Header.Del("X-CSRF-Token") }, func(r *http.Request) { r.Header.Set("X-CSRF-Token", "wrong") }} {
		response, _ := request("PUT", "/v1/settings", update, alter)
		if response.StatusCode != 403 {
			t.Fatal("Origin/CSRF 未拒绝")
		}
	}
	response, data := request("PUT", "/v1/settings", update, nil)
	if response.StatusCode != 200 {
		t.Fatalf("真实配置保存失败 %d %s", response.StatusCode, data)
	}
	doc, err := openapi3.NewLoader().LoadFromFile("../../../api/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	validate := func(schema string, raw []byte) {
		t.Helper()
		var payload any
		json.Unmarshal(raw, &payload)
		if err := doc.Components.Schemas[schema].Value.VisitJSON(payload, openapi3.EnableJSONSchema2020()); err != nil {
			t.Fatal(err)
		}
	}
	validate("LearningSettings", data)
	var view settings.View
	json.Unmarshal(data, &view)
	if !view.Teaching.HasKey || !view.TeachingRestartRequired {
		t.Fatal("保存状态不符合实际配置")
	}
	_, data = request("GET", "/v1/capabilities", nil, nil)
	validate("LearningCapabilities", data)
	if calls.Load() != 0 {
		t.Fatal("只读能力触发了探测")
	}
	response, data = request("POST", "/v1/settings/probes", map[string]any{"target": "teaching", "expected_revision": view.Revision, "consent": true}, nil)
	if response.StatusCode != 200 || calls.Load() != 1 {
		t.Fatal("显式模型测试未执行")
	}
	validate("LearningSettings", data)
	service, err = settings.Open(o)
	if err != nil {
		t.Fatal(err)
	}
	handler.Store(newHandler())
	_, data = request("GET", "/v1/settings", nil, nil)
	json.Unmarshal(data, &view)
	if view.TeachingRestartRequired || view.Teaching.Status != "ready" || !view.Teaching.HasKey {
		t.Fatal("服务重建未恢复真实配置")
	}
	for _, bad := range []any{map[string]any{"expected_revision": view.Revision, "limits": map[string]any{"concurrency": "2"}}, map[string]any{"expected_revision": view.Revision, "target": "teaching", "connection": connection, "new_key": "bad\nkey"}, map[string]any{"expected_revision": 0, "target": "teaching", "clear_key": true}} {
		response, _ := request("PUT", "/v1/settings", bad, nil)
		if response.StatusCode != 400 && response.StatusCode != 409 {
			t.Fatal("非法格式或旧版本未拒绝")
		}
	}
	if _, err := pool.Exec(ctx, `UPDATE device_tokens SET scopes=ARRAY['learning:read','learning:write'] WHERE device_id=$1`, writer); err != nil {
		t.Fatal(err)
	}
	response, _ = request("POST", "/v1/settings/probes", map[string]any{"target": "teaching", "expected_revision": view.Revision, "consent": true}, nil)
	if response.StatusCode != 403 || calls.Load() != 1 {
		t.Fatal("scope 收回未即时生效")
	}
	if err := ids.RevokeDevice(ctx, writer); err != nil {
		t.Fatal(err)
	}
	response, _ = request("GET", "/v1/settings", nil, nil)
	if response.StatusCode != 401 {
		t.Fatal("撤销后学习会话仍可访问设置")
	}
	cookie = nil
	response, _ = request("PUT", "/v1/settings", update, func(r *http.Request) { r.AddCookie(&http.Cookie{Name: "edu_admin_session", Value: "fake-admin"}) })
	if response.StatusCode != 401 {
		t.Fatal("admin Cookie 冒充学习身份")
	}
	if strings.Contains(logs.String(), secret) {
		t.Fatal("HTTP 日志含 Key")
	}
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM device_tokens WHERE device_id=$1 AND 'settings:write'=ANY(scopes)`, reader).Scan(&count); err != nil || count != 0 {
		t.Fatal("旧设备被自动升级")
	}
}
