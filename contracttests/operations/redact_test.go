package operations

import (
	"bytes"
	"strings"
	"testing"
)

func TestRedactionRemovesCredentialsAndSensitiveBodies(t *testing.T) {
	input := `Authorization: Bearer abcdefghijklmnop token="token-value" pairing_code=PAIR-123 answer="private answer" knowledge_body="private knowledge" password=secret-value`
	result := RedactText(input)
	for _, secret := range []string{"abcdefghijklmnop", "token-value", "PAIR-123", "private answer", "private knowledge", "secret-value"} {
		if strings.Contains(result.Text, secret) {
			t.Fatalf("redacted output retained %q: %s", secret, result.Text)
		}
	}
	if result.Unsafe {
		t.Fatal("known labeled credentials should be redacted without an unsafe sentinel failure")
	}
}

func TestRedactionRemovesUnquotedMultiwordAndJSONValues(t *testing.T) {
	plain := RedactText("answer=private learner answer")
	if plain.Unsafe || strings.Contains(plain.Text, "private learner answer") || plain.Text != "answer=[REDACTED]" {
		t.Fatalf("multiword answer was not fully redacted: %+v", plain)
	}

	input := []byte("{\"Action\":\"output\",\"Output\":\"status answer=private learner answer\",\"payload\":{\"knowledge_body\":\"two private words\",\"token\":\"secret-token-value\"}}\n")
	output, unsafe := RedactLogLine(input)
	if unsafe {
		t.Fatal("valid JSON sensitive values should be safely redacted")
	}
	for _, secret := range []string{"private learner answer", "two private words", "secret-token-value"} {
		if bytes.Contains(output, []byte(secret)) {
			t.Fatalf("JSON redaction retained %q: %s", secret, output)
		}
	}
	if !bytes.Contains(output, []byte(`"payload":"[REDACTED]"`)) {
		t.Fatalf("JSON sensitive payload was not replaced as a complete value: %s", output)
	}
}

func TestRedactionMarksAmbiguousStructuredValueUnsafe(t *testing.T) {
	result := RedactText(`payload={"answer":"unterminated"`)
	if !result.Unsafe || result.Text != "[REDACTED unsafe log line]" {
		t.Fatalf("ambiguous structured payload was not rejected: %+v", result)
	}
}

func TestRedactionRejectsSyntheticSecretSentinel(t *testing.T) {
	var output bytes.Buffer
	unsafe, err := RedactStream(strings.NewReader("unlabeled OPERATIONS_SECRET_SENTINEL-123\n"), &output)
	if err != nil {
		t.Fatal(err)
	}
	if !unsafe {
		t.Fatal("synthetic secret sentinel was not rejected")
	}
	if strings.Contains(output.String(), "OPERATIONS_SECRET_SENTINEL") || !strings.Contains(output.String(), "[REDACTED unsafe log line]") {
		t.Fatalf("unsafe line was not safely replaced: %q", output.String())
	}
}

func TestJSONGoLogRedactionPreservesEventShape(t *testing.T) {
	input := []byte("{\"Action\":\"output\",\"Package\":\"p\",\"Test\":\"TestTarget\",\"Output\":\"Authorization: Bearer abcdefghijklmnop\\n\"}\n")
	output, unsafe := RedactLogLine(input)
	if unsafe {
		t.Fatal("known bearer value should not trigger unsafe rejection")
	}
	if bytes.Contains(output, []byte("abcdefghijklmnop")) || !bytes.Contains(output, []byte(`"Action":"output"`)) {
		t.Fatalf("JSON event redaction failed: %s", output)
	}
}
