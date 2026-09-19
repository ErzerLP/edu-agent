package api_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// 与真实 Web 请求路径对照，直连 Go 的浏览器测试不能覆盖反向代理的白名单。
func TestWebReleaseProxyCoversClientAPI(t *testing.T) {
	raw, err := os.ReadFile("../../deploy/web/nginx.conf")
	if err != nil {
		t.Fatal(err)
	}
	var patterns []*regexp.Regexp
	for _, match := range regexp.MustCompile(`(?m)^\s*location ~ (.+) \{$`).FindAllStringSubmatch(string(raw), -1) {
		patterns = append(patterns, regexp.MustCompile(match[1]))
	}
	literal := regexp.MustCompile("['\"`](/v1/[^'\"`\\s]+)['\"`]")
	parameter := regexp.MustCompile(`\{[^}]+\}`)
	paths := map[string]bool{}
	err = filepath.WalkDir("../../clients/web/src", func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || strings.Contains(path, ".test.") || strings.HasSuffix(path, ".d.ts") || (filepath.Ext(path) != ".ts" && filepath.Ext(path) != ".tsx") {
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, match := range literal.FindAllStringSubmatch(string(body), -1) {
			paths[parameter.ReplaceAllString(match[1], "1")] = true
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) == 0 || len(patterns) == 0 {
		t.Fatal("未找到真实请求或代理规则，不能以空检查通过")
	}
	for path := range paths {
		t.Run(path, func(t *testing.T) {
			for _, pattern := range patterns {
				if pattern.MatchString(path) {
					return
				}
			}
			t.Error("Web 正式 API 在部署代理上返回 404")
		})
	}
}
