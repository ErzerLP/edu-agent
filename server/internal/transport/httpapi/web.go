package httpapi

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/identity"
	"github.com/edu-agent/edu-agent/server/internal/privacy"
	"github.com/edu-agent/edu-agent/server/internal/transport/access"
	"github.com/go-chi/chi/v5"
)

type WebIdentityService interface {
	ExchangeWebPairing(context.Context, string, string) (string, identity.WebPrincipal, error)
	AuthenticateWeb(context.Context, string) (identity.WebPrincipal, error)
	LogoutWeb(context.Context, string) error
}

type WebUIOptions struct {
	Enabled           bool
	AllowLoopbackHTTP bool
	PublicBaseURL     *url.URL
	Identity          WebIdentityService
	Assets            fs.FS
}

func validateWebUI(o WebUIOptions) error {
	if !o.Enabled {
		return nil
	}
	if o.Identity == nil || o.Assets == nil || o.PublicBaseURL == nil {
		return errors.New("Web 学习入口缺少身份、静态资源或 Origin")
	}
	u := o.PublicBaseURL
	if u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return errors.New("Web PUBLIC_BASE_URL 必须是完整的同源根地址")
	}
	ip := net.ParseIP(u.Hostname())
	loopback := u.Hostname() == "localhost" || (ip != nil && ip.IsLoopback())
	if u.Scheme != "https" && !(u.Scheme == "http" && loopback && o.AllowLoopbackHTTP) {
		return errors.New("Web 必须使用 HTTPS，loopback HTTP 开发需显式设置 WEB_UI_ALLOW_LOOPBACK_HTTP=true")
	}
	data, err := fs.ReadFile(o.Assets, "index.html")
	if err != nil || !strings.Contains(string(data), `type="module"`) {
		return errors.New("缺少真实 Web 构建资源，请先运行 make web-build")
	}
	references := regexp.MustCompile(`(?:src|href)="/app/([^"]+)"`).FindAllSubmatch(data, -1)
	if len(references) == 0 {
		return errors.New("Web 入口没有可加载的构建资源")
	}
	for _, reference := range references {
		if info, err := fs.Stat(o.Assets, string(reference[1])); err != nil || info.IsDir() {
			return errors.New("Web 入口引用的构建资源缺失")
		}
	}
	return nil
}

func (a *API) webCookieName() string {
	if a.webUI.PublicBaseURL.Scheme == "https" {
		return "__Host-edu_web"
	}
	return "edu_web_dev"
}

func webCSRF(cookie string) string {
	hash := sha256.Sum256([]byte("edu-web-csrf:" + cookie))
	return base64.RawURLEncoding.EncodeToString(hash[:])
}

func (a *API) webOriginOK(r *http.Request) bool {
	u := a.webUI.PublicBaseURL
	return len(r.Header.Values("Origin")) == 1 && r.Header.Get("Origin") == u.Scheme+"://"+u.Host && r.Host == u.Host
}

// webSecurity 放在整个路由器上，既有写 API 也必须经过 Cookie/CSRF 检查。
func (a *API) webSecurity(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var cookie *http.Cookie
		count := 0
		for _, c := range r.Cookies() {
			if c.Name == "__Host-edu_web" || c.Name == "edu_web_dev" {
				cookie = c
				count++
			}
		}
		if count == 0 {
			next.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		if count != 1 || len(r.Header.Values("Authorization")) != 0 {
			writeError(w, r, 400, "ambiguous_identity", "不能混用浏览器与 Authorization 身份")
			return
		}
		if !a.webUI.Enabled || cookie.Name != a.webCookieName() {
			writeError(w, r, 401, "authentication_failed", "浏览器学习会话不可用")
			return
		}
		if strings.HasPrefix(r.URL.Path, "/admin") || strings.HasPrefix(r.URL.Path, "/internal") || r.URL.Path == "/mcp" || r.URL.Path == "/v1/pairings/exchange" {
			writeError(w, r, 403, "forbidden", "学习会话无权访问此入口")
			return
		}
		if r.Method != "GET" && r.Method != "HEAD" && r.Method != "OPTIONS" {
			if !a.webOriginOK(r) || len(r.Header.Values("X-CSRF-Token")) != 1 || subtle.ConstantTimeCompare([]byte(r.Header.Get("X-CSRF-Token")), []byte(webCSRF(cookie.Value))) != 1 {
				writeError(w, r, 403, "csrf_failed", "请求来源或 CSRF 校验失败")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func (a *API) setWebCookie(w http.ResponseWriter, value string, expires time.Time) {
	c := &http.Cookie{Name: a.webCookieName(), Value: value, Path: "/", HttpOnly: true, Secure: a.webUI.PublicBaseURL.Scheme == "https", SameSite: http.SameSiteStrictMode, Expires: expires, MaxAge: int(identity.WebSessionTTL.Seconds())}
	if value == "" {
		c.MaxAge = -1
	}
	http.SetCookie(w, c)
}

func (a *API) mountWeb(r chi.Router) {
	r.Get("/app", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/app/", http.StatusPermanentRedirect)
	})
	r.Get("/app/*", a.webAsset)
	r.Get("/content/{artifactID}", a.contentEntry)
	r.With(a.responseReadPermit("content_redacted", privacy.OwnerIdentity)).Post("/v1/web/pairings", a.webPair)
	r.With(a.responseReadPermit("content_redacted", privacy.OwnerIdentity)).Get("/v1/web/session", a.webSession)
	r.Post("/v1/web/logout", a.webLogout)
}

// 内容短链接仅携带身份和版本，实际页面仍共用 /app 的同源身份边界。
func (a *API) contentEntry(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	id := chi.URLParam(r, "artifactID")
	if !validLearningUUID(id) {
		http.NotFound(w, r)
		return
	}
	q, ok := strictLearningQuery(w, r, "space", "version")
	if !ok {
		return
	}
	if q.Has("space") && !validLearningUUID(q.Get("space")) {
		writeLearningInvalid(w, r)
		return
	}
	if q.Has("version") {
		version, err := strconv.ParseInt(q.Get("version"), 10, 64)
		if err != nil || version < 1 {
			writeLearningInvalid(w, r)
			return
		}
	}
	target := "/app/content/" + id
	if len(q) > 0 {
		target += "?" + q.Encode()
	}
	http.Redirect(w, r, target, http.StatusTemporaryRedirect)
}

func (a *API) webPair(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if !a.webOriginOK(r) {
		writeError(w, r, 403, "csrf_failed", "配对需要同源 Origin")
		return
	}
	if r.Header.Get("Authorization") != "" {
		writeError(w, r, 400, "ambiguous_identity", "浏览器配对不接受 Authorization")
		return
	}
	if _, err := r.Cookie(a.webCookieName()); err == nil {
		writeError(w, r, 409, "web_session_exists", "请先退出当前浏览器会话")
		return
	}
	if !a.pairLimiter.Allow("web-pair:" + clientIP(r)) {
		writeError(w, r, 429, "rate_limited", "配对请求过于频繁")
		return
	}
	var body struct {
		Code        string `json:"code"`
		DisplayName string `json:"display_name"`
	}
	if decodeJSON(w, r, a.maxRequestBody, &body) != nil {
		writeError(w, r, 400, "invalid_request", "配对请求无效")
		return
	}
	cookie, p, err := a.webUI.Identity.ExchangeWebPairing(r.Context(), body.Code, body.DisplayName)
	if err != nil {
		if errors.Is(err, identity.ErrInvalidPairingCode) || errors.Is(err, identity.ErrInvalidInput) {
			writeError(w, r, 400, "invalid_pairing_code", "配对码或设备名称无效")
			return
		}
		writeError(w, r, 503, "web_pairing_unavailable", "暂时无法配对，请重试")
		return
	}
	a.setWebCookie(w, cookie, p.ExpiresAt)
	a.writeWebSession(w, r, 201, cookie, p)
}

func (a *API) writeWebSession(w http.ResponseWriter, r *http.Request, status int, cookie string, p identity.WebPrincipal) {
	goals, ok := a.learning.(goalManagementService)
	save := ok && goals.SupportsGoalManagement() && access.ContainsScope(p.Credential.Scopes, "learning:write")
	_, imports := a.knowledge.(importJobService)
	writeJSON(w, status, map[string]any{"device": p.Device, "generation": p.Generation, "expires_at": p.ExpiresAt, "csrf_token": webCSRF(cookie), "server_id": a.webUI.PublicBaseURL.Scheme + "://" + a.webUI.PublicBaseURL.Host,
		"capabilities": map[string]any{"spaces": a.learningSpaces != nil, "goals": ok && goals.SupportsGoalManagement(), "save_goal": save, "start_learning": a.learning != nil && a.learningContent.Available() && save, "references": a.knowledge != nil && access.ContainsScope(p.Credential.Scopes, "references:manage"), "runs": a.mentorRuns != nil, "import_jobs": imports}})
}

func (a *API) webSession(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if a.authLimiter.Limited("auth:" + clientIP(r)) {
		writeError(w, r, 429, "rate_limited", "认证失败次数过多")
		return
	}
	c, err := r.Cookie(a.webCookieName())
	if err != nil {
		a.authenticationFailed(w, r, "auth:"+clientIP(r))
		return
	}
	p, err := a.webUI.Identity.AuthenticateWeb(r.Context(), c.Value)
	if err != nil {
		if !errors.Is(err, identity.ErrUnauthenticated) {
			writeError(w, r, 503, "web_session_unavailable", "暂时无法读取浏览器会话")
			return
		}
		a.setWebCookie(w, "", time.Unix(1, 0))
		a.authenticationFailed(w, r, "auth:"+clientIP(r))
		return
	}
	a.writeWebSession(w, r, 200, c.Value, p)
}

func (a *API) webLogout(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	c, err := r.Cookie(a.webCookieName())
	if err != nil {
		writeError(w, r, 401, "authentication_failed", "浏览器未配对")
		return
	}
	if err := a.webUI.Identity.LogoutWeb(r.Context(), c.Value); err != nil {
		writeError(w, r, 503, "web_logout_unavailable", "退出未完成，请重试")
		return
	}
	a.setWebCookie(w, "", time.Unix(1, 0))
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) webAsset(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "same-origin")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
	name := strings.TrimPrefix(r.URL.Path, "/app/")
	// 仅页面地址回退；不存在的资源、API 和扩展名请求保留 404。
	if name == "" || name == "settings" || name == "runs" || ((strings.HasPrefix(name, "spaces/") || strings.HasPrefix(name, "content/") || strings.HasPrefix(name, "runs/")) && !strings.Contains(name, ".")) {
		name = "index.html"
	}
	if !fs.ValidPath(name) || path.Clean(name) != name {
		http.NotFound(w, r)
		return
	}
	data, err := fs.ReadFile(a.webUI.Assets, name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	switch path.Ext(name) {
	case ".html":
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
	case ".js":
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	case ".css":
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
	case ".woff2":
		w.Header().Set("Content-Type", "font/woff2")
	case ".woff":
		w.Header().Set("Content-Type", "font/woff")
	case ".ttf":
		w.Header().Set("Content-Type", "font/ttf")
	default:
		http.NotFound(w, r)
		return
	}
	_, _ = fmt.Fprint(w, string(data))
}
