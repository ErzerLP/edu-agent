package agentsession

import (
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/keybackend"
)

type systemSecretBackend struct{}

func (systemSecretBackend) Available(locator keybackend.Locator) error {
	return keybackend.AvailableSecret(locator)
}
func (systemSecretBackend) Load(locator keybackend.Locator, max int) ([]byte, error) {
	return keybackend.LoadSecret(locator, max)
}
func (systemSecretBackend) Store(locator keybackend.Locator, value []byte) error {
	return keybackend.StoreSecret(locator, value)
}
func (systemSecretBackend) Delete(locator keybackend.Locator) error {
	return keybackend.DeleteSecret(locator)
}
