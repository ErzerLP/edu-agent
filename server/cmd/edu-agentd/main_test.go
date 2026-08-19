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
