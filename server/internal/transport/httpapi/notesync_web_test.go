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
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/identity"
	"github.com/edu-agent/edu-agent/server/internal/integrations/notesync"
	"github.com/edu-agent/edu-agent/server/internal/knowledge"
	"github.com/google/uuid"
)

type notesyncWebIdentity struct{ principal identity.WebPrincipal }

func (s notesyncWebIdentity) AuthenticateWeb(context.Context, string) (identity.WebPrincipal, error) {
	return s.principal, nil
}
func (s notesyncWebIdentity) ExchangeWebPairing(context.Context, string, string) (string, identity.WebPrincipal, error) {
	return "", s.principal, nil
}
func (s notesyncWebIdentity) LogoutWeb(context.Context, string) error { return nil }

type notesyncWebService struct{ fakeNotesyncReviewHTTP }

func (s *notesyncWebService) Operation(_ context.Context, device, operation string) (notesync.ResolutionResult, error) {
	if device != notesyncHTTPDeviceID || operation != "93000000-0000-4000-8000-000000000001" {
		return notesync.ResolutionResult{}, &notesync.ReviewError{Code: notesync.CodeReviewNotFound}
	}
	return notesync.ResolutionResult{ReviewID: notesyncHTTPReviewID, ResolutionKind: notesync.ResolutionKeepCanonical}, nil
}
func (s *notesyncWebService) PreviewResolution(_ context.Context, command notesync.ResolutionCommand) (knowledge.ImportPreview, error) {
	s.resolveCmd = command
	return knowledge.ImportPreview{Status: "ready", Diff: []knowledge.DocumentDiff{}}, nil
}

func TestNotesyncLearningCookiePermissionsAndOperationLookup(t *testing.T) {
	for _, scopes := range [][]string{{"knowledge:read"}, {"knowledge:read", "knowledge:write", "research:adopt"}, {"knowledge:read", "knowledge:write", "knowledge:approve", "imports:web"}} {
		u, _ := url.Parse("http://localhost")
		credential := identity.Credential{Device: identity.Device{ID: notesyncHTTPDeviceID}, Scopes: scopes}
		ids := notesyncWebIdentity{identity.WebPrincipal{Credential: credential, Device: credential.Device, Generation: 1}}
		service := &notesyncWebService{fakeNotesyncReviewHTTP: fakeNotesyncReviewHTTP{status: notesync.ReviewStatus{Configured: true}, reviewResult: notesyncHTTPReviewFixture()}}
		handler, err := New(Options{Identity: &fakeIdentity{auth: credential}, Notesync: service, Readiness: fakeReadiness{}, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), PairLimiter: NewFixedWindowLimiter(100, time.Minute), AuthLimiter: NewFixedWindowLimiter(100, time.Minute), DeviceLimiter: NewFixedWindowLimiter(100, time.Minute), WebUI: WebUIOptions{Enabled: true, AllowLoopbackHTTP: true, PublicBaseURL: u, Identity: ids, Assets: fstest.MapFS{"index.html": {Data: []byte(`<script type="module" src="/app/assets/main.js"></script>`)}, "assets/main.js": {Data: []byte(`export {}`)}}}})
		if err != nil {
			t.Fatal(err)
		}
		request := func(method, path string, csrf bool) *httptest.ResponseRecorder {
			body, _ := json.Marshal(map[string]string{"basis_hash": strings.Repeat("a", 64), "operation_id": uuid.NewString(), "kind": "accept_remote"})
			r := httptest.NewRequest(method, "http://localhost"+path, bytes.NewReader(body))
			r.AddCookie(&http.Cookie{Name: "edu_web_dev", Value: "test-cookie"})
			r.Header.Set("Origin", "http://localhost")
			if csrf {
				r.Header.Set("X-CSRF-Token", webCSRF("test-cookie"))
			}
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			return w
		}
		status := request("GET", "/v1/knowledge/notesync/status", false)
		if status.Code != 200 || !strings.Contains(status.Body.String(), `"configuration_source":"environment"`) || status.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("学习状态缺来源或缓存隔离：%s", status.Body)
		}
		base := "/v1/knowledge/notesync/reviews/" + notesyncHTTPReviewID
		if r := request("POST", base+"/resolution-previews", true); r.Code != 200 || service.resolveCmd.DeviceID != notesyncHTTPDeviceID || service.calls["resolve"] != 0 {
			t.Fatalf("学习预览未使用原设备或发生提交：%d", r.Code)
		}
		want := 403
		if contains(scopes, "knowledge:approve") {
			want = 200
		}
		if r := request("POST", base+"/resolutions", true); r.Code != want {
			t.Fatalf("浏览器同步审批权限错误：%d want %d", r.Code, want)
		}
		if r := request("POST", base+"/resolutions", false); r.Code != 403 {
			t.Fatal("缺少 CSRF 仍允许解决")
		}
		if r := request("GET", "/v1/knowledge/notesync/operations/93000000-0000-4000-8000-000000000001", false); r.Code != 200 {
			t.Fatal("原设备不能只读核对收据")
		}
		if r := request("GET", "/v1/knowledge/notesync/operations/"+uuid.NewString(), false); r.Code != 404 {
			t.Fatal("未知操作被认定成功")
		}
		if r := request("GET", "/admin/api/notesync", false); r.Code != 403 {
			t.Fatal("学习会话访问了管理入口")
		}
	}
}

type notesyncMappedKnowledge struct {
	fakeKnowledge
	linked bool
}

func (*notesyncMappedKnowledge) SupportsKnowledgeScopes() bool { return true }
func (s *notesyncMappedKnowledge) Collections(context.Context, bool) ([]knowledge.Collection, error) {
	if s.linked {
		return []knowledge.Collection{{ID: knowledge.DefaultCollectionID}}, nil
	}
	return nil, nil
}
func (*notesyncMappedKnowledge) ChangeCollection(context.Context, knowledge.CollectionCommand) (knowledge.Collection, error) {
	return knowledge.Collection{}, nil
}
func (*notesyncMappedKnowledge) FreezeScope(context.Context, knowledge.ScopeSnapshot) (knowledge.ScopeSnapshot, error) {
	return knowledge.ScopeSnapshot{}, nil
}
func (*notesyncMappedKnowledge) ReadScope(context.Context, string) (knowledge.ScopeSnapshot, error) {
	return knowledge.ScopeSnapshot{}, nil
}

func TestNotesyncMappingChecksBeforeRemoteStatusAndDetail(t *testing.T) {
	service := &fakeNotesyncReviewHTTP{status: notesync.ReviewStatus{Configured: true}}
	mapping := &notesyncMappedKnowledge{linked: true}
	api := &API{knowledge: mapping, notesync: service}
	for _, linked := range []bool{true, false} {
		mapping.linked = linked
		for _, collection := range []string{knowledge.DefaultCollectionID, uuid.NewString()} {
			ctx, _ := knowledge.WithCollection(context.Background(), collection)
			r := httptest.NewRequest(http.MethodGet, "/v1/knowledge/notesync/status", nil).WithContext(ctx)
			w := httptest.NewRecorder()
			before := service.calls["status"]
			api.notesyncStatus(w, r)
			allowed := linked && collection == knowledge.DefaultCollectionID
			if allowed && w.Code != 200 || !allowed && (w.Code != 404 || service.calls["status"] != before) {
				t.Fatalf("状态探测越过映射/引用：linked=%v collection=%s status=%d", linked, collection, w.Code)
			}
			if !allowed {
				w = httptest.NewRecorder()
				api.notesyncReview(w, r)
				if w.Code != 404 || service.calls["review"] != 0 {
					t.Fatal("解除引用后仍读取审阅正文")
				}
			}
		}
	}
}

func TestNotesyncExplicitCollectionHeaderReachesMappingGuard(t *testing.T) {
	service := &fakeNotesyncReviewHTTP{status: notesync.ReviewStatus{Configured: true}}
	handler := newNotesyncTestAPI(t, []string{"knowledge:read"}, service, nil, nil)
	for _, collection := range []string{knowledge.DefaultCollectionID, uuid.NewString()} {
		r := httptest.NewRequest(http.MethodGet, "/v1/knowledge/notesync/status", nil)
		r.Header.Set("Authorization", "Bearer test")
		r.Header.Set(knowledge.CollectionHeader, collection)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		want := 200
		if collection != knowledge.DefaultCollectionID {
			want = 404
		}
		if w.Code != want {
			t.Fatalf("集合边界 status=%d want=%d", w.Code, want)
		}
	}
}
