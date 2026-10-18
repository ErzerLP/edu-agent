package settings

import (
	"net/http"
	"net/url"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/integrations/llm"
)

// StudioClient uses the explicitly configured mentor connection, including the
// teaching connection's API style when sharing is enabled. The legacy mentor
// client and CLI retain their existing protocol and behavior.
func (s *Service) StudioClient(wrap func(http.RoundTripper) http.RoundTripper) (*llm.Client, string, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.studioClient(wrap, nil)
}

// CitedMentorClient exposes the frozen connection only to server composition.
func (s *Service) CitedMentorClient(wrap func(Connection, http.RoundTripper) http.RoundTripper) (*llm.Client, string, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.studioClient(nil, wrap)
}

func (s *Service) studioClient(wrap func(http.RoundTripper) http.RoundTripper, observe func(Connection, http.RoundTripper) http.RoundTripper) (*llm.Client, string, bool, error) {
	slot := s.state.Mentor
	if s.state.MentorUsesTeaching {
		slot = s.state.Teaching
	}
	if !slot.Enabled || !configured(slot) {
		return nil, "", false, nil
	}
	var transport http.RoundTripper = modelTransport()
	if wrap != nil {
		transport = wrap(transport)
	}
	if observe != nil {
		transport = observe(slot.Connection, transport)
	}
	limits := s.state.Limits
	u, _ := url.Parse(slot.Endpoint)
	client, err := llm.New(llm.Options{BaseURL: u, WireAPI: llm.WireAPI(slot.APIStyle), Model: slot.Model, APIKey: slot.Key, AllowNoKey: slot.AuthMode == "none", ContextWindow: limits.ContextTokens, MinimumContext: 4096, Timeout: time.Duration(limits.IdleTimeoutSeconds) * time.Second, MaxOutputTokens: limits.OutputTokens, RequireCompletion: true, HTTPClient: &http.Client{Transport: transport, CheckRedirect: rejectRedirect}})
	return client, slot.Model, slot.APIStyle == string(llm.Responses), err
}
