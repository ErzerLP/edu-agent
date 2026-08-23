package privacy

import (
	"context"
	"strings"
	"testing"
	"time"
)

type disabledEvidenceReader struct {
	evidence DisabledNocturneEvidence
	calls    int
}

func (r *disabledEvidenceReader) ReadDisabledNocturneEvidence(context.Context, DisabledNocturneEvidenceRequest) (DisabledNocturneEvidence, error) {
	r.calls++
	return r.evidence, nil
}

func TestDisabledNocturneVerifierRequiresAffirmativeZeroFootprint(t *testing.T) {
	now := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	reader := &disabledEvidenceReader{}
	verifier, err := NewDisabledNocturneVerifier(reader, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	request := RemoteEraseRequest{
		ErasureID: "10000000-0000-4000-8000-000000000001", LearnerGeneration: 2,
		Receipt: StepReceipt{ID: "20000000-0000-4000-8000-000000000001", Store: StoreNocturnePaths},
	}
	for attempt := 0; attempt < 2; attempt++ {
		result, err := verifier.Erase(context.Background(), request)
		if err != nil || result.Status != StepNotApplicable || result.StableReason != "no_nocturne_or_managed_remote_history" || len(result.EvidenceDigest) != 64 || result.CompletedAt != now {
			t.Fatalf("attempt %d result=%+v err=%v", attempt, result, err)
		}
	}
	backup, err := verifier.VerifyManagedBackups(context.Background(), ManagedBackupVerificationRequest{
		ErasureID: request.ErasureID, LearnerGeneration: request.LearnerGeneration,
	})
	if err != nil || backup.Status != StepNotApplicable || backup.StableReason != "no_pre_barrier_managed_backup_or_remote_history" || len(backup.EvidenceDigest) != 64 {
		t.Fatalf("backup=%+v err=%v", backup, err)
	}
	if reader.calls != 3 {
		t.Fatalf("storage evidence calls=%d", reader.calls)
	}
}

func TestDisabledNocturneVerifierKeepsRemoteHistoryUnknown(t *testing.T) {
	reader := &disabledEvidenceReader{evidence: DisabledNocturneEvidence{
		CompletedReconciliations: 1, RemoteReferences: 2, ManagedBackups: 1,
	}}
	verifier, err := NewDisabledNocturneVerifier(reader, nil)
	if err != nil {
		t.Fatal(err)
	}
	request := RemoteEraseRequest{
		ErasureID: "10000000-0000-4000-8000-000000000002", LearnerGeneration: 3,
		Receipt: StepReceipt{ID: "20000000-0000-4000-8000-000000000002", Store: StoreNocturnePaths},
	}
	remote, err := verifier.Erase(context.Background(), request)
	if err != nil || remote.Status != StepUnknown || !strings.Contains(remote.StableReason, "unresolved") {
		t.Fatalf("remote=%+v err=%v", remote, err)
	}
	backup, err := verifier.VerifyManagedBackups(context.Background(), ManagedBackupVerificationRequest{
		ErasureID: request.ErasureID, LearnerGeneration: request.LearnerGeneration,
	})
	if err != nil || backup.Status != StepUnknown || !strings.Contains(backup.StableReason, "unresolved") {
		t.Fatalf("backup=%+v err=%v", backup, err)
	}
}
