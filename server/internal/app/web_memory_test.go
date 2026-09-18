package app

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/identity"
	"github.com/edu-agent/edu-agent/server/internal/memory"
	"github.com/edu-agent/edu-agent/server/internal/privacy"
	"github.com/edu-agent/edu-agent/server/internal/transport/httpapi"
	"github.com/google/uuid"
)

func TestPostgreSQLWebMemoryApprovalPrivacyAndRevocation(t *testing.T) {
	ctx := context.Background()
	pool := appIntegrationPool(t)
	stores := newApplicationStores(pool)
	bridge, err := composeMemoryBridge(pool, stores, bridgeTestConfig(t, false), memoryBridgeDependencies{})
	if err != nil {
		t.Fatal(err)
	}
	ids, err := identity.NewService(stores.identity, identity.Options{PairingCodeTTL: 5 * time.Minute, PairingCodeMaxAttempts: 5, LastUsedTouchInterval: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(nil)
	origin := "http://" + server.Listener.Addr().String()
	base, _ := url.Parse(origin)
	var logs bytes.Buffer
	handler, err := httpapi.New(httpapi.Options{Identity: ids, Memory: bridge.memoryService, MemoryExporter: bridge.memoryExporter, Privacy: bridge.privacyService, ReadPermits: bridge.readPermits,
		Readiness: crossTransportReadiness{}, Logger: slog.New(slog.NewTextHandler(&logs, nil)),
		PairLimiter: httpapi.NewFixedWindowLimiter(1000, time.Minute), AuthLimiter: httpapi.NewFixedWindowLimiter(1000, time.Minute), DeviceLimiter: httpapi.NewFixedWindowLimiter(1000, time.Minute),
		WebUI: httpapi.WebUIOptions{Enabled: true, AllowLoopbackHTTP: true, PublicBaseURL: base, Identity: ids, Assets: fstest.MapFS{"index.html": {Data: []byte(`<html><script type="module" src="/app/assets/main.js"></script></html>`)}, "assets/main.js": {Data: []byte(`export {}`)}}}})
	if err != nil {
		t.Fatal(err)
	}
	server.Config.Handler = handler
	server.Start()
	defer server.Close()
	var cookie *http.Cookie
	var csrf, device string
	request := func(method, path string, body any, grant string, want int) []byte {
		t.Helper()
		raw, _ := json.Marshal(body)
		r, _ := http.NewRequest(method, origin+path, bytes.NewReader(raw))
		r.Header.Set("Origin", origin)
		r.Header.Set("Content-Type", "application/json")
		if cookie != nil {
			r.AddCookie(cookie)
			r.Header.Set("X-CSRF-Token", csrf)
		}
		if grant != "" {
			r.Header.Set("X-Privacy-Erasure-Grant", grant)
		}
		res, err := server.Client().Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		data, _ := io.ReadAll(res.Body)
		if res.StatusCode != want {
			t.Fatalf("%s %s: %d，预期 %d：%s", method, path, res.StatusCode, want, data)
		}
		if method == "POST" && path == "/v1/web/pairings" {
			cookie = res.Cookies()[0]
			var session struct {
				CSRF   string          `json:"csrf_token"`
				Device identity.Device `json:"device"`
			}
			if err := json.Unmarshal(data, &session); err != nil {
				t.Fatal(err)
			}
			csrf, device = session.CSRF, session.Device.ID
		}
		return data
	}
	pair := func(profile identity.PairingProfile) {
		t.Helper()
		cookie = nil
		code, _, err := ids.CreatePairingCodeForProfile(ctx, profile)
		if err != nil {
			t.Fatal(err)
		}
		request("POST", "/v1/web/pairings", map[string]string{"code": code, "display_name": "记忆浏览器"}, "", 201)
	}
	pair(identity.PairingProfileUser)
	request("POST", "/v1/memory/candidates", map[string]any{}, "", 403)
	pair(identity.PairingProfileMemory)
	request("POST", "/admin/api/pairing-codes", map[string]any{}, "", 403)
	request("GET", "/v1/devices", nil, "", 200)
	create := func(content string) memory.Candidate {
		t.Helper()
		data := request("POST", "/v1/memory/candidates", map[string]any{"operation_id": uuid.NewString(), "payload_schema_version": 1, "content": content, "reason": "用户明确提出稳定偏好", "category": "interaction_preference", "sensitivity": "non_sensitive", "stability": "stable", "valid_until": time.Now().UTC().Add(time.Hour)}, "", 201)
		var result struct {
			Candidate struct {
				Candidate memory.Candidate `json:"candidate"`
			} `json:"candidate"`
			Record *memory.Record `json:"record"`
		}
		if err := json.Unmarshal(data, &result); err != nil {
			t.Fatal(err)
		}
		if result.Candidate.Candidate.Status != memory.CandidatePending || result.Record != nil {
			t.Fatal("Web 新建越过明确审批", string(data))
		}
		request("GET", "/v1/memory/candidates/"+result.Candidate.Candidate.ID, nil, "", 200)
		return result.Candidate.Candidate
	}
	content := "我长期偏好逐步解释-隐私测试正文"
	candidate := create(content)
	decision := map[string]any{"operation_id": uuid.NewString(), "payload_schema_version": 1, "expected_revision": candidate.Revision + 1, "decision": "admit", "reason": "明确批准当前版本"}
	path := "/v1/memory/candidates/" + candidate.ID + "/decisions"
	request("POST", path, decision, "", 409)
	decision["operation_id"], decision["expected_revision"] = uuid.NewString(), candidate.Revision
	data := request("POST", path, decision, "", 200)
	var saved struct {
		Record   memory.Record   `json:"record"`
		Delivery memory.Delivery `json:"delivery"`
	}
	if err = json.Unmarshal(data, &saved); err != nil {
		t.Fatal(err)
	}
	if saved.Record.ID == "" || saved.Delivery.PublicStatus != memory.DeliveryQueued {
		t.Fatal("没有真实排队回执", string(data))
	}
	data = request("GET", "/v1/memory/export", nil, "", 200)
	if strings.Contains(string(data), content) || !strings.Contains(string(data), `"degraded":true`) {
		t.Fatal("未交付却虚构导出正文", string(data))
	}
	rejected := create("另一条不会保存的偏好")
	request("POST", "/v1/memory/candidates/"+rejected.ID+"/decisions", map[string]any{"operation_id": uuid.NewString(), "payload_schema_version": 1, "expected_revision": rejected.Revision, "decision": "reject", "reason": "明确拒绝"}, "", 200)
	data = request("GET", "/v1/memory/candidates/"+rejected.ID, nil, "", 200)
	if strings.Contains(string(data), "另一条不会保存的偏好") {
		t.Fatal("拒绝后候选正文仍可读取")
	}
	// 第二个浏览器设备被撤销后，已有 Cookie 也不能再写入。
	managerCookie, managerCSRF, managerDevice := cookie, csrf, device
	pair(identity.PairingProfileMemory)
	revokedCookie, revokedCSRF, revokedDevice := cookie, csrf, device
	cookie, csrf, device = managerCookie, managerCSRF, managerDevice
	request("DELETE", "/v1/devices/"+revokedDevice, nil, "", 204)
	cookie, csrf = revokedCookie, revokedCSRF
	request("POST", "/v1/memory/candidates", map[string]any{}, "", 401)
	cookie, csrf = managerCookie, managerCSRF
	issued, err := bridge.privacyGrant.Issue(ctx, managerDevice, "本机测试操作者")
	if err != nil {
		t.Fatal(err)
	}
	operation := uuid.NewString()
	erase := map[string]any{"operation_id": operation, "payload_schema_version": 1, "expected_current_learner_generation": 1, "reason_code": "learner_request", "explicit_confirmation": true}
	request("POST", "/v1/privacy/erasures", erase, "", 403)
	data = request("POST", "/v1/privacy/erasures", erase, issued.Token, 202)
	var receipt privacy.ErasureReceipt
	if err = json.Unmarshal(data, &receipt); err != nil || receipt.ErasureID == "" {
		t.Fatal("清除未返回正式回执", err, string(data))
	}
	request("GET", "/v1/memory/candidates", nil, "", 401)
	pair(identity.PairingProfileMemory)
	data = request("GET", "/v1/privacy/operations/"+operation+"?device_id="+managerDevice, nil, "", 200)
	var recovered privacy.ErasureReceipt
	if err = json.Unmarshal(data, &recovered); err != nil || recovered.ErasureID != receipt.ErasureID {
		t.Fatal("重配对不能恢复原清除回执", err, string(data))
	}
	request("GET", "/v1/privacy/operations/"+operation+"?device_id="+uuid.NewString(), nil, "", 404)
	data = request("GET", "/v1/memory/candidates/"+candidate.ID, nil, "", 200)
	if strings.Contains(string(data), content) || !strings.Contains(string(data), `"content_status":"scrubbed"`) {
		t.Fatal("隐私清除后仍可恢复旧正文", string(data))
	}
	if strings.Contains(logs.String(), content) || strings.Contains(logs.String(), issued.Token) || strings.Contains(logs.String(), managerCookie.Value) {
		t.Fatal("日志泄漏正文或凭据")
	}
}
