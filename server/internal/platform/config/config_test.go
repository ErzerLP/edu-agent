package config

import (
	"strings"
	"testing"
)

func env(values map[string]string) envReader {
	return func(key string) (string, bool) {
		value, ok := values[key]
		return value, ok
	}
}

func baseEnv() map[string]string {
	return map[string]string{"DATABASE_URL": "postgres://edu:secret@localhost:5432/edu_agent"}
}

func TestLoadDefaultsToLoopback(t *testing.T) {
	cfg, err := load(env(baseEnv()))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ListenAddr != "127.0.0.1:8080" || cfg.PublicBaseURL.String() != "http://127.0.0.1:8080" || cfg.InsecureNonLoopbackWarning {
		t.Fatalf("unexpected listening policy: %+v", cfg)
	}
	if cfg.Model.Enabled {
		t.Fatal("model should be disabled when no profile is configured")
	}
}

func TestPublicBaseURLFollowsCustomLoopbackAddress(t *testing.T) {
	values := baseEnv()
	values["LISTEN_ADDR"] = "127.0.0.1:9090"
	cfg, err := load(env(values))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PublicBaseURL.String() != "http://127.0.0.1:9090" {
		t.Fatalf("unexpected derived public URL: %s", cfg.PublicBaseURL)
	}
}

func TestNonLoopbackRequiresHTTPSOrExplicitOverride(t *testing.T) {
	values := baseEnv()
	values["LISTEN_ADDR"] = "0.0.0.0:8080"
	values["PUBLIC_BASE_URL"] = "http://localhost:8080"
	_, err := load(env(values))
	if err == nil || !strings.Contains(err.Error(), "non-loopback") {
		t.Fatalf("expected non-loopback policy error, got %v", err)
	}
	values["ALLOW_INSECURE_NON_LOOPBACK"] = "true"
	cfg, err := load(env(values))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.InsecureNonLoopbackWarning {
		t.Fatal("expected persistent insecure warning")
	}
	values["ALLOW_INSECURE_NON_LOOPBACK"] = "false"
	values["PUBLIC_BASE_URL"] = "https://agent.example.test"
	if _, err := load(env(values)); err != nil {
		t.Fatalf("HTTPS public URL should be accepted: %v", err)
	}
}

func TestModelProfileIsAllOrNothingAndChecksContext(t *testing.T) {
	values := baseEnv()
	values["MODEL_BASE_URL"] = "https://model.example.test/v1"
	if _, err := load(env(values)); err == nil {
		t.Fatal("expected partial model profile to fail")
	}
	values["MODEL_NAME"] = "fake"
	values["MODEL_API_KEY"] = "placeholder"
	values["MODEL_CONTEXT_WINDOW"] = "2048"
	if _, err := load(env(values)); err == nil || !strings.Contains(err.Error(), "at least") {
		t.Fatalf("expected context error, got %v", err)
	}
	values["MODEL_CONTEXT_WINDOW"] = "8192"
	cfg, err := load(env(values))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Model.Enabled || cfg.Model.ContextWindow != 8192 {
		t.Fatalf("unexpected model profile: %+v", cfg.Model)
	}
}

func TestRejectsMalformedValues(t *testing.T) {
	for name, value := range map[string]string{
		"DATABASE_URL":                   "mysql://localhost/db",
		"MIGRATE_ON_START":               "perhaps",
		"PAIRING_CODE_TTL":               "0s",
		"PAIRING_CODE_MAX_ATTEMPTS":      "zero",
		"TOKEN_LAST_USED_TOUCH_INTERVAL": "-1s",
		"MODEL_PROBE_CACHE_TTL":          "0s",
	} {
		values := baseEnv()
		values[name] = value
		if _, err := load(env(values)); err == nil {
			t.Fatalf("expected %s=%q to fail", name, value)
		}
	}
}
