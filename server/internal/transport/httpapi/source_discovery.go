package httpapi

import (
	"errors"
	"net/http"
	"slices"

	"github.com/edu-agent/edu-agent/server/internal/privacy"
	"github.com/edu-agent/edu-agent/server/internal/research"
	"github.com/go-chi/chi/v5"
)

var discoveryScopes = []string{"learning:write", "knowledge:read", "knowledge:write", "research:adopt"}

func (a *API) discoveryCapability(r *http.Request) map[string]any {
	reason := ""
	actor, _ := credentialFromContext(r.Context())
	for _, scope := range discoveryScopes {
		if !slices.Contains(actor.Scopes, scope) {
			reason = "missing_scope"
		}
	}
	if reason == "" && (a.settings == nil || !a.settings.View().Search.Enabled || !a.settings.View().Search.Configured) {
		reason = "search_not_configured"
	}
	return map[string]any{"available": reason == "", "reason": reason, "max_candidates": research.MaxSources, "request_budget": 1}
}

func (a *API) mountSourceDiscovery(r chi.Router) {
	r.With(a.requireScope("learning:write"), a.requireScope("knowledge:read"), a.requireScope("knowledge:write"), a.requireScope("research:adopt"), a.responseReadPermit("content_redacted", privacy.OwnerKnowledge)).Post("/v1/knowledge/source-discovery", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		var input research.DiscoveryRequest
		if err := decodeJSON(w, r, 4096, &input); err != nil {
			importInputFailure(w, r, err)
			return
		}
		if input.Validate() != nil {
			writeError(w, r, 400, "research_policy_rejected", "请明确确认公开关键词及域名策略；未发出搜索请求")
			return
		}
		if a.settings == nil {
			writeError(w, r, 503, "source_discovery_unavailable", "请配置 Brave 搜索；未发出搜索请求")
			return
		}
		limits := a.settings.View().Limits
		a.discoveryMu.Lock()
		if a.discoveryActive >= limits.Concurrency {
			a.discoveryMu.Unlock()
			writeError(w, r, 429, "source_discovery_busy", "搜索并发已达上限；未发出本次搜索请求")
			return
		}
		a.discoveryActive++
		a.discoveryMu.Unlock()
		defer func() { a.discoveryMu.Lock(); a.discoveryActive--; a.discoveryMu.Unlock() }()
		factory := a.sourceSearch
		if factory == nil {
			factory = a.settings.SearchClient
		}
		result, err := research.Discover(r.Context(), input, factory, limits.ResearchRequests)
		if err != nil {
			status, code := 502, "source_discovery_unavailable"
			var failure *research.DiscoveryError
			if errors.As(err, &failure) {
				code = failure.Error()
				switch failure.Reason {
				case "unavailable":
					status = 503
				case "budget_exceeded", "rate_limited":
					status = 429
				case "timeout":
					status = 504
				case "cancelled":
					status = 408
				}
			}
			writeError(w, r, status, code, "搜索未完成，未导入资料；已经发出的请求可能产生外部费用")
			return
		}
		writeJSON(w, 200, result)
	})
}
