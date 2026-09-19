package httpapi

import (
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"
	"testing/fstest"
)

// 从实际前端路由取样，避免新增页面后只在客户端导航时正常、刷新却 404。
func TestWebReleasePageEntrypoints(t *testing.T) {
	raw, err := os.ReadFile("../../../../clients/web/src/main.tsx")
	if err != nil {
		t.Fatal(err)
	}
	paths := regexp.MustCompile(`\bpath: '([^']+)'`).FindAllStringSubmatch(string(raw), -1)
	if len(paths) == 0 {
		t.Fatal("没有发现前端页面，不能以空检查通过")
	}
	api := &API{webUI: WebUIOptions{Assets: fstest.MapFS{
		"index.html": {Data: []byte(`<script type="module" src="/app/assets/main.js"></script>`)},
	}}}
	parameter := regexp.MustCompile(`\$[A-Za-z]+`)
	for _, match := range paths {
		path := "/app" + parameter.ReplaceAllString(match[1], "00000000-0000-4000-8000-000000000001")
		t.Run(path, func(t *testing.T) {
			response := httptest.NewRecorder()
			api.webAsset(response, httptest.NewRequest("GET", path, nil))
			if response.Code != 200 || !strings.Contains(response.Body.String(), `type="module"`) {
				t.Fatalf("已注册页面直达失败：%d %s", response.Code, response.Body.String())
			}
			if response.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("页面不可复用旧资产入口")
			}
		})
	}
	for _, path := range []string{"/app/missing", "/app/progress.js", "/app/settings/missing", "/app/assets/missing.js", "/app/spaces/a/missing.css", "/app/api/missing", "/v1/missing", "/admin/missing", "/internal/missing"} {
		t.Run(path, func(t *testing.T) {
			response := httptest.NewRecorder()
			api.webAsset(response, httptest.NewRequest("GET", path, nil))
			if response.Code != 404 || strings.Contains(response.Body.String(), `type="module"`) {
				t.Fatalf("错误路径回退为 SPA：%d", response.Code)
			}
		})
	}
}
