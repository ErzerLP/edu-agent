package config

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestLargeContextExplicitJSONMaxTokens(t *testing.T) {
	for _, field := range []string{"max_tokens", "MAX_TOKENS"} {
		for _, test := range []struct {
			value string
			valid bool
		}{
			{"0", false}, {"null", false}, {"-1", false}, {"128001", false}, {`"128000"`, false},
			{"1", true}, {"512", true}, {"128000", true},
		} {
			t.Run(field+"="+test.value, func(t *testing.T) {
				path := filepath.Join(t.TempDir(), "config.json")
				data := []byte(fmt.Sprintf(`{"agent":{"provider":"ollama","base_url":"http://127.0.0.1:11434/v1","model":"old-model","context_window":32768,"timeout":"2m","max_tool_rounds":9,"%s":%s}}`, field, test.value))
				if err := os.WriteFile(path, data, 0o600); err != nil {
					t.Fatal(err)
				}
				value, err := (Store{Path: path}).Load()
				if (err == nil) != test.valid {
					t.Fatalf("valid=%v result=%+v err=%v", test.valid, value, err)
				}
				after, readErr := os.ReadFile(path)
				if readErr != nil || !bytes.Equal(data, after) {
					t.Fatalf("load rewrote config: %v", readErr)
				}
			})
		}
	}
}
