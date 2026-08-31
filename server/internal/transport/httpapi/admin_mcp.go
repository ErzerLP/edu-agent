package httpapi

import (
	"net/http"
	"strings"

	"github.com/edu-agent/edu-agent/server/internal/transport/mcpadmin"
)

const adminMCPRecentLimit = 50

type adminMCPProbeRequest struct {
	Token string `json:"token"`
}

func (a *API) adminMCP(w http.ResponseWriter, r *http.Request) {
	if a.mcp == nil {
		writeError(w, r, http.StatusServiceUnavailable, "mcp_unavailable", "MCP service is unavailable")
		return
	}
	endpoint := *a.adminUI.PublicBaseURL
	endpoint.Path = "/mcp"
	endpoint.RawPath, endpoint.RawQuery, endpoint.Fragment = "", "", ""
	snapshot := a.mcp.Snapshot(adminMCPRecentLimit)
	writeJSON(w, http.StatusOK, struct {
		Available          bool           `json:"available"`
		Endpoint           string         `json:"endpoint"`
		PairingExchangeURL string         `json:"pairing_exchange_url"`
		ClientConfig       map[string]any `json:"client_config"`
		mcpadmin.Snapshot
	}{
		Available:          true,
		Endpoint:           endpoint.String(),
		PairingExchangeURL: strings.TrimRight(a.adminUI.PublicBaseURL.String(), "/") + "/v1/pairings/exchange",
		ClientConfig: map[string]any{
			"mcpServers": map[string]any{
				"edu-agent": map[string]any{
					"type": "http", "url": endpoint.String(),
					"headers": map[string]string{"Authorization": "Bearer <DEVICE_TOKEN>"},
				},
			},
		},
		Snapshot: snapshot,
	})
}

func (a *API) adminMCPProbe(w http.ResponseWriter, r *http.Request) {
	if !a.adminUI.WriteLimiter.Allow("admin-write:" + clientIP(r)) {
		writeError(w, r, http.StatusTooManyRequests, "rate_limited", "Too many admin changes")
		return
	}
	if a.mcp == nil {
		writeError(w, r, http.StatusServiceUnavailable, "mcp_unavailable", "MCP service is unavailable")
		return
	}
	var request adminMCPProbeRequest
	if err := decodeJSON(w, r, 8<<10, &request); err != nil {
		writeError(w, r, http.StatusBadRequest, "invalid_request", "MCP probe credential is invalid")
		return
	}
	token := strings.TrimSpace(request.Token)
	if token == "" || len(token) > 4096 || sameAdminSecret(token, a.adminUI.Token) {
		writeError(w, r, http.StatusBadRequest, "invalid_request", "MCP probe credential is invalid")
		return
	}
	result := a.mcp.Probe(r.Context(), token, a.adminUI.PublicBaseURL.Host)
	writeJSON(w, http.StatusOK, result)
}
