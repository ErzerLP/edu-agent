package terminal

import (
	"bytes"
	"fmt"
	"os"
	"strings"
	"testing"
)

const (
	nativeSecretHelperEnvironment = "EDU_AGENT_NATIVE_SECRET_HELPER"
	nativeSecretFixture           = "7Hq!M2z#V9p$K4x@R8c%T6n&W3j^F5s"
	nativeSecretPrompt            = "Pairing code: "
	nativeSecretSuccessMarker     = "pair-read-complete"
)

func TestPlatformPairSecretInput(t *testing.T) {
	if os.Getenv(nativeSecretHelperEnvironment) == "1" {
		runNativeSecretHelper(t)
		return
	}

	probe, remainder := splitNativeSecretFixture(nativeSecretFixture)
	if markerContainsSecretFragment(nativeSecretSuccessMarker, nativeSecretFixture, 4) {
		t.Fatal("native hidden-input success marker shares a non-trivial secret fragment")
	}
	output, method, err := runNativeSecretProbe(nativeSecretFixture)
	if err != nil {
		t.Fatalf("native hidden-input probe failed using %s: %v", method, err)
	}
	for name, fragment := range map[string]string{
		"probe":     probe,
		"remainder": remainder,
		"full":      nativeSecretFixture,
	} {
		if bytes.Contains(output, []byte(fragment)) {
			t.Fatalf("native terminal output contained the %s secret fragment", name)
		}
	}
	if !bytes.Contains(output, []byte(nativeSecretSuccessMarker)) {
		t.Fatalf("native hidden-input helper did not report success using %s", method)
	}
	t.Logf("native_hidden_input_method=%s", method)
	t.Log("native_hidden_input_result=pass")
}

func splitNativeSecretFixture(secret string) (string, string) {
	split := len(secret) / 2
	if split == 0 {
		split = len(secret)
	}
	return secret[:split], secret[split:]
}

func markerContainsSecretFragment(marker, secret string, fragmentLength int) bool {
	if fragmentLength <= 0 || len(secret) < fragmentLength {
		return false
	}
	for start := 0; start+fragmentLength <= len(secret); start++ {
		if strings.Contains(marker, secret[start:start+fragmentLength]) {
			return true
		}
	}
	return false
}

func runNativeSecretHelper(t *testing.T) {
	value := New(os.Stdin, os.Stdout, os.Stderr)
	secret, err := value.ReadSecret(nativeSecretPrompt)
	if err != nil {
		t.Fatalf("read native secret: %v", err)
	}
	if secret != nativeSecretFixture {
		t.Fatal("native secret input did not match the fixture")
	}
	_, _ = fmt.Fprintln(os.Stdout, nativeSecretSuccessMarker)
}
