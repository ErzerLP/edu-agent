package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestModelSettingsHaveNoProductCeilings(t *testing.T) {
	for _, timeout := range []string{"10m", "30m", "24h", "2562047h47m16.854775807s"} {
		t.Run(timeout, func(t *testing.T) {
			agent := DefaultAgentConfig("ollama")
			agent.ContextWindow = int(^uint(0) >> 1)
			agent.MaxTokens = agent.ContextWindow
			agent.Timeout = timeout
			directory := t.TempDir()
			if err := os.Chmod(directory, 0o700); err != nil {
				t.Fatal(err)
			}
			store := Store{Path: filepath.Join(directory, "config.json")}
			if err := store.Save(Config{Agent: &agent}); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(store.Path)
			if err != nil {
				t.Fatal(err)
			}
			loaded, err := store.Load()
			if err != nil || *loaded.Agent != agent {
				t.Fatalf("configured values changed: %+v err=%v", loaded.Agent, err)
			}
			after, err := os.ReadFile(store.Path)
			if err != nil || string(after) != string(before) {
				t.Fatal("load changed the saved configuration")
			}
		})
	}
	for _, timeout := range []string{"0s", "-1s", "forever", "2562047h47m16.854775808s"} {
		agent := DefaultAgentConfig("ollama")
		agent.Timeout = timeout
		if err := agent.Validate(); err == nil {
			t.Fatalf("accepted invalid duration %q", timeout)
		}
	}
}
