package identity

import (
	"context"
	"errors"
	"time"
)

var (
	ErrInvalidPairingCode = errors.New("pairing code is invalid or expired")
	ErrUnauthenticated    = errors.New("device credentials are invalid")
	ErrForbidden          = errors.New("device does not have the required scope")
	ErrNotFound           = errors.New("device was not found")
	ErrInvalidInput       = errors.New("invalid identity input")
)

type PairingCodeRecord struct {
	LookupID    string
	CodeHash    [32]byte
	CreatedAt   time.Time
	ExpiresAt   time.Time
	Attempts    int
	MaxAttempts int
	ConsumedAt  *time.Time
}

type Device struct {
	ID          string     `json:"id"`
	DisplayName string     `json:"display_name"`
	Scopes      []string   `json:"scopes,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	LastUsedAt  *time.Time `json:"last_used_at,omitempty"`
	RevokedAt   *time.Time `json:"revoked_at,omitempty"`
}

type TokenRecord struct {
	ID         string
	DeviceID   string
	TokenHash  [32]byte
	Scopes     []string
	CreatedAt  time.Time
	LastUsedAt *time.Time
	RevokedAt  *time.Time
}

type Credential struct {
	Device     Device
	TokenID    string
	TokenHash  [32]byte
	Scopes     []string
	CreatedAt  time.Time
	LastUsedAt *time.Time
	RevokedAt  *time.Time
}

type IssuedCredential struct {
	Device Device `json:"device"`
	Token  string `json:"token"`
}

type Store interface {
	InsertPairingCode(context.Context, PairingCodeRecord) error
	ConsumePairingCode(context.Context, string, [32]byte, Device, TokenRecord, time.Time) error
	FindCredentialByTokenHash(context.Context, [32]byte) (Credential, error)
	TouchToken(context.Context, string, time.Time, time.Duration) error
	ListDevices(context.Context) ([]Device, error)
	RevokeDevice(context.Context, string, time.Time) error
}
