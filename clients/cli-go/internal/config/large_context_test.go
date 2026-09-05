package config

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestLargeContextDefaultsAndValidation(t *testing.T) {
	for _, provider := range []string{"openai", "deepseek", "openrouter", "ollama", "custom"} {
		value := DefaultAgentConfig(provider)
		if value.ContextWindow != 272000 || value.MaxTokens != 128000 {
			t.Fatalf("%s: %+v", provider, value)
		}
		for _, limit := range []int{0, 1, 512, 64000, 128000, -1, 128001} {
			candidate := value
			candidate.MaxTokens = limit
			err := candidate.Validate()
			if (err != nil) != (limit < 0 || limit > 128000) {
				t.Fatalf("limit=%d err=%v", limit, err)
			}
			if limit == 0 && candidate.MaxTokens != 128000 {
				t.Fatal("missing field not defaulted")
			}
		}
	}
}

func TestLargeContextLegacyLoadDoesNotRewriteConfiguration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	data := []byte(`{"timeout":"45s","color":"never","agent":{"provider":"ollama","base_url":"http://127.0.0.1:11434/v1","model":"old-model","context_window":32768,"timeout":"2m","max_tool_rounds":9,"context_compaction":"off","reasoning_effort":"high","session_history":"off"}}`)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	value, err := (Store{Path: path}).Load()
	if err != nil {
		t.Fatal(err)
	}
	if value.Agent.ContextWindow != 32768 || value.Agent.MaxTokens != 128000 || value.Agent.Model != "old-model" || value.Agent.Timeout != "2m" || value.Agent.MaxToolRounds != 9 || value.Agent.ContextCompaction != "off" || value.Agent.SessionHistory != "off" || value.Agent.ReasoningEffort != "high" {
		t.Fatalf("legacy changed: %+v", value.Agent)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(data, after) {
		t.Fatalf("load rewrote config: %v", err)
	}
}
