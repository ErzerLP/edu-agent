package offline

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"testing"
	"time"
)

func TestNonceReservationCrashGapRollbackDuplicateAndOverflow(t *testing.T) {
	root, binding, trust, store := createTestStore(t)
	ctx := context.Background()
	var gapCounter uint64
	if err := store.withLease(ctx, LeaseExclusive, func() error {
		nonce, err := store.reserveNonceLocked()
		if err != nil {
			return err
		}
		_, gapCounter = nonceParts(nonce)
		return nil // simulate crash after durable reservation and before object publish
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.SavePack(ctx, testPack(testPackID, "prompt")); err != nil {
		t.Fatal(err)
	}
	packBytes := readObjectFile(t, root, ObjectPack, testPackID)
	packHeader, err := DecodeObjectHeader(packBytes[:ObjectHeaderSize])
	if err != nil {
		t.Fatal(err)
	}
	_, packCounter := nonceParts(packHeader.Nonce)
	if packCounter <= gapCounter {
		t.Fatalf("reserved crash gap was reused: gap=%d pack=%d", gapCounter, packCounter)
	}

	oldCounter := readObjectFile(t, root, ObjectJournal, store.binding.ProfileUUID())
	if err := store.SavePack(ctx, testPack(testPackIDTwo, "second")); err != nil {
		t.Fatal(err)
	}
	counterPath := objectRelative(ObjectJournal, store.binding.ProfileUUID())
	if err := atomicWriteManaged(root, counterPath, oldCounter, true); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	_, err = OpenPassphrase(ctx, root, binding, trust, testPassphrase)
	requireErrorIs(t, err, ErrCounterRollback)

	// Restore the latest counter by rebuilding a separate store for duplicate and overflow checks.
	_, _, _, duplicateStore := createTestStore(t)
	profileBytes := readObjectFile(t, duplicateStore.root, ObjectProfile, duplicateStore.binding.ProfileUUID())
	profileHeader, err := DecodeObjectHeader(profileBytes[:ObjectHeaderSize])
	if err != nil {
		t.Fatal(err)
	}
	duplicateID, _ := parseUUID(testPackID)
	plain, _ := marshalCanonical(PackRecord{RecordVersion: 1, PackID: testPackID, EligibleUntil: "2030-01-01T00:00:00Z", ArchiveUntil: "2030-02-01T00:00:00Z", ItemCount: 1, CanonicalBytes: json.RawMessage(`{"x":"y"}`)})
	duplicateHeader := NewObjectHeader(duplicateStore.binding, ObjectPack, duplicateID, profileHeader.Nonce, uint64(len(plain)+16))
	duplicateContainer, err := sealContainer(duplicateStore.dek, duplicateHeader, plain)
	zeroBytes(plain)
	if err != nil {
		t.Fatal(err)
	}
	if err := atomicWriteManaged(duplicateStore.root, objectRelative(ObjectPack, testPackID), duplicateContainer, false); err != nil {
		t.Fatal(err)
	}
	zeroBytes(duplicateContainer)
	if err := duplicateStore.withLease(ctx, LeaseExclusive, func() error { _, err := duplicateStore.scanNonceStateLocked(); return err }); !errors.Is(err, ErrCounterRollback) {
		t.Fatalf("duplicate nonce returned %v", err)
	}

	_, _, _, overflowStore := createTestStore(t)
	if err := overflowStore.withLease(ctx, LeaseExclusive, func() error {
		state, err := overflowStore.loadCounterStateLocked()
		if err != nil {
			return err
		}
		nonce, _ := nonceFor(state.prefix, math.MaxUint64-1)
		detail, _ := marshalCanonical(counterDetail{NoncePrefix: hex.EncodeToString(state.prefix[:]), HighWater: canonicalUint(math.MaxUint64)})
		journal := newJournal(overflowStore.binding.ProfileUUID(), JournalCounterReservation, "reserved", overflowStore.binding.ProfileUUID(), overflowStore.binding.ProfileUUID(), math.MaxUint64, detail)
		plain, _ := marshalCanonical(journal)
		header := NewObjectHeader(overflowStore.binding, ObjectJournal, overflowStore.binding.ProfileID, nonce, uint64(len(plain)+16))
		container, sealErr := sealContainer(overflowStore.dek, header, plain)
		zeroBytes(plain)
		if sealErr != nil {
			return sealErr
		}
		defer zeroBytes(container)
		return atomicWriteManaged(overflowStore.root, objectRelative(ObjectJournal, overflowStore.binding.ProfileUUID()), container, true)
	}); err != nil {
		t.Fatal(err)
	}
	err = overflowStore.SavePack(ctx, Pack{ID: testPackID, EligibleUntil: time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC), ArchiveUntil: time.Date(2030, 2, 1, 0, 0, 0, 0, time.UTC), ItemCount: 1, Canonical: json.RawMessage(`{"x":"y"}`)})
	requireErrorIs(t, err, ErrCounterOverflow)
}
