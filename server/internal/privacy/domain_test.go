package privacy

import (
	"context"
	"encoding/json"
	"strings"
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

func TestEventRedactedPayloadCanonicalJSON(t *testing.T) {
	payload := RedactionPayload{
		ErasureID: "10000000-0000-4000-8000-000000000001", Generation: 2,
		RedactedThroughEventSeq: 17, PolicyVersion: PolicyVersion, ReasonCode: string(ReasonLearnerRequest),
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	const expected = `{"erasure_id":"10000000-0000-4000-8000-000000000001","generation":2,"redacted_through_event_seq":17,"policy_version":"privacy-erasure-v1","reason_code":"learner_request"}`
	if string(encoded) != expected {
		t.Fatalf("canonical payload=%s want=%s", encoded, expected)
	}
	if err := payload.Validate(); err != nil {
		t.Fatal(err)
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
	if len(ReceiptSlots) != 17 || len(LocalManagedSlots) != 11 {
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

func TestOfflineDeviceChildTransitionsAreClosed(t *testing.T) {
	statuses := []OfflineDeviceChildStatus{
		OfflineDeviceChildPending, OfflineDeviceChildSucceeded,
		OfflineDeviceChildUnknown, OfflineDeviceChildFailed,
	}
	for _, from := range statuses {
		for _, to := range statuses {
			want := from == OfflineDeviceChildPending && (to == OfflineDeviceChildSucceeded || to == OfflineDeviceChildUnknown || to == OfflineDeviceChildFailed) ||
				(from == OfflineDeviceChildUnknown || from == OfflineDeviceChildFailed) && to == OfflineDeviceChildPending
			if got := CanTransitionOfflineDeviceChild(from, to); got != want {
				t.Fatalf("offline device child transition %s -> %s=%v want=%v", from, to, got, want)
			}
		}
	}
}

func TestPrivacyStatusSummaryVerificationAndReceiptWordingFixture(t *testing.T) {
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	request := ErasureRequest{
		DeviceID: "10000000-0000-4000-8000-000000000001", OperationID: "10000000-0000-4000-8000-000000000002",
		ActorDeviceID: "10000000-0000-4000-8000-000000000001", ReasonCode: string(ReasonLearnerRequest),
		RequestedAt: now, ManagedBackupUnrecoverableAfter: now.Add(24 * time.Hour), ExpectedCurrentLearnerGeneration: 1,
	}
	if err := request.Validate(); err != nil {
		t.Fatal(err)
	}
	redaction := RedactionPayload{
		ErasureID: "10000000-0000-4000-8000-000000000003", Generation: 2,
		RedactedThroughEventSeq: 19, PolicyVersion: PolicyVersion, ReasonCode: string(ReasonLearnerRequest),
	}
	if err := redaction.Validate(); err != nil {
		t.Fatal(err)
	}

	erasureStatuses := []ErasureStatus{
		StatusBarrierCommitted, StatusLocalScrubbed, StatusRemoteDraining, StatusRemotePurged,
		StatusVerified, StatusPartial, StatusBlocked,
	}
	stepStatuses := []StepStatus{
		StepPending, StepSucceeded, StepPartial, StepFailed, StepUnknown, StepNotApplicable, StepUnsupported,
	}
	for _, erasureStatus := range erasureStatuses {
		receipt := ErasureReceipt{
			ErasureID: redaction.ErasureID, Status: erasureStatus, SummaryVersion: 1,
			LearnerGeneration: redaction.Generation, RedactedThroughEventSeq: redaction.RedactedThroughEventSeq,
			PolicyVersion: redaction.PolicyVersion, ReasonCode: redaction.ReasonCode,
			RequestedAt: now, UpdatedAt: now,
		}
		for index, stepStatus := range stepStatuses {
			store := ReceiptSlots[len(ReceiptSlots)-1-index]
			completed := now
			if stepStatus == StepPending {
				completed = time.Time{}
			}
			step := StepReceipt{
				ID:    "20000000-0000-4000-8000-00000000000" + string(rune('1'+index)),
				Store: store, Version: 1, Status: stepStatus,
				StableReason:       "logical content removal or absence verification",
				VerificationMethod: "zero_residual_body_scan", StartedAt: now,
			}
			if !completed.IsZero() {
				step.CompletedAt = &completed
			}
			receipt.Steps = append(receipt.Steps, step)
		}
		receipt.SortSteps()
		for index := 1; index < len(receipt.Steps); index++ {
			previous, current := -1, -1
			for slotIndex, slot := range ReceiptSlots {
				if slot == receipt.Steps[index-1].Store {
					previous = slotIndex
				}
				if slot == receipt.Steps[index].Store {
					current = slotIndex
				}
			}
			if previous >= current {
				t.Fatalf("receipt steps not in fixed order: %+v", receipt.Steps)
			}
		}
		encoded, err := json.Marshal(receipt)
		if err != nil {
			t.Fatal(err)
		}
		wording := strings.ToLower(string(encoded))
		if strings.Contains(wording, "secure erase") || strings.Contains(wording, "securely erased") {
			t.Fatalf("receipt overclaims physical erasure: %s", encoded)
		}
	}

	erasureTransitions := map[[2]ErasureStatus]bool{
		{StatusBarrierCommitted, StatusLocalScrubbed}: true,
		{StatusBarrierCommitted, StatusPartial}:       true,
		{StatusBarrierCommitted, StatusBlocked}:       true,
		{StatusLocalScrubbed, StatusRemoteDraining}:   true,
		{StatusLocalScrubbed, StatusRemotePurged}:     true,
		{StatusLocalScrubbed, StatusPartial}:          true,
		{StatusLocalScrubbed, StatusBlocked}:          true,
		{StatusRemoteDraining, StatusRemotePurged}:    true,
		{StatusRemoteDraining, StatusPartial}:         true,
		{StatusRemoteDraining, StatusBlocked}:         true,
		{StatusRemotePurged, StatusVerified}:          true,
		{StatusRemotePurged, StatusPartial}:           true,
		{StatusRemotePurged, StatusBlocked}:           true,
		{StatusPartial, StatusLocalScrubbed}:          true,
		{StatusPartial, StatusRemoteDraining}:         true,
		{StatusPartial, StatusRemotePurged}:           true,
		{StatusPartial, StatusVerified}:               true,
		{StatusPartial, StatusBlocked}:                true,
		{StatusBlocked, StatusPartial}:                true,
	}
	for _, from := range append(erasureStatuses, ErasureStatus("invalid")) {
		for _, to := range append(erasureStatuses, ErasureStatus("invalid")) {
			want := erasureTransitions[[2]ErasureStatus{from, to}]
			if got := CanTransitionErasure(from, to); got != want {
				t.Errorf("erasure transition %s/%s=%v want=%v", from, to, got, want)
			}
		}
	}
	stepTransitions := make(map[[2]StepStatus]bool)
	for _, to := range []StepStatus{StepSucceeded, StepPartial, StepFailed, StepUnknown, StepNotApplicable, StepUnsupported} {
		stepTransitions[[2]StepStatus{StepPending, to}] = true
	}
	for _, from := range []StepStatus{StepPartial, StepFailed, StepUnknown} {
		for _, to := range []StepStatus{StepSucceeded, StepPartial, StepFailed, StepUnknown, StepNotApplicable} {
			stepTransitions[[2]StepStatus{from, to}] = true
		}
	}
	for _, from := range append(stepStatuses, StepStatus("invalid")) {
		for _, to := range append(stepStatuses, StepStatus("invalid")) {
			want := stepTransitions[[2]StepStatus{from, to}]
			if got := CanTransitionStep(from, to); got != want {
				t.Errorf("step transition %s/%s=%v want=%v", from, to, got, want)
			}
		}
	}

	invalidRequest := request
	invalidRequest.ReasonCode = "free form private text"
	if err := invalidRequest.Validate(); ErrorCode(err) != CodeInvalidRequest {
		t.Fatalf("invalid erasure reason accepted: %v", err)
	}
	invalidRedaction := redaction
	invalidRedaction.Generation = 1
	if err := invalidRedaction.Validate(); ErrorCode(err) != CodeInvalidRequest {
		t.Fatalf("invalid redaction generation accepted: %v", err)
	}
	if OwnerKind("invalid").Valid() || StoreKind("invalid").Valid() {
		t.Fatal("invalid owner or store kind accepted")
	}
	if err := (LocalRedactionRequest{
		ErasureID: redaction.ErasureID, Store: StoreKind("invalid"), ReceiptID: "10000000-0000-4000-8000-000000000004",
		LearnerGeneration: 2, RedactedThroughEvent: 19,
	}).Validate(OwnerLearning); ErrorCode(err) != CodeInvalidRequest {
		t.Fatalf("invalid local redaction slot accepted: %v", err)
	}
	if err := (GenerationTransition{
		ErasureID: redaction.ErasureID, FromGeneration: 2, TargetGeneration: 2, At: now,
	}).Validate(false); ErrorCode(err) != CodeInvalidRequest {
		t.Fatalf("invalid generation close accepted: %v", err)
	}
}
