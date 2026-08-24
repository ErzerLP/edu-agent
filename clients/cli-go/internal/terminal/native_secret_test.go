package terminal

import (
	"bytes"
	"fmt"
	"os"
	"testing"
)

const (
	nativeSecretHelperEnvironment = "EDU_AGENT_NATIVE_SECRET_HELPER"
	nativeSecretFixture           = "native-hidden-secret-7f3a"
	nativeSecretPrompt            = "Pairing code: "
	nativeSecretSuccessMarker     = "native-secret-read-ok"
)

func TestPlatformPairSecretInput(t *testing.T) {
	if os.Getenv(nativeSecretHelperEnvironment) == "1" {
		runNativeSecretHelper(t)
		return
	}

	output, method, err := runNativeSecretProbe(nativeSecretFixture)
	if err != nil {
		t.Fatalf("native hidden-input probe failed using %s: %v", method, err)
	}
	if bytes.Contains(output, []byte(nativeSecretFixture)) {
		t.Fatal("secret was echoed by the native terminal mechanism")
	}
	if !bytes.Contains(output, []byte(nativeSecretSuccessMarker)) {
		t.Fatalf("native hidden-input helper did not report success using %s", method)
	}
	t.Logf("native_hidden_input_method=%s", method)
	t.Log("native_hidden_input_result=pass")
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
