package darwinkeychain

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestStoreCommandKeepsSecretOutOfArgumentsAndSuppliesPassword(t *testing.T) {
	secret := "secret value with spaces"
	stored := valuePrefix + base64.RawURLEncoding.EncodeToString([]byte(secret))
	versionedAccount, ok := validatedAccounts("edu-agent-model-v1", "account-0123")
	if !ok {
		t.Fatal("valid account rejected")
	}
	command := storeCommand(context.Background(), "edu-agent-model-v1", versionedAccount, stored)
	arguments := strings.Join(command.Args, " ")
	if strings.Contains(arguments, secret) || strings.Contains(arguments, stored) || arguments != "security -q -i" {
		t.Fatalf("secret-bearing command arguments = %q", command.Args)
	}
	input, err := io.ReadAll(command.Stdin)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(input), secret) || !strings.Contains(string(input), " -a account-0123"+versionedAccountTail+" -w "+stored+"\n") {
		t.Fatalf("interactive keychain input = %q", input)
	}
}

func TestDecodeValueSeparatesVersionedAndLegacyEntries(t *testing.T) {
	secret := "provider-key"
	stored := valuePrefix + base64.RawURLEncoding.EncodeToString([]byte(secret))
	if value, err := decodeValue(stored, true); err != nil || value != secret {
		t.Fatalf("canonical value=%q err=%v", value, err)
	}
	for _, legacy := range []string{"legacy-key", valuePrefix + "not+", valuePrefix + "c2VjcmV0"} {
		if value, err := decodeValue(legacy, false); err != nil || value != legacy {
			t.Fatalf("legacy value=%q want=%q err=%v", value, legacy, err)
		}
	}
	if _, err := decodeValue(valuePrefix+"not+", true); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("invalid value error=%v", err)
	}
	if _, err := decodeValue("legacy-key", true); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("unmarked versioned value error=%v", err)
	}
}

func TestStoreLoadDeleteMigratesLegacyEntryWithFakeSecurity(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake security helper requires a POSIX shell")
	}
	root := t.TempDir()
	binDir := filepath.Join(root, "bin")
	stateDir := filepath.Join(root, "state")
	if err := os.MkdirAll(binDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	securityPath := filepath.Join(binDir, "security")
	if err := os.WriteFile(securityPath, []byte(fakeSecurityScript), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("EDU_AGENT_TEST_KEYCHAIN_DIR", stateDir)

	const service = "edu-agent-model-v1"
	const account = "account-migration"
	legacy := valuePrefix + "c2VjcmV0"
	if err := os.WriteFile(filepath.Join(stateDir, account), []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	if value, err := Load(t.Context(), service, account); err != nil || value != legacy {
		t.Fatalf("legacy load value=%q err=%v", value, err)
	}

	value := strings.Repeat("k", 32)
	if err := Store(t.Context(), service, account, value); err != nil {
		t.Fatalf("Store error=%v", err)
	}
	if loaded, err := Load(t.Context(), service, account); err != nil || loaded != value {
		t.Fatalf("versioned load value=%q err=%v", loaded, err)
	}
	legacyOnDisk, err := os.ReadFile(filepath.Join(stateDir, account))
	if err != nil || string(legacyOnDisk) != legacy {
		t.Fatalf("legacy entry changed value=%q err=%v", legacyOnDisk, err)
	}
	versionedAccount, _ := validatedAccounts(service, account)
	versionedOnDisk, err := os.ReadFile(filepath.Join(stateDir, versionedAccount))
	if err != nil || !strings.HasPrefix(string(versionedOnDisk), valuePrefix) || strings.Contains(string(versionedOnDisk), value) {
		t.Fatalf("versioned entry value=%q err=%v", versionedOnDisk, err)
	}

	if err := Delete(t.Context(), service, account); err != nil {
		t.Fatalf("Delete error=%v", err)
	}
	for _, target := range []string{account, versionedAccount} {
		if _, err := os.Stat(filepath.Join(stateDir, target)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("entry %q still exists: %v", target, err)
		}
	}
	if err := Delete(t.Context(), service, account); err != nil {
		t.Fatalf("idempotent Delete error=%v", err)
	}
}

const fakeSecurityScript = `#!/bin/sh
set -eu
state=${EDU_AGENT_TEST_KEYCHAIN_DIR:?}
case "$1" in
  -q)
    [ "$2" = "-i" ] || exit 2
    IFS= read -r line
    set -- $line
    [ "$1" = "add-generic-password" ] && [ "$2" = "-U" ] && [ "$3" = "-s" ] && [ "$5" = "-a" ] && [ "$7" = "-w" ] || exit 2
    printf '%s' "$8" > "$state/$6"
    ;;
  find-generic-password)
    [ "$2" = "-s" ] && [ "$4" = "-a" ] && [ "$6" = "-w" ] || exit 2
    [ -f "$state/$5" ] || exit 44
    cat "$state/$5"
    printf '\n'
    ;;
  delete-generic-password)
    [ "$2" = "-s" ] && [ "$4" = "-a" ] || exit 2
    [ -f "$state/$5" ] || exit 44
    rm "$state/$5"
    ;;
  *) exit 2 ;;
esac
`

func TestTokenValidationRejectsInteractiveCommandSyntax(t *testing.T) {
	for _, value := range []string{"", "service name", "service\nquit", "service;quit", strings.Repeat("a", 257)} {
		if validToken(value) {
			t.Fatalf("validToken(%q) = true", value)
		}
	}
	for _, value := range []string{"edu-agent-model-v1", "account_01.example"} {
		if !validToken(value) {
			t.Fatalf("validToken(%q) = false", value)
		}
	}
}
