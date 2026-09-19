package httpapi

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
	"testing/fstest"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/identity"
	identitydb "github.com/edu-agent/edu-agent/server/internal/identity/postgresstore"
	knowledgedb "github.com/edu-agent/edu-agent/server/internal/knowledge/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/learning"
	learningdb "github.com/edu-agent/edu-agent/server/internal/learning/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/privacy"
	tutoringdb "github.com/edu-agent/edu-agent/server/internal/tutoring/postgresstore"
	"github.com/google/uuid"
)

func TestPostgreSQLWebOfflineIdentityIsolationAndPurge(t *testing.T) {
	pool := webTestPool(t)
	ctx := context.Background()
	ids, err := identity.NewService(identitydb.New(pool), identity.Options{PairingCodeTTL: 5 * time.Minute, PairingCodeMaxAttempts: 5, LastUsedTouchInterval: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	origin := "http://127.0.0.1:32929"
	u, _ := url.Parse(origin)
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	signer, err := learning.NewEd25519OfflineSigner("浏览器验收", key, origin, time.Now().Add(-time.Hour), time.Now().Add(365*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	offline, err := learning.NewOfflineService(learningdb.New(pool, tutoringdb.New(pool), knowledgedb.New(pool)), signer, origin, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	privacyService := &fakePrivacyHTTP{}
	enabled := true
	handler := func() http.Handler {
		h, err := New(Options{Identity: ids, Offline: offline, Privacy: privacyService, Readiness: fakeReadiness{}, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), PairLimiter: NewFixedWindowLimiter(1000, time.Minute), AuthLimiter: NewFixedWindowLimiter(1000, time.Minute), DeviceLimiter: NewFixedWindowLimiter(1000, time.Minute), WebUI: WebUIOptions{Enabled: true, OfflineEnabled: enabled, AllowLoopbackHTTP: true, PublicBaseURL: u, Identity: ids, Assets: fstest.MapFS{"index.html": {Data: []byte(`<script type="module" src="/app/assets/main.js"></script>`)}, "assets/main.js": {Data: []byte(`export {}`)}}}})
		if err != nil {
			t.Fatal(err)
		}
		return h
	}
	code, _, err := ids.CreatePairingCode(ctx)
	if err != nil {
		t.Fatal(err)
	}
	web, p, err := ids.ExchangeWebPairing(ctx, code, "离线设备")
	if err != nil {
		t.Fatal(err)
	}
	var offlineCookie *http.Cookie
	request := func(method, path string, body any, online bool, alter func(*http.Request)) *httptest.ResponseRecorder {
		raw, _ := json.Marshal(body)
		r := httptest.NewRequest(method, origin+path, bytes.NewReader(raw))
		r.Header.Set("Origin", origin)
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-Offline-Adapter", "1")
		r.Header.Set("X-Web-Principal-ID", p.Device.ID)
		r.Header.Set("X-Web-Generation", "1")
		if online {
			r.AddCookie(&http.Cookie{Name: "edu_web_dev", Value: web})
			r.Header.Set("X-CSRF-Token", webCSRF(web))
		}
		if offlineCookie != nil {
			r.AddCookie(offlineCookie)
			if !online {
				r.Header.Set("X-CSRF-Token", webCSRF(offlineCookie.Value))
			}
		}
		if alter != nil {
			alter(r)
		}
		w := httptest.NewRecorder()
		handler().ServeHTTP(w, r)
		return w
	}
	body := map[string]any{"adapter_version": 1, "save_consent": true}
	enabled = false
	if w := request("POST", "/v1/web/offline/enable", body, true, nil); w.Code != 403 {
		t.Fatalf("禁用 gate 仍签发身份：%d %s", w.Code, w.Body)
	}
	enabled = true
	if w := request("POST", "/v1/web/offline/enable", map[string]any{"adapter_version": 1, "save_consent": false}, true, nil); w.Code != 400 {
		t.Fatalf("没有明确保存授权：%d", w.Code)
	}
	w := request("POST", "/v1/web/offline/enable", body, true, nil)
	if w.Code != 201 {
		t.Fatalf("初始化失败：%d %s", w.Code, w.Body)
	}
	for _, c := range w.Result().Cookies() {
		if c.Name == "edu_offline_dev" {
			offlineCookie = c
		}
	}
	if offlineCookie == nil || !offlineCookie.HttpOnly || offlineCookie.SameSite != http.SameSiteStrictMode || offlineCookie.MaxAge < 36*86400 {
		t.Fatal("离线 Cookie 不满足固定期限与浏览器隔离")
	}
	for _, tc := range []struct {
		path, method string
		status       int
		alter        func(*http.Request)
	}{
		{"/v1/web/offline/session", "GET", 200, nil},
		{"/v1/web/session", "GET", 401, nil},
		{"/v1/web/offline/sync", "POST", 403, func(r *http.Request) { r.Header.Del("X-CSRF-Token") }},
		{"/v1/web/offline/session", "GET", 400, func(r *http.Request) { r.Header.Set("Authorization", "Bearer test") }},
		{"/v1/web/offline/session", "GET", 400, func(r *http.Request) { r.Header.Set("X-Offline-Adapter", "2") }},
		{"/v1/web/offline/session", "GET", 409, func(r *http.Request) { r.Header.Set("X-Web-Principal-ID", uuid.NewString()) }},
		{"/v1/web/offline/packs", "POST", 400, nil},
	} {
		t.Run(tc.method+tc.path+strconv.Itoa(tc.status), func(t *testing.T) {
			w := request(tc.method, tc.path, nil, false, tc.alter)
			if w.Code != tc.status {
				t.Fatalf("返回 %d，期待 %d：%s", w.Code, tc.status, w.Body)
			}
		})
	}
	if err := ids.LogoutWeb(ctx, web); err != nil {
		t.Fatal(err)
	}
	if w := request("GET", "/v1/web/offline/session", nil, false, nil); w.Code != 200 {
		t.Fatal("普通登录退出后原设备无法核对")
	}
	// 模拟原代次已关闭；身份恢复仅用于清除，不再提供签发或同步。
	if _, err := pool.Exec(ctx, `UPDATE identity_web_offline_sessions SET learner_generation=2`); err != nil {
		t.Fatal(err)
	}
	setGeneration := func(r *http.Request) { r.Header.Set("X-Web-Generation", "2") }
	if w := request("POST", "/v1/web/offline/sync", nil, false, setGeneration); w.Code != 409 {
		t.Fatalf("旧代次仍能同步：%d %s", w.Code, w.Body)
	}
	privacyService.purgeFound = true
	privacyService.purgeChallenge = privacy.OfflinePurgeChallenge{ErasureID: uuid.NewString(), DeviceID: p.Device.ID, OldGeneration: 2, CurrentGeneration: 3, ChallengeRevision: 1, Challenge: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", Status: privacy.OfflineDeviceChildPending}
	if w := request("GET", "/v1/web/offline/session", nil, false, setGeneration); w.Code != 200 || !bytes.Contains(w.Body.Bytes(), []byte(`"purge":{`)) {
		t.Fatalf("无法查询待清除任务：%d %s", w.Code, w.Body)
	}
	privacyService.childReceipt = privacy.OfflineDeviceChildReceipt{ErasureID: privacyService.purgeChallenge.ErasureID, DeviceID: p.Device.ID, Status: privacy.OfflineDeviceChildSucceeded}
	ack := map[string]any{"challenge_revision": 1, "challenge": privacyService.purgeChallenge.Challenge, "outcome": "succeeded", "managed_objects_absent": true}
	if w := request("POST", "/v1/web/offline/purge/"+privacyService.purgeChallenge.ErasureID+"/ack", ack, false, setGeneration); w.Code != 200 || privacyService.ackDeviceID != p.Device.ID {
		t.Fatalf("清除回执失败：%d %s", w.Code, w.Body)
	}
	if err := ids.RevokeDevice(ctx, p.Device.ID); err != nil {
		t.Fatal(err)
	}
	if w := request("GET", "/v1/web/offline/session", nil, false, setGeneration); w.Code != 401 {
		t.Fatalf("撤销后仍可恢复身份：%d", w.Code)
	}
}
