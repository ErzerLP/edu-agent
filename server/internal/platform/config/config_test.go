package config

import (
	"bytes"
	"encoding/base64"
	"strings"
	"testing"
	"time"
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
	if cfg.Model.Enabled || cfg.Nocturne.Enabled {
		t.Fatal("optional integrations should be disabled when no profile is configured")
	}
	if cfg.Nocturne.CandidateSweepInterval != 5*time.Minute || cfg.Nocturne.DeliverySweepInterval != 5*time.Minute || cfg.Nocturne.BackupControllerInterval != 24*time.Hour || cfg.Nocturne.BackupRetention != 30*24*time.Hour || cfg.Nocturne.WorkerLeaseDuration != 2*time.Minute {
		t.Fatalf("unexpected Nocturne defaults: %+v", cfg.Nocturne)
	}
	if cfg.Privacy.ErasureGrantTTL != 10*time.Minute || cfg.Privacy.ErasureGrantMaxAttempts != 5 {
		t.Fatalf("unexpected privacy defaults: %+v", cfg.Privacy)
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
		"DATABASE_URL":                       "mysql://localhost/db",
		"MIGRATE_ON_START":                   "perhaps",
		"PAIRING_CODE_TTL":                   "0s",
		"PAIRING_CODE_MAX_ATTEMPTS":          "zero",
		"DEVICE_RATE_LIMIT_PER_MINUTE":       "0",
		"TOKEN_LAST_USED_TOUCH_INTERVAL":     "-1s",
		"MODEL_PROBE_CACHE_TTL":              "0s",
		"PRIVACY_ERASURE_GRANT_TTL":          "0s",
		"PRIVACY_ERASURE_GRANT_MAX_ATTEMPTS": "0",
	} {
		values := baseEnv()
		values[name] = value
		if _, err := load(env(values)); err == nil {
			t.Fatalf("expected %s=%q to fail", name, value)
		}
	}
}

func TestNocturneEnabledProfileLoadsStrictConfiguration(t *testing.T) {
	values := validNocturneEnv()
	cfg, err := load(env(values))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Nocturne.Enabled || cfg.Nocturne.BaseURL.String() != values["NOCTURNE_BASE_URL"] || cfg.Nocturne.Namespace != "edu-agent" || cfg.Nocturne.Domain != "core" {
		t.Fatalf("unexpected Nocturne profile: %+v", cfg.Nocturne)
	}
	if len(cfg.Nocturne.MasterWrappingKey) != 32 || !bytes.Equal(cfg.Nocturne.MasterWrappingKey, bytes.Repeat([]byte{0x33}, 32)) {
		t.Fatal("master wrapping key was not decoded to exactly 32 bytes")
	}
	if cfg.Nocturne.WorkerBatchSize != 50 || cfg.Nocturne.DeliveryTTL != 24*time.Hour || cfg.Nocturne.WorkerLeaseDuration != 2*time.Minute {
		t.Fatalf("unexpected worker defaults: %+v", cfg.Nocturne)
	}
}

func TestNocturneProfileIsAllOrNothing(t *testing.T) {
	values := baseEnv()
	values["NOCTURNE_BASE_URL"] = "http://nocturne.internal:8000"
	if _, err := load(env(values)); err == nil || !strings.Contains(err.Error(), "NOCTURNE_ENABLED") {
		t.Fatalf("disabled partial profile accepted: %v", err)
	}
	values = validNocturneEnv()
	delete(values, "NOCTURNE_API_TOKEN")
	if _, err := load(env(values)); err == nil || !strings.Contains(err.Error(), "requires") {
		t.Fatalf("enabled partial profile accepted: %v", err)
	}
}

func TestNocturneRejectsUnsafeURLsAndSecretsWithoutEcho(t *testing.T) {
	for _, rawURL := range []string{
		"http://user:password@nocturne.internal:8000",
		"http://nocturne.internal:8000?token=secret",
		"http://nocturne.internal:8000#secret",
	} {
		values := validNocturneEnv()
		values["NOCTURNE_BASE_URL"] = rawURL
		if _, err := load(env(values)); err == nil {
			t.Fatalf("unsafe URL accepted: %s", rawURL)
		}
	}
	for name, secret := range map[string]string{
		"NOCTURNE_API_TOKEN":                  "short-api-secret",
		"NOCTURNE_MAINTENANCE_TOKEN":          "maintenance-secret-not-canonical",
		"NOCTURNE_BACKUP_MASTER_WRAPPING_KEY": "wrapping-secret-not-canonical",
	} {
		values := validNocturneEnv()
		values[name] = secret
		_, err := load(env(values))
		if err == nil || strings.Contains(err.Error(), secret) {
			t.Fatalf("secret validation for %s leaked or accepted secret: %v", name, err)
		}
	}
	values := validNocturneEnv()
	values["NOCTURNE_API_TOKEN"] = values["NOCTURNE_MAINTENANCE_TOKEN"]
	if _, err := load(env(values)); err == nil || !strings.Contains(err.Error(), "must differ") {
		t.Fatalf("identical Nocturne tokens accepted: %v", err)
	}
}

func TestNocturneRejectsInvalidWorkerAndBackupMatrix(t *testing.T) {
	for name, value := range map[string]string{
		"NOCTURNE_CANDIDATE_SWEEP_INTERVAL":   "5m1s",
		"NOCTURNE_DELIVERY_SWEEP_INTERVAL":    "6m",
		"NOCTURNE_BACKUP_CONTROLLER_INTERVAL": "24h1m",
		"NOCTURNE_BACKUP_RETENTION":           "721h",
		"NOCTURNE_WORKER_BATCH_SIZE":          "0",
		"NOCTURNE_WORKER_LEASE_DURATION":      "119s",
		"NOCTURNE_BACKUP_ROOT":                "relative/backups",
		"NOCTURNE_PG_DUMP_DSN":                "mysql://db/nocturne",
		"NOCTURNE_NAMESPACE":                  "Edu-Agent",
		"NOCTURNE_IMAGE_LOCK_REFERENCE":       "registry.example/nocturne:2.5.6",
	} {
		values := validNocturneEnv()
		values[name] = value
		if _, err := load(env(values)); err == nil {
			t.Fatalf("invalid Nocturne setting accepted: %s=%q", name, value)
		}
	}
}

func TestNocturneImageLockRequiresCanonicalDigestAndLeaseRatio(t *testing.T) {
	for _, reference := range []string{
		"registry.example/nocturne:2.5.6",
		"registry.example/nocturne@sha256:" + strings.Repeat("a", 63),
		"registry.example/nocturne@sha256:" + strings.Repeat("A", 64),
		"registry.example/nocturne:tag@sha256:" + strings.Repeat("a", 64),
		"https://registry.example/nocturne@sha256:" + strings.Repeat("a", 64),
		"registry.example/nocturne@sha256:" + strings.Repeat("b", 64),
	} {
		values := validNocturneEnv()
		values["NOCTURNE_IMAGE_LOCK_REFERENCE"] = reference
		if _, err := load(env(values)); err == nil {
			t.Fatalf("invalid image lock accepted: %q", reference)
		}
	}
	values := validNocturneEnv()
	values["NOCTURNE_HTTP_TIMEOUT"] = "11s"
	if _, err := load(env(values)); err == nil || !strings.Contains(err.Error(), "12 times") {
		t.Fatalf("unsafe timeout/lease ratio accepted: %v", err)
	}
}

func validNocturneEnv() map[string]string {
	values := baseEnv()
	values["NOCTURNE_ENABLED"] = "true"
	values["NOCTURNE_BASE_URL"] = "http://nocturne.internal:8000"
	values["NOCTURNE_API_TOKEN"] = strings.Repeat("a", 32)
	values["NOCTURNE_MAINTENANCE_TOKEN"] = encodedSecret32(0x22)
	values["NOCTURNE_NAMESPACE"] = "edu-agent"
	values["NOCTURNE_DOMAIN"] = "core"
	values["NOCTURNE_BACKUP_ROOT"] = "/var/lib/edu-agent/nocturne-backups"
	values["NOCTURNE_BACKUP_MASTER_WRAPPING_KEY"] = encodedSecret32(0x33)
	values["NOCTURNE_PG_DUMP_DSN"] = "postgres://nocturne:secret@database:5432/nocturne?sslmode=require"
	values["NOCTURNE_IMAGE_LOCK_REFERENCE"] = "registry.example/edu-agent/nocturne@sha256:908eb9533589633857daf0792f376c6e94d76803da58c2afd4b314f970291f3a"
	return values
}

func encodedSecret32(value byte) string {
	return base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{value}, 32))
}
