//go:build !windows

package credentials

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPlatformCredentialRoundTripCleanup(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "edu-agent", "credential.json")
	store := FileStore{Path: path}
	record := Record{ServerURL: "http://127.0.0.1:8080", DeviceID: "device-1", Token: "token-secret"}
	if err := store.Save(record); err != nil {
		t.Fatal(err)
	}
	fileInfo, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	dirInfo, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if fileInfo.Mode().Perm() != 0o600 || dirInfo.Mode().Perm() != 0o700 {
		t.Fatalf("permissions file=%04o dir=%04o", fileInfo.Mode().Perm(), dirInfo.Mode().Perm())
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
}

func TestFileStoreRejectsSymlink(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, []byte(`{"server_url":"x","device_id":"x","token":"x"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "credential.json")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	store := FileStore{Path: link}
	if _, err := store.Load(); err == nil {
		t.Fatal("Load accepted a credential symlink")
	}
	if err := store.Save(Record{ServerURL: "x", DeviceID: "x", Token: "x"}); err == nil {
		t.Fatal("Save replaced a credential symlink")
	}
}
