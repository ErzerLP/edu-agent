package agentsession

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"testing"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/keybackend"
)

func TestNativePlatformAgentSessionPrivacyClear(t *testing.T) {
	if os.Getenv("EDU_AGENT_NATIVE_KEYBACKEND_TEST") != "1" {
		t.Skip("native Agent Session key backend evidence is opt-in")
	}
	profileBytes := make([]byte, 32)
	if _, err := rand.Read(profileBytes); err != nil {
		t.Fatal(err)
	}
	profile := hex.EncodeToString(profileBytes)
	locator := keybackend.Locator{Service: profileSecretService, Account: "profile-" + profile}
	defer func() {
		if err := keybackend.DeleteSecret(locator); err != nil {
			t.Errorf("cleanup native Agent Session profile key: %v", err)
		}
	}()
	root := t.TempDir()
	store, err := Open(t.Context(), Options{Root: root, ProfileFingerprint: profile})
	if err != nil {
		t.Fatal(err)
	}
	handle, record, err := store.Create(t.Context(), CreateInput{Title: "native privacy clear", Checkpoint: []byte(`{"v":1}`)})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Clear(t.Context()); err != nil {
		t.Fatal(err)
	}
	candidate := record
	candidate.Checkpoint = []byte(`{"v":2}`)
	if _, err := handle.Save(t.Context(), record.RecordRevision, candidate); !errors.Is(err, ErrPrivacyInvalidated) {
		t.Fatalf("old writer remained valid after cryptographic clear: %v", err)
	}
	if err := handle.Close(); err != nil {
		t.Fatal(err)
	}
	if listed, err := store.List(t.Context()); err != nil || len(listed) != 0 {
		t.Fatalf("privacy clear list=%+v err=%v", listed, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(t.Context(), Options{Root: root, ProfileFingerprint: profile})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if listed, err := reopened.List(t.Context()); err != nil || len(listed) != 0 {
		t.Fatalf("reopened privacy-cleared store=%+v err=%v", listed, err)
	}
}
