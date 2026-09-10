package api

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestProtocolDiagnosticsAreSafeAndKeepStrictDecoding(t *testing.T) {
	for _, tc := range []struct{ name, body, category, field string }{
		{"未知字段", `{"private-body-secret":true}`, "unknown_field", ""},
		{"缺少字段", `{}`, "missing_field", "response.version"},
		{"字段为空", `{"version":null}`, "null_field", "response.version"},
		{"类型错误", `{"version":"private-body-secret"}`, "type_mismatch", ""},
		{"重复字段", `{"private-body-secret":1,"private-body-secret":2}`, "duplicate_field", ""},
		{"非法JSON", `{"version":`, "invalid_json", ""},
		{"多个JSON值", `{} {}`, "multiple_values", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json; charset=utf-8")
				w.Header().Set("X-Request-ID", "request-17")
				io.WriteString(w, tc.body)
			}))
			defer server.Close()
			client := NewClient(server.URL, "private-token-secret", time.Second, nil)
			var value LearningSpaceCapabilities
			err := client.doJSON(t.Context(), "GET", "/v1/learning/progress?cursor=private-query-secret", true, nil, map[int]bool{200: true}, true, &value)
			var protocol *ProtocolError
			if !errors.As(err, &protocol) || protocol.Category != "malformed_success_response" || protocol.DecodeCategory != tc.category || protocol.Field != tc.field {
				t.Fatalf("严格解码类别错误：%v", err)
			}
			if protocol.Method != "GET" || protocol.Path != "/v1/learning/progress" || protocol.Status != 200 || protocol.ContentType != "application/json" || protocol.RequestID != "request-17" {
				t.Fatalf("请求诊断不完整：%v", err)
			}
			if strings.Contains(err.Error(), "private-") || strings.Contains(err.Error(), server.URL) {
				t.Fatal("诊断泄漏响应正文、查询参数、端点或凭据")
			}
		})
	}
}

func TestProtocolDiagnosticsRejectUnsafeHeaderValues(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/private-secret")
		w.Header().Set("X-Request-ID", "private-secret\tvalue")
		io.WriteString(w, "private-secret")
	}))
	defer server.Close()
	_, err := NewClient(server.URL, "", time.Second, nil).LearningSpacesCapabilities(t.Context())
	var protocol *ProtocolError
	if !errors.As(err, &protocol) || protocol.Category != "unexpected_content_type" || protocol.ContentType != "other" || protocol.RequestID != "" || strings.Contains(err.Error(), "private-secret") {
		t.Fatalf("未拒绝或未清理异常响应头：%v", err)
	}
}

func TestProtocolPresenceHidesDynamicMapKeys(t *testing.T) {
	var value struct {
		Details map[string]LearningSpaceCapabilities `json:"details"`
	}
	err := decodeStrict([]byte(`{"details":{"private-body-secret":{}}}`), &value)
	category, field := classifyDecodeError(err)
	if category != "missing_field" || field != "response.details[*].version" || strings.Contains(err.Error(), "private-") {
		t.Fatalf("字段定位泄漏动态键：%v", err)
	}
}
