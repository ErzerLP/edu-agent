package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/identity"
	"github.com/edu-agent/edu-agent/server/internal/learningcontent"
	"github.com/edu-agent/edu-agent/server/internal/learningspace"
	"github.com/getkin/kin-openapi/openapi3"
	"github.com/go-chi/chi/v5"
)

type contentTestSpaces struct{ LearningSpaceService }

func (contentTestSpaces) Get(context.Context, string) (learningspace.Space, error) {
	return learningspace.Space{ID: learningspace.DefaultID, Status: "active"}, nil
}

func TestContentDeepLinkOnlyAcceptsIdentityAndVersion(t *testing.T) {
	router := chi.NewRouter()
	a := &API{}
	router.Get("/content/{artifactID}", a.contentEntry)
	for _, c := range []struct {
		query  string
		status int
	}{{"?space=" + learningspace.DefaultID + "&version=3", 307}, {"?text=私人正文", 400}, {"?version=-1", 400}, {"?space=错误身份", 400}} {
		r := httptest.NewRequest("GET", "/content/"+testAggregateID+c.query, nil)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, r)
		if w.Code != c.status {
			t.Fatalf("深链接返回 %d：%s", w.Code, w.Body.String())
		}
		if w.Code == 307 && w.Header().Get("Location") != "/app/content/"+testAggregateID+c.query {
			t.Fatalf("跳转丢失版本身份：%s", w.Header().Get("Location"))
		}
	}
}

func TestLearningContentNegotiationAndStrictBoundary(t *testing.T) {
	store, _ := learningcontent.New(nil, nil, nil)
	actor := &fakeIdentity{auth: identity.Credential{Device: identity.Device{ID: testAggregateID}, Scopes: []string{"learning:read", "learning:write"}}}
	handler, err := New(Options{Identity: actor, LearningSpaces: contentTestSpaces{}, LearningContent: store, Learning: &fakeLearning{}, Readiness: fakeReadiness{}, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), PairLimiter: NewFixedWindowLimiter(100, time.Minute), AuthLimiter: NewFixedWindowLimiter(100, time.Minute), DeviceLimiter: NewFixedWindowLimiter(100, time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	path := "/v1/learning/content/" + testAggregateID
	for _, c := range []struct {
		name, method, path, protocol, space, body string
		status                                    int
	}{
		{"能力先协商", "GET", "/v1/learning/content/capabilities", "", "", "", 200},
		{"不协商不读正文", "GET", path, "", learningspace.DefaultID, "", 422},
		{"未知协议明确升级", "GET", path, "2", learningspace.DefaultID, "", 422},
		{"新正文禁止隐式区", "GET", path, "1", "", "", 400},
		{"未配置密钥不能降级明文", "GET", path, "1", learningspace.DefaultID, "", 503},
		{"非法版本", "GET", path + "?version=0", "1", learningspace.DefaultID, "", 400},
		{"Studio 拒绝非法分页", "GET", "/v1/learning/content?limit=0", "1", learningspace.DefaultID, "", 400},
		{"来源禁止隐式版本", "GET", path + "/sources/" + testAggregateID, "1", learningspace.DefaultID, "", 400},
		{"导出禁止隐式版本", "GET", path + "/export?format=json", "1", learningspace.DefaultID, "", 400},
		{"恢复拒绝目标改写", "POST", path + "/restore", "1", learningspace.DefaultID, `{"goal":"改目标"}`, 400},
		{"引用拒绝模型提供授权", "POST", path + "/reuse", "1", learningspace.DefaultID, `{"approved":true}`, 400},
		{"重复版本", "GET", path + "?version=1&version=2", "1", learningspace.DefaultID, "", 400},
		{"未完成 JSON", "POST", path + "/revisions", "1", learningspace.DefaultID, `{"blocks":[`, 400},
		{"拒绝伪造评分规则", "POST", path + "/revisions", "1", learningspace.DefaultID, `{"rubric":{"answer":"伪造"}}`, 400},
		{"拒绝把讨论交作答案", "POST", path + "/answers", "1", learningspace.DefaultID, `{"action":"ask_free_question","content_version":1}`, 400},
		{"正文请求有字节上限", "POST", path + "/revisions", "1", learningspace.DefaultID, `{"padding":"` + strings.Repeat("x", (1<<20)+1) + `"}`, 413},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := httptest.NewRequest(c.method, c.path, strings.NewReader(c.body))
			r.Header.Set("Authorization", "Bearer test-content-token")
			if c.protocol != "" {
				r.Header.Set("X-Learning-Content-Version", c.protocol)
			}
			if c.space != "" {
				r.Header.Set(learningspace.Header, c.space)
			}
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			if w.Code != c.status {
				t.Fatalf("返回 %d，期望 %d：%s", w.Code, c.status, w.Body.String())
			}
			if c.protocol != "" && w.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("私人正文响应允许缓存")
			}
		})
	}
	actor.auth.Scopes = []string{"learning:read"}
	r := httptest.NewRequest("POST", path+"/revisions", strings.NewReader(`{}`))
	r.Header.Set("Authorization", "Bearer test-content-token")
	r.Header.Set("X-Learning-Content-Version", "1")
	r.Header.Set(learningspace.Header, learningspace.DefaultID)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("只读身份可提交正文：%d", w.Code)
	}
}

func TestContentCollaborationOpenAPI(t *testing.T) {
	doc, err := openapi3.NewLoader().LoadFromFile("../../../api/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	selection := learningcontent.Selection{SpaceID: testAggregateID, GoalID: testAggregateID, SessionID: testAggregateID, ArtifactID: testAggregateID, Version: 1, BlockID: testAggregateID, Start: 0, End: 6, Hash: learningcontent.TextHash("中文")}
	for name, value := range map[string]any{
		"ContentSelection":   selection,
		"ContentEditRequest": learningcontent.EditRequest{Selection: selection, Action: "example"},
		"ContentPreference":  learningcontent.Preference{},
		"ContentRestore":     learningcontent.Restore{OperationID: testAggregateID, ExpectedVersion: 2, Version: 1, Reason: "恢复原版"},
		"ContentLibraryPage": learningcontent.LibraryPage{Items: []learningcontent.LibraryItem{}},
		"ContentExport":      learningcontent.Export{Filename: "说明.md", MediaType: "text/markdown", Text: "正文"},
	} {
		raw, _ := json.Marshal(value)
		var payload any
		_ = json.Unmarshal(raw, &payload)
		if err = doc.Components.Schemas[name].Value.VisitJSON(payload, openapi3.EnableJSONSchema2020()); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	for _, path := range []string{"/v1/learning/content", "/v1/learning/content/{artifactID}/restore", "/v1/learning/content/{artifactID}/reuse", "/v1/learning/content/{artifactID}/preferences", "/v1/learning/content/{artifactID}/export", "/v1/learning/content/{artifactID}/sources/{referenceID}", "/v1/learning/goals/{goalID}/content-edits"} {
		if doc.Paths.Value(path) == nil {
			t.Fatal("公开合同缺少路径", path)
		}
	}
}
