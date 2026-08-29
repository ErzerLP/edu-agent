package modelsecret

import (
	"errors"
	"testing"
)

type fakeBackend struct {
	values    map[string]string
	getErr    error
	setErr    error
	deleteErr error
}

func (b *fakeBackend) Get(_, account string) (string, error) {
	if b.getErr != nil {
		return "", b.getErr
	}
	value, ok := b.values[account]
	if !ok {
		return "", errBackendNotFound
	}
	return value, nil
}
func (b *fakeBackend) Set(_, account, value string) error {
	if b.setErr == nil {
		if b.values == nil {
			b.values = make(map[string]string)
		}
		b.values[account] = value
	}
	return b.setErr
}
func (b *fakeBackend) Delete(_, account string) error {
	if b.deleteErr == nil {
		delete(b.values, account)
	}
	return b.deleteErr
}

func TestStoreScopesSecretsToProviderEndpointBinding(t *testing.T) {
	backend := &fakeBackend{}
	store := NewWithBackend(backend)
	openAI := Binding("openai", "https://api.openai.com/v1/")
	deepSeek := Binding("deepseek", "https://api.deepseek.com/v1")
	if openAI == deepSeek || openAI != Binding(" OPENAI ", "https://api.openai.com/v1") {
		t.Fatalf("bindings openai=%q deepseek=%q", openAI, deepSeek)
	}
	if err := store.Save(openAI, "openai-token"); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(deepSeek, "deepseek-token"); err != nil {
		t.Fatal(err)
	}
	if value, err := store.Load(openAI); err != nil || value != "openai-token" {
		t.Fatalf("openai load value=%q err=%v", value, err)
	}
	if value, err := store.Load(deepSeek); err != nil || value != "deepseek-token" {
		t.Fatalf("deepseek load value=%q err=%v", value, err)
	}
	if err := store.Delete(openAI); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(openAI); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted binding error=%v", err)
	}
	if value, err := store.Load(deepSeek); err != nil || value != "deepseek-token" {
		t.Fatalf("other binding changed value=%q err=%v", value, err)
	}
}

func TestStoreMapsBackendFailures(t *testing.T) {
	binding := Binding("openai", "https://api.openai.com/v1")
	backend := &fakeBackend{getErr: errBackendNotFound}
	store := NewWithBackend(backend)
	if _, err := store.Load(binding); !errors.Is(err, ErrNotFound) {
		t.Fatalf("not found error=%v", err)
	}
	backend.getErr = errors.New("backend unavailable")
	if _, err := store.Load(binding); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("unavailable error=%v", err)
	}
}

func TestStoreRejectsInvalidBindingOrSecret(t *testing.T) {
	store := NewWithBackend(&fakeBackend{})
	binding := Binding("openai", "https://api.openai.com/v1")
	for _, value := range []string{"", "line-one\nline-two", "line-one\rline-two"} {
		if err := store.Save(binding, value); !errors.Is(err, ErrUnavailable) {
			t.Fatalf("Save(%q) error=%v", value, err)
		}
	}
	if err := store.Save("not-a-binding", "model-token"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("invalid binding error=%v", err)
	}
}
