package keybackend

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestAccountIsStableAndSeparatesProfiles(t *testing.T) {
	first := Account("https://example.test/api", "10000000-0000-4000-8000-000000000001")
	repeated := Account("https://example.test/api", "10000000-0000-4000-8000-000000000001")
	otherDevice := Account("https://example.test/api", "10000000-0000-4000-8000-000000000002")
	otherOrigin := Account("https://other.test/api", "10000000-0000-4000-8000-000000000001")
	if first != repeated {
		t.Fatalf("same profile produced different accounts: %q != %q", first, repeated)
	}
	if first == otherDevice || first == otherOrigin {
		t.Fatal("different profiles produced the same account")
	}
}

func TestLinuxLoadDistinguishesMissingEntryFromBackendFailure(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("fake secret-tool is Linux-specific")
	}
	bin := t.TempDir()
	tool := filepath.Join(bin, "secret-tool")
	script := `#!/bin/sh
if [ "$EDU_AGENT_SECRET_TOOL_MODE" = "missing" ]; then
  exit 1
fi
echo 'secret service unavailable' >&2
exit 1
`
	if err := os.WriteFile(tool, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", "unix:path=/tmp/edu-agent-keybackend-test")
	t.Setenv("EDU_AGENT_SECRET_TOOL_MODE", "missing")
	if _, err := Load("account"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing entry error=%v", err)
	}
	t.Setenv("EDU_AGENT_SECRET_TOOL_MODE", "unavailable")
	if _, err := Load("account"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("backend failure error=%v", err)
	}
}

func TestGenerateReturnsFreshCanonicalKey(t *testing.T) {
	first, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	second, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 32 || len(second) != 32 {
		t.Fatalf("generated key lengths=%d,%d", len(first), len(second))
	}
	if string(first) == string(second) {
		t.Fatal("generated keys unexpectedly match")
	}
}
