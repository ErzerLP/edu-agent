package keybackend

import (
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestGenericLocatorAndSecretBounds(t *testing.T) {
	invalid := []Locator{
		{}, {Service: "service", Account: ""}, {Service: " service", Account: "account"},
		{Service: "service\n", Account: "account"}, {Service: "service", Account: "account\x00"},
	}
	for _, locator := range invalid {
		if err := locator.validate(); err == nil {
			t.Fatalf("accepted locator=%+v", locator)
		}
	}
	valid := Locator{Service: "edu-agent-test", Account: "profile-123"}
	if err := valid.validate(); err != nil {
		t.Fatal(err)
	}
	if err := StoreSecret(valid, nil); err == nil {
		t.Fatal("empty secret accepted")
	}
	if err := StoreSecret(valid, make([]byte, maxSecretBytes+1)); err == nil {
		t.Fatal("oversized secret accepted")
	}
	if _, err := LoadSecret(valid, 0); err == nil {
		t.Fatal("invalid load limit accepted")
	}
}

func TestLinuxGenericSecretRoundTripProtocol(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("fake secret-tool is Linux-specific")
	}
	bin := t.TempDir()
	tool := filepath.Join(bin, "secret-tool")
	payloadPath := filepath.Join(bin, "payload")
	argsPath := filepath.Join(bin, "args")
	locator := Locator{Service: "edu-agent-session-test", Account: "profile-test"}
	secret := []byte(strings.Repeat("s", 96))
	encoded := base64.RawStdEncoding.EncodeToString(secret)
	loadScript := `#!/bin/sh
if [ "$1" != "lookup" ] || [ "$2" != "service" ] || [ "$3" != "edu-agent-session-test" ] || [ "$4" != "account" ] || [ "$5" != "profile-test" ]; then
  echo bad-arguments >&2
  exit 2
fi
printf '%s\n' "$EA_TEST_SECRET"
`
	if err := os.WriteFile(tool, []byte(loadScript), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", "unix:path=/tmp/edu-agent-keybackend-generic-test")
	t.Setenv("EA_TEST_SECRET", encoded)
	loaded, err := LoadSecret(locator, len(secret))
	if err != nil || string(loaded) != string(secret) {
		t.Fatalf("loaded=%q err=%v", loaded, err)
	}
	if _, err := LoadSecret(locator, len(secret)-1); err == nil {
		t.Fatal("load bound was not enforced")
	}

	storeScript := `#!/bin/sh
printf '%s\n' "$@" > "$EA_TEST_ARGS"
/bin/cat > "$EA_TEST_PAYLOAD"
`
	if err := os.WriteFile(tool, []byte(storeScript), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("EA_TEST_ARGS", argsPath)
	t.Setenv("EA_TEST_PAYLOAD", payloadPath)
	if err := StoreSecret(locator, secret); err != nil {
		t.Fatal(err)
	}
	payload, err := os.ReadFile(payloadPath)
	if err != nil || strings.TrimSpace(string(payload)) != encoded {
		t.Fatalf("payload=%q err=%v", payload, err)
	}
	arguments, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(arguments), "edu-agent-session-test") || !strings.Contains(string(arguments), "profile-test") {
		t.Fatalf("arguments=%q", arguments)
	}

	missingScript := "#!/bin/sh\nexit 1\n"
	if err := os.WriteFile(tool, []byte(missingScript), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := DeleteSecret(locator); err != nil {
		t.Fatalf("missing delete=%v", err)
	}
	failureScript := "#!/bin/sh\necho backend-failed >&2\nexit 1\n"
	if err := os.WriteFile(tool, []byte(failureScript), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := DeleteSecret(locator); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("delete failure=%v", err)
	}
}
