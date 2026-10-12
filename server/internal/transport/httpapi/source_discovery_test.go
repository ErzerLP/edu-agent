package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/identity"
	"github.com/edu-agent/edu-agent/server/internal/integrations/websearch"
	"github.com/edu-agent/edu-agent/server/internal/privacy"
	"github.com/edu-agent/edu-agent/server/internal/research"
	"github.com/edu-agent/edu-agent/server/internal/settings"
)

func TestSourceDiscoveryAuthorization(t *testing.T) {
	for _, state := range []string{"missing_learning:write", "missing_knowledge:read", "missing_knowledge:write", "missing_research:adopt", "closed_owner", "unconfigured"} {
		t.Run(state, func(t *testing.T) {
			scopes := []string{"learning:write", "knowledge:read", "knowledge:write", "research:adopt"}
			if strings.HasPrefix(state, "missing_") {
				scopes = slices.DeleteFunc(scopes, func(scope string) bool { return scope == strings.TrimPrefix(state, "missing_") })
			}
			manager := privacy.NewReadPermitManager()
			if state == "closed_owner" {
				if err := manager.CloseAndDrain(t.Context(), 2, privacy.OwnerKnowledge); err != nil {
					t.Fatal(err)
				}
			}
			h, err := New(Options{Identity: &fakeIdentity{auth: identity.Credential{Device: identity.Device{ID: maintenanceDeviceID}, Scopes: scopes}}, Knowledge: &fakeKnowledge{}, ReadPermits: manager, Readiness: fakeReadiness{}, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), PairLimiter: NewFixedWindowLimiter(100, time.Minute), AuthLimiter: NewFixedWindowLimiter(100, time.Minute), DeviceLimiter: NewFixedWindowLimiter(100, time.Minute)})
			if err != nil {
				t.Fatal(err)
			}
			response := maintenanceRequest(h, "POST", "/v1/knowledge/source-discovery", `{"keywords":"公开关键词","external_consent":true,"policy":{"mode":"supplement","domains":[]}}`)
			want := 503
			if strings.HasPrefix(state, "missing_") {
				want = 403
			}
			if response.Code != want || strings.Contains(response.Body.String(), `"items"`) {
				t.Fatal(response.Code, response.Body.String())
			}
		})
	}
}

func TestSourceDiscoveryConcurrentAndDeviceLimits(t *testing.T) {
	for _, state := range []string{"concurrency", "device"} {
		t.Run(state, func(t *testing.T) {
			limits := settings.DefaultLimits()
			limits.Concurrency = 1
			limits.ResearchRequests = 1
			s, err := settings.Open(settings.Options{Limits: &limits})
			if err != nil {
				t.Fatal(err)
			}
			started, release := make(chan struct{}), make(chan struct{})
			factory := func(wrap func(http.RoundTripper) http.RoundTripper) (websearch.Adapter, string, error) {
				return websearch.NewBrave("https://example.org/search", "fixture", &http.Client{Transport: wrap(discoveryTestTransport(func(r *http.Request) (*http.Response, error) {
					close(started)
					select {
					case <-release:
					case <-r.Context().Done():
						return nil, r.Context().Err()
					}
					return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"web":{"results":[]}}`))}, nil
				}))}), "", nil
			}
			deviceLimit := 100
			if state == "device" {
				deviceLimit = 1
			}
			h, err := New(Options{Identity: &fakeIdentity{auth: identity.Credential{Device: identity.Device{ID: maintenanceDeviceID}, Scopes: []string{"learning:write", "knowledge:read", "knowledge:write", "research:adopt"}}}, Knowledge: &fakeKnowledge{}, Settings: s, SourceSearch: factory, Readiness: fakeReadiness{}, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), PairLimiter: NewFixedWindowLimiter(100, time.Minute), AuthLimiter: NewFixedWindowLimiter(100, time.Minute), DeviceLimiter: NewFixedWindowLimiter(deviceLimit, time.Minute)})
			if err != nil {
				t.Fatal(err)
			}
			body := `{"keywords":"public","external_consent":true,"policy":{"mode":"supplement","domains":[]}}`
			done := make(chan *httptest.ResponseRecorder, 1)
			go func() { done <- maintenanceRequest(h, "POST", "/v1/knowledge/source-discovery", body) }()
			select {
			case <-started:
			case <-time.After(5 * time.Second):
				close(release)
				t.Fatal("search did not start")
			}
			response := maintenanceRequest(h, "POST", "/v1/knowledge/source-discovery", body)
			close(release)
			first := <-done
			want := "source_discovery_busy"
			if state == "device" {
				want = "rate_limited"
			}
			if response.Code != 429 || !strings.Contains(response.Body.String(), want) || first.Code != 200 {
				t.Fatal(response.Code, response.Body.String(), first.Code)
			}
		})
	}
}

type discoveryTestTransport func(*http.Request) (*http.Response, error)

func (f discoveryTestTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestSourceDiscoveryHTTPUsesSettingsAndRejectsPrivateContext(t *testing.T) {
	s, err := settings.Open(settings.Options{Path: filepath.Join(t.TempDir(), "private", "settings.json")})
	if err != nil {
		t.Fatal(err)
	}
	key := "local-fixture-only"
	_, err = s.Update(settings.Update{ExpectedRevision: s.View().Revision, Target: settings.Search, Connection: &settings.Connection{Enabled: true, Provider: "brave", Endpoint: "https://api.search.brave.com/res/v1/web/search", AuthMode: "bearer", APIStyle: "chat_completions"}, NewKey: &key})
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Query().Get("q") != "public" || r.URL.Query().Get("count") != "4" {
			t.Error("unexpected query", r.URL.RawQuery)
		}
		fmt.Fprint(w, `{"web":{"results":[{"title":"Public","url":"https://example.org/","description":"SUMMARY_ONLY"}]}}`)
	}))
	defer upstream.Close()
	factory := func(wrap func(http.RoundTripper) http.RoundTripper) (websearch.Adapter, string, error) {
		return s.SearchClient(func(_ http.RoundTripper) http.RoundTripper {
			return wrap(discoveryTestTransport(func(r *http.Request) (*http.Response, error) {
				if r.URL.Scheme != "https" || r.URL.Host != "api.search.brave.com" || r.Header.Get("X-Subscription-Token") != key {
					t.Error("settings adapter boundary lost")
				}
				u := r.Clone(r.Context())
				u.URL.Scheme = "http"
				u.URL.Host = strings.TrimPrefix(upstream.URL, "http://")
				return http.DefaultTransport.RoundTrip(u)
			}))
		})
	}
	h, err := New(Options{Identity: &fakeIdentity{auth: identity.Credential{Device: identity.Device{ID: maintenanceDeviceID}, Scopes: []string{"learning:write", "knowledge:read", "knowledge:write", "research:adopt"}}}, Knowledge: &fakeKnowledge{}, Settings: s, SourceSearch: factory, Readiness: fakeReadiness{}, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), PairLimiter: NewFixedWindowLimiter(100, time.Minute), AuthLimiter: NewFixedWindowLimiter(100, time.Minute), DeviceLimiter: NewFixedWindowLimiter(100, time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{
		`{"keywords":"public","external_consent":false,"policy":{"mode":"supplement","domains":[]}}`,
		`{"keywords":"public","external_consent":true,"policy":{"mode":"supplement","domains":[]},"goal_id":"private"}`,
		`{"keywords":"public","keywords":"private","external_consent":true,"policy":{"mode":"supplement","domains":[]}}`,
		`{"keywords":"public","external_consent":true,"policy":{"mode":"restrict","domains":["127.0.0.1"]}}`,
	} {
		response := maintenanceRequest(h, "POST", "/v1/knowledge/source-discovery", body)
		if response.Code != 400 || calls != 0 {
			t.Fatal(response.Code, response.Body.String(), calls)
		}
	}
	response := maintenanceRequest(h, "POST", "/v1/knowledge/source-discovery", `{"keywords":"public","external_consent":true,"policy":{"mode":"supplement","domains":[]}}`)
	var result research.DiscoveryResult
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &result) != nil || len(result.Items) != 1 || result.Items[0].Summary != "SUMMARY_ONLY" || calls != 1 || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatal(response.Code, response.Body.String(), calls)
	}
	capability := maintenanceRequest(h, "GET", "/v1/knowledge/source-capabilities", "")
	if !strings.Contains(capability.Body.String(), `"available":true`) || calls != 1 {
		t.Fatal("capability made an external request", capability.Body.String(), calls)
	}
}

func TestDiscoveryFetchKeepsDomainRestrictionAcrossRedirects(t *testing.T) {
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		http.Redirect(w, r, "http://outside.example/body", 302)
	}))
	defer upstream.Close()
	fetcher := research.NewFetcherWithNetwork(func(context.Context, string, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
	}, func(ctx context.Context, network, address string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, upstream.Listener.Addr().String())
	})
	h, err := New(Options{Identity: &fakeIdentity{auth: identity.Credential{Device: identity.Device{ID: maintenanceDeviceID}, Scopes: []string{"references:manage", "knowledge:write"}}}, Knowledge: &fakeKnowledge{}, ReferenceFetcher: fetcher, Readiness: fakeReadiness{}, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), PairLimiter: NewFixedWindowLimiter(100, time.Minute), AuthLimiter: NewFixedWindowLimiter(100, time.Minute), DeviceLimiter: NewFixedWindowLimiter(100, time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	response := maintenanceRequest(h, "POST", "/v1/knowledge/reference-sources", `{"url":"http://example.org/start","external_consent":true,"policy":{"mode":"restrict","domains":["example.org"]}}`)
	if response.Code != 200 || !strings.Contains(response.Body.String(), `"failure":"network_policy_rejected"`) || strings.Contains(response.Body.String(), `"source_receipt"`) || calls != 1 {
		t.Fatal(response.Code, response.Body.String(), calls)
	}
}
