package research

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
)

func fixtureFetcher(t *testing.T, handler http.HandlerFunc) (*Fetcher, *int) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	calls := 0
	f := NewFetcher()
	f.lookup = func(context.Context, string, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
	}
	f.dial = func(ctx context.Context, network, address string) (net.Conn, error) {
		calls++
		if address != "93.184.216.34:80" {
			t.Errorf("未固定已验证地址：%s", address)
		}
		return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
	}
	return f, &calls
}

func TestFetchPublicBoundary(t *testing.T) {
	for _, raw := range []string{"file:///etc/passwd", "http://127.0.0.1", "http://169.254.169.254/latest", "http://10.1.2.3", "http://[::1]", "http://[::ffff:127.0.0.1]", "http://[64:ff9b::a00:1]", "http://[2002:7f00:1::]", "http://[2001:db8::1]", "http://example.com:8080", "https://example.com:80", "http://user:pass@example.com", "http://example.com./"} {
		if _, err := ValidateURL(raw); err == nil {
			t.Errorf("危险来源未拒绝：%s", raw)
		}
	}
	f, calls := fixtureFetcher(t, func(w http.ResponseWriter, r *http.Request) { t.Error("私网 DNS 不应连接") })
	f.lookup = func(context.Context, string, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("93.184.216.34"), netip.MustParseAddr("::ffff:127.0.0.1")}, nil
	}
	source, err := f.Fetch(context.Background(), Source{Locator: "http://example.com"}, Policy{Mode: "supplement"}, func() error { return nil })
	if err != nil || source.Failure != "network_policy_rejected" || *calls != 0 {
		t.Fatalf("混合 DNS 绕过：%+v %v", source, err)
	}
}

func TestFetchRedirectRebindingAndScope(t *testing.T) {
	f, calls := fixtureFetcher(t, func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "http://example.com/next", 302) })
	lookups := 0
	f.lookup = func(context.Context, string, string) ([]netip.Addr, error) {
		lookups++
		ip := "93.184.216.34"
		if lookups > 1 {
			ip = "127.0.0.1"
		}
		return []netip.Addr{netip.MustParseAddr(ip)}, nil
	}
	source, err := f.Fetch(context.Background(), Source{Locator: "http://example.com"}, Policy{Mode: "supplement"}, func() error { return nil })
	if err != nil || source.Failure != "network_policy_rejected" || *calls != 1 {
		t.Fatalf("DNS 重绑定未阻断：%+v %v", source, err)
	}
	f, calls = fixtureFetcher(t, func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "http://other.example/page", 302) })
	source, err = f.Fetch(context.Background(), Source{Locator: "http://example.com"}, Policy{Mode: "restrict", Domains: []string{"example.com"}}, func() error { return nil })
	if err != nil || source.Failure != "network_policy_rejected" || *calls != 1 {
		t.Fatal("跳转绕过限制域")
	}
}

func TestFetchParsingLimitsAndRestrictions(t *testing.T) {
	var compressed bytes.Buffer
	zip := gzip.NewWriter(&compressed)
	_, _ = zip.Write(bytes.Repeat([]byte("x"), MaxDecoded+1))
	_ = zip.Close()
	for _, tc := range []struct{ name, media, encoding, restriction, body, want string }{
		{"正文", "text/plain; charset=utf-8", "", "", "概率原文", ""},
		{"HTML", "text/html; charset=utf-8", "", "", "<html><head><title>隐藏标题</title></head><body><p>概率原文</p><script>秘密指令</script></body></html>", ""},
		{"损坏 PDF", "application/pdf", "", "", "%PDF", "pdf_invalid"},
		{"压缩炸弹", "text/plain", "gzip", "", compressed.String(), "decoded_limit"},
		{"正文超限", "text/plain", "", "", strings.Repeat("x", MaxWire+1), "body_limit"},
		{"禁止归档", "text/plain", "", "noarchive", "不能缓存", "storage_restricted"},
		{"页面限制", "text/html", "", "", `<html><head><meta name="robots" content="noindex"></head><body>不能索引</body></html>`, "storage_restricted"},
		{"普通正文提及限制", "text/html", "", "", `<p>概率原文中讨论 noarchive 并不是元数据指令。</p>`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, _ := fixtureFetcher(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Cookie") != "" || r.Header.Get("Authorization") != "" {
					t.Error("泄露身份")
				}
				w.Header().Set("Content-Type", tc.media)
				w.Header().Set("Content-Encoding", tc.encoding)
				w.Header().Set("X-Robots-Tag", tc.restriction)
				_, _ = io.WriteString(w, tc.body)
			})
			source, err := f.Fetch(context.Background(), Source{Locator: "http://example.com"}, Policy{Mode: "supplement"}, func() error { return nil })
			if err != nil || source.Failure != tc.want {
				t.Fatalf("实际失败类别：%s %v", source.Failure, err)
			}
			if tc.want == "" {
				if !strings.Contains(source.Text, "概率原文") || strings.Contains(source.Text, "秘密指令") || len(source.Fragments) == 0 || source.Fingerprint == "" {
					t.Fatalf("解析不可信：%+v", source)
				}
			} else if source.Text != "" || len(source.Fragments) > 0 {
				t.Fatal("失败项伪造原文")
			}
		})
	}
}

func TestCitationsAndPolicy(t *testing.T) {
	r := Request{Topic: "概率", ExternalConsent: true, Policy: Policy{Mode: "restrict", Domains: []string{"example.com"}}}
	if r.Validate() != nil || !strings.Contains(r.Query(), "site:example.com") || r.Policy.Allows("https://evil.example") {
		t.Fatal("限制未在搜索前执行")
	}
	source := Source{ID: "s", RevisionID: "r", Text: "真实原文", Fragments: []Fragment{{ID: "f", Start: 0, End: 12, Text: "真实原文"}}}
	s := Synthesis{Points: []Point{{Text: "综合结论", Citations: []Citation{{SourceID: "s", RevisionID: "r", FragmentID: "f", Quote: "原文"}}}}}
	if s.Validate([]Source{source}) != nil {
		t.Fatal("真实引用拒绝")
	}
	s.Points[0].Citations[0].Quote = "搜索摘要"
	if s.Validate([]Source{source}) == nil {
		t.Fatal("不存在片段被采纳")
	}
	s.Points[0].Citations[0] = Citation{SourceID: "https://fake.example", RevisionID: "r", FragmentID: "f", Quote: "原文"}
	if s.Validate([]Source{source}) == nil {
		t.Fatal("伪 URL 被当作来源身份")
	}
}

func TestFetchPrefersMainTextBeforeApplyingLimit(t *testing.T) {
	for _, wrapper := range []string{"main", "article", `div id="mw-content-text"`} {
		t.Run(wrapper, func(t *testing.T) {
			end := strings.Fields(wrapper)[0]
			body := `<html><body><div>` + strings.Repeat("navigation link ", 2000) + `</div><` + wrapper + `><nav>menu</nav><div class="toc">Contents only</div><p>Linear independence means only the trivial linear combination is zero.</p><footer>copyright</footer></` + end + `><aside>sidebar</aside></body></html>`
			f, _ := fixtureFetcher(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/html; charset=utf-8")
				_, _ = io.WriteString(w, body)
			})
			source, err := f.Fetch(t.Context(), Source{Locator: "http://example.com"}, Policy{Mode: "supplement"}, func() error { return nil })
			if err != nil || !strings.Contains(source.Text, "trivial linear combination") || len(source.Text) > 200 || source.Coverage != "partial_html_text_only" {
				t.Fatalf("main text displaced by navigation: bytes=%d coverage=%s error=%v", len(source.Text), source.Coverage, err)
			}
			for _, hidden := range []string{"navigation link", "menu", "Contents only", "copyright", "sidebar"} {
				if strings.Contains(source.Text, hidden) {
					t.Errorf("boilerplate retained: %q", hidden)
				}
			}
		})
	}
}

func TestHTMLContentSelectionStillHonorsLateStorageRestriction(t *testing.T) {
	text, coverage := parseHTML([]byte(`<html><body><main><p>valid body</p></main><meta name="robots" content="noarchive"></body></html>`))
	if text != "" || coverage != "storage_restricted" {
		t.Fatalf("late restriction bypassed: %q %s", text, coverage)
	}
}
