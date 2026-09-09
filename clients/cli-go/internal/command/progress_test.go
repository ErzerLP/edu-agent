package command

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/api"
)

// 旧客户端拼装多个投影的测试由此替代：服务端拥有分页快照，错误不能变成空事项。
func TestProgressUsesAuthoritativePagesAndPreservesFailures(t *testing.T) {
	for _, tc := range []struct {
		name, cursor, code string
		status             int
	}{
		{"首页", "", "", 200}, {"下一页", "page2", "", 200}, {"游标失效", "stale", "stale_cursor", 409}, {"投影不可用", "", "projection_unavailable", 503}, {"已清除", "", "content_redacted", 409}, {"网络失败", "", "internal_error", 500},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.URL.Path != "/v1/learning/progress" || r.URL.Query().Get("global") != "true" || r.URL.Query().Get("cursor") != tc.cursor || r.URL.Query().Get("limit") != "1" {
					t.Errorf("未使用权威范围分页：%s", r.URL)
				}
				if tc.status != 200 {
					writeJSONTest(w, tc.status, api.ErrorResponse{Error: api.ErrorBody{Code: tc.code, Message: tc.name, RequestID: "progress-test"}})
					return
				}
				page := api.ProgressPage{Metadata: commandSessionView("GoalReady", "", "", false, false).Metadata, Items: []api.GoalProgress{}, Total: 2, NextCursor: "page2"}
				writeJSONTest(w, 200, page)
			}))
			defer server.Close()
			store, credentials := pairedStores(server.URL, "token")
			app, out, errOut := newTestApp(store, credentials, &fakeTerminal{})
			exit := app.Run(t.Context(), []string{"progress", "--global", "--limit", "1", "--cursor", tc.cursor, "--json"})
			if tc.status == 200 {
				if exit != ExitOK || calls != 1 || !strings.Contains(out.String(), `"total":2`) || !strings.Contains(out.String(), `"next_cursor":"page2"`) {
					t.Fatalf("服务端结果丢失：%s %s", out, errOut)
				}
			} else {
				if exit == ExitOK || out.Len() != 0 || !strings.Contains(errOut.String(), tc.code) {
					t.Fatalf("查询失败伪装为空：%d %s %s", exit, out, errOut)
				}
			}
		})
	}
}
