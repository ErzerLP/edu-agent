//go:build darwin

package keybackend

import (
	"context"
	"errors"
	"time"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/darwinkeychain"
)

const nativeSecretTimeout = 5 * time.Second

func availableSecret(Locator) error {
	if !darwinkeychain.Available() {
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
	value, err := darwinkeychain.Load(ctx, locator.Service, locator.Account)
	if errors.Is(err, darwinkeychain.ErrNotFound) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", ErrUnavailable
	}
	return value, nil
}

func storeSecret(locator Locator, encoded string) error {
	if err := availableSecret(locator); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), nativeSecretTimeout)
	defer cancel()
	if err := darwinkeychain.Store(ctx, locator.Service, locator.Account, encoded); err != nil {
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
	if err := darwinkeychain.Delete(ctx, locator.Service, locator.Account); err != nil && !errors.Is(err, darwinkeychain.ErrNotFound) {
		return ErrUnavailable
	}
	return nil
}
