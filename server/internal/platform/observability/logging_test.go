package observability

import (
	"bytes"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

func TestJSONLoggerRedactsSecrets(t *testing.T) {
	var output bytes.Buffer
	logger := NewJSON(&output, slog.LevelDebug)
	logger.Info("request failed",
		"authorization", "Bearer device-secret",
		"api_key", "model-secret",
		"error", fmt.Errorf("dial postgres://user:db-secret@localhost/db with Bearer another-secret"),
	)
	text := output.String()
	for _, secret := range []string{"device-secret", "model-secret", "db-secret", "another-secret"} {
		if strings.Contains(text, secret) {
			t.Fatalf("log leaked %q: %s", secret, text)
		}
	}
	if !strings.Contains(text, redacted) {
		t.Fatalf("expected redaction marker: %s", text)
	}
}
