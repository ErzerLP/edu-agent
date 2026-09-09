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

type jobHTTPFake struct{ *importHTTPFake }

func (s *jobHTTPFake) ImportJobs(context.Context, string, string) (knowledge.ImportJobPage, error) {
	return knowledge.ImportJobPage{Items: []knowledge.ImportJob{}}, nil
}
func (s *jobHTTPFake) RunImportJob(_ context.Context, actor string, c knowledge.ImportJobCommand) (knowledge.ImportJob, error) {
	s.calls++
	s.actor = actor
	return knowledge.ImportJob{ID: c.ID}, nil
}
func TestImportJobsHTTPAuthorityAndClosedInput(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		scopes     []string
		want       int
	}{
		{"确认缺少审批", `{"id":"10000000-0000-4000-8000-000000000001","action":"confirm","plan_version":1}`, []string{"knowledge:read", "knowledge:write"}, 403},
		{"继续缺少审批", `{"id":"10000000-0000-4000-8000-000000000001","action":"continue"}`, []string{"knowledge:read", "knowledge:write"}, 403},
		{"上传无需审批", `{"id":"10000000-0000-4000-8000-000000000001","action":"upload"}`, []string{"knowledge:read", "knowledge:write"}, 200},
		{"禁止伪造归属", `{"id":"10000000-0000-4000-8000-000000000001","action":"create","space_id":"forged"}`, []string{"knowledge:read", "knowledge:write"}, 400},
		{"拒绝重复动作", `{"id":"10000000-0000-4000-8000-000000000001","action":"upload","action":"continue"}`, []string{"knowledge:read", "knowledge:write", "knowledge:approve"}, 400},
		{"保留请求预算", `{"content_base64":"` + strings.Repeat("a", 1100) + `"}`, []string{"knowledge:read", "knowledge:write"}, 413},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &jobHTTPFake{&importHTTPFake{fakeKnowledge: &fakeKnowledge{}}}
			h, e := New(Options{Identity: &fakeIdentity{auth: identity.Credential{Device: identity.Device{ID: "90000000-0000-4000-8000-000000000001"}, Scopes: tc.scopes}}, Knowledge: s, Readiness: fakeReadiness{}, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), MaxKnowledgeRequestBody: 1024, PairLimiter: NewFixedWindowLimiter(100, time.Minute), AuthLimiter: NewFixedWindowLimiter(100, time.Minute), DeviceLimiter: NewFixedWindowLimiter(100, time.Minute)})
			if e != nil {
				t.Fatal(e)
			}
			r := httptest.NewRequest("POST", "/v1/knowledge/import-jobs", strings.NewReader(tc.body))
			r.Header.Set("Authorization", "Bearer test")
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tc.want {
				t.Fatalf("status=%d body=%s", w.Code, w.Body)
			}
			if tc.want != 200 && s.calls != 0 {
				t.Fatal("拒绝请求仍调用业务")
			}
			if tc.want == 200 && s.actor != "90000000-0000-4000-8000-000000000001" {
				t.Fatal("未注入认证设备")
			}
		})
	}
}
