package offline

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

const (
	testDeviceID     = "11111111-1111-4111-8111-111111111111"
	testPackID       = "22222222-2222-4222-8222-222222222222"
	testPackIDTwo    = "33333333-3333-4333-8333-333333333333"
	testOperationID  = "44444444-4444-4444-8444-444444444444"
	testOperationTwo = "55555555-5555-4555-8555-555555555555"
	testSubmissionID = "66666666-6666-4666-8666-666666666666"
	testJournalID    = "77777777-7777-4777-8777-777777777777"
)

var testPassphrase = []byte("correct horse battery staple")

func testBindingValue(t *testing.T) Binding {
	t.Helper()
	binding, err := NewBinding("https://example.test/", testDeviceID, 7)
	if err != nil {
		t.Fatal(err)
	}
	return binding
}

func testTrustValue(t *testing.T) TrustState {
	t.Helper()
	trust, err := NewTrustState(json.RawMessage(`{"manifest_digest":"abc","manifest_revision":"1","payload":{"issuer":"edu-agent"}}`))
	if err != nil {
		t.Fatal(err)
	}
	return trust
}

func createTestStore(t *testing.T) (string, Binding, TrustState, *Store) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "offline")
	binding := testBindingValue(t)
	trust := testTrustValue(t)
	store, err := CreatePassphrase(context.Background(), root, CreateOptions{Binding: binding, TrustState: trust, LeaseTimeout: 200 * time.Millisecond}, testPassphrase)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return root, binding, trust, store
}

func testPack(id, secret string) Pack {
	return Pack{ID: id, EligibleUntil: time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC), ArchiveUntil: time.Date(2030, 2, 1, 0, 0, 0, 0, time.UTC), ItemCount: 2, Canonical: json.RawMessage(`{"pack_id":"` + id + `","prompt":"` + secret + `"}`)}
}

func testOperation(id string, sequence uint64, secret string) QueuedOperation {
	return QueuedOperation{ID: id, SubmissionID: testSubmissionID, PackID: testPackID, DeviceSequence: sequence, QueuedAt: time.Date(2028, 1, 1, 0, 0, 0, 0, time.UTC), Canonical: json.RawMessage(`{"answer":"` + secret + `","operation_id":"` + id + `"}`)}
}

func requireErrorIs(t *testing.T, err, target error) {
	t.Helper()
	if !errors.Is(err, target) {
		t.Fatalf("got error %v, want %v", err, target)
	}
}

func readObjectFile(t *testing.T, root string, kind ObjectKind, id string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(objectRelative(kind, id))))
	if err != nil {
		t.Fatal(err)
	}
	return data
}
