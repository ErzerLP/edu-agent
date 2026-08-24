//go:build !windows

package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestStoreRejectsSymlinkAndBroadPermissions(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "config.json")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	store := Store{Path: link}
	if _, err := store.Load(); err == nil {
		t.Fatal("Load accepted a symlink")
	}
	if err := store.Save(Config{ServerURL: DefaultServerURL, DeviceID: "x", DisplayName: "x", Timeout: "30s", Color: "never"}); err == nil {
		t.Fatal("Save replaced a symlink")
	}

	privateDir := filepath.Join(dir, "private")
	if err := os.Mkdir(privateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	broad := filepath.Join(privateDir, "config.json")
	if err := os.WriteFile(broad, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := (Store{Path: broad}).Load(); err == nil {
		t.Fatal("Load accepted broad file permissions")
	}
}
