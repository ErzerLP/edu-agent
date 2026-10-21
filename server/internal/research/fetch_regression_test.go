package research

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/pdffixture"
)

func TestFetchModernHTMLAndLinks(t *testing.T) {
	f, _ := fixtureFetcher(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		io.WriteString(w, `<!doctype html><html><head><meta charset=utf-8><title>版本说明</title><script>if (a < b) { document.write('<a href="/fake">fake</a>') }</script></head><body><header><a href="#" aria-label=Why&#32;Go>菜单</a></header><main><p>概率原文<br>最新版本<input disabled><p>已发布 &copy;<a href=/release>发行说明</a><a href=http://127.0.0.1/private>私网</a></main></body></html>`)
	})
	s, err := f.Fetch(t.Context(), Source{Locator: "http://example.com/doc"}, Policy{Mode: "supplement"}, func() error { return nil })
	if err != nil || s.Failure != "" || !strings.Contains(s.Text, "概率原文") || !strings.Contains(s.Text, "已发布 ©") || s.Title != "版本说明" {
		t.Fatalf("现代 HTML 正文丢失: %+v err=%v", s, err)
	}
	if strings.Contains(s.Text, "document.write") || strings.Contains(s.Text, "菜单") {
		t.Fatalf("隐藏内容进入正文: %s", s.Text)
	}
	if len(s.Links) != 2 || s.Links[1] != "http://example.com/release" {
		t.Fatalf("真实链接未恢复或脚本/私网链接混入: %v", s.Links)
	}
}

func TestFetchRealisticSizeAndCharset(t *testing.T) {
	for _, tc := range []struct{ name, media, body, want string }{
		{"3MiB网页", "text/html; charset=utf-8", "<html><head><style>" + strings.Repeat("x", 3<<20) + "</style></head><body><main>概率原文</main></body></html>", "概率原文"},
		{"Latin1文本", "text/plain; charset=iso-8859-1", "caf\xe9", "café"},
		{"HTML声明GBK", "text/html", "<meta charset=gbk><main>\xd6\xd0\xce\xc4</main>", "中文"},
		{"UTF8无声明", "text/html", "<main>概率原文</main>", "概率原文"},
		{"HTML编码声明优先", "text/html", "<meta charset=windows-1252><main>\xc2\xa3</main>", "Â£"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, _ := fixtureFetcher(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", tc.media)
				io.WriteString(w, tc.body)
			})
			s, e := f.Fetch(t.Context(), Source{Locator: "http://example.com"}, Policy{Mode: "supplement"}, func() error { return nil })
			if e != nil || s.Failure != "" || s.Text != tc.want {
				t.Fatalf("大小/字符集误拒: failure=%s text=%q err=%v", s.Failure, s.Text, e)
			}
		})
	}
}

func TestFetchDNSFailureClassification(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"lookup failed", &net.DNSError{IsNotFound: true}, "dns_failed"},
		{"lookup timeout", &net.DNSError{IsTimeout: true}, "timeout"},
		{"lookup cancelled", context.Canceled, "cancelled"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := NewFetcherWithNetwork(func(context.Context, string, string) ([]netip.Addr, error) { return nil, tc.err }, func(context.Context, string, string) (net.Conn, error) {
				t.Fatal("DNS failure dialed")
				return nil, nil
			})
			s, e := f.Fetch(t.Context(), Source{Locator: "http://example.com"}, Policy{Mode: "supplement"}, func() error { return nil })
			if e != nil || s.Failure != tc.want {
				t.Fatalf("DNS error misclassified: %s %v", s.Failure, e)
			}
		})
	}
}

func TestFetchNoindexIsNotStorageProhibition(t *testing.T) {
	for _, tc := range []struct{ name, cache, robots, body, want string }{
		{"响应noindex", "", "NOINDEX, follow", "<main>概率原文</main>", ""},
		{"页面noindex", "", "", "<meta name=robots content=noindex><main>概率原文</main>", ""},
		{"非指令子串", "x-no-store-report=yes", "x-noarchive-report", "<main>概率原文</main>", ""},
		{"缓存禁止保存", "public, no-store", "", "<main>概率原文</main>", "storage_restricted"},
		{"多头禁止保存", "", "noindex, noarchive", "<main>概率原文</main>", "storage_restricted"},
		{"机器人禁止归档", "", "edu-agent:noarchive", "<main>概率原文</main>", "storage_restricted"},
		{"页面禁止摘要", "", "", "<main>概率原文</main><meta name=robots content=nosnippet>", "storage_restricted"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, _ := fixtureFetcher(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/html")
				w.Header().Set("Cache-Control", tc.cache)
				w.Header().Set("X-Robots-Tag", tc.robots)
				io.WriteString(w, tc.body)
			})
			s, e := f.Fetch(t.Context(), Source{Locator: "http://example.com"}, Policy{Mode: "supplement"}, func() error { return nil })
			if e != nil || s.Failure != tc.want {
				t.Fatalf("错误留存判定: %+v %v", s, e)
			}
			if tc.want == "" && s.Text != "概率原文" {
				t.Fatalf("正文缺失: %+v", s)
			}
		})
	}
}

func TestFetchFailureReasons(t *testing.T) {
	for _, tc := range []struct {
		name                        string
		status                      int
		media, encoding, body, want string
	}{
		{"HTTP403", 403, "text/html", "", "blocked", "http_403"},
		{"HTTP404", 404, "text/html", "", "missing", "http_404"},
		{"HTTP429", 429, "text/html", "", "retry later", "http_429"},
		{"未知格式", 200, "image/png", "", "image", "unsupported_format"},
		{"空HTML", 200, "text/html", "", "<html><head><title>empty</title></head><body><script>app()</script></body></html>", "parse_empty"},
		{"损坏gzip", 200, "text/plain", "gzip", "broken", "invalid_compression"},
		{"未知字符集", 200, "text/plain; charset=made-up", "", "text", "unsupported_encoding"},
		{"损坏UTF8", 200, "text/plain; charset=utf-8", "", "\xff", "unsupported_encoding"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, _ := fixtureFetcher(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", tc.media)
				w.Header().Set("Content-Encoding", tc.encoding)
				w.WriteHeader(tc.status)
				io.WriteString(w, tc.body)
			})
			s, e := f.Fetch(t.Context(), Source{Locator: "http://example.com"}, Policy{Mode: "supplement"}, func() error { return nil })
			if e != nil || s.Failure != tc.want || len(s.Fragments) > 0 || s.StorageAllowed {
				t.Fatalf("错误失败类别: %+v %v", s, e)
			}
		})
	}
}

func TestFetchInterruptedBodyIsNotSizeLimit(t *testing.T) {
	for _, timeout := range []bool{false, true} {
		t.Run(fmt.Sprint(timeout), func(t *testing.T) {
			f, _ := fixtureFetcher(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/plain")
				w.Header().Set("Content-Length", "1000")
				io.WriteString(w, "short")
				w.(http.Flusher).Flush()
				if timeout {
					<-r.Context().Done()
				}
			})
			ctx := t.Context()
			cancel := func() {}
			want := "body_read_failed"
			if timeout {
				ctx, cancel = context.WithTimeout(ctx, 100*time.Millisecond)
				want = "timeout"
			}
			defer cancel()
			s, e := f.Fetch(ctx, Source{Locator: "http://example.com"}, Policy{Mode: "supplement"}, func() error { return nil })
			if e != nil || s.Failure != want {
				t.Fatalf("读取错误被误报: %s %v", s.Failure, e)
			}
		})
	}
}

func TestFetchCorruptGzipChecksumIsNotSizeLimit(t *testing.T) {
	var compressed bytes.Buffer
	z := gzip.NewWriter(&compressed)
	z.Write([]byte("正文"))
	z.Close()
	data := compressed.Bytes()
	data[len(data)-8] ^= 0xff
	f, _ := fixtureFetcher(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Header().Set("Content-Encoding", "gzip")
		w.Write(data)
	})
	s, e := f.Fetch(t.Context(), Source{Locator: "http://example.com"}, Policy{Mode: "supplement"}, func() error { return nil })
	if e != nil || s.Failure != "invalid_compression" {
		t.Fatalf("校验失败被误报超限: %s %v", s.Failure, e)
	}
}

func TestFetchPDFWithoutTextHasFailureReason(t *testing.T) {
	f, _ := fixtureFetcher(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/pdf")
		w.Write(pdffixture.Build(""))
	})
	s, e := f.Fetch(t.Context(), Source{Locator: "http://example.com/scan.pdf"}, Policy{Mode: "supplement"}, func() error { return nil })
	if e != nil || s.Failure != "pdf_no_text" || s.StorageAllowed || len(s.Fragments) > 0 {
		t.Fatalf("无文本PDF没有缺口: %+v %v", s, e)
	}
}
