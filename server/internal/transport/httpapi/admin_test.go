package httpapi

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/identity"
	"github.com/edu-agent/edu-agent/server/internal/integrations/notesync"
	"github.com/edu-agent/edu-agent/server/internal/knowledge"
	"github.com/edu-agent/edu-agent/server/internal/memory"
	"github.com/edu-agent/edu-agent/server/internal/platform/config"
	"github.com/edu-agent/edu-agent/server/internal/platform/health"
)

var adminTestToken = base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{0x42}, 32))

type fakeAdminIdentity struct {
	*fakeIdentity
	pairingCode    string
	pairingExpiry  time.Time
	pairingProfile identity.PairingProfile
	pairingCalls   int
}

type adminTestSession struct {
	cookie *http.Cookie
	csrf   string
}

func (f *fakeAdminIdentity) CreatePairingCodeForProfile(_ context.Context, profile identity.PairingProfile) (string, time.Time, error) {
	f.pairingCalls++
	f.pairingProfile = profile
	return f.pairingCode, f.pairingExpiry, nil
}

func newAdminTestAPI(t *testing.T, admin *fakeAdminIdentity, authLimit, writeLimit int) http.Handler {
	t.Helper()
	return newAdminTestAPIWithOptions(t, admin, authLimit, writeLimit, true, time.Now)
}

func newAdminTestAPIWithTrustedProxy(t *testing.T, admin *fakeAdminIdentity, authLimit, writeLimit int, trustedProxy bool) http.Handler {
	t.Helper()
	return newAdminTestAPIWithOptions(t, admin, authLimit, writeLimit, trustedProxy, time.Now)
}

func newAdminTestAPIWithOptions(t *testing.T, admin *fakeAdminIdentity, authLimit, writeLimit int, trustedProxy bool, now func() time.Time) http.Handler {
	t.Helper()
	baseURL, err := url.Parse("http://127.0.0.1:8080")
	if err != nil {
		t.Fatal(err)
	}
	handler, err := New(Options{
		Identity: admin.fakeIdentity,
		Readiness: fakeReadiness{report: health.Report{
			Status: health.StatusDegraded,
			Components: map[string]health.Component{
				"postgresql": {Status: health.StatusHealthy},
				"model":      {Status: health.StatusDegraded, Reason: "not_configured"},
			},
		}},
		Logger:        slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil)),
		PairLimiter:   NewFixedWindowLimiter(100, time.Minute),
		AuthLimiter:   NewFixedWindowLimiter(100, time.Minute),
		DeviceLimiter: NewFixedWindowLimiter(100, time.Minute),
		Now:           now,
		AdminUI: AdminUIOptions{
			Enabled: true, Identity: admin, PublicBaseURL: baseURL, Token: adminTestToken,
			TrustedLoopbackProxy: trustedProxy,
			AuthLimiter:          NewFixedWindowLimiter(authLimit, time.Minute), WriteLimiter: NewFixedWindowLimiter(writeLimit, time.Minute),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return handler
}

func adminRequest(method, target, body, contentType string) *http.Request {
	request := httptest.NewRequest(method, target, strings.NewReader(body))
	request.Host = "127.0.0.1:8080"
	if method != http.MethodGet && method != http.MethodHead {
		request.Header.Set("Origin", "http://127.0.0.1:8080")
	}
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	return request
}

func adminLoginRequest(username, password string) *http.Request {
	form := url.Values{"username": {username}, "password": {password}}
	return adminRequest(http.MethodPost, "/admin/login", form.Encode(), "application/x-www-form-urlencoded")
}

func loginAdmin(t *testing.T, handler http.Handler) adminTestSession {
	t.Helper()
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, adminLoginRequest("admin", adminTestToken))
	if response.Code != http.StatusSeeOther || response.Header().Get("Location") != "/admin/" {
		t.Fatalf("login status/location = %d/%q body=%s", response.Code, response.Header().Get("Location"), response.Body.String())
	}
	var sessionCookie *http.Cookie
	for _, cookie := range response.Result().Cookies() {
		if cookie.Name == adminSessionCookieName {
			sessionCookie = cookie
			break
		}
	}
	if sessionCookie == nil {
		t.Fatal("login did not set the admin session cookie")
	}

	sessionRequest := adminRequest(http.MethodGet, "/admin/api/session", "", "")
	sessionRequest.AddCookie(sessionCookie)
	sessionResponse := httptest.NewRecorder()
	handler.ServeHTTP(sessionResponse, sessionRequest)
	if sessionResponse.Code != http.StatusOK {
		t.Fatalf("session status = %d body=%s", sessionResponse.Code, sessionResponse.Body.String())
	}
	var sessionBody struct {
		CSRF string `json:"csrf_token"`
	}
	if err := json.Unmarshal(sessionResponse.Body.Bytes(), &sessionBody); err != nil {
		t.Fatal(err)
	}
	if sessionBody.CSRF == "" {
		t.Fatal("session response omitted the CSRF token")
	}
	return adminTestSession{cookie: sessionCookie, csrf: sessionBody.CSRF}
}

func authenticatedAdminRequest(method, target, body string, session adminTestSession) *http.Request {
	contentType := ""
	if body != "" {
		contentType = "application/json"
	}
	request := adminRequest(method, target, body, contentType)
	request.AddCookie(session.cookie)
	if method != http.MethodGet && method != http.MethodHead {
		request.Header.Set(adminCSRFHeader, session.csrf)
	}
	return request
}

func TestAdminUIIsDisabledByDefault(t *testing.T) {
	handler := newTestAPI(t, &fakeIdentity{}, 100, 100, &bytes.Buffer{})
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/admin/", nil))
	if response.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", response.Code)
	}
}

func TestAdminUILoginReplacesBasicChallengeWithShortLivedSession(t *testing.T) {
	admin := &fakeAdminIdentity{fakeIdentity: &fakeIdentity{}}
	handler := newAdminTestAPI(t, admin, 2, 10)

	wrongHost := adminRequest(http.MethodGet, "/admin/login", "", "")
	wrongHost.Host = "server:8080"
	wrongHostResponse := httptest.NewRecorder()
	handler.ServeHTTP(wrongHostResponse, wrongHost)
	if wrongHostResponse.Code != http.StatusNotFound {
		t.Fatalf("wrong-host status = %d, want 404", wrongHostResponse.Code)
	}

	loginPage := httptest.NewRecorder()
	handler.ServeHTTP(loginPage, adminRequest(http.MethodGet, "/admin/login", "", ""))
	if loginPage.Code != http.StatusOK || !strings.Contains(loginPage.Body.String(), "登录管理控制台") {
		t.Fatalf("login page status/body = %d %q", loginPage.Code, loginPage.Body.String())
	}
	if loginPage.Header().Get("WWW-Authenticate") != "" {
		t.Fatalf("login page sent a Basic challenge: %q", loginPage.Header().Get("WWW-Authenticate"))
	}

	if !strings.Contains(loginPage.Body.String(), `autocomplete="off"`) || strings.Contains(loginPage.Body.String(), `autocomplete="current-password"`) {
		t.Fatalf("login page allows browser credential persistence: %q", loginPage.Body.String())
	}

	pageRedirect := httptest.NewRecorder()
	handler.ServeHTTP(pageRedirect, adminRequest(http.MethodGet, "/admin/", "", ""))
	if pageRedirect.Code != http.StatusSeeOther || pageRedirect.Header().Get("Location") != "/admin/login" {
		t.Fatalf("page redirect status/location = %d/%q", pageRedirect.Code, pageRedirect.Header().Get("Location"))
	}

	for attempt := 1; attempt <= 2; attempt++ {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, adminLoginRequest("admin", "wrong-token"))
		if response.Code != http.StatusUnauthorized || response.Header().Get("WWW-Authenticate") != "" {
			t.Fatalf("attempt %d status/challenge = %d/%q, want 401/empty", attempt, response.Code, response.Header().Get("WWW-Authenticate"))
		}
	}
	blockedCorrect := httptest.NewRecorder()
	handler.ServeHTTP(blockedCorrect, adminLoginRequest("admin", adminTestToken))
	if blockedCorrect.Code != http.StatusTooManyRequests || len(blockedCorrect.Result().Cookies()) != 0 {
		t.Fatalf("rate-limited correct credential status/cookies = %d/%v", blockedCorrect.Code, blockedCorrect.Result().Cookies())
	}

	handler = newAdminTestAPI(t, admin, 2, 10)
	session := loginAdmin(t, handler)
	if session.cookie.Path != "/admin" || !session.cookie.HttpOnly || session.cookie.Secure || session.cookie.SameSite != http.SameSiteStrictMode || session.cookie.MaxAge != int(adminSessionTTL/time.Second) {
		t.Fatalf("unexpected admin cookie: %+v", session.cookie)
	}
	page := httptest.NewRecorder()
	handler.ServeHTTP(page, authenticatedAdminRequest(http.MethodGet, "/admin/", "", session))
	if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), "服务总览") {
		t.Fatalf("admin page status/body = %d %q", page.Code, page.Body.String())
	}
	if page.Header().Get("Cache-Control") != "no-store" || page.Header().Get("Content-Security-Policy") == "" || page.Header().Get("X-Frame-Options") != "DENY" {
		t.Fatalf("missing admin security headers: %v", page.Header())
	}
}

func TestAdminUILoginRequiresExactOriginAndSessionExpires(t *testing.T) {
	now := time.Date(2026, time.August, 29, 9, 0, 0, 0, time.UTC)
	admin := &fakeAdminIdentity{fakeIdentity: &fakeIdentity{}}
	handler := newAdminTestAPIWithOptions(t, admin, 10, 10, true, func() time.Time { return now })

	missingOrigin := adminLoginRequest("admin", adminTestToken)
	missingOrigin.Header.Del("Origin")
	missingOriginResponse := httptest.NewRecorder()
	handler.ServeHTTP(missingOriginResponse, missingOrigin)
	if missingOriginResponse.Code != http.StatusForbidden {
		t.Fatalf("missing-origin login status = %d, want 403", missingOriginResponse.Code)
	}

	session := loginAdmin(t, handler)
	now = now.Add(adminSessionTTL + time.Second)

	apiResponse := httptest.NewRecorder()
	handler.ServeHTTP(apiResponse, authenticatedAdminRequest(http.MethodGet, "/admin/api/overview", "", session))
	if apiResponse.Code != http.StatusUnauthorized {
		t.Fatalf("expired API status = %d, want 401", apiResponse.Code)
	}
	pageResponse := httptest.NewRecorder()
	handler.ServeHTTP(pageResponse, authenticatedAdminRequest(http.MethodGet, "/admin/", "", session))
	if pageResponse.Code != http.StatusSeeOther || pageResponse.Header().Get("Location") != "/admin/login" {
		t.Fatalf("expired page status/location = %d/%q", pageResponse.Code, pageResponse.Header().Get("Location"))
	}
}

func TestAdminUIDirectModeRejectsNonLoopbackPeer(t *testing.T) {
	admin := &fakeAdminIdentity{fakeIdentity: &fakeIdentity{}}
	handler := newAdminTestAPIWithTrustedProxy(t, admin, 10, 10, false)

	remote := adminRequest(http.MethodGet, "/admin/login", "", "")
	remote.RemoteAddr = "192.0.2.10:41000"
	remoteResponse := httptest.NewRecorder()
	handler.ServeHTTP(remoteResponse, remote)
	if remoteResponse.Code != http.StatusNotFound {
		t.Fatalf("remote status = %d, want 404", remoteResponse.Code)
	}

	local := adminRequest(http.MethodGet, "/admin/login", "", "")
	local.RemoteAddr = "127.0.0.1:41000"
	localResponse := httptest.NewRecorder()
	handler.ServeHTTP(localResponse, local)
	if localResponse.Code != http.StatusOK {
		t.Fatalf("loopback status = %d, want 200", localResponse.Code)
	}
}

func TestAdminUIScriptUsesSessionCSRFAndServerErrorEnvelope(t *testing.T) {
	for _, expected := range []string{"body.error?.message", "X-Admin-CSRF", "/admin/api/session", "/admin/api/logout", "/admin/login"} {
		if !bytes.Contains(adminScriptJS, []byte(expected)) {
			t.Fatalf("admin UI script is missing %q", expected)
		}
	}
}

func TestAdminUIOverviewPairingRevocationAndLogout(t *testing.T) {
	expiresAt := time.Date(2026, time.August, 28, 12, 0, 0, 0, time.UTC)
	deviceID := "30000000-0000-4000-8000-000000000001"
	admin := &fakeAdminIdentity{
		fakeIdentity:  &fakeIdentity{devices: []identity.Device{{ID: deviceID, DisplayName: "Laptop", Scopes: []string{"devices:read"}}}},
		pairingCode:   "lookup.secret",
		pairingExpiry: expiresAt,
	}
	handler := newAdminTestAPI(t, admin, 10, 10)
	session := loginAdmin(t, handler)

	overviewResponse := httptest.NewRecorder()
	handler.ServeHTTP(overviewResponse, authenticatedAdminRequest(http.MethodGet, "/admin/api/overview", "", session))
	if overviewResponse.Code != http.StatusOK {
		t.Fatalf("overview status = %d body=%s", overviewResponse.Code, overviewResponse.Body.String())
	}
	var overview struct {
		ServerURL string            `json:"server_url"`
		Devices   []identity.Device `json:"devices"`
	}
	if err := json.Unmarshal(overviewResponse.Body.Bytes(), &overview); err != nil {
		t.Fatal(err)
	}
	if overview.ServerURL != "http://127.0.0.1:8080" || len(overview.Devices) != 1 {
		t.Fatalf("unexpected overview: %+v", overview)
	}

	missingOrigin := authenticatedAdminRequest(http.MethodPost, "/admin/api/pairing-codes", `{"profile":"user"}`, session)
	missingOrigin.Header.Del("Origin")
	missingOriginResponse := httptest.NewRecorder()
	handler.ServeHTTP(missingOriginResponse, missingOrigin)
	if missingOriginResponse.Code != http.StatusForbidden || admin.pairingCalls != 0 {
		t.Fatalf("missing-origin status/calls = %d/%d", missingOriginResponse.Code, admin.pairingCalls)
	}

	missingCSRF := authenticatedAdminRequest(http.MethodPost, "/admin/api/pairing-codes", `{"profile":"user"}`, session)
	missingCSRF.Header.Del(adminCSRFHeader)
	missingCSRFResponse := httptest.NewRecorder()
	handler.ServeHTTP(missingCSRFResponse, missingCSRF)
	if missingCSRFResponse.Code != http.StatusForbidden || admin.pairingCalls != 0 {
		t.Fatalf("missing-CSRF status/calls = %d/%d", missingCSRFResponse.Code, admin.pairingCalls)
	}

	otherSession := loginAdmin(t, handler)
	wrongCSRF := authenticatedAdminRequest(http.MethodPost, "/admin/api/pairing-codes", `{"profile":"user"}`, session)
	wrongCSRF.Header.Set(adminCSRFHeader, otherSession.csrf)
	wrongCSRFResponse := httptest.NewRecorder()
	handler.ServeHTTP(wrongCSRFResponse, wrongCSRF)
	if wrongCSRFResponse.Code != http.StatusForbidden || admin.pairingCalls != 0 {
		t.Fatalf("cross-session CSRF status/calls = %d/%d", wrongCSRFResponse.Code, admin.pairingCalls)
	}

	pairingResponse := httptest.NewRecorder()
	handler.ServeHTTP(pairingResponse, authenticatedAdminRequest(http.MethodPost, "/admin/api/pairing-codes", `{"profile":"agent"}`, session))
	if pairingResponse.Code != http.StatusCreated || admin.pairingProfile != identity.PairingProfileAgent || !strings.Contains(pairingResponse.Body.String(), "lookup.secret") {
		t.Fatalf("pairing status/profile/body = %d/%q/%s", pairingResponse.Code, admin.pairingProfile, pairingResponse.Body.String())
	}

	revokeResponse := httptest.NewRecorder()
	handler.ServeHTTP(revokeResponse, authenticatedAdminRequest(http.MethodPost, "/admin/api/devices/"+deviceID+"/revoke", `{}`, session))
	if revokeResponse.Code != http.StatusNoContent || admin.revoked != deviceID {
		t.Fatalf("revoke status/device = %d/%q", revokeResponse.Code, admin.revoked)
	}

	logoutResponse := httptest.NewRecorder()
	handler.ServeHTTP(logoutResponse, authenticatedAdminRequest(http.MethodPost, "/admin/api/logout", `{}`, session))
	if logoutResponse.Code != http.StatusNoContent {
		t.Fatalf("logout status = %d, want 204", logoutResponse.Code)
	}
	var cleared bool
	for _, cookie := range logoutResponse.Result().Cookies() {
		if cookie.Name == adminSessionCookieName && cookie.MaxAge < 0 {
			cleared = true
		}
	}
	if !cleared {
		t.Fatal("logout did not clear the admin session cookie")
	}

	afterLogout := httptest.NewRecorder()
	handler.ServeHTTP(afterLogout, authenticatedAdminRequest(http.MethodGet, "/admin/api/overview", "", session))
	if afterLogout.Code != http.StatusUnauthorized {
		t.Fatalf("post-logout status = %d, want 401", afterLogout.Code)
	}
}

func TestAdminSessionStoreBoundsActiveSessions(t *testing.T) {
	now := time.Date(2026, time.August, 29, 9, 0, 0, 0, time.UTC)
	store := newAdminSessionStore(func() time.Time { return now })
	var firstToken string
	for index := 0; index <= adminSessionLimit; index++ {
		token, _, err := store.create()
		if err != nil {
			t.Fatal(err)
		}
		if index == 0 {
			firstToken = token
		}
		now = now.Add(time.Millisecond)
	}
	if len(store.sessions) != adminSessionLimit {
		t.Fatalf("session count = %d, want %d", len(store.sessions), adminSessionLimit)
	}
	if _, ok := store.lookup(firstToken); ok {
		t.Fatal("oldest session remained valid after the session limit was reached")
	}
}

func newAdminResourceTestAPI(t *testing.T, settingsPath string, limits ...int) (http.Handler, *fakeMemoryExporter, *fakeKnowledge, *fakeNotesyncReviewHTTP) {
	t.Helper()
	writeLimit := 100
	if len(limits) > 0 {
		writeLimit = limits[0]
	}
	scanPageSize := 100
	if len(limits) > 1 {
		scanPageSize = limits[1]
	}
	baseURL, err := url.Parse("http://127.0.0.1:8080")
	if err != nil {
		t.Fatal(err)
	}
	notesURL, err := url.Parse("https://notes.example.test")
	if err != nil {
		t.Fatal(err)
	}

	admin := &fakeAdminIdentity{fakeIdentity: &fakeIdentity{}}
	memoryService := &fakeMemoryHTTP{}
	memoryExporter := &fakeMemoryExporter{}
	memoryExporter.page.Items = []memory.ExportItem{
		{
			Record:         memory.Record{ID: "record-1", LogicalMemoryID: "memory-1", Revision: 1, Status: memory.RecordApplied},
			DeliveryStatus: memory.DeliveryApplied,
			ContentStatus:  memory.ExportContentAvailable,
			Content:        "prefers focused sessions",
		},
	}
	knowledgeService := &fakeKnowledge{}
	knowledgeService.head = &knowledge.KnowledgeRevision{ID: "revision-1", RevisionNo: 3}
	knowledgeService.tree = knowledge.TreeResult{Revision: knowledge.KnowledgeRevision{ID: "revision-1"}}
	knowledgeService.export = knowledge.ExportResult{
		RevisionID: "revision-1",
		Documents:  []knowledge.ExportDocument{{Path: "learning.md", Markdown: "# Learning map"}},
	}
	notesyncService := &fakeNotesyncReviewHTTP{}
	notesyncService.status = notesync.ReviewStatus{Configured: true, Compatible: true, Version: "1", Vault: "learning"}
	notesyncService.previewResult.Items = []notesync.PreviewItem{{Category: "local_changed", RemotePath: "learning.md"}}
	notesyncService.listResult.Items = []notesync.ReviewSummary{}

	notesyncConfig := config.NotesyncConfig{
		Enabled:        true,
		BaseURL:        notesURL,
		APIToken:       strings.Repeat("n", 32),
		Vault:          "learning",
		PathPrefix:     "edu-agent",
		HTTPTimeout:    10 * time.Second,
		WorkerInterval: 30 * time.Second,
		WorkerBatch:    20,
		ScanPageSize:   scanPageSize,
		ScanMaxPages:   100,
	}
	adminOptions := AdminUIOptions{
		Enabled:              true,
		Identity:             admin,
		PublicBaseURL:        baseURL,
		Token:                adminTestToken,
		TrustedLoopbackProxy: true,
		SettingsFile:         settingsPath,
		Notesync:             notesyncConfig,
		AuthLimiter:          NewFixedWindowLimiter(100, time.Minute),
		WriteLimiter:         NewFixedWindowLimiter(writeLimit, time.Minute),
	}
	options := Options{
		Identity:       admin.fakeIdentity,
		Knowledge:      knowledgeService,
		Notesync:       notesyncService,
		Memory:         memoryService,
		MemoryExporter: memoryExporter,
		Readiness:      fakeReadiness{report: health.Report{Status: health.StatusHealthy, Components: map[string]health.Component{}}},
		Logger:         slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil)),
		PairLimiter:    NewFixedWindowLimiter(100, time.Minute),
		AuthLimiter:    NewFixedWindowLimiter(100, time.Minute),
		DeviceLimiter:  NewFixedWindowLimiter(100, time.Minute),
		AdminUI:        adminOptions,
	}
	handler, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	return handler, memoryExporter, knowledgeService, notesyncService
}

func TestAdminNotesyncPreviewRequiresWriteProtections(t *testing.T) {
	handler, _, _, notesyncService := newAdminResourceTestAPI(t, "", 1)
	session := loginAdmin(t, handler)
	otherSession := loginAdmin(t, handler)
	body := `{"path":"learning.md","page":1,"page_size":25}`

	missingOrigin := authenticatedAdminRequest(http.MethodPost, "/admin/api/notesync/preview", body, session)
	missingOrigin.Header.Del("Origin")
	missingOriginResponse := httptest.NewRecorder()
	handler.ServeHTTP(missingOriginResponse, missingOrigin)
	if missingOriginResponse.Code != http.StatusForbidden || notesyncService.calls["preview"] != 0 {
		t.Fatalf("missing-origin preview status/calls/body = %d/%d/%s", missingOriginResponse.Code, notesyncService.calls["preview"], missingOriginResponse.Body.String())
	}

	missingCSRF := authenticatedAdminRequest(http.MethodPost, "/admin/api/notesync/preview", body, session)
	missingCSRF.Header.Del(adminCSRFHeader)
	missingCSRFResponse := httptest.NewRecorder()
	handler.ServeHTTP(missingCSRFResponse, missingCSRF)
	if missingCSRFResponse.Code != http.StatusForbidden || notesyncService.calls["preview"] != 0 {
		t.Fatalf("missing-CSRF preview status/calls/body = %d/%d/%s", missingCSRFResponse.Code, notesyncService.calls["preview"], missingCSRFResponse.Body.String())
	}

	wrongCSRF := authenticatedAdminRequest(http.MethodPost, "/admin/api/notesync/preview", body, session)
	wrongCSRF.Header.Set(adminCSRFHeader, otherSession.csrf)
	wrongCSRFResponse := httptest.NewRecorder()
	handler.ServeHTTP(wrongCSRFResponse, wrongCSRF)
	if wrongCSRFResponse.Code != http.StatusForbidden || notesyncService.calls["preview"] != 0 {
		t.Fatalf("wrong-CSRF preview status/calls/body = %d/%d/%s", wrongCSRFResponse.Code, notesyncService.calls["preview"], wrongCSRFResponse.Body.String())
	}

	accepted := httptest.NewRecorder()
	handler.ServeHTTP(accepted, authenticatedAdminRequest(http.MethodPost, "/admin/api/notesync/preview", body, session))
	if accepted.Code != http.StatusOK || notesyncService.calls["preview"] != 1 || notesyncService.previewCmd.PageSize != notesync.MaxPreviewPageSize {
		t.Fatalf("accepted preview status/calls/command/body = %d/%d/%+v/%s", accepted.Code, notesyncService.calls["preview"], notesyncService.previewCmd, accepted.Body.String())
	}

	limited := httptest.NewRecorder()
	handler.ServeHTTP(limited, authenticatedAdminRequest(http.MethodPost, "/admin/api/notesync/preview", body, session))
	if limited.Code != http.StatusTooManyRequests || notesyncService.calls["preview"] != 1 {
		t.Fatalf("limited preview status/calls/body = %d/%d/%s", limited.Code, notesyncService.calls["preview"], limited.Body.String())
	}
}

func TestAdminNotesyncPreviewClampsToConfiguredScanPageSize(t *testing.T) {
	handler, _, _, notesyncService := newAdminResourceTestAPI(t, "", 100, 10)
	session := loginAdmin(t, handler)
	body := `{"path":"learning.md","page":1,"page_size":25}`

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, authenticatedAdminRequest(http.MethodPost, "/admin/api/notesync/preview", body, session))
	if response.Code != http.StatusOK || notesyncService.calls["preview"] != 1 || notesyncService.previewCmd.PageSize != 10 {
		t.Fatalf("configured-page preview status/calls/command/body = %d/%d/%+v/%s", response.Code, notesyncService.calls["preview"], notesyncService.previewCmd, response.Body.String())
	}
}

func TestAdminNotesyncReviewsForwardBoundedPagination(t *testing.T) {
	handler, _, _, notesyncService := newAdminResourceTestAPI(t, "")
	session := loginAdmin(t, handler)

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, authenticatedAdminRequest(http.MethodGet, "/admin/api/notesync/reviews?status=all&cursor=next-review", "", session))
	if response.Code != http.StatusOK {
		t.Fatalf("review pagination status/body = %d/%s", response.Code, response.Body.String())
	}
	if notesyncService.calls["list"] != 1 || notesyncService.listCmd.Status != "all" || notesyncService.listCmd.Cursor != "next-review" || notesyncService.listCmd.Limit != notesync.MaxReviewPageSize {
		t.Fatalf("review pagination calls/command = %d/%+v", notesyncService.calls["list"], notesyncService.listCmd)
	}

	invalid := httptest.NewRecorder()
	handler.ServeHTTP(invalid, authenticatedAdminRequest(http.MethodGet, "/admin/api/notesync/reviews?cursor="+strings.Repeat("x", 4097), "", session))
	if invalid.Code != http.StatusBadRequest || notesyncService.calls["list"] != 1 {
		t.Fatalf("invalid review pagination status/calls/body = %d/%d/%s", invalid.Code, notesyncService.calls["list"], invalid.Body.String())
	}
}

func TestAdminNotesyncSettingsRequireCSRFAndPersistWithoutEchoingSecret(t *testing.T) {
	settingsPath := filepath.Join(t.TempDir(), "private", "admin-settings.json")
	handler, _, _, _ := newAdminResourceTestAPI(t, settingsPath)
	session := loginAdmin(t, handler)
	body := `{"enabled":true,"base_url":"https://notes.example.test","api_token":"new-notesync-secret-0000000000000","vault":"learning","path_prefix":"edu-agent"}`

	missingCSRF := authenticatedAdminRequest(http.MethodPost, "/admin/api/notesync/settings", body, session)
	missingCSRF.Header.Del(adminCSRFHeader)
	missingCSRFResponse := httptest.NewRecorder()
	handler.ServeHTTP(missingCSRFResponse, missingCSRF)
	if missingCSRFResponse.Code != http.StatusForbidden {
		t.Fatalf("missing-CSRF status/body = %d/%s", missingCSRFResponse.Code, missingCSRFResponse.Body.String())
	}
	if _, err := os.Stat(settingsPath); !os.IsNotExist(err) {
		t.Fatalf("settings file exists after rejected request: %v", err)
	}

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, authenticatedAdminRequest(http.MethodPost, "/admin/api/notesync/settings", body, session))
	if response.Code != http.StatusOK || strings.Contains(response.Body.String(), "new-notesync-secret") {
		t.Fatalf("settings status/body = %d/%s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), `"restart_required":true`) || !strings.Contains(response.Body.String(), `"configuration_source":"environment"`) || !strings.Contains(response.Body.String(), `"configuration_source":"admin_settings"`) {
		t.Fatalf("settings response omitted restart/source metadata: %s", response.Body.String())
	}
	info, err := os.Stat(settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("settings mode = %o, want 600", info.Mode().Perm())
	}
	stored, found, err := config.LoadNotesyncAdminSettings(settingsPath)
	if err != nil || !found || stored.APIToken != "new-notesync-secret-0000000000000" {
		t.Fatalf("stored settings = %+v found=%v err=%v", stored, found, err)
	}
}

func TestAdminNotesyncSettingsRejectManagementSecretReuse(t *testing.T) {
	settingsPath := filepath.Join(t.TempDir(), "private", "admin-settings.json")
	handler, _, _, _ := newAdminResourceTestAPI(t, settingsPath)
	session := loginAdmin(t, handler)
	body, err := json.Marshal(adminNotesyncSettingsRequest{
		Enabled: true, BaseURL: "https://notes.example.test", APIToken: adminTestToken, Vault: "learning", PathPrefix: "edu-agent",
	})
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, authenticatedAdminRequest(http.MethodPost, "/admin/api/notesync/settings", string(body), session))
	if response.Code != http.StatusBadRequest {
		t.Fatalf("secret-reuse status/body = %d/%s", response.Code, response.Body.String())
	}
	if _, err := os.Stat(settingsPath); !os.IsNotExist(err) {
		t.Fatalf("settings file exists after secret-reuse rejection: %v", err)
	}
}

func TestAdminNotesyncSettingsRequireNewSecretWhenEndpointChanges(t *testing.T) {
	settingsPath := filepath.Join(t.TempDir(), "private", "admin-settings.json")
	handler, _, _, _ := newAdminResourceTestAPI(t, settingsPath)
	session := loginAdmin(t, handler)
	body := `{"enabled":true,"base_url":"https://other-notes.example.test","api_token":"","vault":"learning","path_prefix":"edu-agent"}`

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, authenticatedAdminRequest(http.MethodPost, "/admin/api/notesync/settings", body, session))
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "new NoteSync API token") {
		t.Fatalf("endpoint switch without new secret status/body = %d/%s", response.Code, response.Body.String())
	}
	if _, err := os.Stat(settingsPath); !os.IsNotExist(err) {
		t.Fatalf("settings file exists after endpoint-switch rejection: %v", err)
	}
}

func TestAdminMemoryForwardsBoundedPagination(t *testing.T) {
	handler, exporter, _, _ := newAdminResourceTestAPI(t, "")
	session := loginAdmin(t, handler)

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, authenticatedAdminRequest(http.MethodGet, "/admin/api/memory?limit=25&cursor=next-page", "", session))
	if response.Code != http.StatusOK {
		t.Fatalf("memory pagination status/body = %d/%s", response.Code, response.Body.String())
	}
	if len(exporter.requests) != 1 || exporter.requests[0].Limit != 25 || exporter.requests[0].Cursor != "next-page" {
		t.Fatalf("memory pagination request = %+v", exporter.requests)
	}

	invalid := httptest.NewRecorder()
	handler.ServeHTTP(invalid, authenticatedAdminRequest(http.MethodGet, "/admin/api/memory?limit=201", "", session))
	if invalid.Code != http.StatusBadRequest || len(exporter.requests) != 1 {
		t.Fatalf("invalid memory pagination status/calls/body = %d/%d/%s", invalid.Code, len(exporter.requests), invalid.Body.String())
	}
}

func TestAdminUIUsesOneNavigationStateForAllPages(t *testing.T) {
	page := string(adminPageHTML)
	for _, name := range []string{"overview", "pairing", "devices", "memory", "knowledge", "notesync"} {
		if !strings.Contains(page, `data-page="`+name+`"`) || !strings.Contains(page, `id="page-`+name+`"`) {
			t.Fatalf("admin page does not bind navigation and panel for %q", name)
		}
	}
	script := string(adminScriptJS)
	for _, contract := range []string{
		`document.querySelectorAll("[data-page]")`,
		`const active = panel.dataset.pagePanel === page`,
		`panel.hidden = !active`,
		`const active = item.dataset.page === page`,
		`item.classList.toggle("active", active)`,
		`history[replaceHash ? "replaceState" : "pushState"](null, "", targetHash)`,
		`scrollIntoView({ block: "nearest", inline: "center" })`,
	} {
		if !strings.Contains(script, contract) {
			t.Fatalf("admin navigation contract missing %q", contract)
		}
	}
}

func TestAdminUIMapsPublicResourceStatuses(t *testing.T) {
	script := string(adminScriptJS)
	for _, status := range []string{
		"applied", "superseded", "delete_pending", "deleted", "fenced", "expiry_reconciling",
		"queued", "prepared", "sent", "reconciling", "succeeded", "failed", "permanently_rejected", "absence_verified", "unknown",
		"available", "redacted", "unavailable", "pending", "not_applicable", "unsupported", "partial", "confirmed",
		"in_sync", "local_changed", "remote_unchanged", "remote_changed", "remote_missing", "both_changed", "remote_moved",
		"unbased_remote", "path_occupied", "invalid_remote_markdown", "open", "resolved",
	} {
		if !strings.Contains(script, status+`: [`) {
			t.Fatalf("admin status map does not cover public status %q", status)
		}
	}
	for _, contract := range []string{
		`deleted: ["已删除", "danger"]`, `fenced: ["已隔离", "danger"]`, `permanently_rejected: ["永久拒绝", "danger"]`,
		`redacted: ["已清除", "danger"]`, `unavailable: ["不可用", "danger"]`, `both_changed: ["两端均已变化", "danger"]`,
		`remote_moved: ["远端身份已移动", "danger"]`, `path_occupied: ["路径已占用", "danger"]`, `invalid_remote_markdown: ["远端文档无效", "danger"]`,
		`const statusLabel = (value) => statusMeta[value]?.[0] || "未知状态"`, `}[value] || "原因未知"`,
		`tree-node-meta status-badge ${statusClass(item.delivery_status)}`, `status-badge ${statusClass(status)}`,
		`statusLabel(review.category)`, `statusClass(review.category)`, `badges.className = "compact-badges"`,
	} {
		if !strings.Contains(script, contract) {
			t.Fatalf("admin localized status contract missing %q", contract)
		}
	}
}

func TestAdminUIUsesAuthenticatedAssetAndDeviceContracts(t *testing.T) {
	page := string(adminPageHTML)
	for _, asset := range []string{
		`href="/admin/assets/admin.css"`, `src="/admin/assets/admin.js"`, `id="mobileLogoutButton"`,
		`id="loadMoreNotesyncPreview"`, `id="loadMoreNotesyncReviews"`,
	} {
		if !strings.Contains(page, asset) {
			t.Fatalf("authenticated admin page missing %q", asset)
		}
	}
	if strings.Contains(page, `src="/admin/login.js"`) || strings.Contains(page, `id="loginView"`) {
		t.Fatal("authenticated admin page still embeds the dedicated login flow")
	}
	script := string(adminScriptJS)
	for _, contract := range []string{
		`device.display_name`, `device.id`, `/revoke`, `method: "POST"`, `bootstrapConsole();`,
		`window.location.replace("/admin/login")`, `scheduleSessionExpiry(session.expires_at)`,
		`notesync_not_configured`, `NoteSync 尚未启用。保存启用配置并重启服务后再试。`,
		`page_size: 25`, `method: "POST", csrf: true`, `reviewsNextCursor`,
		`permanently_rejected: ["永久拒绝", "danger"]`, `invalid_remote_markdown: ["远端文档无效", "danger"]`,
		`const statusLabel = (value) => statusMeta[value]?.[0] || "未知状态"`,
	} {
		if !strings.Contains(script, contract) {
			t.Fatalf("authenticated admin script missing %q", contract)
		}
	}
	for _, obsolete := range []string{`device.label`, `device.device_id`, `method: "DELETE"`, `page_size: 50`} {
		if strings.Contains(script, obsolete) {
			t.Fatalf("authenticated admin script still contains obsolete device contract %q", obsolete)
		}
	}
}
