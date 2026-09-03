package keybackend

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"testing"
)

func TestNativePlatformSecretRoundTripCleanup(t *testing.T) {
	if os.Getenv("EDU_AGENT_NATIVE_KEYBACKEND_TEST") != "1" {
		t.Skip("native key backend evidence is opt-in")
	}
	nonce := make([]byte, 12)
	if _, err := rand.Read(nonce); err != nil {
		t.Fatal(err)
	}
	locator := Locator{Service: "edu-agent-agent-sessions-v1", Account: "native-evidence-" + hex.EncodeToString(nonce)}
	secret := make([]byte, 64)
	if _, err := rand.Read(secret); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := DeleteSecret(locator); err != nil {
			t.Errorf("cleanup native secret: %v", err)
		}
	}()
	if err := AvailableSecret(locator); err != nil {
		t.Fatalf("native key backend unavailable: %v", err)
	}
	if err := StoreSecret(locator, secret); err != nil {
		t.Fatalf("store native secret: %v", err)
	}
	loaded, err := LoadSecret(locator, len(secret))
	if err != nil || !bytes.Equal(loaded, secret) {
		t.Fatalf("native secret round trip mismatch: loaded=%d err=%v", len(loaded), err)
	}
	clear(loaded)
	replacement := make([]byte, len(secret))
	if _, err := rand.Read(replacement); err != nil {
		t.Fatal(err)
	}
	if err := StoreSecret(locator, replacement); err != nil {
		t.Fatalf("replace native secret: %v", err)
	}
	loaded, err = LoadSecret(locator, len(replacement))
	if err != nil || !bytes.Equal(loaded, replacement) {
		t.Fatalf("replacement native secret mismatch: loaded=%d err=%v", len(loaded), err)
	}
	clear(loaded)
	clear(replacement)
	if err := DeleteSecret(locator); err != nil {
		t.Fatalf("delete native secret: %v", err)
	}
	if _, err := LoadSecret(locator, len(secret)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted native secret remained readable: %v", err)
	}
}
