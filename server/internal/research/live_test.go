package research

import (
	"context"
	"os"
	"strings"
	"testing"
)

// 显式开启的公开网页 smoke 不使用模型或搜索密钥，也不代替提供商搜索验收。
func TestLivePublicPageSmoke(t *testing.T) {
	if os.Getenv("RESEARCH_LIVE_SMOKE") != "1" {
		t.Skip("未启用真实公共网页 smoke")
	}
	calls := 0
	source, err := NewFetcher().Fetch(context.Background(), Source{Locator: "https://www.rfc-editor.org/rfc/rfc9110.txt"}, Policy{Mode: "restrict", Domains: []string{"www.rfc-editor.org"}}, func() error { calls++; return nil })
	if err != nil || source.Failure != "" || !strings.Contains(source.Text, "HTTP Semantics") || len(source.Fragments) == 0 {
		t.Fatalf("真实正文核对失败：%s %v", source.Failure, err)
	}
	t.Logf("已核对 HTTP Semantics 原文：%s，指纹 %s，片段 %d，覆盖 %s，请求 %d", source.FinalURL, source.Fingerprint, len(source.Fragments), source.Coverage, calls)
}
