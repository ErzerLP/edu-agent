//go:build linux

package keybackend

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"time"
)

const nativeSecretTimeout = 5 * time.Second

func availableSecret(Locator) error {
	if _, err := exec.LookPath("secret-tool"); err != nil || strings.TrimSpace(os.Getenv("DBUS_SESSION_BUS_ADDRESS")) == "" {
		return ErrUnavailable
	}
	return nil
}

func loadSecret(locator Locator) (string, error) {
	if err := availableSecret(locator); err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), nativeSecretTimeout)
	defer cancel()
	command := exec.CommandContext(ctx, "secret-tool", "lookup", "service", locator.Service, "account", locator.Account)
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		if stdout.Len() == 0 && strings.TrimSpace(stderr.String()) == "" {
			return "", ErrNotFound
		}
		return "", ErrUnavailable
	}
	value := strings.TrimSpace(stdout.String())
	if value == "" {
		return "", ErrNotFound
	}
	return value, nil
}

func storeSecret(locator Locator, encoded string) error {
	if err := availableSecret(locator); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), nativeSecretTimeout)
	defer cancel()
	command := exec.CommandContext(ctx, "secret-tool", "store", "--label=Edu Agent local secret", "service", locator.Service, "account", locator.Account)
	command.Stdin = strings.NewReader(encoded + "\n")
	if output, err := command.CombinedOutput(); err != nil {
		_ = output
		return ErrUnavailable
	}
	return nil
}

func deleteSecret(locator Locator) error {
	if err := availableSecret(locator); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), nativeSecretTimeout)
	defer cancel()
	command := exec.CommandContext(ctx, "secret-tool", "clear", "service", locator.Service, "account", locator.Account)
	if output, err := command.CombinedOutput(); err != nil {
		_ = output
		if _, loadErr := loadSecret(locator); errors.Is(loadErr, ErrNotFound) {
			return nil
		}
		return ErrUnavailable
	}
	return nil
}
