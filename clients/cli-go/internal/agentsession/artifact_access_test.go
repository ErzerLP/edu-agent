package agentsession

import (
	"errors"
	"testing"
)

func TestArtifactAccessFenceWithoutSavedBlob(t *testing.T) {
	root, backend := t.TempDir(), &memorySecretBackend{}
	store := openTestStore(t, root, backend, Limits{})
	defer store.Close()
	h, _ := artifactTestSession(t, store)
	if err := h.CheckArtifactAccess(t.Context()); err != nil {
		t.Fatalf("empty artifact namespace blocked valid access: %v", err)
	}
	other := openTestStore(t, root, backend, Limits{})
	defer other.Close()
	if err := other.Clear(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := h.CheckArtifactAccess(t.Context()); !errors.Is(err, ErrPrivacyInvalidated) {
		t.Fatalf("stale generation passed the cache access fence: %v", err)
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	if err := h.CheckArtifactAccess(t.Context()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("closed handle passed the cache access fence: %v", err)
	}
	var missing *Handle
	if err := missing.CheckArtifactAccess(t.Context()); !errors.Is(err, ErrInvalid) {
		t.Fatalf("nil handle passed access fence: %v", err)
	}
}
