package identity

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"time"
)

const WebOfflineTTL = 37 * 24 * time.Hour

// WebOfflinePrincipal 不提供通用学习登录；旧代次只允许完成设备清除。
type WebOfflinePrincipal struct {
	WebPrincipal
	ContentAllowed bool
}

type WebOfflineStore interface {
	CreateWebOfflineSession(context.Context, [32]byte, [32]byte, time.Time, time.Time) error
	FindWebOfflineSession(context.Context, [32]byte, time.Time) (WebOfflinePrincipal, error)
	DeleteWebOfflineSession(context.Context, [32]byte) error
	RenewWebOfflineSession(context.Context, [32]byte, time.Time, time.Time) error
}

func (s *Service) EnableWebOffline(ctx context.Context, webCookie string) (string, WebOfflinePrincipal, error) {
	store, ok := s.store.(WebOfflineStore)
	if !ok {
		return "", WebOfflinePrincipal{}, ErrUnauthenticated
	}
	source, err := webHash(webCookie)
	if err != nil {
		return "", WebOfflinePrincipal{}, err
	}
	raw, err := randomBytes(s.random, 32)
	if err != nil {
		return "", WebOfflinePrincipal{}, err
	}
	cookie := base64.RawURLEncoding.EncodeToString(raw)
	now := s.now().UTC()
	if err := store.CreateWebOfflineSession(ctx, source, sha256.Sum256(raw), now, now.Add(WebOfflineTTL)); err != nil {
		return "", WebOfflinePrincipal{}, err
	}
	p, err := s.AuthenticateWebOffline(ctx, cookie)
	return cookie, p, err
}

func (s *Service) AuthenticateWebOffline(ctx context.Context, cookie string) (WebOfflinePrincipal, error) {
	store, ok := s.store.(WebOfflineStore)
	if !ok {
		return WebOfflinePrincipal{}, ErrUnauthenticated
	}
	hash, err := webHash(cookie)
	if err != nil {
		return WebOfflinePrincipal{}, err
	}
	p, err := store.FindWebOfflineSession(ctx, hash, s.now().UTC())
	if errors.Is(err, ErrNotFound) {
		err = ErrUnauthenticated
	}
	return p, err
}

func (s *Service) DisableWebOffline(ctx context.Context, cookie string) error {
	store, ok := s.store.(WebOfflineStore)
	if !ok {
		return ErrUnauthenticated
	}
	hash, err := webHash(cookie)
	if err != nil {
		return err
	}
	return store.DeleteWebOfflineSession(ctx, hash)
}

// 只有明确的新下载操作可以续期；后台查询、同步和普通登录都不延长专用身份。
func (s *Service) RenewWebOffline(ctx context.Context, cookie string) (WebOfflinePrincipal, error) {
	store, ok := s.store.(WebOfflineStore)
	if !ok {
		return WebOfflinePrincipal{}, ErrUnauthenticated
	}
	hash, err := webHash(cookie)
	if err != nil {
		return WebOfflinePrincipal{}, err
	}
	now := s.now().UTC()
	if err := store.RenewWebOfflineSession(ctx, hash, now, now.Add(WebOfflineTTL)); err != nil {
		return WebOfflinePrincipal{}, err
	}
	return s.AuthenticateWebOffline(ctx, cookie)
}
