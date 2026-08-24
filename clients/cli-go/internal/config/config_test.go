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
