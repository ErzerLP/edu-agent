package identity

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode/utf8"
)

var defaultScopes = []string{
	"devices:read", "devices:manage", "model:probe", "knowledge:read", "knowledge:write",
	"learning:read", "learning:write", "memory:read", "memory:write", "privacy:read",
}

type Options struct {
	PairingCodeTTL         time.Duration
	PairingCodeMaxAttempts int
	LastUsedTouchInterval  time.Duration
	Random                 io.Reader
	Now                    func() time.Time
}

type Service struct {
	store                  Store
	pairingCodeTTL         time.Duration
	pairingCodeMaxAttempts int
	lastUsedTouchInterval  time.Duration
	random                 io.Reader
	now                    func() time.Time
}

func NewService(store Store, options Options) (*Service, error) {
	if store == nil {
		return nil, fmt.Errorf("identity store is required")
	}
	if options.PairingCodeTTL <= 0 || options.PairingCodeMaxAttempts <= 0 || options.LastUsedTouchInterval <= 0 {
		return nil, fmt.Errorf("identity timing and attempt limits must be positive")
	}
	if options.Random == nil {
		options.Random = rand.Reader
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	return &Service{
		store:                  store,
		pairingCodeTTL:         options.PairingCodeTTL,
		pairingCodeMaxAttempts: options.PairingCodeMaxAttempts,
		lastUsedTouchInterval:  options.LastUsedTouchInterval,
		random:                 options.Random,
		now:                    options.Now,
	}, nil
}

func (s *Service) CreatePairingCode(ctx context.Context) (string, time.Time, error) {
	lookupBytes, err := randomBytes(s.random, 16)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("generate pairing lookup: %w", err)
	}
	secret, err := randomBytes(s.random, 16)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("generate pairing secret: %w", err)
	}
	lookup := base64.RawURLEncoding.EncodeToString(lookupBytes)
	secretText := base64.RawURLEncoding.EncodeToString(secret)
	hash := sha256.Sum256(secret)
	now := s.now().UTC()
	expires := now.Add(s.pairingCodeTTL)
	if err := s.store.InsertPairingCode(ctx, PairingCodeRecord{
		LookupID: lookup, CodeHash: hash, CreatedAt: now, ExpiresAt: expires,
		MaxAttempts: s.pairingCodeMaxAttempts,
	}); err != nil {
		return "", time.Time{}, fmt.Errorf("store pairing code: %w", err)
	}
	return lookup + "." + secretText, expires, nil
}

func (s *Service) ExchangePairingCode(ctx context.Context, code, displayName string) (IssuedCredential, error) {
	displayName = strings.TrimSpace(displayName)
	if displayName == "" || !utf8.ValidString(displayName) || utf8.RuneCountInString(displayName) > 100 {
		return IssuedCredential{}, ErrInvalidInput
	}
	lookup, codeHash, err := parsePairingCode(code)
	if err != nil {
		return IssuedCredential{}, ErrInvalidPairingCode
	}
	tokenBytes, err := randomBytes(s.random, 32)
	if err != nil {
		return IssuedCredential{}, fmt.Errorf("generate device token: %w", err)
	}
	deviceID, err := randomUUID(s.random)
	if err != nil {
		return IssuedCredential{}, fmt.Errorf("generate device id: %w", err)
	}
	tokenID, err := randomUUID(s.random)
	if err != nil {
		return IssuedCredential{}, fmt.Errorf("generate token id: %w", err)
	}
	tokenText := base64.RawURLEncoding.EncodeToString(tokenBytes)
	tokenHash := sha256.Sum256(tokenBytes)
	now := s.now().UTC()
	device := Device{ID: deviceID, DisplayName: displayName, Scopes: append([]string(nil), defaultScopes...), CreatedAt: now}
	token := TokenRecord{ID: tokenID, DeviceID: deviceID, TokenHash: tokenHash, Scopes: append([]string(nil), defaultScopes...), CreatedAt: now}
	if err := s.store.ConsumePairingCode(ctx, lookup, codeHash, device, token, now); err != nil {
		if err == ErrInvalidPairingCode {
			return IssuedCredential{}, err
		}
		return IssuedCredential{}, fmt.Errorf("exchange pairing code: %w", err)
	}
	return IssuedCredential{Device: device, Token: tokenText}, nil
}

func (s *Service) Authenticate(ctx context.Context, token, requiredScope string) (Credential, error) {
	tokenBytes, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(token))
	if err != nil || len(tokenBytes) != 32 {
		return Credential{}, ErrUnauthenticated
	}
	hash := sha256.Sum256(tokenBytes)
	credential, err := s.store.FindCredentialByTokenHash(ctx, hash)
	if err != nil {
		if err == ErrNotFound {
			return Credential{}, ErrUnauthenticated
		}
		return Credential{}, fmt.Errorf("authenticate device: %w", err)
	}
	if subtle.ConstantTimeCompare(hash[:], credential.TokenHash[:]) != 1 || credential.RevokedAt != nil || credential.Device.RevokedAt != nil {
		return Credential{}, ErrUnauthenticated
	}
	if requiredScope != "" && !hasScope(credential.Scopes, requiredScope) {
		return Credential{}, ErrForbidden
	}
	now := s.now().UTC()
	if credential.LastUsedAt == nil || now.Sub(*credential.LastUsedAt) >= s.lastUsedTouchInterval {
		if err := s.store.TouchToken(ctx, credential.TokenID, now, s.lastUsedTouchInterval); err != nil {
			return Credential{}, fmt.Errorf("update token use: %w", err)
		}
		credential.LastUsedAt = &now
	}
	credential.Device.Scopes = append([]string(nil), credential.Scopes...)
	credential.Device.LastUsedAt = credential.LastUsedAt
	return credential, nil
}

func (s *Service) ListDevices(ctx context.Context) ([]Device, error) {
	return s.store.ListDevices(ctx)
}

func (s *Service) RevokeDevice(ctx context.Context, deviceID string) error {
	deviceID = strings.TrimSpace(deviceID)
	if !validUUID(deviceID) {
		return ErrInvalidInput
	}
	return s.store.RevokeDevice(ctx, deviceID, s.now().UTC())
}

func validUUID(value string) bool {
	if len(value) != 36 || value[8] != '-' || value[13] != '-' || value[18] != '-' || value[23] != '-' {
		return false
	}
	decoded := strings.ReplaceAll(value, "-", "")
	if len(decoded) != 32 {
		return false
	}
	_, err := hex.DecodeString(decoded)
	return err == nil
}

func parsePairingCode(code string) (string, [32]byte, error) {
	var empty [32]byte
	parts := strings.Split(strings.TrimSpace(code), ".")
	if len(parts) != 2 {
		return "", empty, ErrInvalidPairingCode
	}
	lookupBytes, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil || len(lookupBytes) != 16 {
		return "", empty, ErrInvalidPairingCode
	}
	secret, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || len(secret) != 16 {
		return "", empty, ErrInvalidPairingCode
	}
	return parts[0], sha256.Sum256(secret), nil
}

func hasScope(scopes []string, required string) bool {
	for _, scope := range scopes {
		if scope == required {
			return true
		}
	}
	return false
}

func randomBytes(reader io.Reader, size int) ([]byte, error) {
	value := make([]byte, size)
	if _, err := io.ReadFull(reader, value); err != nil {
		return nil, err
	}
	return value, nil
}

func randomUUID(reader io.Reader) (string, error) {
	value, err := randomBytes(reader, 16)
	if err != nil {
		return "", err
	}
	value[6] = (value[6] & 0x0f) | 0x40
	value[8] = (value[8] & 0x3f) | 0x80
	encoded := hex.EncodeToString(value)
	return encoded[0:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:32], nil
}
