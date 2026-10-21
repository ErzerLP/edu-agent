package mentorrun

import (
	"context"
	"net/url"
	"strings"

	"golang.org/x/net/publicsuffix"
)

const sourceResearchDomainLimit = 3

// Group sibling hosts so www/docs aliases cannot consume a round's diversity.
func sourceResearchDomain(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	host := strings.ToLower(u.Hostname())
	if domain, err := publicsuffix.EffectiveTLDPlusOne(host); err == nil {
		return domain
	}
	return host
}

// Re-evaluate the queue after every attempt: a newly rejected domain or one
// that reached its cap must not occupy the next reading slot.
func (h *executionHost) nextSourceResearchResource(ctx context.Context) (string, error) {
	state := h.body.SourceResearch
	if state.ResourceDomains == nil {
		state.ResourceDomains = map[string]string{}
	}
	best, bestReads := "", sourceResearchDomainLimit
	queue := state.Queue[:0]
	for _, id := range state.Queue {
		domain := state.ResourceDomains[id]
		if domain == "" { // Also supports checkpoints created before this policy.
			r, err := h.webResource(ctx, id)
			if err != nil {
				return "", err
			}
			domain = sourceResearchDomain(r.URL)
			state.ResourceDomains[id] = domain
		}
		if state.BlockedDomains[domain] || state.DomainReads[domain] >= sourceResearchDomainLimit {
			continue
		}
		queue = append(queue, id)
		if reads := state.DomainReads[domain]; reads < bestReads {
			best, bestReads = id, reads
		}
	}
	state.Queue = queue
	return best, nil
}

func (s *SourceResearchState) rejectDomain(requestURL, finalURL, reason string) {
	switch reason {
	case "http_401", "http_403", "http_429", "storage_restricted":
		if s.BlockedDomains == nil {
			s.BlockedDomains = map[string]bool{}
		}
		for _, raw := range []string{requestURL, finalURL} {
			if domain := sourceResearchDomain(raw); domain != "" {
				s.BlockedDomains[domain] = true
			}
		}
	}
}

func (s *SourceResearchState) trimQueues() {
	queued := len(s.Queue)
	for _, queue := range s.DeferredQueues {
		queued += len(queue)
	}
	excess := queued - max(0, 2*(s.Limits.MaxPagesPerRound-len(s.ReadURLs)))
	// Make room for the active direction before retaining older leftovers.
	for _, direction := range s.Directions {
		if excess <= 0 {
			break
		}
		queue := s.DeferredQueues[direction.ID]
		remove := min(excess, len(queue))
		if remove > 0 {
			s.DeferredQueues[direction.ID] = queue[:len(queue)-remove]
		}
		excess -= remove
	}
	if excess > 0 {
		s.Queue = s.Queue[:len(s.Queue)-min(excess, len(s.Queue))]
	}
	if queued > max(0, 2*(s.Limits.MaxPagesPerRound-len(s.ReadURLs))) {
		s.UnreadLeads = true
	}
}
