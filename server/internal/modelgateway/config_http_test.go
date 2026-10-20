package modelgateway_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"testing"
)

func cliAssertError(t *testing.T, res *http.Response, raw []byte, status int, code string) {
	t.Helper()
	var envelope struct {
		Error struct{ Type, Code string } `json:"error"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatalf("错误 JSON：%v %s", err, raw)
	}
	if res.StatusCode != status || envelope.Error.Type != "gateway_error" || envelope.Error.Code != code {
		t.Fatalf("错误合同：%d %s，期望 %d/%s", res.StatusCode, raw, status, code)
	}
}

func TestGatewayConfigurationValidatesRefreshHeaders(t *testing.T) {
	f := newGatewayFixture(t, "chat_completions", func(http.ResponseWriter, *http.Request, int32) {
		t.Error("读取配置不应调用上游")
	})
	for _, tc := range []struct {
		name, version, generation, code string
		status                          int
	}{
		{name: "first_read", status: 200},
		{name: "refresh", version: "1", generation: fmt.Sprint(f.projection.PrivacyGeneration), status: 200},
		{name: "unsupported_version", version: "9", code: "gateway_version_unsupported", status: 426},
		{name: "stale_generation", version: "1", generation: fmt.Sprint(f.projection.PrivacyGeneration + 1), code: "privacy_generation_changed", status: 409},
		{name: "invalid_generation", version: "1", generation: "0", code: "invalid_request", status: 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := f.request(t, "GET", "/config", "", "")
			r.Header.Del("X-Model-Gateway-Version")
			r.Header.Del("X-Learning-Generation")
			if tc.version != "" {
				r.Header.Set("X-Model-Gateway-Version", tc.version)
			}
			if tc.generation != "" {
				r.Header.Set("X-Learning-Generation", tc.generation)
			}
			res := f.do(t, r)
			defer res.Body.Close()
			raw, err := io.ReadAll(res.Body)
			if err != nil {
				t.Fatal(err)
			}
			if tc.code == "" {
				if res.StatusCode != tc.status {
					t.Fatalf("配置读取：%d %s", res.StatusCode, raw)
				}
			} else {
				cliAssertError(t, res, raw, tc.status, tc.code)
			}
		})
	}
}
