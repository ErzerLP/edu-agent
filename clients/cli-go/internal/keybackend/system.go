package keybackend

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
)

const (
	// ServiceOfflineV1 is retained for compatibility with the original offline
	// profile-key API.
	ServiceOfflineV1 = "edu-agent-offline-v1"
	maxSecretBytes   = 64 << 10
)

var (
	ErrUnavailable = errors.New("offline system key backend unavailable")
	ErrNotFound    = errors.New("offline system key not found")
)

// Locator is a logical native-secret identity. Service and Account are passed
// as keychain attributes, never interpolated into a shell command.
type Locator struct {
	Service string
	Account string
}

func (l Locator) validate() error {
	if !validLocatorPart(l.Service, 128) || !validLocatorPart(l.Account, 256) {
		return errors.New("系统密钥定位信息无效")
	}
	return nil
}

func validLocatorPart(value string, max int) bool {
	if value == "" || len(value) > max {
		return false
	}
	for _, current := range value {
		if current >= 'a' && current <= 'z' || current >= 'A' && current <= 'Z' || current >= '0' && current <= '9' || current == '-' || current == '_' || current == '.' {
			continue
		}
		return false
	}
	return true
}

// Generate returns a fresh 256-bit offline profile key.
func Generate() ([]byte, error) {
	value := make([]byte, 32)
	if _, err := rand.Read(value); err != nil {
		return nil, fmt.Errorf("generate offline system wrapping key: %w", err)
	}
	return value, nil
}

// Account derives the legacy offline key account from server origin and
// device identity.
func Account(normalizedServerURL, deviceID string) string {
	sum := sha256.Sum256([]byte(normalizedServerURL + "\x00" + deviceID))
	return "profile-" + hex.EncodeToString(sum[:])
}

// AvailableSecret reports whether the platform native-secret backend can be
// used for a locator. It does not create or mutate a secret.
func AvailableSecret(locator Locator) error {
	if err := locator.validate(); err != nil {
		return err
	}
	return availableSecret(locator)
}

// LoadSecret loads and decodes an opaque secret with an explicit plaintext
// size bound.
func LoadSecret(locator Locator, maxBytes int) ([]byte, error) {
	if err := locator.validate(); err != nil {
		return nil, err
	}
	if maxBytes <= 0 || maxBytes > maxSecretBytes {
		return nil, errors.New("系统密钥大小上限无效")
	}
	encoded, err := loadSecret(locator)
	if err != nil {
		return nil, err
	}
	value, err := base64.RawURLEncoding.Strict().DecodeString(encoded)
	if err != nil || base64.RawURLEncoding.EncodeToString(value) != encoded {
		clear(value)
		return nil, ErrUnavailable
	}
	if len(value) == 0 || len(value) > maxBytes {
		clear(value)
		return nil, errors.New("系统密钥长度无效")
	}
	result := append([]byte(nil), value...)
	clear(value)
	return result, nil
}

// StoreSecret atomically replaces an opaque native secret.
func StoreSecret(locator Locator, secret []byte) error {
	if err := locator.validate(); err != nil {
		return err
	}
	if len(secret) == 0 || len(secret) > maxSecretBytes {
		return errors.New("系统密钥长度无效")
	}
	return storeSecret(locator, base64.RawURLEncoding.EncodeToString(secret))
}

// DeleteSecret removes a native secret. Deleting an absent secret succeeds.
func DeleteSecret(locator Locator) error {
	if err := locator.validate(); err != nil {
		return err
	}
	return deleteSecret(locator)
}

// Available retains the original offline profile-key API. The account is
// accepted for interface compatibility but backend availability is global.
func Available(account string) bool {
	locator := Locator{Service: ServiceOfflineV1, Account: account}
	if account == "" {
		locator.Account = "availability-probe"
	}
	return AvailableSecret(locator) == nil
}

// Load retains the original fixed-size offline profile-key API.
func Load(account string) ([]byte, error) {
	value, err := LoadSecret(Locator{Service: ServiceOfflineV1, Account: account}, 32)
	if errors.Is(err, ErrNotFound) {
		return nil, err
	}
	if err != nil {
		return nil, ErrUnavailable
	}
	if len(value) != 32 {
		return nil, ErrUnavailable
	}
	return value, nil
}

// Store retains the original fixed-size offline profile-key API.
func Store(account string, key []byte) error {
	if len(key) != 32 {
		return ErrUnavailable
	}
	if err := StoreSecret(Locator{Service: ServiceOfflineV1, Account: account}, key); err != nil {
		return ErrUnavailable
	}
	return nil
}

// Delete retains the original offline profile-key API.
func Delete(account string) error {
	return DeleteSecret(Locator{Service: ServiceOfflineV1, Account: account})
}
