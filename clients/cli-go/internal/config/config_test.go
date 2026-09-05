package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestValidateServerURLSafetyMatrix(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		value     string
		insecure  bool
		want      string
		wantError bool
	}{
		{name: "default loopback", value: "http://127.0.0.1:8080/", want: DefaultServerURL},
		{name: "IPv6 loopback", value: "http://[::1]:8080", want: "http://[::1]:8080"},
		{name: "remote HTTPS", value: "https://learn.example.test/base/", want: "https://learn.example.test/base"},
		{name: "remote HTTP rejected", value: "http://learn.example.test", wantError: true},
		{name: "remote HTTP explicit", value: "http://learn.example.test", insecure: true, want: "http://learn.example.test"},
		{name: "userinfo rejected", value: "https://user:pass@example.test", wantError: true},
		{name: "query rejected", value: "https://example.test?q=1", wantError: true},
		{name: "empty query rejected", value: "https://example.test?", wantError: true},
		{name: "fragment rejected", value: "https://example.test/#x", wantError: true},
		{name: "scheme rejected", value: "file:///tmp/socket", wantError: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := ValidateServerURL(test.value, test.insecure)
			if test.wantError {
				if err == nil {
					t.Fatalf("ValidateServerURL(%q) succeeded with %q", test.value, got)
				}
				return
			}
			if err != nil || got != test.want {
				t.Fatalf("ValidateServerURL(%q) = %q, %v; want %q", test.value, got, err, test.want)
			}
		})
	}
}

func TestResolveUsesExplicitEnvironmentFileDefaultPrecedence(t *testing.T) {
	t.Parallel()
	base := Config{ServerURL: "https://file.example", DeviceID: "device", DisplayName: "Laptop", Timeout: "20s", Color: "never"}
	getenv := func(name string) string {
		values := map[string]string{"EDU_AGENT_SERVER": "https://env.example", "EDU_AGENT_TIMEOUT": "10s", "EDU_AGENT_COLOR": "auto"}
		return values[name]
	}
	resolved, err := Resolve(base, Overrides{ServerURL: "https://flag.example", Timeout: "5s", Color: "always"}, getenv)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.ServerURL != "https://flag.example" || resolved.Timeout != "5s" || resolved.Color != "always" {
		t.Fatalf("unexpected resolved config: %+v", resolved)
	}
	if duration, err := ParseTimeout(resolved.Timeout); err != nil || duration != 5*time.Second {
		t.Fatalf("timeout = %v, %v", duration, err)
	}
}

func TestPairingJournalRoundTripAndCleanup(t *testing.T) {
	t.Parallel()
	store := Store{Path: filepath.Join(t.TempDir(), "edu-agent", "config.json")}
	journal := PairingJournal{ServerURL: DefaultServerURL, DeviceID: "device-1", DisplayName: "Laptop"}
	if err := store.SavePairingJournal(journal); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.LoadPairingJournal()
	if err != nil || loaded.SchemaVersion != 1 || loaded.ServerURL != journal.ServerURL || loaded.DeviceID != journal.DeviceID {
		t.Fatalf("LoadPairingJournal = %+v, %v", loaded, err)
	}
	if err := store.DeletePairingJournal(); err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadPairingJournal(); err != ErrJournalNotFound {
		t.Fatalf("LoadPairingJournal after delete = %v", err)
	}
}

func TestConfigAllowsLocalAgentSettingsWithoutPairing(t *testing.T) {
	t.Parallel()
	agent := DefaultAgentConfig("deepseek")
	value := Config{Timeout: "30s", Color: "never", Agent: &agent}
	if err := value.Validate(); err != nil {
		t.Fatalf("unpaired agent config rejected: %v", err)
	}
	if value.HasPairingBinding() {
		t.Fatalf("unpaired config reported pairing: %+v", value)
	}

	partial := value
	partial.ServerURL = DefaultServerURL
	if err := partial.Validate(); err == nil {
		t.Fatal("partial pairing tuple accepted")
	}

	paired := value
	paired.ServerURL, paired.DeviceID, paired.DisplayName = DefaultServerURL, "device-1", "Laptop"
	if err := paired.Validate(); err != nil || !paired.HasPairingBinding() {
		t.Fatalf("paired config rejected: %+v err=%v", paired, err)
	}
	local := paired.WithoutPairing()
	if local.HasPairingBinding() || local.Agent == nil || local.Agent.Model != agent.Model {
		t.Fatalf("WithoutPairing lost local settings: %+v", local)
	}

	custom := DefaultAgentConfig("custom")
	if err := custom.Validate(); err != nil || custom.Provider != "custom" || custom.Model == "" {
		t.Fatalf("custom provider preset is unusable: %+v err=%v", custom, err)
	}
	custom.ContextWindow = 4095
	if err := custom.Validate(); err == nil {
		t.Fatal("agent context window below runtime minimum was accepted")
	}
}

func TestAgentMaxToolRoundsAllowsUnlimitedAndHasNoFixedMaximum(t *testing.T) {
	t.Parallel()
	for _, value := range []int{0, 1, 16, 60, 1_000_000} {
		agent := DefaultAgentConfig("custom")
		agent.MaxToolRounds = value
		if err := agent.Validate(); err != nil {
			t.Fatalf("MaxToolRounds=%d rejected: %v", value, err)
		}
	}
	agent := DefaultAgentConfig("custom")
	agent.MaxToolRounds = -1
	if err := agent.Validate(); err == nil {
		t.Fatal("negative MaxToolRounds accepted")
	}
	if got := DefaultAgentConfig("custom").MaxToolRounds; got != 0 {
		t.Fatalf("default MaxToolRounds=%d want unlimited", got)
	}
}

func TestAgentContextCompactionDefaultsAndValidatesStrictly(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"auto", "recent-only", "off"} {
		value := DefaultAgentConfig("openai")
		value.ContextCompaction = mode
		if err := value.Validate(); err != nil || value.ContextCompaction != mode {
			t.Fatalf("mode %q rejected or changed: %+v err=%v", mode, value, err)
		}
	}
	missing := DefaultAgentConfig("openai")
	missing.ContextCompaction = ""
	if err := missing.Validate(); err != nil || missing.ContextCompaction != DefaultAgentContextCompaction {
		t.Fatalf("missing mode did not normalize to auto: %+v err=%v", missing, err)
	}
	invalid := DefaultAgentConfig("openai")
	invalid.ContextCompaction = "automatic"
	if err := invalid.Validate(); err == nil {
		t.Fatal("invalid context compaction mode was accepted")
	}
}

func TestAgentSessionHistoryDefaultsAndValidatesStrictly(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"auto", "off"} {
		value := DefaultAgentConfig("openai")
		value.SessionHistory = mode
		if err := value.Validate(); err != nil || value.SessionHistory != mode {
			t.Fatalf("session history %q rejected or changed: %+v err=%v", mode, value, err)
		}
	}
	missing := DefaultAgentConfig("openai")
	missing.SessionHistory = ""
	if err := missing.Validate(); err != nil || missing.SessionHistory != DefaultAgentSessionHistory {
		t.Fatalf("missing session history did not normalize to auto: %+v err=%v", missing, err)
	}
	invalid := DefaultAgentConfig("openai")
	invalid.SessionHistory = "enabled"
	if err := invalid.Validate(); err == nil {
		t.Fatal("invalid session history was accepted")
	}
}

func TestAgentReasoningEffortDefaultsAndValidatesStrictly(t *testing.T) {
	t.Parallel()
	for _, effort := range []string{"auto", "none", "minimal", "low", "medium", "high", "xhigh", "max"} {
		value := DefaultAgentConfig("openai")
		value.ReasoningEffort = effort
		if err := value.Validate(); err != nil || value.ReasoningEffort != effort {
			t.Fatalf("effort %q rejected or changed: %+v err=%v", effort, value, err)
		}
	}
	missing := DefaultAgentConfig("openai")
	missing.ReasoningEffort = ""
	if err := missing.Validate(); err != nil || missing.ReasoningEffort != DefaultAgentReasoningEffort {
		t.Fatalf("missing effort did not normalize to auto: %+v err=%v", missing, err)
	}
	invalid := DefaultAgentConfig("openai")
	invalid.ReasoningEffort = "extreme"
	if err := invalid.Validate(); err == nil {
		t.Fatal("invalid reasoning effort was accepted")
	}
}

func TestStoreLoadsLegacyAgentJSONWithAutoReasoningEffort(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "edu-agent", "config.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	legacy := `{"server_url":"","device_id":"","display_name":"","timeout":"30s","color":"never","allow_insecure_http":false,"agent":{"provider":"ollama","base_url":"http://127.0.0.1:11434/v1","model":"qwen3:8b","context_window":32768,"timeout":"1m0s","max_tool_rounds":8,"context_compaction":"auto"}}`
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := (Store{Path: path}).Load()
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Agent == nil || loaded.Agent.ReasoningEffort != DefaultAgentReasoningEffort || loaded.Agent.SessionHistory != DefaultAgentSessionHistory {
		t.Fatalf("legacy agent config = %+v", loaded.Agent)
	}
}

func TestAgentAPIKeyOptionalOnlyForOllamaAndLoopbackCustom(t *testing.T) {
	t.Parallel()
	for name, testCase := range map[string]struct {
		config AgentConfig
		want   bool
	}{
		"ollama":        {config: DefaultAgentConfig("ollama"), want: true},
		"loopback":      {config: AgentConfig{Provider: "custom", BaseURL: "http://127.0.0.1:9000/v1"}, want: true},
		"localhost TLS": {config: AgentConfig{Provider: "custom", BaseURL: "https://localhost:9443/v1"}, want: true},
		"remote custom": {config: AgentConfig{Provider: "custom", BaseURL: "https://gateway.example.test/v1"}, want: false},
		"openai":        {config: DefaultAgentConfig("openai"), want: false},
	} {
		t.Run(name, func(t *testing.T) {
			if got := testCase.config.APIKeyOptional(); got != testCase.want {
				t.Fatalf("APIKeyOptional() = %t, want %t", got, testCase.want)
			}
		})
	}
}

func TestStoreStrictRoundTripAndUnknownField(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "edu-agent", "config.json")
	store := Store{Path: path}
	value := Config{ServerURL: DefaultServerURL, DeviceID: "device-1", DisplayName: "Laptop", Timeout: "30s", Color: "never"}
	if err := store.Save(value); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if loaded.DeviceID != value.DeviceID || loaded.ServerURL != value.ServerURL {
		t.Fatalf("loaded = %+v", loaded)
	}
	if err := os.WriteFile(path, []byte(`{"server_url":"http://127.0.0.1:8080","device_id":"x","display_name":"x","timeout":"30s","color":"never","allow_insecure_http":false,"unknown":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(); err == nil {
		t.Fatal("Load accepted an unknown field")
	}
}
