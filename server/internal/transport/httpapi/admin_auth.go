package httpapi

import (
	"context"
	cryptorand "crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	_ "embed"
	"encoding/base64"
	"fmt"
	"html/template"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	adminSessionCookieName = "edu_agent_admin"
	adminCSRFHeader        = "X-Admin-CSRF"
	adminSessionTTL        = 15 * time.Minute
	adminSessionLimit      = 64
)

//go:embed adminui/login.html
var adminLoginPageHTML []byte

var adminLoginPage = template.Must(template.New("admin-login").Parse(string(adminLoginPageHTML)))

type adminSessionContextKey struct{}

type adminSession struct {
	expiresAt time.Time
	csrfToken string
}

type adminSessionPrincipal struct {
	key       [sha256.Size]byte
	expiresAt time.Time
	csrfToken string
}

type adminSessionStore struct {
	mu       sync.Mutex
	now      func() time.Time
	ttl      time.Duration
	limit    int
	sessions map[[sha256.Size]byte]adminSession
}

func newAdminSessionStore(now func() time.Time) *adminSessionStore {
	return &adminSessionStore{
		now:      now,
		ttl:      adminSessionTTL,
		limit:    adminSessionLimit,
		sessions: make(map[[sha256.Size]byte]adminSession),
	}
}

func (s *adminSessionStore) create() (string, adminSessionPrincipal, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now()
	s.removeExpiredLocked(now)
	if len(s.sessions) >= s.limit {
		s.removeEarliestLocked()
	}

	sessionToken, err := randomAdminToken()
	if err != nil {
		return "", adminSessionPrincipal{}, err
	}
	csrfToken, err := randomAdminToken()
	if err != nil {
		return "", adminSessionPrincipal{}, err
	}
	key := sha256.Sum256([]byte(sessionToken))
	session := adminSession{expiresAt: now.Add(s.ttl), csrfToken: csrfToken}
	s.sessions[key] = session
	return sessionToken, adminSessionPrincipal{key: key, expiresAt: session.expiresAt, csrfToken: csrfToken}, nil
}

func (s *adminSessionStore) lookup(token string) (adminSessionPrincipal, bool) {
	decoded, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(decoded) != 32 || base64.RawURLEncoding.EncodeToString(decoded) != token {
		return adminSessionPrincipal{}, false
	}
	key := sha256.Sum256([]byte(token))

	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	s.removeExpiredLocked(now)
	session, ok := s.sessions[key]
	if !ok || !now.Before(session.expiresAt) {
		return adminSessionPrincipal{}, false
	}
	return adminSessionPrincipal{key: key, expiresAt: session.expiresAt, csrfToken: session.csrfToken}, true
}

func (s *adminSessionStore) delete(key [sha256.Size]byte) {
	s.mu.Lock()
	delete(s.sessions, key)
	s.mu.Unlock()
}

func (s *adminSessionStore) removeExpiredLocked(now time.Time) {
	for key, session := range s.sessions {
		if !now.Before(session.expiresAt) {
			delete(s.sessions, key)
		}
	}
}

func (s *adminSessionStore) removeEarliestLocked() {
	var earliestKey [sha256.Size]byte
	var earliest time.Time
	found := false
	for key, session := range s.sessions {
		if !found || session.expiresAt.Before(earliest) {
			earliestKey = key
			earliest = session.expiresAt
			found = true
		}
	}
	if found {
		delete(s.sessions, earliestKey)
	}
}

func randomAdminToken() (string, error) {
	value := make([]byte, 32)
	read, err := cryptorand.Read(value)
	if err != nil {
		return "", fmt.Errorf("generate admin token: %w", err)
	}
	if read != len(value) {
		return "", fmt.Errorf("generate admin token: read %d random bytes, want %d", read, len(value))
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
}

func (a *API) adminLogin(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.lookupAdminSession(r); ok {
		http.Redirect(w, r, "/admin/", http.StatusSeeOther)
		return
	}
	a.writeAdminLogin(w, http.StatusOK, "")
}

func (a *API) adminCreateSession(w http.ResponseWriter, r *http.Request) {
	if !a.adminUI.AuthLimiter.Allow("admin-auth:" + clientIP(r)) {
		a.writeAdminLogin(w, http.StatusTooManyRequests, "登录尝试过于频繁，请稍后再试。")
		return
	}
	if !strings.HasPrefix(strings.ToLower(r.Header.Get("Content-Type")), "application/x-www-form-urlencoded") {
		a.writeAdminLogin(w, http.StatusBadRequest, "登录请求格式无效，请重新提交。")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 2<<10)
	if err := r.ParseForm(); err != nil {
		a.writeAdminLogin(w, http.StatusBadRequest, "登录请求格式无效，请重新提交。")
		return
	}
	username := r.PostForm.Get("username")
	password := r.PostForm.Get("password")
	usernameOK := subtle.ConstantTimeCompare([]byte(username), []byte("admin")) == 1
	passwordOK := subtle.ConstantTimeCompare([]byte(password), []byte(a.adminUI.Token)) == 1
	if !usernameOK || !passwordOK {
		a.writeAdminLogin(w, http.StatusUnauthorized, "用户名或管理密码不正确。")
		return
	}

	sessionToken, principal, err := a.adminSessions.create()
	if err != nil {
		a.logger.ErrorContext(r.Context(), "admin session creation failed", "error", err)
		a.writeAdminLogin(w, http.StatusInternalServerError, "暂时无法创建登录会话，请稍后再试。")
		return
	}
	http.SetCookie(w, a.adminSessionCookie(sessionToken, principal.expiresAt, false))
	http.Redirect(w, r, "/admin/", http.StatusSeeOther)
}

func (a *API) adminSession(w http.ResponseWriter, r *http.Request) {
	principal, ok := adminSessionFromContext(r.Context())
	if !ok {
		writeError(w, r, http.StatusUnauthorized, "unauthenticated", "管理会话无效或已过期")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"csrf_token": principal.csrfToken,
		"expires_at": principal.expiresAt,
	})
}

func (a *API) adminLogout(w http.ResponseWriter, r *http.Request) {
	principal, ok := adminSessionFromContext(r.Context())
	if ok {
		a.adminSessions.delete(principal.key)
	}
	http.SetCookie(w, a.adminSessionCookie("", time.Unix(1, 0), true))
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) requireAdminPageSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		principal, ok := a.lookupAdminSession(r)
		if !ok {
			http.Redirect(w, r, "/admin/login", http.StatusSeeOther)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), adminSessionContextKey{}, principal)))
	})
}

func (a *API) requireAdminAPISession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		principal, ok := a.lookupAdminSession(r)
		if !ok {
			writeError(w, r, http.StatusUnauthorized, "unauthenticated", "管理会话无效或已过期")
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), adminSessionContextKey{}, principal)))
	})
}

func (a *API) requireAdminCSRF(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		principal, ok := adminSessionFromContext(r.Context())
		token := r.Header.Get(adminCSRFHeader)
		if !ok || subtle.ConstantTimeCompare([]byte(token), []byte(principal.csrfToken)) != 1 {
			writeError(w, r, http.StatusForbidden, "forbidden", "管理请求校验失败")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (a *API) lookupAdminSession(r *http.Request) (adminSessionPrincipal, bool) {
	cookie, err := r.Cookie(adminSessionCookieName)
	if err != nil {
		return adminSessionPrincipal{}, false
	}
	return a.adminSessions.lookup(cookie.Value)
}

func adminSessionFromContext(ctx context.Context) (adminSessionPrincipal, bool) {
	principal, ok := ctx.Value(adminSessionContextKey{}).(adminSessionPrincipal)
	return principal, ok
}

func (a *API) adminSessionCookie(value string, expiresAt time.Time, clear bool) *http.Cookie {
	maxAge := int(adminSessionTTL / time.Second)
	if clear {
		maxAge = -1
	}
	return &http.Cookie{
		Name:     adminSessionCookieName,
		Value:    value,
		Path:     "/admin",
		Expires:  expiresAt,
		MaxAge:   maxAge,
		HttpOnly: true,
		Secure:   strings.EqualFold(a.adminUI.PublicBaseURL.Scheme, "https"),
		SameSite: http.SameSiteStrictMode,
	}
}

func (a *API) writeAdminLogin(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := adminLoginPage.Execute(w, struct{ Error string }{Error: message}); err != nil {
		a.logger.Error("admin login template failed", "error", err)
	}
}
