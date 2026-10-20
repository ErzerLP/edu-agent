package agentloop

import (
	"time"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/modelgateway"
)

// SetGatewayModel runs only at the user-turn boundary. Existing background jobs
// retain the model snapshot captured when they were scheduled.
func (s *Session) SetGatewayModel(model Model, gateway *modelgateway.Model) {
	s.model = model
	window := gateway.Selected.ContextTokens
	s.options.ContextWindow = window
	s.options.MaxTokens = gateway.Selected.OutputTokens
	s.options.ModelTimeout = time.Duration(gateway.Configuration.Limits.IdleTimeoutSeconds+5) * time.Second
	s.hotRawTokenLimit = clampInt(percentRoundUp(window, 55), 1024, window)
	s.currentToolResultBudget = clampInt(percentRoundUp(window, 8), 512, 2048)
	r := s.contextRuntime
	r.mu.Lock()
	defer r.mu.Unlock()
	r.model = model
	r.contextWindow = window
	r.observeAfterTokens = clampInt(percentRoundUp(window, 12), 2000, 8000)
	r.observerChunkTokens = clampInt(percentRoundUp(window, 20), 512, 8192)
	r.status.ContextWindow = window
}
