package darwinkeychain

import (
	"context"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"os/exec"
	"strings"
)

const (
	valuePrefix          = "edu-agent-v1."
	versionedAccountTail = ".edu-agent-v1"
)

var (
	ErrNotFound    = errors.New("keychain item not found")
	ErrUnavailable = errors.New("keychain unavailable")
)

func Available() bool {
	_, err := exec.LookPath("security")
	return err == nil
}

func Load(ctx context.Context, service, account string) (string, error) {
	versionedAccount, ok := validatedAccounts(service, account)
	if !ok {
		return "", ErrUnavailable
	}
	stored, err := loadRaw(ctx, service, versionedAccount)
	if err == nil {
		return decodeValue(stored, true)
	}
	if !errors.Is(err, ErrNotFound) {
		return "", ErrUnavailable
	}
	stored, err = loadRaw(ctx, service, account)
	if err != nil {
		return "", err
	}
	return decodeValue(stored, false)
}

func Store(ctx context.Context, service, account, value string) error {
	versionedAccount, ok := validatedAccounts(service, account)
	if !ok || value == "" {
		return ErrUnavailable
	}
	stored := valuePrefix + base64.RawURLEncoding.EncodeToString([]byte(value))
	command := storeCommand(ctx, service, versionedAccount, stored)
	if err := command.Run(); err != nil {
		return ErrUnavailable
	}
	loaded, err := Load(ctx, service, account)
	if err != nil || subtle.ConstantTimeCompare([]byte(loaded), []byte(value)) != 1 {
		return ErrUnavailable
	}
	return nil
}

func Delete(ctx context.Context, service, account string) error {
	versionedAccount, ok := validatedAccounts(service, account)
	if !ok {
		return ErrUnavailable
	}
	for _, target := range []string{versionedAccount, account} {
		if err := deleteRaw(ctx, service, target); err != nil && !errors.Is(err, ErrNotFound) {
			return ErrUnavailable
		}
	}
	return nil
}

func loadRaw(ctx context.Context, service, account string) (string, error) {
	command := exec.CommandContext(ctx, "security", "find-generic-password", "-s", service, "-a", account, "-w")
	output, err := command.Output()
	if err != nil {
		if exit, ok := err.(*exec.ExitError); ok && exit.ExitCode() == 44 {
			return "", ErrNotFound
		}
		return "", ErrUnavailable
	}
	stored := strings.TrimSpace(string(output))
	if stored == "" {
		return "", ErrNotFound
	}
	return stored, nil
}

func deleteRaw(ctx context.Context, service, account string) error {
	command := exec.CommandContext(ctx, "security", "delete-generic-password", "-s", service, "-a", account)
	if err := command.Run(); err != nil {
		if _, loadErr := loadRaw(ctx, service, account); errors.Is(loadErr, ErrNotFound) {
			return ErrNotFound
		}
		return ErrUnavailable
	}
	return nil
}

func storeCommand(ctx context.Context, service, account, stored string) *exec.Cmd {
	command := exec.CommandContext(ctx, "security", "-q", "-i")
	command.Stdin = strings.NewReader("add-generic-password -U -s " + service + " -a " + account + " -w " + stored + "\n")
	return command
}

func decodeValue(stored string, encoded bool) (string, error) {
	if !encoded {
		return stored, nil
	}
	if !strings.HasPrefix(stored, valuePrefix) {
		return "", ErrUnavailable
	}
	value := strings.TrimPrefix(stored, valuePrefix)
	decoded, err := base64.RawURLEncoding.Strict().DecodeString(value)
	if err != nil || base64.RawURLEncoding.EncodeToString(decoded) != value {
		return "", ErrUnavailable
	}
	return string(decoded), nil
}

func validatedAccounts(service, account string) (string, bool) {
	versionedAccount := account + versionedAccountTail
	if !validToken(service) || !validToken(account) || !validToken(versionedAccount) {
		return "", false
	}
	return versionedAccount, true
}

func validToken(value string) bool {
	if value == "" || len(value) > 256 {
		return false
	}
	for _, character := range value {
		if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' {
			continue
		}
		switch character {
		case '-', '_', '.':
			continue
		default:
			return false
		}
	}
	return true
}
