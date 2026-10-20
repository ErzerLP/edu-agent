package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRejectsLocalModelConnectionWithoutMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	raw := []byte(`{"agent":{"provider":"ollama","base_url":"http://127.0.0.1:11434/v1","model":"local","context_window":32768,"max_tokens":2048}}`)
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := (Store{Path: path}).Load(); err == nil {
		t.Fatal("accepted removed local connection")
	}
	after, err := os.ReadFile(path)
	if err != nil || string(after) != string(raw) {
		t.Fatal("load rewrote old settings")
	}
}

func TestGatewayStatePersistsWithBindingAndClearsOnUnpair(t *testing.T) {
	directory := t.TempDir()
	if err := os.Chmod(directory, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "config.json")
	value := Config{ServerURL: "https://server.example", DeviceID: "device", DisplayName: "CLI", Gateway: &GatewayState{ServerURL: "https://server.example", DeviceID: "device", SelectedTarget: "mentor", DestinationRevision: "destination", NoticeVersion: 1}}
	store := Store{Path: path}
	if err := store.Save(value); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.Load()
	if err != nil || loaded.Gateway == nil || *loaded.Gateway != *value.Gateway {
		t.Fatalf("gateway state not persisted: %+v %v", loaded.Gateway, err)
	}
	if loaded.WithoutPairing().Gateway != nil {
		t.Fatal("unpair kept gateway consent")
	}
	loaded.DeviceID = "other"
	if err := store.Save(loaded); err == nil {
		t.Fatal("consent rebound to another device")
	}
}
