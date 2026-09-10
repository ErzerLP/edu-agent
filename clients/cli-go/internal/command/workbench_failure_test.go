package command

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/api"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/workbench"
)

func TestWorkbenchReportsFirstFailingRead(t *testing.T) {
	for _, path := range []string{"/v1/learning-spaces/capabilities", "/v1/learning-spaces/" + api.DefaultLearningSpaceID, "/v1/learning/progress"} {
		t.Run(path, func(t *testing.T) {
			var calls []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls = append(calls, r.URL.Path)
				if r.URL.Path == path {
					w.Header().Set("X-Request-ID", "issue17-read")
					writeJSONTest(w, 200, map[string]any{"private-body-secret": "private-body-secret"})
					return
				}
				if !workbenchSpaceHTTP(w, r) {
					t.Errorf("失败后继续读取：%s", r.URL.Path)
				}
			}))
			defer server.Close()
			cfg, creds := pairedStores(server.URL, "private-token-secret")
			app, _, _ := newTestApp(cfg, creds, &fakeTerminal{})
			_, err := (workbenchService{app: *app}).Load(t.Context(), workbench.Request{Space: api.DefaultLearningSpaceID, Page: "overview"})
			if err == nil {
				t.Fatal("畸形响应被当作合法空页")
			}
			for _, text := range []string{"protocol_error", "path=" + path, "method=GET", "status=200", "request_id=issue17-read", "decode=unknown_field"} {
				if !strings.Contains(err.Error(), text) {
					t.Fatalf("诊断缺少 %s：%v", text, err)
				}
			}
			if strings.Contains(err.Error(), "private-") || calls[len(calls)-1] != path {
				t.Fatal("诊断泄密或未停在首个失败请求")
			}
		})
	}
}

func TestWorkbenchDistinguishesOldServerAndNetworkFailure(t *testing.T) {
	for _, tc := range []struct {
		name, path, code string
		status           int
		disconnect       bool
	}{
		{"旧服务无学习区", "/v1/learning-spaces/capabilities", "learning_spaces_unsupported", 404, false},
		{"旧服务无进度", "/v1/learning/progress", "progress_unavailable", 501, false},
		{"网络连接失败", "", "service_unavailable", 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == tc.path {
					writeJSONTest(w, tc.status, api.ErrorResponse{Error: api.ErrorBody{Code: tc.code, Message: "不可用", RequestID: "issue17-old"}})
					return
				}
				if !workbenchSpaceHTTP(w, r) {
					t.Error("意外读取业务数据")
				}
			}))
			defer server.Close()
			if tc.disconnect {
				server.Close()
			}
			cfg, creds := pairedStores(server.URL, "token")
			app, _, _ := newTestApp(cfg, creds, &fakeTerminal{})
			page, err := (workbenchService{app: *app}).Load(t.Context(), workbench.Request{Space: api.DefaultLearningSpaceID, Page: "overview"})
			if err == nil || !strings.Contains(err.Error(), "error["+tc.code+"]") || strings.Contains(page.Content, "没有符合条件") {
				t.Fatalf("故障类别丢失或伪装成空数据：%v", err)
			}
		})
	}
}
