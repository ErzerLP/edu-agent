package research

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/integrations/websearch"
)

func discoveryInput() DiscoveryRequest {
	return DiscoveryRequest{Keywords: "public algebra", ExternalConsent: true, Policy: Policy{Mode: "restrict", Domains: []string{"example.org"}}}
}

func TestDiscoveryUsesOnlyConfirmedQueryAndReturnsSummaries(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Query().Get("q") != "(site:example.org) (public algebra)" || r.URL.Query().Get("count") != "4" {
			t.Error("unexpected external payload", r.URL.RawQuery)
		}
		fmt.Fprint(w, `{"web":{"results":[{"title":"Public","url":"https://example.org/a#part","description":"SUMMARY ONLY"},{"title":"Duplicate","url":"https://example.org/a","description":"duplicate"},{"title":"Outside","url":"https://other.org/a","description":"outside"},{"title":"Private","url":"http://127.0.0.1/a","description":"private"}]}}`)
	}))
	defer upstream.Close()
	factory := func(wrap func(http.RoundTripper) http.RoundTripper) (websearch.Adapter, string, error) {
		return websearch.NewBrave(upstream.URL, "fixture", &http.Client{Transport: wrap(http.DefaultTransport)}), "fixture", nil
	}
	result, err := Discover(t.Context(), discoveryInput(), factory, 20)
	if err != nil || len(result.Items) != 1 || result.Items[0].URL != "https://example.org/a" || result.Items[0].Summary != "SUMMARY ONLY" || result.Items[0].Status != "summary_only" || result.RequestsUsed != 1 || result.CostEstimate != "unknown" {
		t.Fatal(result, err)
	}
	raw, _ := json.Marshal(result)
	for _, forbidden := range []string{`"text":`, `"fragments":`, `"source_receipt":`, `"import_markdown":`, `"revision_id":`} {
		if strings.Contains(string(raw), forbidden) {
			t.Fatal("summary exposed as body", string(raw))
		}
	}
	if calls.Load() != 1 {
		t.Fatal("unexpected fetch or retry", calls.Load())
	}
}

func TestDiscoveryRejectsBeforeExternalRequest(t *testing.T) {
	for _, state := range []string{"no_consent", "empty", "private_domain", "missing_domains", "too_long", "query_budget", "cancelled"} {
		t.Run(state, func(t *testing.T) {
			input, budget, ctx := discoveryInput(), 1, t.Context()
			switch state {
			case "no_consent":
				input.ExternalConsent = false
			case "empty":
				input.Keywords = " "
			case "private_domain":
				input.Policy.Domains = []string{"127.0.0.1"}
			case "missing_domains":
				input.Policy.Domains = nil
			case "too_long":
				input.Keywords = strings.Repeat("a", 301)
			case "query_budget":
				input.Policy.Domains = []string{strings.Repeat("a", 240) + ".org", strings.Repeat("b", 240) + ".org", strings.Repeat("c", 240) + ".org", strings.Repeat("d", 240) + ".org", strings.Repeat("e", 240) + ".org"}
			case "cancelled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			factory := func(func(http.RoundTripper) http.RoundTripper) (websearch.Adapter, string, error) {
				t.Fatal("constructed search before validation")
				return nil, "", nil
			}
			_, err := Discover(ctx, input, factory, budget)
			if err == nil {
				t.Fatal("accepted invalid discovery")
			}
		})
	}
}

func TestDiscoveryFailureAndCancellationDoNotRetry(t *testing.T) {
	for _, state := range []string{"rate_limited", "invalid_response", "cancelled", "timeout"} {
		t.Run(state, func(t *testing.T) {
			var calls atomic.Int32
			started := make(chan struct{})
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				close(started)
				switch state {
				case "rate_limited":
					w.WriteHeader(429)
				case "invalid_response":
					fmt.Fprint(w, `{"secret":"do not expose"}`)
				default:
					<-r.Context().Done()
				}
			}))
			defer upstream.Close()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if state == "cancelled" {
				go func() { <-started; cancel() }()
			}
			factory := func(wrap func(http.RoundTripper) http.RoundTripper) (websearch.Adapter, string, error) {
				client := &http.Client{Transport: wrap(http.DefaultTransport)}
				if state == "timeout" {
					client.Timeout = 30 * time.Millisecond
				}
				return websearch.NewBrave(upstream.URL, "fixture", client), "", nil
			}
			_, err := Discover(ctx, discoveryInput(), factory, 1)
			var failure *DiscoveryError
			if !errors.As(err, &failure) || failure.Reason != state || failure.RequestsUsed != 1 || calls.Load() != 1 || strings.Contains(err.Error(), "secret") {
				t.Fatal(err, calls.Load())
			}
		})
	}
}

type repeatedSearch struct{ transport http.RoundTripper }

func (s repeatedSearch) Search(ctx context.Context, _ websearch.Request) ([]websearch.Result, error) {
	for range 2 {
		r, _ := http.NewRequestWithContext(ctx, "GET", "https://example.org/search", nil)
		response, err := s.transport.RoundTrip(r)
		if err != nil {
			return nil, err
		}
		response.Body.Close()
	}
	return nil, nil
}

type discoveryRoundTrip func(*http.Request) (*http.Response, error)

func (f discoveryRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestDiscoveryTransportIgnoresLegacyBudget(t *testing.T) {
	calls := 0
	factory := func(wrap func(http.RoundTripper) http.RoundTripper) (websearch.Adapter, string, error) {
		return repeatedSearch{wrap(discoveryRoundTrip(func(r *http.Request) (*http.Response, error) {
			calls++
			return &http.Response{StatusCode: 200, Body: http.NoBody, Header: make(http.Header)}, nil
		}))}, "", nil
	}
	result, err := Discover(t.Context(), discoveryInput(), factory, 0)
	if err != nil || result.RequestsUsed != 2 || calls != 2 {
		t.Fatal(err, calls)
	}
}
