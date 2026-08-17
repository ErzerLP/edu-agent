package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/identity"
	"github.com/edu-agent/edu-agent/server/internal/integrations/llm"
	"github.com/edu-agent/edu-agent/server/internal/platform/health"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

type IdentityService interface {
	ExchangePairingCode(context.Context, string, string) (identity.IssuedCredential, error)
	Authenticate(context.Context, string, string) (identity.Credential, error)
	ListDevices(context.Context) ([]identity.Device, error)
	RevokeDevice(context.Context, string) error
}

type ModelProber interface {
	Probe(context.Context) llm.Capabilities
}

type Readiness interface {
	Ready(context.Context) health.Report
}

type Options struct {
	Identity       IdentityService
	Model          ModelProber
	Readiness      Readiness
	Logger         *slog.Logger
	PairLimiter    *FixedWindowLimiter
	AuthLimiter    *FixedWindowLimiter
	DeviceLimiter  *FixedWindowLimiter
	MaxRequestBody int64
}

type API struct {
	identity       IdentityService
	model          ModelProber
	readiness      Readiness
	logger         *slog.Logger
	pairLimiter    *FixedWindowLimiter
	authLimiter    *FixedWindowLimiter
	deviceLimiter  *FixedWindowLimiter
	maxRequestBody int64
}

type credentialContextKey struct{}

func New(options Options) (http.Handler, error) {
	if options.Identity == nil || options.Readiness == nil || options.Logger == nil || options.PairLimiter == nil || options.AuthLimiter == nil || options.DeviceLimiter == nil {
		return nil, errors.New("HTTP API dependencies are required")
	}
	if options.MaxRequestBody <= 0 {
		options.MaxRequestBody = 64 << 10
	}
	api := &API{
		identity: options.Identity, model: options.Model, readiness: options.Readiness, logger: options.Logger,
		pairLimiter: options.PairLimiter, authLimiter: options.AuthLimiter,
		deviceLimiter: options.DeviceLimiter, maxRequestBody: options.MaxRequestBody,
	}
	router := chi.NewRouter()
	router.Use(middleware.RequestID)
	router.Use(api.recoverer)
	router.Use(api.audit)
	router.Get("/livez", api.livez)
	router.Get("/readyz", api.readyz)
	router.Post("/v1/pairings/exchange", api.exchangePairingCode)
	router.Group(func(protected chi.Router) {
		protected.Use(api.authenticate)
		protected.With(api.requireScope("devices:read")).Get("/v1/devices", api.listDevices)
		protected.With(api.requireScope("devices:manage")).Delete("/v1/devices/{deviceID}", api.revokeDevice)
		protected.With(api.requireScope("model:probe")).Get("/v1/model/capabilities", api.modelCapabilities)
	})
	return router, nil
}

func (a *API) livez(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "alive"})
}

func (a *API) readyz(w http.ResponseWriter, r *http.Request) {
	report := a.readiness.Ready(r.Context())
	status := http.StatusOK
	if report.Status == health.StatusNotReady {
		status = http.StatusServiceUnavailable
	}
	writeJSON(w, status, report)
}

func (a *API) exchangePairingCode(w http.ResponseWriter, r *http.Request) {
	if !a.pairLimiter.Allow("pair:" + clientIP(r)) {
		writeError(w, r, http.StatusTooManyRequests, "rate_limited", "Too many pairing attempts")
		return
	}
	var request struct {
		Code        string `json:"code"`
		DisplayName string `json:"display_name"`
	}
	if err := decodeJSON(w, r, a.maxRequestBody, &request); err != nil {
		writeError(w, r, http.StatusBadRequest, "invalid_request", "Request body is invalid")
		return
	}
	credential, err := a.identity.ExchangePairingCode(r.Context(), request.Code, request.DisplayName)
	if err != nil {
		switch {
		case errors.Is(err, identity.ErrInvalidInput):
			writeError(w, r, http.StatusBadRequest, "invalid_request", "Device display name is invalid")
		case errors.Is(err, identity.ErrInvalidPairingCode):
			writeError(w, r, http.StatusUnauthorized, "pairing_failed", "Pairing code is invalid or expired")
		default:
			a.logger.ErrorContext(r.Context(), "pairing exchange failed", "request_id", middleware.GetReqID(r.Context()), "error", err)
			writeError(w, r, http.StatusInternalServerError, "internal_error", "Request could not be completed")
		}
		return
	}
	writeJSON(w, http.StatusCreated, credential)
}

func (a *API) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		failureKey := "auth:" + clientIP(r)
		if a.authLimiter.Limited(failureKey) {
			writeError(w, r, http.StatusTooManyRequests, "rate_limited", "Too many authentication failures")
			return
		}
		token, ok := bearerToken(r.Header.Get("Authorization"))
		if !ok {
			a.authenticationFailed(w, r, failureKey)
			return
		}
		credential, err := a.identity.Authenticate(r.Context(), token, "")
		if err != nil {
			if errors.Is(err, identity.ErrUnauthenticated) {
				a.authenticationFailed(w, r, failureKey)
				return
			}
			a.logger.ErrorContext(r.Context(), "device authentication failed", "request_id", middleware.GetReqID(r.Context()), "error", err)
			writeError(w, r, http.StatusInternalServerError, "internal_error", "Request could not be completed")
			return
		}
		if !a.deviceLimiter.Allow("device:" + credential.Device.ID) {
			writeError(w, r, http.StatusTooManyRequests, "rate_limited", "Device request rate exceeded")
			return
		}
		ctx := context.WithValue(r.Context(), credentialContextKey{}, credential)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (a *API) authenticationFailed(w http.ResponseWriter, r *http.Request, failureKey string) {
	if !a.authLimiter.Allow(failureKey) {
		writeError(w, r, http.StatusTooManyRequests, "rate_limited", "Too many authentication failures")
		return
	}
	writeError(w, r, http.StatusUnauthorized, "authentication_failed", "Device credentials are invalid")
}

func (a *API) requireScope(scope string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			credential, ok := credentialFromContext(r.Context())
			if !ok || !contains(credential.Scopes, scope) {
				writeError(w, r, http.StatusForbidden, "forbidden", "Device does not have the required scope")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func (a *API) listDevices(w http.ResponseWriter, r *http.Request) {
	devices, err := a.identity.ListDevices(r.Context())
	if err != nil {
		a.logger.ErrorContext(r.Context(), "list devices failed", "request_id", middleware.GetReqID(r.Context()), "error", err)
		writeError(w, r, http.StatusInternalServerError, "internal_error", "Request could not be completed")
		return
	}
	if devices == nil {
		devices = []identity.Device{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"devices": devices})
}

func (a *API) revokeDevice(w http.ResponseWriter, r *http.Request) {
	deviceID := strings.TrimSpace(chi.URLParam(r, "deviceID"))
	if deviceID == "" {
		writeError(w, r, http.StatusBadRequest, "invalid_request", "Device ID is invalid")
		return
	}
	if err := a.identity.RevokeDevice(r.Context(), deviceID); err != nil {
		if errors.Is(err, identity.ErrInvalidInput) {
			writeError(w, r, http.StatusBadRequest, "invalid_request", "Device ID is invalid")
			return
		}
		if errors.Is(err, identity.ErrNotFound) {
			writeError(w, r, http.StatusNotFound, "not_found", "Device was not found")
			return
		}
		a.logger.ErrorContext(r.Context(), "revoke device failed", "request_id", middleware.GetReqID(r.Context()), "error", err)
		writeError(w, r, http.StatusInternalServerError, "internal_error", "Request could not be completed")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) modelCapabilities(w http.ResponseWriter, r *http.Request) {
	if a.model == nil {
		writeJSON(w, http.StatusOK, llm.Capabilities{Profile: "openai-chat-completions-v1", Compatible: false, IncompatibilityReasons: []string{"not_configured"}})
		return
	}
	writeJSON(w, http.StatusOK, a.model.Probe(r.Context()))
}

func (a *API) audit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		wrapped := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
		next.ServeHTTP(wrapped, r)
		a.logger.InfoContext(r.Context(), "http_request",
			"request_id", middleware.GetReqID(r.Context()), "method", r.Method, "path", r.URL.Path,
			"status", wrapped.Status(), "duration_ms", time.Since(started).Milliseconds(), "remote_ip", clientIP(r))
	})
}

func (a *API) recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if recovered := recover(); recovered != nil {
				a.logger.ErrorContext(r.Context(), "http handler panic", "request_id", middleware.GetReqID(r.Context()), "panic_type", fmt.Sprintf("%T", recovered))
				writeError(w, r, http.StatusInternalServerError, "internal_error", "Request could not be completed")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func credentialFromContext(ctx context.Context) (identity.Credential, bool) {
	credential, ok := ctx.Value(credentialContextKey{}).(identity.Credential)
	return credential, ok
}

func bearerToken(header string) (string, bool) {
	parts := strings.Fields(header)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") || parts[1] == "" {
		return "", false
	}
	return parts[1], true
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil {
		return host
	}
	return r.RemoteAddr
}

func decodeJSON(w http.ResponseWriter, r *http.Request, limit int64, target any) error {
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("request must contain one JSON value")
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, r *http.Request, status int, code, message string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{
		"code": code, "message": message, "request_id": middleware.GetReqID(r.Context()),
	}})
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
