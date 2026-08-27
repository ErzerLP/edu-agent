package offline

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestDefaultRootAndExists(t *testing.T) {
	root, err := DefaultRoot()
	if err != nil || !filepath.IsAbs(root) || filepath.Base(root) != "offline" {
		t.Fatalf("default root = %q, %v", root, err)
	}
	missing := filepath.Join(t.TempDir(), "missing")
	exists, err := Exists(missing)
	if err != nil || exists {
		t.Fatalf("missing Exists = %v, %v", exists, err)
	}
}

func TestJournalRecoveryReturnsUploadingToQueuedAndCompletesDiscard(t *testing.T) {
	root, binding, trust, store := createTestStore(t)
	ctx := context.Background()
	if err := store.SaveImmutableOperation(ctx, testOperation(testOperationID, 1, "answer")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.BeginSync(ctx, testJournalID, []string{testOperationID}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenPassphrase(ctx, root, binding, trust, testPassphrase)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	queued, err := reopened.ListQueuedOperations(ctx, 50)
	if err != nil || len(queued) != 1 {
		t.Fatalf("upload recovery did not requeue: %#v %v", queued, err)
	}
	if _, err := reopened.readJournalForTest(testJournalID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("sync journal remains: %v", err)
	}

	journalID := "88888888-8888-4888-8888-888888888888"
	detail, _ := marshalCanonical(discardDetail{All: false, Targets: []discardTarget{{Kind: ObjectOperation, ID: testOperationID}}})
	journal := newJournal(journalID, JournalCryptoDiscard, "deleting", testOperationID, testOperationID, 1, detail)
	if err := reopened.withLease(ctx, LeaseExclusive, func() error { return reopened.writeRecordLocked(ObjectJournal, journalID, journal, false) }); err != nil {
		t.Fatal(err)
	}
	if err := reopened.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := reopened.GetOperation(ctx, testOperationID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("discard recovery retained operation: %v", err)
	}

	badID := "99999999-9999-4999-8999-999999999999"
	badDetail := json.RawMessage(`{"x":"y"}`)
	bad := newJournal(badID, JournalKind("unknown"), "mystery", badID, badID, 1, badDetail)
	if err := bad.validate(); err == nil {
		t.Fatal("unknown journal kind/state accepted")
	}
}

func (s *Store) readJournalForTest(id string) (JournalRecord, error) {
	var result JournalRecord
	err := s.withLease(context.Background(), LeaseShared, func() error { return s.readRecordLocked(ObjectJournal, id, &result) })
	return result, err
}

func TestLeaseContentionAndSharedReaders(t *testing.T) {
	root := filepath.Join(t.TempDir(), "offline")
	ctx := context.Background()
	sharedOne, err := AcquireLease(ctx, root, LeaseShared, 100*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	defer sharedOne.Close()
	sharedTwo, err := AcquireLease(ctx, root, LeaseShared, 100*time.Millisecond)
	if err != nil {
		t.Fatalf("second shared lease failed: %v", err)
	}
	_ = sharedTwo.Close()
	_, err = AcquireLease(ctx, root, LeaseExclusive, 50*time.Millisecond)
	requireErrorIs(t, err, ErrProfileBusy)
	if err := sharedOne.Close(); err != nil {
		t.Fatal(err)
	}
	exclusive, err := AcquireLease(ctx, root, LeaseExclusive, 100*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	defer exclusive.Close()
	_, err = AcquireLease(ctx, root, LeaseShared, 50*time.Millisecond)
	requireErrorIs(t, err, ErrProfileBusy)
}

func TestSymlinkAndRootEscapeAreRejected(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("native reparse-point behavior requires Windows test execution")
	}
	base := t.TempDir()
	outside := filepath.Join(base, "outside")
	if err := os.Mkdir(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	rootLink := filepath.Join(base, "root-link")
	if err := os.Symlink(outside, rootLink); err != nil {
		t.Fatal(err)
	}
	_, err := CreatePassphrase(context.Background(), rootLink, CreateOptions{Binding: testBindingValue(t), TrustState: testTrustValue(t)}, testPassphrase)
	requireErrorIs(t, err, ErrUnsafePath)

	root, _, _, store := createTestStore(t)
	packDir := filepath.Join(root, "objects", "pack")
	if err := os.Symlink(outside, packDir); err != nil {
		t.Fatal(err)
	}
	err = store.SavePack(context.Background(), testPack(testPackID, "prompt"))
	requireErrorIs(t, err, ErrUnsafePath)
	if _, err := managedPath(root, "../escape", true); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("root escape returned %v", err)
	}
}
