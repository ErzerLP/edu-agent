package config

import (
	"bytes"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNotesyncAdminSettingsRoundTripAndStartupOverride(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private", "admin-settings.json")
	settings := NotesyncAdminSettings{
		Enabled: true, BaseURL: "https://notes.example.test", APIToken: strings.Repeat("n", 32),
		Vault: "learning", PathPrefix: "edu-agent", SavedAt: time.Date(2026, time.August, 30, 8, 0, 0, 123, time.UTC),
	}
	if err := SaveNotesyncAdminSettings(path, settings); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("settings mode = %o, want 600", got)
	}
	loaded, found, err := LoadNotesyncAdminSettings(path)
	if err != nil {
		t.Fatal(err)
	}
	if !found || loaded.APIToken != settings.APIToken || loaded.BaseURL != settings.BaseURL || !loaded.SavedAt.Equal(settings.SavedAt.Truncate(time.Second)) {
		t.Fatalf("unexpected settings round trip: found=%v settings=%+v", found, loaded)
	}

	values := baseEnv()
	values["ADMIN_UI_SETTINGS_FILE"] = path
	values["NOTESYNC_ENABLED"] = "true"
	values["NOTESYNC_BASE_URL"] = "https://environment.example.test"
	values["NOTESYNC_API_TOKEN"] = strings.Repeat("e", 32)
	values["NOTESYNC_VAULT"] = "environment-vault"
	values["NOTESYNC_PATH_PREFIX"] = "environment-prefix"
	cfg, err := load(env(values))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Notesync.Enabled || cfg.Notesync.BaseURL.String() != settings.BaseURL || cfg.Notesync.APIToken != settings.APIToken || cfg.Notesync.Vault != settings.Vault {
		t.Fatalf("persisted admin settings did not override startup connection: %+v", cfg.Notesync)
	}
	if cfg.AdminUI.NotesyncSource != "admin_settings" || !cfg.AdminUI.NotesyncSettingsSavedAt.Equal(settings.SavedAt.Truncate(time.Second)) {
		t.Fatalf("persisted admin settings source metadata = %q/%s", cfg.AdminUI.NotesyncSource, cfg.AdminUI.NotesyncSettingsSavedAt)
	}
}

func TestNotesyncAdminSettingsRejectUnsafeAndInvalidFiles(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "private")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "admin-settings.json")
	if err := os.WriteFile(path, []byte(`{"version":1,"notesync":{"enabled":false}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := LoadNotesyncAdminSettings(path); err == nil || !strings.Contains(err.Error(), "0600") {
		t.Fatalf("expected permissive-mode rejection, got %v", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, "missing"), path); err != nil {
		t.Fatal(err)
	}
	if _, _, err := LoadNotesyncAdminSettings(path); err == nil || !strings.Contains(err.Error(), "regular file") {
		t.Fatalf("expected symlink rejection, got %v", err)
	}

	sharedDir := filepath.Join(t.TempDir(), "shared")
	if err := os.Mkdir(sharedDir, 0o770); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(sharedDir, 0o770); err != nil {
		t.Fatal(err)
	}
	if err := SaveNotesyncAdminSettings(filepath.Join(sharedDir, "settings.json"), NotesyncAdminSettings{}); err == nil || !strings.Contains(err.Error(), "writable by group") {
		t.Fatalf("expected shared-directory rejection, got %v", err)
	}

	realDir := filepath.Join(t.TempDir(), "real")
	realPath := filepath.Join(realDir, "settings.json")
	if err := SaveNotesyncAdminSettings(realPath, NotesyncAdminSettings{}); err != nil {
		t.Fatal(err)
	}
	linkRoot := t.TempDir()
	linkDir := filepath.Join(linkRoot, "linked")
	if err := os.Symlink(realDir, linkDir); err != nil {
		t.Fatal(err)
	}
	if _, _, err := LoadNotesyncAdminSettings(filepath.Join(linkDir, "settings.json")); err == nil || !strings.Contains(err.Error(), "symbolic links") {
		t.Fatalf("expected parent-symlink rejection, got %v", err)
	}
}

func TestApplyNotesyncAdminSettingsUsesExistingRuntimeBounds(t *testing.T) {
	base := NotesyncConfig{HTTPTimeout: 4 * time.Second, WorkerInterval: 5 * time.Second, WorkerBatch: 8, ScanPageSize: 25, ScanMaxPages: 40}
	settings := NotesyncAdminSettings{Enabled: true, BaseURL: "https://notes.example.test", APIToken: strings.Repeat("s", 32), Vault: "vault", PathPrefix: "prefix"}
	cfg, err := ApplyNotesyncAdminSettings(base, settings)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HTTPTimeout != base.HTTPTimeout || cfg.WorkerInterval != base.WorkerInterval || cfg.WorkerBatch != base.WorkerBatch || cfg.ScanPageSize != base.ScanPageSize || cfg.ScanMaxPages != base.ScanMaxPages || cfg.BaseURL.String() != settings.BaseURL {
		t.Fatalf("unexpected applied config: %+v", cfg)
	}
	settings.BaseURL = "http://notes.example.test"
	if _, err := ApplyNotesyncAdminSettings(base, settings); err == nil || !strings.Contains(err.Error(), "HTTPS") {
		t.Fatalf("expected remote HTTP rejection, got %v", err)
	}
	base.AllowInsecureNonLoopback = true
	if _, err := ApplyNotesyncAdminSettings(base, settings); err != nil {
		t.Fatalf("dedicated remote HTTP override was not honored: %v", err)
	}
}

func TestAdminUIRejectsNoteSyncSecretReuseFromSettingsFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private", "admin-settings.json")
	adminToken := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{0x5a}, 32))
	if err := SaveNotesyncAdminSettings(path, NotesyncAdminSettings{
		Enabled: true, BaseURL: "https://notes.example.test", APIToken: adminToken, Vault: "vault", PathPrefix: "prefix",
	}); err != nil {
		t.Fatal(err)
	}
	values := baseEnv()
	values["ADMIN_UI_ENABLED"] = "true"
	values["ADMIN_UI_TOKEN"] = adminToken
	values["ADMIN_UI_SETTINGS_FILE"] = path
	if _, err := load(env(values)); err == nil || !strings.Contains(err.Error(), "NOTESYNC_API_TOKEN") {
		t.Fatalf("expected management-secret reuse rejection, got %v", err)
	}
}
