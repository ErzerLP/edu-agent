package privacy

import (
	"bytes"
	"testing"
)

func TestOfflineChallengeKeyringSeparatesContextAndSupportsRotation(t *testing.T) {
	keyring, err := NewOfflineChallengeKeyring(map[int][]byte{
		2: bytes.Repeat([]byte{0x21}, 32),
		3: bytes.Repeat([]byte{0x31}, 32),
	})
	if err != nil {
		t.Fatal(err)
	}
	if keyring.CurrentVersion() != 3 {
		t.Fatalf("current version=%d", keyring.CurrentVersion())
	}
	erasureID := "11111111-1111-4111-8111-111111111111"
	deviceID := "22222222-2222-4222-8222-222222222222"
	challenge, err := keyring.Challenge(2, erasureID, deviceID, 1, 2, 1)
	if err != nil || len(challenge) != 43 {
		t.Fatalf("challenge=%q err=%v", challenge, err)
	}
	if !keyring.Verify(2, erasureID, deviceID, 1, 2, 1, challenge) || keyring.Verify(3, erasureID, deviceID, 1, 2, 1, challenge) {
		t.Fatal("rotated key verification did not stay version-bound")
	}
	for _, changed := range []struct {
		erasureID       string
		deviceID        string
		sourceGeneration int64
		targetGeneration int64
	}{
		{"33333333-3333-4333-8333-333333333333", deviceID, 1, 2},
		{erasureID, "44444444-4444-4444-8444-444444444444", 1, 2},
		{erasureID, deviceID, 2, 3},
	} {
		other, otherErr := keyring.Challenge(2, changed.erasureID, changed.deviceID, changed.sourceGeneration, changed.targetGeneration, 1)
		if otherErr != nil || other == challenge {
			t.Fatalf("context-separated challenge=%q err=%v", other, otherErr)
		}
	}
	if _, err := keyring.Challenge(99, erasureID, deviceID, 1, 2, 1); ErrorCode(err) != CodeOfflineChallengeUnavailable {
		t.Fatalf("unknown version err=%v", err)
	}
}

func TestOfflineDevicePurgeAcknowledgmentRequiresClosedOutcomeShape(t *testing.T) {
	managedAbsent := true
	valid := OfflineDevicePurgeAcknowledgment{
		ChallengeRevision:   1,
		Challenge:           "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
		Outcome:             OfflinePurgeOutcomeSucceeded,
		ManagedObjectsAbsent: &managedAbsent,
	}
	if err := valid.Validate(); err != nil {
		t.Fatal(err)
	}
	invalid := valid
	invalid.ManagedObjectsAbsent = nil
	if err := invalid.Validate(); ErrorCode(err) != CodeInvalidRequest {
		t.Fatalf("incomplete success acknowledgment err=%v", err)
	}
	failure := OfflineDevicePurgeAcknowledgment{
		ChallengeRevision: 1,
		Challenge:         valid.Challenge,
		Outcome:           OfflinePurgeOutcomeFailed,
		FailureCode:       OfflinePurgeFailureProfileBusy,
	}
	if err := failure.Validate(); err != nil {
		t.Fatal(err)
	}
}
