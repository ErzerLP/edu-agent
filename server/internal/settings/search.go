package settings

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/integrations/websearch"
	"github.com/edu-agent/edu-agent/server/internal/modelusage"
	"github.com/google/uuid"
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
	if s.usage != nil {
		transport = s.usage.SearchTransport(transport, slot.Provider, hex.EncodeToString(hash[:]))
	}
	if wrap != nil {
		transport = wrap(transport)
	}
	return attributedSearch{websearch.NewBrave(slot.Endpoint, slot.Key, &http.Client{Transport: transport, Timeout: 20 * time.Second, CheckRedirect: rejectRedirect})}, hex.EncodeToString(hash[:]), nil
}

type attributedSearch struct{ websearch.Adapter }

func (a attributedSearch) Search(ctx context.Context, r websearch.Request) ([]websearch.Result, error) {
	ctx = modelusage.ForTask(ctx, modelusage.TaskContext{Kind: "source_discovery", OriginID: uuid.NewString(), Stage: "search_web"})
	return a.Adapter.Search(ctx, r)
}
