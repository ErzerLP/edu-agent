package offline

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"testing"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/keybackend"
)

type nativeSystemKeyProvider struct{}

func (nativeSystemKeyProvider) Generate() ([]byte, error) { return keybackend.Generate() }
func (nativeSystemKeyProvider) Load(locator string) ([]byte, error) {
	secret, err := keybackend.Load(locator)
	if errors.Is(err, keybackend.ErrNotFound) {
		return nil, ErrSystemKeyNotFound
	}
	if err != nil {
		return nil, ErrKeyBackendUnavailable
	}
	return secret, nil
}
func (nativeSystemKeyProvider) Store(locator string, secret []byte) error {
	if err := keybackend.Store(locator, secret); err != nil {
		return ErrKeyBackendUnavailable
	}
	return nil
}
func (nativeSystemKeyProvider) Delete(locator string) error {
	if err := keybackend.Delete(locator); err != nil {
		return ErrKeyBackendUnavailable
	}
	return nil
}

const nativeKeyBackendTestEnv = "EDU_AGENT_NATIVE_KEYBACKEND_TEST"

// TestNativeSystemKeyMigrationAndPurgeCleanup is an opt-in native-platform
// acceptance test. It must run on each supported OS with a real credential
// backend; ordinary package tests skip it because Linux developer and CI
// sessions do not necessarily expose a Secret Service session bus.
func TestNativeSystemKeyMigrationAndPurgeCleanup(t *testing.T) {
	if os.Getenv(nativeKeyBackendTestEnv) != "1" {
		t.Skip("native system-key backend evidence is opt-in")
	}
	if !keybackend.Available("") {
		t.Fatal("native system-key backend is unavailable")
	}

	ctx := context.Background()
	root, binding, trust, store := createTestStore(t)
	pack := testPack(testPackID, "NATIVE_SYSTEM_KEY_SECRET")
	if err := store.SavePack(ctx, pack); err != nil {
		t.Fatal(err)
	}

	account := keybackend.Account("https://native-system-key-test.invalid/", fmt.Sprintf("%s-%d", testDeviceID, os.Getpid()))
	deleted := false
	t.Cleanup(func() {
		if !deleted {
			_ = keybackend.Delete(account)
		}
	})
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	migration, err := BeginKeyMigration(ctx, root, binding, trust, testPassphrase, KeyBackendPassphrase)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := migration.Migrate(KeyMigrationOptions{
		DestinationBackend: KeyBackendSystem,
		SystemLocator:      account,
		SystemKeys:         nativeSystemKeyProvider{},
	}); err != nil {
		_ = migration.Close()
		t.Fatal(err)
	}
	if err := migration.Close(); err != nil {
		t.Fatal(err)
	}

	loadedKey, err := keybackend.Load(account)
	if err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenPassphrase(ctx, root, binding, trust, loadedKey)
	for index := range loadedKey {
		loadedKey[index] = 0
	}
	if err != nil {
		t.Fatal(err)
	}
	got, err := reopened.GetPack(ctx, pack.ID)
	if err != nil || !bytes.Equal(got.Canonical, pack.Canonical) {
		t.Fatalf("system-key reopened pack=%#v err=%v", got, err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}

	purge, err := BeginPurgeProfile(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	if purge.KeyBackend() != KeyBackendSystem {
		t.Fatalf("purge backend=%d", purge.KeyBackend())
	}
	if err := keybackend.Delete(account); err != nil {
		_ = purge.Release()
		t.Fatal(err)
	}
	deleted = true
	if _, err := keybackend.Load(account); !errors.Is(err, keybackend.ErrNotFound) {
		_ = purge.Release()
		t.Fatalf("system key remains after deletion: %v", err)
	}
	if err := purge.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("purged profile root remains: %v", err)
	}
}
