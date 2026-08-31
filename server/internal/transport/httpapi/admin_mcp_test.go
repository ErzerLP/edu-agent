package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/platform/health"
	"github.com/edu-agent/edu-agent/server/internal/transport/mcpadmin"
)

type fakeMCPManagement struct {
	snapshot      mcpadmin.Snapshot
	probe         mcpadmin.ProbeResult
	snapshotLimit int
	probeToken    string
	probeHost     string
	probeCalls    int
}

func (f *fakeMCPManagement) ServeHTTP(http.ResponseWriter, *http.Request) {}
func (f *fakeMCPManagement) Snapshot(limit int) mcpadmin.Snapshot {
	f.snapshotLimit = limit
	return f.snapshot
}
func (f *fakeMCPManagement) Probe(_ context.Context, token, host string) mcpadmin.ProbeResult {
	f.probeCalls++
	f.probeToken, f.probeHost = token, host
	return f.probe
}

func newAdminMCPTestAPI(t *testing.T, service mcpadmin.Service, writeLimit int) http.Handler {
	t.Helper()
	baseURL, err := url.Parse("http://127.0.0.1:8080")
	if err != nil {
		t.Fatal(err)
	}
	admin := &fakeAdminIdentity{fakeIdentity: &fakeIdentity{}}
	handler, err := New(Options{
		Identity: admin.fakeIdentity, MCP: service,
		Readiness:     fakeReadiness{report: health.Report{Status: health.StatusHealthy, Components: map[string]health.Component{}}},
		Logger:        slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil)),
		PairLimiter:   NewFixedWindowLimiter(100, time.Minute),
		AuthLimiter:   NewFixedWindowLimiter(100, time.Minute),
		DeviceLimiter: NewFixedWindowLimiter(100, time.Minute),
		AdminUI: AdminUIOptions{
			Enabled: true, Identity: admin, PublicBaseURL: baseURL, Token: adminTestToken,
			TrustedLoopbackProxy: true, AuthLimiter: NewFixedWindowLimiter(100, time.Minute),
			WriteLimiter: NewFixedWindowLimiter(writeLimit, time.Minute),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return handler
}

func TestAdminMCPReturnsLiveSnapshotConfigAndRedactedAudit(t *testing.T) {
	service := &fakeMCPManagement{snapshot: mcpadmin.Snapshot{
		ImplementationName: "edu-agent", ImplementationVersion: "mcp-surface-v1",
		Transport: "streamable_http", Stateless: true, JSONResponse: true,
		MaxRequestBodyBytes: 1 << 20, StaticResourceCount: 4, ResourceTemplateCount: 5,
		ResourceCount: 9, ToolCount: 15,
		Descriptors: []mcpadmin.Descriptor{{
			Kind: "tool", Name: "knowledge.retrieve", RequiredScope: "knowledge:read",
			PrivacyOwners: []string{"knowledge"}, ReadOnly: true, InputLimit: 256 << 10,
			OutputLimit: 16 << 20, AuditName: "knowledge_retrieve", HTTPOperationID: "retrieveKnowledge",
		}},
		RecentInvocations: []mcpadmin.Invocation{{
			CompletedAt: time.Date(2026, time.August, 31, 10, 0, 0, 0, time.UTC),
			RequestID:   "request-1", Descriptor: "knowledge_retrieve", DeviceID: "device-1",
			Result: "success", DurationMS: 8, Peer: "127.0.0.1",
		}},
	}}
	handler := newAdminMCPTestAPI(t, service, 10)
	session := loginAdmin(t, handler)

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, authenticatedAdminRequest(http.MethodGet, "/admin/api/mcp", "", session))
	if response.Code != http.StatusOK {
		t.Fatalf("MCP snapshot status/body = %d/%s", response.Code, response.Body.String())
	}
	if service.snapshotLimit != adminMCPRecentLimit {
		t.Fatalf("snapshot limit = %d, want %d", service.snapshotLimit, adminMCPRecentLimit)
	}
	body := response.Body.String()
	for _, expected := range []string{
		`"endpoint":"http://127.0.0.1:8080/mcp"`, `"pairing_exchange_url":"http://127.0.0.1:8080/v1/pairings/exchange"`,
		`"implementation_version":"mcp-surface-v1"`, `"resource_count":9`, `"tool_count":15`,
		`"Authorization":"Bearer \u003cDEVICE_TOKEN\u003e"`, `"descriptor":"knowledge_retrieve"`,
	} {
		if !strings.Contains(body, expected) {
			t.Fatalf("MCP snapshot omitted %q: %s", expected, body)
		}
	}
	if strings.Contains(body, adminTestToken) {
		t.Fatalf("MCP snapshot exposed management credential: %s", body)
	}
}

func TestAdminMCPProbeRequiresWriteProtectionsAndNeverEchoesToken(t *testing.T) {
	service := &fakeMCPManagement{probe: mcpadmin.ProbeResult{
		OK: true, HTTPStatus: http.StatusOK, RequestID: "probe-request", ToolCount: 15, DurationMS: 4,
	}}
	handler := newAdminMCPTestAPI(t, service, 1)
	session := loginAdmin(t, handler)
	body := `{"token":"valid-probe-device-token"}`

	missingOrigin := authenticatedAdminRequest(http.MethodPost, "/admin/api/mcp/probe", body, session)
	missingOrigin.Header.Del("Origin")
	missingOriginResponse := httptest.NewRecorder()
	handler.ServeHTTP(missingOriginResponse, missingOrigin)
	if missingOriginResponse.Code != http.StatusForbidden || service.probeCalls != 0 {
		t.Fatalf("missing-origin status/calls = %d/%d", missingOriginResponse.Code, service.probeCalls)
	}

	missingCSRF := authenticatedAdminRequest(http.MethodPost, "/admin/api/mcp/probe", body, session)
	missingCSRF.Header.Del(adminCSRFHeader)
	missingCSRFResponse := httptest.NewRecorder()
	handler.ServeHTTP(missingCSRFResponse, missingCSRF)
	if missingCSRFResponse.Code != http.StatusForbidden || service.probeCalls != 0 {
		t.Fatalf("missing-CSRF status/calls = %d/%d", missingCSRFResponse.Code, service.probeCalls)
	}

	accepted := httptest.NewRecorder()
	handler.ServeHTTP(accepted, authenticatedAdminRequest(http.MethodPost, "/admin/api/mcp/probe", body, session))
	if accepted.Code != http.StatusOK || service.probeCalls != 1 || service.probeToken != "valid-probe-device-token" || service.probeHost != "127.0.0.1:8080" {
		t.Fatalf("accepted probe status/calls/token/host/body = %d/%d/%q/%q/%s", accepted.Code, service.probeCalls, service.probeToken, service.probeHost, accepted.Body.String())
	}
	if strings.Contains(accepted.Body.String(), service.probeToken) {
		t.Fatalf("probe response echoed token: %s", accepted.Body.String())
	}

	limited := httptest.NewRecorder()
	handler.ServeHTTP(limited, authenticatedAdminRequest(http.MethodPost, "/admin/api/mcp/probe", body, session))
	if limited.Code != http.StatusTooManyRequests || service.probeCalls != 1 {
		t.Fatalf("limited probe status/calls/body = %d/%d/%s", limited.Code, service.probeCalls, limited.Body.String())
	}
}

func TestAdminUIMCPPageUsesLiveCatalogWithoutPersistingCredentials(t *testing.T) {
	page := string(adminPageHTML)
	for _, contract := range []string{
		`data-page="mcp"`, `id="page-mcp"`, `id="mcpConfig"`, `id="mcpPairingButton"`,
		`id="mcpProbeForm"`, `id="mcpProbeToken"`, `type="password"`, `autocomplete="off"`,
		`id="mcpCatalog"`, `id="mcpAudit"`,
	} {
		if !strings.Contains(page, contract) {
			t.Fatalf("MCP admin page missing %q", contract)
		}
	}
	script := string(adminScriptJS)
	for _, contract := range []string{
		`mcp: ["AGENT PROTOCOL", "MCP 管理"]`, `api("/admin/api/mcp")`,
		`api("/admin/api/mcp/probe"`, `body: JSON.stringify({ profile: "agent" })`,
		`body: JSON.stringify({ token })`, `input.value = ""`, `data.descriptors || []`,
		`descriptor.uri || descriptor.uri_template || descriptor.kind`, `data.recent_invocations || []`,
		`byId("mcpConfig").textContent`, `copyText(`,
	} {
		if !strings.Contains(script, contract) {
			t.Fatalf("MCP admin script missing %q", contract)
		}
	}
	for _, forbidden := range []string{"localStorage", "sessionStorage", "indexedDB", "knowledge.retrieve", "learning.create_goal"} {
		if strings.Contains(script, forbidden) {
			t.Fatalf("MCP admin script contains forbidden persisted credential or duplicate descriptor contract %q", forbidden)
		}
	}
	styles := string(adminStylesheet)
	for _, contract := range []string{`.mcp-grid`, `.mcp-catalog-list`, `.mcp-probe-facts`, `repeat(7, minmax(76px, 1fr))`} {
		if !strings.Contains(styles, contract) {
			t.Fatalf("MCP admin stylesheet missing %q", contract)
		}
	}
}

func TestAdminMCPProbeRejectsManagementCredential(t *testing.T) {
	service := &fakeMCPManagement{}
	handler := newAdminMCPTestAPI(t, service, 10)
	session := loginAdmin(t, handler)
	body, err := json.Marshal(adminMCPProbeRequest{Token: adminTestToken})
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, authenticatedAdminRequest(http.MethodPost, "/admin/api/mcp/probe", string(body), session))
	if response.Code != http.StatusBadRequest || service.probeCalls != 0 || strings.Contains(response.Body.String(), adminTestToken) {
		t.Fatalf("management-token probe status/calls/body = %d/%d/%s", response.Code, service.probeCalls, response.Body.String())
	}
}
