package modelsecret

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"unicode/utf8"
)

const (
	serviceName = "edu-agent-model-v1"
	maxKeyBytes = 16 << 10
)

var (
	ErrNotFound    = errors.New("model API key not found")
	ErrUnavailable = errors.New("model API key store unavailable")
)

type Backend interface {
	Get(service, account string) (string, error)
	Set(service, account, value string) error
	Delete(service, account string) error
}

type Store struct{ backend Backend }

type systemBackend struct{}

func New() Store { return Store{backend: systemBackend{}} }

func NewWithBackend(backend Backend) Store { return Store{backend: backend} }

// Binding scopes a credential to one provider endpoint without exposing either
// value in the platform credential account name.
func Binding(provider, baseURL string) string {
	value := strings.ToLower(strings.TrimSpace(provider)) + "\x00" + strings.TrimRight(strings.TrimSpace(baseURL), "/")
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}

func (s Store) Load(binding string) (string, error) {
	if s.backend == nil || !validBinding(binding) {
		return "", ErrUnavailable
	}
	value, err := s.backend.Get(serviceName, binding)
	if errors.Is(err, errBackendNotFound) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", ErrUnavailable
	}
	if !valid(value) {
		return "", ErrUnavailable
	}
	return value, nil
}

func (s Store) Save(binding, value string) error {
	if s.backend == nil || !validBinding(binding) || !valid(value) {
		return ErrUnavailable
	}
	if err := s.backend.Set(serviceName, binding, value); err != nil {
		return ErrUnavailable
	}
	return nil
}

func (s Store) Delete(binding string) error {
	if s.backend == nil || !validBinding(binding) {
		return ErrUnavailable
	}
	err := s.backend.Delete(serviceName, binding)
	if errors.Is(err, errBackendNotFound) {
		return nil
	}
	if err != nil {
		return ErrUnavailable
	}
	return nil
}

func validBinding(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil && value == strings.ToLower(value)
}

func valid(value string) bool {
	return value != "" && len(value) <= maxKeyBytes && utf8.ValidString(value) &&
		strings.IndexFunc(value, func(r rune) bool { return r == '\r' || r == '\n' || r == 0 }) < 0
}
