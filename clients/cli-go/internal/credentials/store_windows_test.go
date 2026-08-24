//go:build windows

package credentials

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPlatformCredentialRoundTripCleanup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "edu-agent", "credential.dpapi")
	store := FileStore{Path: path}
	record := Record{ServerURL: "https://example.test", DeviceID: "device-1", Token: "token-secret"}
	if err := store.Save(record); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.Load()
	if err != nil || loaded != record {
		t.Fatalf("Load = %+v, %v", loaded, err)
	}
	if err := store.Delete(); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(); err != ErrNotFound {
		t.Fatalf("Load after delete = %v", err)
	}

	target := filepath.Join(t.TempDir(), "target.dpapi")
	if err := os.WriteFile(target, []byte("not-a-credential"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(filepath.Dir(target), "credential.dpapi")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := (FileStore{Path: link}).Load(); err == nil {
		t.Fatal("Load accepted a Windows reparse-point credential")
	}
}
