package websearch

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestBraveDeterministicConnection(t *testing.T) {
	for _, tc := range []struct {
		name         string
		status       int
		body, reason string
	}{
		{"成功", 200, `{"web":{"results":[{"title":"Education","url":"https://example.org/education","description":"Public example"}]}}`, ""},
		{"鉴权失败", 401, "search-key-sentinel", "unauthorized"}, {"限流", 429, "secret", "rate_limited"}, {"无效响应", 200, `{}`, "invalid_response"}, {"超大响应", 200, strings.Repeat("x", (256<<10)+1), "invalid_response"}, {"重定向", 302, "", "redirect_rejected"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.URL.Path != "/res/v1/web/search" || r.URL.Query().Get("q") != "education" || r.URL.Query().Get("count") != "1" || r.Header.Get("X-Subscription-Token") != "search-key-sentinel" || strings.Contains(r.URL.String(), "search-key-sentinel") {
					t.Error("Brave 请求不符合供应商协议或凭据边界")
				}
				w.Header().Set("Location", "http://127.0.0.1:1/private")
				w.WriteHeader(tc.status)
				io.WriteString(w, tc.body)
			}))
			defer server.Close()
			client := server.Client()
			client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
			var adapter Adapter = NewBrave(server.URL+"/res/v1/web/search", "search-key-sentinel", client)
			results, err := adapter.Search(context.Background(), Request{Query: "education", Limit: 1})
			if calls != 1 {
				t.Fatal("搜索请求次数不符")
			}
			if tc.reason == "" {
				if err != nil || len(results) != 1 {
					t.Fatal("真实 HTTP 适配器未返回结果")
				}
			} else if err == nil || Category(err) != tc.reason || strings.Contains(err.Error(), "search-key-sentinel") {
				t.Fatalf("错误分类不符: %v", err)
			}
		})
	}
}

func TestBraveEmptyWebResults(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		valid      bool
	}{
		{"omitted_web", `{"type":"search","query":{"original":"education","more_results_available":false},"mixed":{"type":"mixed","main":[]}}`, true},
		{"empty_array", `{"web":{"results":[]}}`, true},
		{"missing_envelope", `{}`, false},
		{"incomplete_envelope", `{"type":"search"}`, false},
		{"wrong_type", `{"type":"error","query":{"original":"education"}}`, false},
		{"malformed_web", `{"type":"search","query":{"original":"education"},"web":{}}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				io.WriteString(w, tc.body)
			}))
			defer server.Close()
			results, err := NewBrave(server.URL, "fixture", server.Client()).Search(t.Context(), Request{Query: "education", Limit: 10})
			if tc.valid {
				if err != nil || len(results) != 0 {
					t.Fatalf("valid empty search stopped the caller: results=%v err=%v", results, err)
				}
			} else if err == nil || Category(err) != "invalid_response" {
				t.Fatalf("malformed response treated as empty search: %v", err)
			}
		})
	}
}
