package httpapi

import (
	"context"
	"crypto/subtle"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/identity"
	"github.com/edu-agent/edu-agent/server/internal/learning"
	"github.com/edu-agent/edu-agent/server/internal/privacy"
	"github.com/edu-agent/edu-agent/server/internal/transport/access"
	"github.com/go-chi/chi/v5"
)

type webOfflineIdentity interface {
	EnableWebOffline(context.Context, string) (string, identity.WebOfflinePrincipal, error)
	AuthenticateWebOffline(context.Context, string) (identity.WebOfflinePrincipal, error)
	DisableWebOffline(context.Context, string) error
	RenewWebOffline(context.Context, string) (identity.WebOfflinePrincipal, error)
}

type webOfflineContext struct{}
type webOfflineAuth struct {
	cookie    string
	principal identity.WebOfflinePrincipal
}

func (a *API) offlineCookieName() string {
	if a.webUI.PublicBaseURL.Scheme == "https" {
		return "__Host-edu_offline"
	}
	return "edu_offline_dev"
}

func (a *API) setOfflineCookie(w http.ResponseWriter, value string, expires time.Time) {
	age := int(time.Until(expires).Seconds())
	if value == "" {
		age = -1
	}
	http.SetCookie(w, &http.Cookie{Name: a.offlineCookieName(), Value: value, Path: "/", HttpOnly: true, Secure: a.webUI.PublicBaseURL.Scheme == "https", SameSite: http.SameSiteStrictMode, Expires: expires, MaxAge: age})
}

func (a *API) mountWebOffline(r chi.Router) {
	r.Get("/v1/web/offline/capabilities", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		_, identityOK := a.webUI.Identity.(webOfflineIdentity)
		_, signerOK := a.offline.(offlinePairingBootstrapService)
		writeJSON(w, 200, map[string]any{"adapter_version": 1, "enabled": a.webUI.OfflineEnabled && identityOK && signerOK && a.privacy != nil, "answer_protocols": []string{"text-answer-v1"}, "content_protocols": []string{"offline-pack-v1"}})
	})
	r.With(a.authenticate, a.requireScope("learning:write"), a.requireScope("knowledge:read"), a.responseReadPermit("content_redacted", privacy.OwnerIdentity, privacy.OwnerLearning)).Post("/v1/web/offline/enable", a.enableWebOffline)
	// 停止新签发不阻止已下载设备完成同步和清除。
	r.Route("/v1/web/offline", func(r chi.Router) {
		r.Use(a.authenticateWebOffline)
		r.Get("/session", a.webOfflineSession)
		r.Delete("/session", a.disableWebOffline)
		r.Post("/purge/{erasureID}/ack", a.handlePrivacyOfflineDeviceAck)
		r.Group(func(r chi.Router) {
			r.Use(a.webOfflineContent)
			owners := []privacy.OwnerKind{privacy.OwnerLearning, privacy.OwnerTutoring, privacy.OwnerKnowledge}
			r.With(a.requireScope("learning:write"), a.requireScope("knowledge:read"), a.resolveLearningSpace, a.responseReadPermit("privacy_clear_in_progress", owners...)).Post("/packs", func(w http.ResponseWriter, r *http.Request) {
				if !a.webUI.OfflineEnabled {
					writeError(w, r, 403, "web_offline_disabled", "浏览器离线签发未启用")
					return
				}
				a.handleWebOfflinePrepare(w, r)
			})
			r.With(a.requireScope("learning:write"), a.responseReadPermit("privacy_clear_in_progress", owners...)).Post("/sync", a.handleOfflineSync)
			r.With(a.requireScope("learning:read"), a.responseReadPermit("content_redacted", owners...)).Get("/operations/{operationID}", a.handleOfflineStatus)
		})
	})
}

func (a *API) enableWebOffline(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	service, ok := a.webUI.Identity.(webOfflineIdentity)
	bootstrap, signerOK := a.offline.(offlinePairingBootstrapService)
	if !a.webUI.OfflineEnabled || !ok || !signerOK || a.privacy == nil {
		writeError(w, r, 403, "web_offline_disabled", "浏览器离线学习未启用")
		return
	}
	var body struct {
		AdapterVersion int  `json:"adapter_version"`
		SaveConsent    bool `json:"save_consent"`
	}
	if decodeJSON(w, r, a.maxRequestBody, &body) != nil || body.AdapterVersion != 1 || !body.SaveConsent {
		writeLearningInvalid(w, r)
		return
	}
	source, err := r.Cookie(a.webCookieName())
	if err != nil || !a.webOriginOK(r) {
		writeError(w, r, 401, "authentication_failed", "需要当前浏览器配对身份")
		return
	}
	trust, err := bootstrap.PairingBootstrap(r.Context())
	if err != nil {
		a.writeOfflineFailure(w, r, "web_offline_bootstrap", err)
		return
	}
	var cookie string
	var p identity.WebOfflinePrincipal
	if existing, e := r.Cookie(a.offlineCookieName()); e == nil {
		p, err = service.AuthenticateWebOffline(r.Context(), existing.Value)
		credential, _ := credentialFromContext(r.Context())
		if err == nil && (p.Device.ID != credential.Device.ID || !p.ContentAllowed) {
			writeError(w, r, 409, "offline_owner_conflict", "请先处理原设备离线库及待清除任务")
			return
		}
		if err == nil {
			cookie = existing.Value
		}
	}
	if cookie == "" {
		cookie, p, err = service.EnableWebOffline(r.Context(), source.Value)
	}
	if err != nil {
		writeError(w, r, 503, "offline_identity_unavailable", "无法保留原设备同步身份")
		return
	}
	a.setOfflineCookie(w, cookie, p.ExpiresAt)
	writeJSON(w, 201, map[string]any{"adapter_version": 1, "device_id": p.Device.ID, "generation": strconv.FormatInt(p.Generation, 10), "expires_at": p.ExpiresAt, "csrf_token": webCSRF(cookie), "bootstrap": trust})
}

func (a *API) authenticateWebOffline(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if r.Header.Get("X-Offline-Adapter") != "1" {
			writeError(w, r, 400, "offline_adapter_unsupported", "浏览器离线适配版本不兼容")
			return
		}
		service, ok := a.webUI.Identity.(webOfflineIdentity)
		if !ok || a.offline == nil || a.privacy == nil {
			writeError(w, r, 503, "offline_unavailable", "离线合同不可用")
			return
		}
		if r.Header.Get("Authorization") != "" {
			writeError(w, r, 400, "ambiguous_identity", "离线浏览器入口不接受 Authorization")
			return
		}
		var cookie *http.Cookie
		count := 0
		for _, c := range r.Cookies() {
			if c.Name == "__Host-edu_offline" || c.Name == "edu_offline_dev" {
				cookie = c
				count++
			}
		}
		if count != 1 || cookie.Name != a.offlineCookieName() {
			writeError(w, r, 401, "authentication_failed", "原设备离线身份缺失或过期")
			return
		}
		if r.Method != "GET" && (!a.webOriginOK(r) || len(r.Header.Values("X-CSRF-Token")) != 1 || subtle.ConstantTimeCompare([]byte(r.Header.Get("X-CSRF-Token")), []byte(webCSRF(cookie.Value))) != 1) {
			writeError(w, r, 403, "csrf_failed", "离线请求来源或 CSRF 校验失败")
			return
		}
		failureKey := "auth:" + clientIP(r)
		if a.authLimiter.Limited(failureKey) {
			writeError(w, r, 429, "rate_limited", "认证请求过于频繁")
			return
		}
		p, err := service.AuthenticateWebOffline(r.Context(), cookie.Value)
		if errors.Is(err, identity.ErrUnauthenticated) {
			a.authenticationFailed(w, r, failureKey)
			return
		}
		if err != nil {
			writeError(w, r, 503, "offline_identity_unavailable", "原设备身份暂不可核对")
			return
		}
		if !a.deviceLimiter.Allow("device:" + p.Device.ID) {
			writeError(w, r, 429, "rate_limited", "设备请求过于频繁")
			return
		}
		identifying := r.URL.Path != "/v1/web/offline/session" || r.Method != "GET" || r.Header.Get("X-Web-Principal-ID") != ""
		if identifying && (r.Header.Get("X-Web-Principal-ID") != p.Device.ID || r.Header.Get("X-Web-Generation") != strconv.FormatInt(p.Generation, 10)) {
			writeError(w, r, 409, "offline_owner_conflict", "离线库与原设备身份不匹配")
			return
		}
		ctx := context.WithValue(access.WithCredential(r.Context(), p.Credential), webOfflineContext{}, webOfflineAuth{cookie.Value, p})
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (a *API) webOfflineContent(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Context().Value(webOfflineContext{}).(webOfflineAuth)
		if !auth.principal.ContentAllowed {
			writeError(w, r, 409, "offline_purge_required", "旧代次离线正文需清除")
			return
		}
		task, found, err := a.privacy.CurrentOfflineDevicePurge(r.Context(), auth.principal.Device.ID)
		if err != nil {
			a.writePrivacyFailure(w, r, "web_offline_purge", err)
			return
		}
		if found && task.Status != "succeeded" {
			writeError(w, r, 409, "offline_purge_required", "设备有待清除任务")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (a *API) handleWebOfflinePrepare(w http.ResponseWriter, r *http.Request) {
	if _, ok := strictLearningQuery(w, r); !ok {
		return
	}
	var request learning.OfflinePrepareRequest
	if !a.decodeOffline(w, r, &request) || request.Validate() != nil || request.SessionID == "" {
		writeLearningInvalid(w, r)
		return
	}
	credential, _ := credentialFromContext(r.Context())
	response, err := a.offline.Prepare(r.Context(), credential.Device.ID, request)
	if err != nil {
		a.writeOfflineFailure(w, r, "web_offline_prepare", err)
		return
	}
	status := http.StatusCreated
	if response.Replayed {
		status = http.StatusOK
	}
	auth := r.Context().Value(webOfflineContext{}).(webOfflineAuth)
	principal, err := a.webUI.Identity.(webOfflineIdentity).RenewWebOffline(r.Context(), auth.cookie)
	if err != nil {
		writeError(w, r, 503, "offline_identity_unavailable", "专用同步身份续期未完成，请核对原下载")
		return
	}
	a.setOfflineCookie(w, auth.cookie, principal.ExpiresAt)
	writeJSON(w, status, response)
}

func (a *API) webOfflineSession(w http.ResponseWriter, r *http.Request) {
	auth := r.Context().Value(webOfflineContext{}).(webOfflineAuth)
	task, found, err := a.privacy.CurrentOfflineDevicePurge(r.Context(), auth.principal.Device.ID)
	if err != nil {
		a.writePrivacyFailure(w, r, "web_offline_purge", err)
		return
	}
	var purge any
	var receipt any
	if found {
		purge = task
	} else if !auth.principal.ContentAllowed {
		if reader, ok := a.privacy.(interface {
			OfflineDevicePurgeReceipt(context.Context, string, int64) (privacy.OfflineDeviceChildReceipt, bool, error)
		}); ok {
			value, foundReceipt, err := reader.OfflineDevicePurgeReceipt(r.Context(), auth.principal.Device.ID, auth.principal.Generation)
			if err != nil {
				a.writePrivacyFailure(w, r, "web_offline_receipt", err)
				return
			}
			if foundReceipt {
				receipt = value
			}
		}
	}
	writeJSON(w, 200, map[string]any{
		"adapter_version": 1, "device_id": auth.principal.Device.ID,
		"generation": strconv.FormatInt(auth.principal.Generation, 10), "expires_at": auth.principal.ExpiresAt,
		"server_time": time.Now().UTC(), "content_allowed": auth.principal.ContentAllowed && !found,
		"csrf_token": webCSRF(auth.cookie), "purge": purge, "purge_receipt": receipt,
	})
}

func (a *API) disableWebOffline(w http.ResponseWriter, r *http.Request) {
	auth := r.Context().Value(webOfflineContext{}).(webOfflineAuth)
	if err := a.webUI.Identity.(webOfflineIdentity).DisableWebOffline(r.Context(), auth.cookie); err != nil {
		writeError(w, r, 503, "offline_identity_unavailable", "离线身份清除尚未完成")
		return
	}
	a.setOfflineCookie(w, "", time.Unix(1, 0))
	w.WriteHeader(204)
}
