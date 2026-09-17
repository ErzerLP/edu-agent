package identity

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"time"
)

const WebSessionTTL = 12 * time.Hour

// WebSessionRecord 仅保存会话摘要，设备凭据与浏览器 Cookie 使用不同随机值。
type WebSessionRecord struct {
	Hash      [32]byte
	ExpiresAt time.Time
}

type WebPrincipal struct {
	Credential Credential `json:"-"`
	Device     Device     `json:"device"`
	Generation int64      `json:"generation"`
	ExpiresAt  time.Time  `json:"expires_at"`
}

type WebStore interface {
	FindWebSession(context.Context, [32]byte, time.Time) (WebPrincipal, error)
	DeleteWebSession(context.Context, [32]byte) error
}

// WebScopes 仅显式导入或参考档案保留资料审批；普通档案不继承管理与通用审批权限。
func WebScopes(scopes []string) []string {
	result := []string{}
	for _, scope := range scopes {
		if hasScope(scopes, "imports:web") && (scope == "imports:web" || scope == "knowledge:write" || scope == "knowledge:approve") {
			result = append(result, scope)
			continue
		}
		if hasScope(scopes, "references:manage") && (scope == "references:manage" || scope == "knowledge:approve" || scope == "knowledge:write") {
			result = append(result, scope)
			continue
		}
		if hasScope(scopes, "research:adopt") && (scope == "research:adopt" || scope == "knowledge:read" || scope == "knowledge:write") {
			result = append(result, scope)
			continue
		}
		if scope == "learning:read" || scope == "learning:write" || scope == "knowledge:read" || scope == "settings:write" || scope == "settings:probe" {
			result = append(result, scope)
		}
	}
	return result
}

func (s *Service) ExchangeWebPairing(ctx context.Context, code, name string) (string, WebPrincipal, error) {
	if _, ok := s.store.(WebStore); !ok {
		return "", WebPrincipal{}, errors.New("身份存储不支持浏览器会话")
	}
	raw, err := randomBytes(s.random, 32)
	if err != nil {
		return "", WebPrincipal{}, err
	}
	cookie := base64.RawURLEncoding.EncodeToString(raw)
	record := &WebSessionRecord{Hash: sha256.Sum256(raw), ExpiresAt: s.now().UTC().Add(WebSessionTTL)}
	_, err = s.exchangePairingCode(ctx, code, name, record)
	if err != nil {
		return "", WebPrincipal{}, err
	}
	// 长期设备 Token 从此丢弃，永远不传入 HTTP 响应或页面。
	principal, err := s.AuthenticateWeb(ctx, cookie)
	return cookie, principal, err
}

func webHash(cookie string) ([32]byte, error) {
	raw, err := base64.RawURLEncoding.DecodeString(cookie)
	if err != nil || len(raw) != 32 || base64.RawURLEncoding.EncodeToString(raw) != cookie {
		return [32]byte{}, ErrUnauthenticated
	}
	return sha256.Sum256(raw), nil
}

func (s *Service) AuthenticateWeb(ctx context.Context, cookie string) (WebPrincipal, error) {
	hash, err := webHash(cookie)
	if err != nil {
		return WebPrincipal{}, err
	}
	store, ok := s.store.(WebStore)
	if !ok {
		return WebPrincipal{}, ErrUnauthenticated
	}
	p, err := store.FindWebSession(ctx, hash, s.now().UTC())
	if errors.Is(err, ErrNotFound) {
		return WebPrincipal{}, ErrUnauthenticated
	}
	return p, err
}

func (s *Service) LogoutWeb(ctx context.Context, cookie string) error {
	hash, err := webHash(cookie)
	if err != nil {
		return err
	}
	store, ok := s.store.(WebStore)
	if !ok {
		return ErrUnauthenticated
	}
	return store.DeleteWebSession(ctx, hash)
}
