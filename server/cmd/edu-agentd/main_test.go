package main

import (
	"os"
	"strings"
	"testing"
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
}
