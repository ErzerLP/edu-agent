package privacy

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

const (
	DefaultErasureGrantTTL         = 10 * time.Minute
	DefaultErasureGrantMaxAttempts = 5
	erasureGrantBytes              = 32
)

var (
	ErrErasureGrantInvalid           = errors.New("privacy erasure grant is invalid or unavailable")
	ErrErasureGrantDeviceUnavailable = errors.New("privacy erasure grant device is unavailable")
	ErrErasureGrantStoreUnavailable  = errors.New("privacy erasure grant store is unavailable")
)

type ErasureGrant struct {
	ID          string
	DeviceID    string
	TokenHash   [sha256.Size]byte
	CreatedAt   time.Time
	ExpiresAt   time.Time
	Attempts    int
	MaxAttempts int
	CreatedBy   string
}

type ErasureGrantCreate struct {
	ID          string
	DeviceID    string
	TokenHash   [sha256.Size]byte
	TTL         time.Duration
	MaxAttempts int
	CreatedBy   string
}

func (r ErasureGrantCreate) Validate() error {
	if !CanonicalUUID(r.ID) || !CanonicalUUID(r.DeviceID) || r.TTL <= 0 || r.MaxAttempts <= 0 ||
		strings.TrimSpace(r.CreatedBy) != r.CreatedBy || r.CreatedBy == "" || !utf8.ValidString(r.CreatedBy) || utf8.RuneCountInString(r.CreatedBy) > 200 {
		return &Error{Code: CodeInvalidRequest, Reason: "invalid_erasure_grant_create"}
	}
	return nil
}

type ErasureGrantAuthorization struct {
	DeviceID      string
	CandidateHash [sha256.Size]byte
	Canonical     bool
}

func (r ErasureGrantAuthorization) Validate() error {
	if !CanonicalUUID(r.DeviceID) {
		return &Error{Code: CodeInvalidRequest, Reason: "invalid_erasure_grant_device"}
	}
	return nil
}

type ErasureGrantStore interface {
	ReplaceErasureGrant(context.Context, ErasureGrantCreate) (ErasureGrant, error)
	ConsumeErasureGrant(context.Context, ErasureGrantAuthorization) error
}

type ErasureGrantOptions struct {
	TTL         time.Duration
	MaxAttempts int
	Random      io.Reader
}

type ErasureGrantService struct {
	store       ErasureGrantStore
	ttl         time.Duration
	maxAttempts int
	random      io.Reader
}

type IssuedErasureGrant struct {
	Token     string
	DeviceID  string
	ExpiresAt time.Time
}

func NewErasureGrantService(store ErasureGrantStore, options ErasureGrantOptions) (*ErasureGrantService, error) {
	if store == nil {
		return nil, errors.New("privacy erasure grant store is required")
	}
	if options.TTL == 0 {
		options.TTL = DefaultErasureGrantTTL
	}
	if options.MaxAttempts == 0 {
		options.MaxAttempts = DefaultErasureGrantMaxAttempts
	}
	if options.TTL < 0 || options.MaxAttempts < 0 {
		return nil, errors.New("privacy erasure grant TTL and attempt budget must be positive")
	}
	if options.Random == nil {
		options.Random = rand.Reader
	}
	return &ErasureGrantService{store: store, ttl: options.TTL, maxAttempts: options.MaxAttempts, random: options.Random}, nil
}

func (s *ErasureGrantService) Issue(ctx context.Context, deviceID, createdBy string) (IssuedErasureGrant, error) {
	if !CanonicalUUID(deviceID) || strings.TrimSpace(createdBy) != createdBy || createdBy == "" || !utf8.ValidString(createdBy) || utf8.RuneCountInString(createdBy) > 200 {
		return IssuedErasureGrant{}, &Error{Code: CodeInvalidRequest, Reason: "invalid_erasure_grant_issue"}
	}
	secret := make([]byte, erasureGrantBytes)
	if _, err := io.ReadFull(s.random, secret); err != nil {
		return IssuedErasureGrant{}, errors.New("generate privacy erasure grant failed")
	}
	defer clear(secret)
	id, err := uuid.NewRandomFromReader(s.random)
	if err != nil {
		return IssuedErasureGrant{}, errors.New("generate privacy erasure grant identity failed")
	}
	hash := sha256.Sum256(secret)
	grant, err := s.store.ReplaceErasureGrant(ctx, ErasureGrantCreate{
		ID: id.String(), DeviceID: deviceID, TokenHash: hash, TTL: s.ttl,
		MaxAttempts: s.maxAttempts, CreatedBy: createdBy,
	})
	if err != nil {
		return IssuedErasureGrant{}, err
	}
	if grant.ID != id.String() || grant.DeviceID != deviceID || grant.TokenHash != hash || !grant.ExpiresAt.After(grant.CreatedAt) {
		return IssuedErasureGrant{}, ErrErasureGrantStoreUnavailable
	}
	return IssuedErasureGrant{
		Token: base64.RawURLEncoding.EncodeToString(secret), DeviceID: deviceID, ExpiresAt: grant.ExpiresAt.UTC(),
	}, nil
}

func (s *ErasureGrantService) Consume(ctx context.Context, deviceID, token string) error {
	authorization := NewErasureGrantAuthorization(deviceID, token)
	if authorization.Validate() != nil {
		return ErrErasureGrantInvalid
	}
	err := s.store.ConsumeErasureGrant(ctx, authorization)
	if errors.Is(err, ErrErasureGrantInvalid) {
		return ErrErasureGrantInvalid
	}
	return err
}

func NewErasureGrantAuthorization(deviceID, token string) ErasureGrantAuthorization {
	candidateHash, canonical := erasureGrantCandidate(token)
	return ErasureGrantAuthorization{DeviceID: deviceID, CandidateHash: candidateHash, Canonical: canonical}
}

func CanonicalUUID(value string) bool {
	parsed, err := uuid.Parse(value)
	return err == nil && parsed.String() == value
}

func erasureGrantCandidate(token string) ([sha256.Size]byte, bool) {
	decoded, err := base64.RawURLEncoding.DecodeString(token)
	if err == nil && len(decoded) == erasureGrantBytes && base64.RawURLEncoding.EncodeToString(decoded) == token {
		return sha256.Sum256(decoded), true
	}
	return sha256.Sum256([]byte(fmt.Sprintf("privacy-erasure-grant-invalid-v1\x00%s", token))), false
}
