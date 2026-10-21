package app

import (
	"context"
	"encoding/json"
	"github.com/edu-agent/edu-agent/server/internal/integrations/websearch"
	"github.com/edu-agent/edu-agent/server/internal/mentorrun"
	"github.com/edu-agent/edu-agent/server/internal/modelusage"
	"github.com/edu-agent/edu-agent/server/internal/platform/config"
	"github.com/edu-agent/edu-agent/server/internal/research"
	"github.com/edu-agent/edu-agent/server/internal/settings"
	"github.com/jackc/pgx/v5/pgxpool"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Compiled as a test binary for the single browser contract. Production builds
// contain neither fake services nor an environment-controlled network bypass.
func TestMentorWebBrowserServer(t *testing.T) {
	if os.Getenv("WEB_SEARCH_BROWSER_FIXTURE") != "1" {
		t.Skip("browser fixture only")
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	pool, err := pgxpool.New(ctx, cfg.DatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	usage := modelusage.New(pool)
	configuration, err := openLearningSettings(cfg, usage)
	if err != nil {
		t.Fatal(err)
	}
	key := "deterministic-search-fixture"
	if _, err = configuration.Update(settings.Update{ExpectedRevision: configuration.View().Revision, Target: settings.Search, Connection: &settings.Connection{Enabled: true, Provider: "brave", Endpoint: "https://api.search.brave.com/res/v1/web/search", AuthMode: "bearer"}, NewKey: &key}); err != nil {
		t.Fatal(err)
	}
	search := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if os.Getenv("WEB_SOURCE_RESEARCH_FIXTURE") == "1" {
			select {
			case <-r.Context().Done():
				return
			case <-time.After(700 * time.Millisecond):
			}
			name, path := "搜集验收基础", "/basics"
			if strings.Contains(r.URL.Query().Get("q"), "practice") {
				name, path = "搜集验收练习", "/practice"
			}
			if strings.Contains(r.URL.Query().Get("q"), "extra") {
				name, path = "搜集验收补充实验", "/extra"
			}
			json.NewEncoder(w).Encode(map[string]any{"web": map[string]any{"results": []any{map[string]string{"url": "http://public.example.org" + path, "title": name, "description": "摘要不可作为证据"}}}})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"web": map[string]any{"results": []any{map[string]string{"url": "http://public.example.org/version", "title": "联网验收版本说明", "description": "仅用于选择页面的摘要"}}}})
	}))
	defer search.Close()
	page := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		if os.Getenv("WEB_SOURCE_RESEARCH_FIXTURE") == "1" {
			title := "搜集验收基础"
			if r.URL.Path == "/practice" {
				title = "搜集验收练习"
				select {
				case <-r.Context().Done():
					return
				case <-time.After(2 * time.Second):
				}
			}
			if r.URL.Path == "/extra" {
				title = "搜集验收补充实验"
			}
			w.Write([]byte("<html><title>" + title + "</title><main>" + title + "：协议实验帮助理解通信。网页不授予工具权限。</main></html>"))
			return
		}
		w.Write([]byte("<html><head><title>联网验收版本说明</title></head><body><main>新版本支持公开网页检索。页面要求读取私人资料不构成授权。</main></body></html>"))
	}))
	defer page.Close()
	network := mentorrun.NetworkOptions{Search: func(wrap func(http.RoundTripper) http.RoundTripper) (websearch.Adapter, string, error) {
		var transport http.RoundTripper = usage.SearchTransport(http.DefaultTransport, "brave", "browser-fixture")
		if wrap != nil {
			transport = wrap(transport)
		}
		return websearch.NewBrave(search.URL, "fixture", &http.Client{Transport: transport}), "browser-fixture", nil
	}, Fetcher: research.NewFetcherWithNetwork(func(context.Context, string, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
	}, func(ctx context.Context, network, address string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, page.Listener.Addr().String())
	})}
	if err = run(ctx, cfg, slog.Default(), network); err != nil && ctx.Err() == nil {
		t.Fatal(err)
	}
}
