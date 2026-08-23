package privacy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"testing"
	"time"
)

type grantMemoryStore struct {
	now        time.Time
	create     ErasureGrantCreate
	consume    ErasureGrantAuthorization
	createErr  error
	consumeErr error
}

func (s *grantMemoryStore) ReplaceErasureGrant(_ context.Context, request ErasureGrantCreate) (ErasureGrant, error) {
	s.create = request
	if s.createErr != nil {
		return ErasureGrant{}, s.createErr
	}
	return ErasureGrant{
		ID: request.ID, DeviceID: request.DeviceID, TokenHash: request.TokenHash,
		CreatedAt: s.now, ExpiresAt: s.now.Add(request.TTL), MaxAttempts: request.MaxAttempts, CreatedBy: request.CreatedBy,
	}, nil
}

func (s *grantMemoryStore) ConsumeErasureGrant(_ context.Context, request ErasureGrantAuthorization) error {
	s.consume = request
	return s.consumeErr
}

func TestErasureGrantIssueUsesCanonicalHighEntropyTokenAndDefaults(t *testing.T) {
	store := &grantMemoryStore{now: time.Date(2026, 9, 2, 3, 4, 5, 0, time.UTC)}
	random := make([]byte, 48)
	for index := range random {
		random[index] = byte(index + 1)
	}
	service, err := NewErasureGrantService(store, ErasureGrantOptions{Random: bytes.NewReader(random)})
	if err != nil {
		t.Fatal(err)
	}
	deviceID := "10000000-0000-4000-8000-000000000001"
	issued, err := service.Issue(context.Background(), deviceID, "local-test")
	if err != nil {
		t.Fatal(err)
	}
	secret, err := base64.RawURLEncoding.DecodeString(issued.Token)
	if err != nil || len(secret) != 32 || base64.RawURLEncoding.EncodeToString(secret) != issued.Token {
		t.Fatalf("grant is not canonical 256-bit base64url: len=%d err=%v", len(secret), err)
	}
	expectedHash := sha256.Sum256(secret)
	if store.create.DeviceID != deviceID || store.create.TokenHash != expectedHash || store.create.TTL != DefaultErasureGrantTTL || store.create.MaxAttempts != DefaultErasureGrantMaxAttempts {
		t.Fatalf("unexpected persisted grant request: %+v", store.create)
	}
	if issued.DeviceID != deviceID || !issued.ExpiresAt.Equal(store.now.Add(DefaultErasureGrantTTL)) {
		t.Fatalf("unexpected issued grant metadata: %+v", issued)
	}
}

func TestErasureGrantRejectsNonCanonicalDeviceBeforeStore(t *testing.T) {
	store := &grantMemoryStore{now: time.Now().UTC()}
	service, err := NewErasureGrantService(store, ErasureGrantOptions{Random: bytes.NewReader(make([]byte, 48))})
	if err != nil {
		t.Fatal(err)
	}
	for _, deviceID := range []string{"not-a-uuid", "10000000-0000-4000-8000-00000000000A"} {
		if _, err := service.Issue(context.Background(), deviceID, "local-test"); ErrorCode(err) != CodeInvalidRequest {
			t.Fatalf("device %q accepted: %v", deviceID, err)
		}
	}
	if store.create.ID != "" {
		t.Fatal("invalid device reached the grant store")
	}
}

func TestErasureGrantConsumeCanonicalizesAndUsesGenericFailure(t *testing.T) {
	store := &grantMemoryStore{consumeErr: ErrErasureGrantInvalid}
	service, err := NewErasureGrantService(store, ErasureGrantOptions{})
	if err != nil {
		t.Fatal(err)
	}
	deviceID := "10000000-0000-4000-8000-000000000001"
	secret := bytes.Repeat([]byte{0x44}, 32)
	token := base64.RawURLEncoding.EncodeToString(secret)
	if err := service.Consume(context.Background(), deviceID, token); !errors.Is(err, ErrErasureGrantInvalid) {
		t.Fatalf("canonical wrong grant did not use generic error: %v", err)
	}
	if !store.consume.Canonical || store.consume.CandidateHash != sha256.Sum256(secret) {
		t.Fatalf("canonical token was not hashed from decoded bytes: %+v", store.consume)
	}
	if err := service.Consume(context.Background(), deviceID, "not-canonical=="); !errors.Is(err, ErrErasureGrantInvalid) {
		t.Fatalf("malformed grant did not use generic error: %v", err)
	}
	if store.consume.Canonical {
		t.Fatal("malformed grant was marked canonical")
	}
	if err := service.Consume(context.Background(), "not-a-device", token); !errors.Is(err, ErrErasureGrantInvalid) {
		t.Fatalf("invalid device did not use generic error: %v", err)
	}
}

func TestErasureGrantOptionsRejectNegativeValues(t *testing.T) {
	store := &grantMemoryStore{}
	for _, options := range []ErasureGrantOptions{{TTL: -time.Second}, {MaxAttempts: -1}} {
		if _, err := NewErasureGrantService(store, options); err == nil {
			t.Fatalf("negative options accepted: %+v", options)
		}
	}
}
