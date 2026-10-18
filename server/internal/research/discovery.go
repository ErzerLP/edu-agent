package research

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/integrations/websearch"
)

// SearchClient is the settings-owned factory. Trusted hosts may substitute its
// transport, but must preserve the cancellation and request accounting wrapper.
type SearchClient func(func(http.RoundTripper) http.RoundTripper) (websearch.Adapter, string, error)

type DiscoveryRequest struct {
	Keywords        string `json:"keywords"`
	ExternalConsent bool   `json:"external_consent"`
	Policy          Policy `json:"policy"`
}

func (r DiscoveryRequest) Validate() error {
	request := Request{Topic: r.Keywords, ExternalConsent: r.ExternalConsent, Policy: r.Policy}
	if request.Validate() != nil || r.Policy.Mode != "supplement" && r.Policy.Mode != "restrict" || r.Policy.Mode == "supplement" && len(r.Policy.Domains) != 0 || len(request.Query()) > 1024 {
		return ErrPolicy
	}
	return nil
}

// DiscoveryCandidate deliberately has no body, fragments, revision or receipt.
type DiscoveryCandidate struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Summary string `json:"summary"`
	Status  string `json:"status"`
}
type DiscoveryResult struct {
	Items         []DiscoveryCandidate `json:"items"`
	RequestBudget int                  `json:"request_budget"`
	RequestsUsed  int                  `json:"requests_used"`
	CostEstimate  string               `json:"cost_estimate"`
}
type DiscoveryError struct {
	Reason       string
	RequestsUsed int
}

func (e *DiscoveryError) Error() string { return "source_discovery_" + e.Reason }

type discoveryTransport struct {
	mu      sync.Mutex
	next    http.RoundTripper
	used    int
	failure error
}

func (t *discoveryTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	t.mu.Lock()
	if err := r.Context().Err(); err != nil {
		t.failure = err
		t.mu.Unlock()
		return nil, err
	}
	t.used++ // Reserve before sending; a failed/cancelled request may still cost money.
	t.mu.Unlock()
	return t.next.RoundTrip(r)
}

// Discover is synchronous and stateless: one provider request, no fetch, no
// retry, no model, and no saved task. Cancellation cannot undo external costs.
func Discover(ctx context.Context, input DiscoveryRequest, factory SearchClient, _ int) (DiscoveryResult, error) {
	result := DiscoveryResult{Items: []DiscoveryCandidate{}, RequestBudget: 1, CostEstimate: "unknown"}
	if input.Validate() != nil {
		return result, ErrPolicy
	}
	if ctx.Err() != nil {
		return result, &DiscoveryError{Reason: "cancelled"}
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	transport := &discoveryTransport{}
	adapter, _, err := factory(func(next http.RoundTripper) http.RoundTripper { transport.next = next; return transport })
	if err != nil {
		return result, &DiscoveryError{Reason: "unavailable"}
	}
	request := Request{Topic: input.Keywords, Policy: input.Policy}
	candidates, err := adapter.Search(ctx, websearch.Request{Query: request.Query(), Limit: MaxSources})
	transport.mu.Lock()
	result.RequestsUsed = transport.used
	reservedFailure := transport.failure
	transport.mu.Unlock()
	if reservedFailure != nil {
		err = reservedFailure
	}
	if ctx.Err() != nil {
		err = ctx.Err()
	}
	if err != nil {
		reason := websearch.Category(err)
		var failure *DiscoveryError
		if errors.As(err, &failure) {
			reason = failure.Reason
		}
		if errors.Is(err, context.Canceled) {
			reason = "cancelled"
		}
		return result, &DiscoveryError{Reason: reason, RequestsUsed: result.RequestsUsed}
	}
	seen := map[string]bool{}
	for _, candidate := range candidates {
		u, err := ValidateURL(candidate.URL)
		if err != nil || !input.Policy.Allows(candidate.URL) {
			continue
		}
		if seen[u.String()] {
			continue
		}
		seen[u.String()] = true
		result.Items = append(result.Items, DiscoveryCandidate{Title: candidate.Title, URL: u.String(), Summary: candidate.Description, Status: "summary_only"})
		if len(result.Items) == MaxSources {
			break
		}
	}
	return result, nil
}
