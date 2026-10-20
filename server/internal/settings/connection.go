package settings

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"

	"github.com/edu-agent/edu-agent/server/internal/integrations/llm"
	"github.com/edu-agent/edu-agent/server/internal/modelusage"
)

type ModelProjection struct {
	Target                Target `json:"target"`
	DisplayName           string `json:"display_name"`
	ModelName             string `json:"model_name"`
	ConfigurationRevision string `json:"configuration_revision"`
	DestinationRevision   string `json:"destination_revision"`
	ContextTokens         int    `json:"context_tokens"`
	OutputTokens          int    `json:"output_tokens"`
}

// ConfigureGatewayRevisionKey is called once by the composition root before
// serving requests, using the shared deployment secret from PostgreSQL.
func (s *Service) ConfigureGatewayRevisionKey(key []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.revisionKey = append([]byte(nil), key...)
}

// FrozenConnection keeps secrets private and fixes the exact target and limits
// for one invocation; wrappers are applied at the real upstream transport.
type FrozenConnection struct {
	slot          secretSlot
	Limits        Limits
	projection    ModelProjection
	Model         modelusage.Model
	Configuration string
}

func (f FrozenConnection) Projection() ModelProjection { return f.projection }
func (FrozenConnection) String() string                { return "模型连接（秘密已隐藏）" }
func (f FrozenConnection) GoString() string            { return f.String() }
func (f FrozenConnection) Client(wrap func(http.RoundTripper) http.RoundTripper) (*llm.Client, error) {
	var transport http.RoundTripper = modelTransport()
	if wrap != nil {
		transport = wrap(transport)
	}
	return modelClient(f.slot, f.Limits, false, transport)
}

func connectionIdentity(slot secretSlot, limits Limits) (modelusage.Model, string) {
	destination := sha256.Sum256([]byte(slot.Provider + "\n" + slot.Endpoint))
	raw, _ := json.Marshal(struct {
		Connection Connection
		Key        string
		Limits     any
	}{slot.Connection, slot.Key, limits.configuration()})
	configuration := sha256.Sum256(raw)
	return modelusage.Model{Provider: slot.Provider, Destination: hex.EncodeToString(destination[:]), Model: slot.Model, APIStyle: slot.APIStyle}, hex.EncodeToString(configuration[:])
}
func (s *Service) freeze(target Target, slot secretSlot, limits Limits) FrozenConnection {
	model, configuration := connectionIdentity(slot, limits)
	revision := func(value any) string {
		raw, _ := json.Marshal(value)
		h := hmac.New(sha256.New, s.revisionKey)
		_, _ = h.Write(raw)
		return hex.EncodeToString(h.Sum(nil))
	}
	destination := revision([]string{string(target), slot.Provider, slot.Endpoint, slot.Model, slot.APIStyle})
	name := "教学模型"
	if target == Mentor {
		name = "AI 助手模型"
	}
	return FrozenConnection{slot: slot, Limits: limits, Model: model, Configuration: configuration, projection: ModelProjection{Target: target, DisplayName: name, ModelName: slot.Model, ConfigurationRevision: revision([]string{string(target), configuration}), DestinationRevision: destination, ContextTokens: limits.ContextTokens, OutputTokens: limits.OutputTokens}}
}
func (s *Service) ModelConnection(target Target) (FrozenConnection, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if target != Teaching && target != Mentor {
		return FrozenConnection{}, ErrInvalid
	}
	slot := *s.state.slot(target)
	if !slot.Enabled || !configured(slot) {
		return FrozenConnection{}, ErrUnavailable
	}
	return s.freeze(target, slot, s.state.Limits), nil
}
func (s *Service) GatewayModels() []ModelProjection {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []ModelProjection{}
	for _, target := range []Target{Teaching, Mentor} {
		slot := *s.state.slot(target)
		if slot.Enabled && configured(slot) {
			out = append(out, s.freeze(target, slot, s.state.Limits).Projection())
		}
	}
	return out
}
