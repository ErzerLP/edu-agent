package httpapi

import (
	_ "embed"
	"encoding/base64"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/identity"
	"github.com/edu-agent/edu-agent/server/internal/memory"
	"github.com/edu-agent/edu-agent/server/internal/platform/config"
	"github.com/edu-agent/edu-agent/server/internal/privacy"
	"github.com/go-chi/chi/v5"
)

//go:embed adminui/index.html
var adminPageHTML []byte

//go:embed adminui/admin.css
var adminStylesheet []byte

//go:embed adminui/admin.js
var adminScriptJS []byte

//go:embed adminui/login.js
var adminLoginScriptJS []byte

type AdminUIOptions struct {
	Enabled                 bool
	Identity                AdminIdentityService
	PublicBaseURL           *url.URL
	Token                   string
	TrustedLoopbackProxy    bool
	SettingsFile            string
	Notesync                config.NotesyncConfig
	NotesyncSource          string
	NotesyncSettingsSavedAt time.Time
	AuthLimiter             *FixedWindowLimiter
	WriteLimiter            *FixedWindowLimiter
}

func validateAdminUIOptions(options AdminUIOptions) error {
	if !options.Enabled {
		return nil
	}
	if options.Identity == nil || options.PublicBaseURL == nil || options.AuthLimiter == nil || options.WriteLimiter == nil {
		return errors.New("admin UI dependencies are required")
	}
	if !isLoopbackHostname(options.PublicBaseURL.Hostname()) {
		return errors.New("admin UI requires a loopback public base URL")
	}
	if !validAdminUIToken(options.Token) {
		return errors.New("admin UI token must be the canonical unpadded base64url encoding of 32 random bytes")
	}
	return nil
}

func validAdminUIToken(token string) bool {
	decoded, err := base64.RawURLEncoding.DecodeString(token)
	return err == nil && len(decoded) == 32 && base64.RawURLEncoding.EncodeToString(decoded) == token
}

func isLoopbackHostname(host string) bool {
	if strings.EqualFold(strings.TrimSpace(host), "localhost") {
		return true
	}
	ip := net.ParseIP(strings.TrimSpace(host))
	return ip != nil && ip.IsLoopback()
}

func (a *API) mountAdminUI(router chi.Router) {
	router.Group(func(admin chi.Router) {
		admin.Use(a.adminSecurityHeaders)
		admin.Use(a.requireAdminLocalTransport)
		admin.Use(a.requireAdminHost)

		admin.Get("/admin", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "/admin/", http.StatusSeeOther)
		})
		admin.Get("/admin/login", a.adminLogin)
		admin.With(a.requireAdminOrigin).Post("/admin/login", a.adminCreateSession)
		admin.Get("/admin/assets/admin.css", a.adminStyles)
		admin.Get("/admin/assets/admin.js", a.adminScript)
		admin.Get("/admin/assets/login.js", a.adminLoginScript)
		admin.With(a.requireAdminPageSession).Get("/admin/", a.adminIndex)

		admin.Group(func(api chi.Router) {
			api.Use(a.requireAdminAPISession)
			api.Get("/admin/api/session", a.adminSession)
			api.Get("/admin/api/overview", a.adminOverview)
			api.With(a.responseReadPermit(memory.CodeContentRedacted, privacy.OwnerMemory)).Get("/admin/api/memory", a.adminMemory)
			api.With(a.responseReadPermit(memory.CodeContentRedacted, privacy.OwnerKnowledge)).Get("/admin/api/knowledge", a.adminKnowledge)
			api.Get("/admin/api/notesync", a.adminNotesync)
			api.With(a.requireAdminOrigin, a.requireAdminCSRF, a.responseReadPermit(memory.CodeContentRedacted, privacy.OwnerKnowledge)).Post("/admin/api/notesync/preview", a.adminNotesyncPreview)
			api.With(a.responseReadPermit(memory.CodeContentRedacted, privacy.OwnerKnowledge)).Get("/admin/api/notesync/reviews", a.adminNotesyncReviews)
			api.With(a.requireAdminOrigin, a.requireAdminCSRF).Post("/admin/api/notesync/settings", a.adminUpdateNotesync)
			api.With(a.requireAdminOrigin, a.requireAdminCSRF).Post("/admin/api/logout", a.adminLogout)
			api.With(a.requireAdminOrigin, a.requireAdminCSRF).Post("/admin/api/pairing-codes", a.adminCreatePairingCode)
			api.With(a.requireAdminOrigin, a.requireAdminCSRF).Post("/admin/api/devices/{deviceID}/revoke", a.adminRevokeDevice)
		})
	})
}

func (a *API) adminSecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'")
		w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		next.ServeHTTP(w, r)
	})
}

func (a *API) requireAdminLocalTransport(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !a.adminUI.TrustedLoopbackProxy {
			host, _, err := net.SplitHostPort(r.RemoteAddr)
			if err != nil || !isLoopbackHostname(host) {
				http.NotFound(w, r)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func (a *API) requireAdminHost(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.EqualFold(r.Host, a.adminUI.PublicBaseURL.Host) {
			http.NotFound(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (a *API) requireAdminOrigin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			expectedOrigin := a.adminUI.PublicBaseURL.Scheme + "://" + a.adminUI.PublicBaseURL.Host
			if r.Header.Get("Origin") != expectedOrigin {
				writeError(w, r, http.StatusForbidden, "forbidden", "Admin request origin is invalid")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func (a *API) adminIndex(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(adminPageHTML)
}

func (a *API) adminStyles(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/css; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(adminStylesheet)
}

func (a *API) adminScript(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(adminScriptJS)
}

func (a *API) adminLoginScript(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(adminLoginScriptJS)
}

func (a *API) adminOverview(w http.ResponseWriter, r *http.Request) {
	devices, err := a.adminUI.Identity.ListDevices(r.Context())
	if err != nil {
		a.logger.ErrorContext(r.Context(), "admin list devices failed", "error", err)
		writeError(w, r, http.StatusInternalServerError, "internal_error", "Admin overview could not be loaded")
		return
	}
	if devices == nil {
		devices = []identity.Device{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":     a.readiness.Ready(r.Context()),
		"devices":    devices,
		"server_url": a.adminUI.PublicBaseURL.String(),
	})
}

func (a *API) adminCreatePairingCode(w http.ResponseWriter, r *http.Request) {
	if !a.adminUI.WriteLimiter.Allow("admin-write:" + clientIP(r)) {
		writeError(w, r, http.StatusTooManyRequests, "rate_limited", "Too many admin changes")
		return
	}
	var request struct {
		Profile string `json:"profile"`
	}
	if err := decodeJSON(w, r, 1<<10, &request); err != nil {
		writeError(w, r, http.StatusBadRequest, "invalid_request", "Request body is invalid")
		return
	}
	profile := identity.PairingProfile(strings.TrimSpace(request.Profile))
	if profile != identity.PairingProfileUser && profile != identity.PairingProfileAgent {
		writeError(w, r, http.StatusBadRequest, "invalid_request", "Pairing profile is invalid")
		return
	}
	code, expiresAt, err := a.adminUI.Identity.CreatePairingCodeForProfile(r.Context(), profile)
	if err != nil {
		a.logger.ErrorContext(r.Context(), "admin create pairing code failed", "profile", profile, "error", err)
		writeError(w, r, http.StatusInternalServerError, "internal_error", "Pairing code could not be created")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"code":       code,
		"expires_at": expiresAt,
		"profile":    profile,
		"server_url": a.adminUI.PublicBaseURL.String(),
	})
}

func (a *API) adminRevokeDevice(w http.ResponseWriter, r *http.Request) {
	if !a.adminUI.WriteLimiter.Allow("admin-write:" + clientIP(r)) {
		writeError(w, r, http.StatusTooManyRequests, "rate_limited", "Too many admin changes")
		return
	}
	deviceID := strings.TrimSpace(chi.URLParam(r, "deviceID"))
	if deviceID == "" {
		writeError(w, r, http.StatusBadRequest, "invalid_request", "Device ID is invalid")
		return
	}
	if err := a.adminUI.Identity.RevokeDevice(r.Context(), deviceID); err != nil {
		switch {
		case errors.Is(err, identity.ErrInvalidInput):
			writeError(w, r, http.StatusBadRequest, "invalid_request", "Device ID is invalid")
		case errors.Is(err, identity.ErrNotFound):
			writeError(w, r, http.StatusNotFound, "not_found", "Device was not found")
		default:
			a.logger.ErrorContext(r.Context(), "admin revoke device failed", "device_id", deviceID, "error", err)
			writeError(w, r, http.StatusInternalServerError, "internal_error", "Device could not be revoked")
		}
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
