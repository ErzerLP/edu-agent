package operations

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAttestationKeyCreationAndPrivatePathRules(t *testing.T) {
	base := t.TempDir()
	repository := filepath.Join(base, "repository")
	evidence := filepath.Join(base, "evidence")
	for _, directory := range []string{repository, evidence} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	keyPath := filepath.Join(base, "state", "attestation.key")
	first, resolved, err := LoadOrCreateAttestor(keyPath, repository, evidence)
	if err != nil {
		t.Fatal(err)
	}
	if resolved != keyPath {
		t.Fatalf("resolved key path=%q want=%q", resolved, keyPath)
	}
	info, err := os.Stat(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 || info.Size() != attestationKeyBytes {
		t.Fatalf("key mode=%o size=%d", info.Mode().Perm(), info.Size())
	}
	entries, err := os.ReadDir(filepath.Dir(keyPath))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != filepath.Base(keyPath) {
		t.Fatalf("atomic key creation left temporary files: %v", entries)
	}
	second, _, err := LoadOrCreateAttestor(keyPath, repository, evidence)
	if err != nil {
		t.Fatal(err)
	}
	if first.keyID != second.keyID {
		t.Fatal("existing attestation key was replaced")
	}

	for name, forbidden := range map[string]string{
		"repository": filepath.Join(repository, "attestation.key"),
		"evidence":   filepath.Join(evidence, "attestation.key"),
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := LoadOrCreateAttestor(forbidden, repository, evidence); err == nil {
				t.Fatal("attestation key inside a forbidden output tree was accepted")
			}
		})
	}

	symlinkParent := filepath.Join(base, "repository-link")
	if err := os.Symlink(repository, symlinkParent); err != nil {
		t.Fatal(err)
	}
	throughMissingChild := filepath.Join(symlinkParent, "new-state", "attestation.key")
	if _, _, err := LoadOrCreateAttestor(throughMissingChild, repository, evidence); err == nil {
		t.Fatal("attestation key through a symlink ancestor into the repository was accepted")
	}
	if _, err := os.Stat(filepath.Join(repository, "new-state")); !os.IsNotExist(err) {
		t.Fatalf("forbidden key path was created before rejection: %v", err)
	}
}

func TestDefaultAttestationKeyUsesUserStateDirectory(t *testing.T) {
	base := t.TempDir()
	stateHome := filepath.Join(base, "state")
	repository := filepath.Join(base, "repository")
	evidence := filepath.Join(base, "evidence")
	for _, directory := range []string{repository, evidence} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("XDG_STATE_HOME", stateHome)
	_, path, err := LoadOrCreateAttestor("", repository, evidence)
	if err != nil {
		t.Fatal(err)
	}
	expected := filepath.Join(stateHome, "edu-agent", "operations", "attestation.key")
	if path != expected {
		t.Fatalf("default key path=%q want=%q", path, expected)
	}
	if _, _, err := LoadAttestor("", repository, evidence); err != nil {
		t.Fatalf("verifier could not load the runtime key: %v", err)
	}
}
