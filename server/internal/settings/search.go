package settings

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/integrations/websearch"
)

// SearchClient 只返回受搜索网络政策约束的适配器，不复用模型端点许可。
func (s *Service) SearchClient(wrap func(http.RoundTripper) http.RoundTripper) (websearch.Adapter, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	slot := s.state.Search
	if !slot.Enabled || !configured(slot) {
		return nil, "", ErrInvalid
	}
	raw, err := json.Marshal(struct {
		Connection Connection
		Key        string
		Policy     Policy
	}{slot.Connection, slot.Key, s.state.Policy})
	if err != nil {
		return nil, "", err
	}
	hash := sha256.Sum256(raw)
	var transport http.RoundTripper = searchTransport()
	if wrap != nil {
		transport = wrap(transport)
	}
	return websearch.NewBrave(slot.Endpoint, slot.Key, &http.Client{Transport: transport, Timeout: 20 * time.Second, CheckRedirect: rejectRedirect}), hex.EncodeToString(hash[:]), nil
}
