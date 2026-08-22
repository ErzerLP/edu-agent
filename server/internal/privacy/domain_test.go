package privacy

import (
	"context"
	"testing"
	"time"
)

func TestErasureOperationHashIsStableAndCoversGeneration(t *testing.T) {
	now := time.Date(2026, 9, 2, 10, 11, 12, 123, time.FixedZone("offset", 3600))
	request := ErasureRequest{
		DeviceID: "10000000-0000-4000-8000-000000000001", OperationID: "10000000-0000-4000-8000-000000000002",
		ActorDeviceID: "10000000-0000-4000-8000-000000000001", ReasonCode: "learner_request",
		RequestedAt: now, ManagedBackupUnrecoverableAfter: now.Add(24 * time.Hour), ExpectedCurrentLearnerGeneration: 3,
	}
	left, err := request.OperationHash()
	if err != nil {
		t.Fatal(err)
	}
	request.RequestedAt = request.RequestedAt.Add(6 * time.Hour).UTC()
	request.ManagedBackupUnrecoverableAfter = request.RequestedAt.Add(12 * time.Hour)
	right, err := request.OperationHash()
	if err != nil || left != right || len(left) != 64 {
		t.Fatalf("cross-clock stable hash left=%q right=%q err=%v", left, right, err)
	}
	request.ManagedBackupUnrecoverableAfter = request.RequestedAt.Add(31 * 24 * time.Hour)
	if _, err := request.OperationHash(); ErrorCode(err) != CodeInvalidRequest {
		t.Fatalf("invalid dynamic deadline bypassed validation: %v", err)
	}
	request.ManagedBackupUnrecoverableAfter = request.RequestedAt.Add(12 * time.Hour)
	request.ExpectedCurrentLearnerGeneration++
	changed, err := request.OperationHash()
	if err != nil || changed == left {
		t.Fatalf("generation was not covered by operation hash: %q err=%v", changed, err)
	}
	request.ReasonCode = "delete this private sentence"
	if _, err := request.OperationHash(); ErrorCode(err) != CodeInvalidRequest {
		t.Fatalf("free-form erasure reason accepted: %v", err)
	}
}

func TestReadPermitCloseCancelsDrainsAndReopens(t *testing.T) {
	manager := NewReadPermitManager()
	permit, err := manager.Acquire(context.Background(), OwnerLearning, OwnerTutoring)
	if err != nil {
		t.Fatal(err)
	}
	drained := make(chan error, 1)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	go func() { drained <- manager.CloseAndDrain(ctx, 2, OwnerLearning) }()
	select {
	case <-permit.Context().Done():
	case <-time.After(time.Second):
		t.Fatal("active read was not canceled")
	}
	select {
	case err := <-drained:
		t.Fatalf("drain returned before permit release: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	if _, err := manager.Acquire(context.Background(), OwnerLearning); ErrorCode(err) != CodeContentRedacted {
		t.Fatalf("new permit while closed err=%v", err)
	}
	permit.Release()
	if err := <-drained; err != nil {
		t.Fatal(err)
	}
	manager.Open(2, OwnerLearning)
	reopened, err := manager.Acquire(context.Background(), OwnerLearning)
	if err != nil {
		t.Fatal(err)
	}
	reopened.Release()
}

func TestReceiptSlotsAreFixedAndOwned(t *testing.T) {
	if len(ReceiptSlots) != 16 || len(LocalManagedSlots) != 11 {
		t.Fatalf("receipt slots=%d local=%d", len(ReceiptSlots), len(LocalManagedSlots))
	}
	for _, store := range LocalManagedSlots {
		if store == StoreProcessCache {
			continue
		}
		owner, ok := OwnerForStore(store)
		if !ok || !owner.Valid() {
			t.Fatalf("local store %q has no owner", store)
		}
	}
}
