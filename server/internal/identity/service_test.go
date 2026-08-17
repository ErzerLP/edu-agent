package identity

import (
	"context"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"sync"
	"testing"
	"time"
)

type memoryStore struct {
	mu      sync.Mutex
	codes   map[string]PairingCodeRecord
	devices map[string]Device
	tokens  map[[32]byte]TokenRecord
}

func newMemoryStore() *memoryStore {
	return &memoryStore{codes: map[string]PairingCodeRecord{}, devices: map[string]Device{}, tokens: map[[32]byte]TokenRecord{}}
}

func (m *memoryStore) InsertPairingCode(_ context.Context, code PairingCodeRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.codes[code.LookupID] = code
	return nil
}

func (m *memoryStore) ConsumePairingCode(_ context.Context, lookup string, hash [32]byte, device Device, token TokenRecord, now time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	code, ok := m.codes[lookup]
	if !ok || code.ConsumedAt != nil || !now.Before(code.ExpiresAt) || code.Attempts >= code.MaxAttempts {
		return ErrInvalidPairingCode
	}
	if subtle.ConstantTimeCompare(code.CodeHash[:], hash[:]) != 1 {
		code.Attempts++
		m.codes[lookup] = code
		return ErrInvalidPairingCode
	}
	code.ConsumedAt = &now
	m.codes[lookup] = code
	m.devices[device.ID] = device
	m.tokens[token.TokenHash] = token
	return nil
}

func (m *memoryStore) FindCredentialByTokenHash(_ context.Context, hash [32]byte) (Credential, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	token, ok := m.tokens[hash]
	if !ok {
		return Credential{}, ErrNotFound
	}
	device := m.devices[token.DeviceID]
	return Credential{Device: device, TokenID: token.ID, TokenHash: token.TokenHash, Scopes: token.Scopes, CreatedAt: token.CreatedAt, LastUsedAt: token.LastUsedAt, RevokedAt: token.RevokedAt}, nil
}

func (m *memoryStore) TouchToken(_ context.Context, id string, now time.Time, _ time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for hash, token := range m.tokens {
		if token.ID == id {
			token.LastUsedAt = &now
			m.tokens[hash] = token
			return nil
		}
	}
	return ErrNotFound
}

func (m *memoryStore) ListDevices(context.Context) ([]Device, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	result := make([]Device, 0, len(m.devices))
	for _, device := range m.devices {
		result = append(result, device)
	}
	return result, nil
}

func (m *memoryStore) RevokeDevice(_ context.Context, id string, now time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	device, ok := m.devices[id]
	if !ok {
		return ErrNotFound
	}
	device.RevokedAt = &now
	m.devices[id] = device
	for hash, token := range m.tokens {
		if token.DeviceID == id {
			token.RevokedAt = &now
			m.tokens[hash] = token
		}
	}
	return nil
}

func testService(t *testing.T, store Store, now *time.Time) *Service {
	t.Helper()
	service, err := NewService(store, Options{
		PairingCodeTTL: time.Minute, PairingCodeMaxAttempts: 3,
		LastUsedTouchInterval: time.Minute, Now: func() time.Time { return *now },
	})
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func TestPairingLifecycleAndTokenRevocation(t *testing.T) {
	now := time.Date(2026, 8, 17, 0, 0, 0, 0, time.UTC)
	store := newMemoryStore()
	service := testService(t, store, &now)
	code, expires, err := service.CreatePairingCode(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	parts := splitCode(t, code)
	lookupBytes, _ := base64.RawURLEncoding.DecodeString(parts[0])
	secretBytes, _ := base64.RawURLEncoding.DecodeString(parts[1])
	if len(lookupBytes) != 16 || len(secretBytes) != 16 || !expires.Equal(now.Add(time.Minute)) {
		t.Fatalf("pairing code does not meet entropy/TTL requirements")
	}

	issued, err := service.ExchangePairingCode(context.Background(), code, "Laptop")
	if err != nil {
		t.Fatal(err)
	}
	tokenBytes, err := base64.RawURLEncoding.DecodeString(issued.Token)
	if err != nil || len(tokenBytes) != 32 {
		t.Fatalf("token must contain 256 bits: %v", err)
	}
	if _, err := service.ExchangePairingCode(context.Background(), code, "Replay"); !errors.Is(err, ErrInvalidPairingCode) {
		t.Fatalf("pairing code replay should fail: %v", err)
	}
	credential, err := service.Authenticate(context.Background(), issued.Token, "devices:read")
	if err != nil || credential.Device.ID != issued.Device.ID {
		t.Fatalf("authenticate issued token: %v", err)
	}
	if err := service.RevokeDevice(context.Background(), issued.Device.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Authenticate(context.Background(), issued.Token, "devices:read"); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("revoked token should fail immediately: %v", err)
	}
}

func TestWrongSecretsConsumeAttemptBudget(t *testing.T) {
	now := time.Now().UTC()
	store := newMemoryStore()
	service := testService(t, store, &now)
	code, _, err := service.CreatePairingCode(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	parts := splitCode(t, code)
	wrong := parts[0] + "." + base64.RawURLEncoding.EncodeToString(make([]byte, 16))
	for range 3 {
		if _, err := service.ExchangePairingCode(context.Background(), wrong, "Laptop"); !errors.Is(err, ErrInvalidPairingCode) {
			t.Fatalf("wrong code should use generic pairing error: %v", err)
		}
	}
	if _, err := service.ExchangePairingCode(context.Background(), code, "Laptop"); !errors.Is(err, ErrInvalidPairingCode) {
		t.Fatalf("attempt budget should block valid code: %v", err)
	}
}

func TestRevokeDeviceRejectsInvalidUUID(t *testing.T) {
	now := time.Now().UTC()
	service := testService(t, newMemoryStore(), &now)
	if err := service.RevokeDevice(context.Background(), "not-a-uuid"); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("expected invalid input, got %v", err)
	}
}

func TestPairingCodeExpiresAndScopeIsEnforced(t *testing.T) {
	now := time.Now().UTC()
	store := newMemoryStore()
	service := testService(t, store, &now)
	code, _, _ := service.CreatePairingCode(context.Background())
	now = now.Add(2 * time.Minute)
	if _, err := service.ExchangePairingCode(context.Background(), code, "Late"); !errors.Is(err, ErrInvalidPairingCode) {
		t.Fatalf("expired code should fail: %v", err)
	}

	now = now.Add(-2 * time.Minute)
	code, _, _ = service.CreatePairingCode(context.Background())
	issued, _ := service.ExchangePairingCode(context.Background(), code, "Scoped")
	if _, err := service.Authenticate(context.Background(), issued.Token, "admin:unknown"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("missing scope should fail: %v", err)
	}
}

func splitCode(t *testing.T, code string) []string {
	t.Helper()
	for i := range code {
		if code[i] == '.' {
			return []string{code[:i], code[i+1:]}
		}
	}
	t.Fatal("pairing code separator missing")
	return nil
}
