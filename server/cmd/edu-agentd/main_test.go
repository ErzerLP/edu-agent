package main

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/edu-agent/edu-agent/server/internal/platform/config"
)

func TestRunRejectsUnknownCommandBeforeLoadingConfig(t *testing.T) {
	originalArgs := os.Args
	os.Args = []string{"edu-agentd", "unknown"}
	t.Cleanup(func() { os.Args = originalArgs })
	t.Setenv("DATABASE_URL", "")

	err := run()
	if err == nil || !strings.Contains(err.Error(), "usage: edu-agentd") {
		t.Fatalf("expected usage error before configuration loading, got %v", err)
	}
}

func TestPrivacyGrantUsageRequiresCanonicalDeviceBeforeLoadingConfig(t *testing.T) {
	originalArgs := os.Args
	t.Cleanup(func() { os.Args = originalArgs })
	t.Setenv("DATABASE_URL", "")

	for _, args := range [][]string{
		{"edu-agentd", "privacy-grant", "create"},
		{"edu-agentd", "privacy-grant", "create", "--device"},
		{"edu-agentd", "privacy-grant", "create", "--device", "not-a-uuid"},
		{"edu-agentd", "privacy-grant", "create", "--device", "10000000-0000-4000-8000-00000000000A"},
	} {
		os.Args = args
		err := run()
		if err == nil {
			t.Fatalf("invalid privacy grant command accepted: %v", args)
		}
		if strings.Contains(err.Error(), "DATABASE_URL") {
			t.Fatalf("privacy grant usage loaded configuration before validation: %v", err)
		}
	}
}

func TestUsageDocumentsLocalPrivacyGrantCommand(t *testing.T) {
	if !strings.Contains(usage, "privacy-grant create --device <uuid>") {
		t.Fatalf("privacy grant command missing from usage: %s", usage)
	}
	if !strings.Contains(usage, "nocturne-backup restore --artifact <relative-path> --output <tmpfs-path>") {
		t.Fatalf("Nocturne restore command missing from usage: %s", usage)
	}
}

func TestNocturneBackupRestoreParsesBeforeLoadingConfiguration(t *testing.T) {
	originalArgs := os.Args
	originalLoad := loadConfiguration
	originalRestore := restoreNocturneBackup
	t.Cleanup(func() {
		os.Args = originalArgs
		loadConfiguration = originalLoad
		restoreNocturneBackup = originalRestore
	})
	t.Setenv("DATABASE_URL", "")

	for _, args := range [][]string{
		{"edu-agentd", "nocturne-backup", "restore"},
		{"edu-agentd", "nocturne-backup", "restore", "--artifact", "../escape", "--output"},
		{"edu-agentd", "nocturne-backup", "restore", "--artifact", "fixture.backup.enc"},
	} {
		os.Args = args
		if err := run(); err == nil || strings.Contains(err.Error(), "DATABASE_URL") {
			t.Fatalf("invalid restore command was not rejected before configuration: args=%v err=%v", args, err)
		}
	}

	called := false
	loadConfiguration = func() (config.Config, error) { return config.Config{}, nil }
	restoreNocturneBackup = func(_ context.Context, _ config.Config, artifact, output string) error {
		called = artifact == "fixture.backup.enc" && output == "/run/rollback/fixture.dump"
		return nil
	}
	os.Args = []string{"edu-agentd", "nocturne-backup", "restore", "--output", "/run/rollback/fixture.dump", "--artifact", "fixture.backup.enc"}
	if err := run(); err != nil || !called {
		t.Fatalf("restore command err=%v called=%v", err, called)
	}
}
