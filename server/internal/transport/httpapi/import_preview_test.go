package httpapi

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/identity"
	"github.com/edu-agent/edu-agent/server/internal/knowledge"
)

type importHTTPFake struct {
	*fakeKnowledge
	calls int
	actor string
}

func (s *importHTTPFake) PreviewImport(_ context.Context, c knowledge.ImportCommand) (knowledge.ImportPreview, error) {
	s.calls++
	s.actor = c.ActorDeviceID
	return knowledge.ImportPreview{Status: "ready"}, nil
}
func (s *importHTTPFake) ConfirmImport(_ context.Context, c knowledge.ConfirmImportCommand) (knowledge.ImportResult, error) {
	s.calls++
	s.actor = c.Request.ActorDeviceID
	return knowledge.ImportResult{}, nil
}
func (s *importHTTPFake) ImportOperation(_ context.Context, id, actor string) (knowledge.ImportResult, error) {
	s.calls++
	s.actor = actor
	return knowledge.ImportResult{}, nil
}

func TestImportPreviewHTTPScopesClosedInputAndLimits(t *testing.T) {
	for _, tc := range []struct {
		name, path, body string
		scopes           []string
		want             int
	}{
		{"预览不需要审批", "previews", `{"operation_id":"10000000-0000-4000-8000-000000000001","expected_parent_revision_id":null,"source":"test","documents":[]}`, []string{"knowledge:read", "knowledge:write"}, 200},
		{"提交需要审批", "confirm", `{"request":{"expected_parent_revision_id":null},"receipt":"test"}`, []string{"knowledge:read", "knowledge:write"}, 403},
		{"禁止伪造设备", "previews", `{"expected_parent_revision_id":null,"actor_device_id":"forged"}`, []string{"knowledge:read", "knowledge:write"}, 400},
		{"拒绝隐含空父版本", "previews", `{"operation_id":"10000000-0000-4000-8000-000000000001","source":"test","documents":[]}`, []string{"knowledge:read", "knowledge:write"}, 400},
		{"拒绝重复字段", "previews", `{"expected_parent_revision_id":null,"source":"a","source":"b"}`, []string{"knowledge:read", "knowledge:write"}, 400},
		{"预算超限", "previews", `{"source":"` + strings.Repeat("x", 1100) + `"}`, []string{"knowledge:read", "knowledge:write"}, 413},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &importHTTPFake{fakeKnowledge: &fakeKnowledge{}}
			h, err := New(Options{Identity: &fakeIdentity{auth: identity.Credential{Device: identity.Device{ID: "90000000-0000-4000-8000-000000000001"}, Scopes: tc.scopes}}, Knowledge: s, Readiness: fakeReadiness{}, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), MaxKnowledgeRequestBody: 1024, PairLimiter: NewFixedWindowLimiter(100, time.Minute), AuthLimiter: NewFixedWindowLimiter(100, time.Minute), DeviceLimiter: NewFixedWindowLimiter(100, time.Minute)})
			if err != nil {
				t.Fatal(err)
			}
			r := httptest.NewRequest("POST", "/v1/knowledge/imports/"+tc.path, strings.NewReader(tc.body))
			r.Header.Set("Authorization", "Bearer test")
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tc.want {
				t.Fatalf("状态=%d 内容=%s", w.Code, w.Body)
			}
			if tc.want != 200 && s.calls != 0 {
				t.Fatal("无效请求调用业务服务")
			}
			if tc.want == 200 && s.actor != "90000000-0000-4000-8000-000000000001" {
				t.Fatal("设备身份未由认证注入")
			}
		})
	}
}
