// webnotebook composes the real HTTP/knowledge/identity stores in a browser-only
// isolated schema. Only the fetcher's trusted network port is substituted.
// It is never built into edu-agentd and accepts no production schema.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"os/signal"
	"regexp"
	"syscall"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/identity"
	identitydb "github.com/edu-agent/edu-agent/server/internal/identity/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/knowledge"
	knowledgedb "github.com/edu-agent/edu-agent/server/internal/knowledge/postgresstore"
	spacedb "github.com/edu-agent/edu-agent/server/internal/learningspace/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/pdfsource"
	"github.com/edu-agent/edu-agent/server/internal/platform/health"
	"github.com/edu-agent/edu-agent/server/internal/privacy"
	privacydb "github.com/edu-agent/edu-agent/server/internal/privacy/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/research"
	"github.com/edu-agent/edu-agent/server/internal/settings"
	"github.com/edu-agent/edu-agent/server/internal/transport/httpapi"
	"github.com/edu-agent/edu-agent/server/internal/webassets"
	"github.com/jackc/pgx/v5/pgxpool"
)

func requireBrowserSchema(schema string) error {
	if !regexp.MustCompile(`^web_candidate_[0-9a-f]{32}$`).MatchString(schema) {
		return fmt.Errorf("来源夹具仅允许 webdatabase 的隔离 schema")
	}
	return nil
}
func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run() error {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	pool, err := pgxpool.New(ctx, os.Getenv("DATABASE_URL"))
	if err != nil {
		return err
	}
	defer pool.Close()
	var schema string
	if err := pool.QueryRow(ctx, `SELECT current_schema()`).Scan(&schema); err != nil {
		return err
	}
	if err := requireBrowserSchema(schema); err != nil {
		return err
	}
	if err := pdfsource.Prepare(ctx); err != nil {
		return err
	}
	ids, err := identity.NewService(identitydb.New(pool), identity.Options{PairingCodeTTL: time.Minute, PairingCodeMaxAttempts: 5, LastUsedTouchInterval: time.Minute})
	if err != nil {
		return err
	}
	ks, err := knowledge.NewService(knowledgedb.New(pool), knowledge.NewCanonicalizer(), knowledge.ServiceOptions{})
	if err != nil {
		return err
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/no-store" {
			w.Header().Set("Content-Type", "text/plain")
			w.Header().Set("Cache-Control", "no-store")
			fmt.Fprint(w, "不可保存的正文")
			return
		}
		if r.URL.Path == "/escape" {
			http.Redirect(w, r, "http://outside.example/body", http.StatusFound)
			return
		}
		if r.URL.Path == "/start" {
			http.Redirect(w, r, "/page", http.StatusFound)
			return
		}
		if r.URL.Path != "/page" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, `<!doctype html><html><head><title>Notebook 网页</title></head><body><main><h1>公开来源</h1><p>Notebook 网页片段：正文由真实抓取器经过重定向读取。</p></main></body></html>`)
	}))
	defer upstream.Close()
	configuration, err := settings.Open(settings.Options{Path: os.Getenv("LEARNING_SETTINGS_FILE")})
	if err != nil {
		return err
	}
	fixtureKey := "notebook-local-fixture"
	if _, err := configuration.Update(settings.Update{ExpectedRevision: configuration.View().Revision, Target: settings.Search, Connection: &settings.Connection{Enabled: true, Provider: "brave", Endpoint: "https://api.search.brave.com/res/v1/web/search", AuthMode: "bearer", APIStyle: "chat_completions"}, NewKey: &fixtureKey}); err != nil {
		return err
	}
	fetcher := research.NewFetcherWithNetwork(func(_ context.Context, _, host string) ([]netip.Addr, error) {
		if host != "notebook.example" {
			return nil, fmt.Errorf("fixture host denied")
		}
		return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
	}, func(ctx context.Context, network, address string) (net.Conn, error) {
		if address != "93.184.216.34:80" {
			return nil, fmt.Errorf("fixture address denied")
		}
		return (&net.Dialer{}).DialContext(ctx, network, upstream.Listener.Addr().String())
	})
	base, _ := url.Parse("http://127.0.0.1:32929")
	permits := privacy.NewReadPermitManager(privacy.WithResponseCommitGate(privacydb.NewResponseCommitGate(pool)))
	h, err := httpapi.New(httpapi.Options{Identity: ids, Knowledge: ks, LearningSpaces: spacedb.New(pool), ReferenceFetcher: fetcher, Settings: configuration, ReadPermits: permits,
		Readiness: health.New(health.Options{Database: pool}), Logger: slog.Default(), PairLimiter: httpapi.NewFixedWindowLimiter(1000, time.Minute), AuthLimiter: httpapi.NewFixedWindowLimiter(1000, time.Minute), DeviceLimiter: httpapi.NewFixedWindowLimiter(10000, time.Minute),
		WebUI: httpapi.WebUIOptions{Enabled: true, AllowLoopbackHTTP: true, PublicBaseURL: base, Identity: ids, Assets: webassets.Files()},
	})
	if err != nil {
		return err
	}
	server := &http.Server{Addr: "127.0.0.1:32929", Handler: h, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, done := context.WithTimeout(context.Background(), 5*time.Second)
		defer done()
		_ = server.Shutdown(shutdown)
	}()
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}
