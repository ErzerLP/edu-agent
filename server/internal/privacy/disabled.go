package privacy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
)

const disabledNocturneEvidenceFormat = "disabled-nocturne-storage-evidence-v1"

type DisabledNocturneEvidenceRequest struct {
	ErasureID         string
	LearnerGeneration int64
}

func (r DisabledNocturneEvidenceRequest) Validate() error {
	if uuid.Validate(r.ErasureID) != nil || r.LearnerGeneration < 2 {
		return &Error{Code: CodeInvalidRequest, Reason: "invalid_disabled_nocturne_evidence_request"}
	}
	return nil
}

// DisabledNocturneEvidence contains only receipt-safe counts from managed
// storage. A disabled integration may be not_applicable only when every count
// is zero; historical remote activity must never be inferred away.
type DisabledNocturneEvidence struct {
	PendingReconciliations   int64 `json:"pending_reconciliations"`
	ReconciliationConflicts  int64 `json:"reconciliation_conflicts"`
	CompletedReconciliations int64 `json:"completed_reconciliations"`
	RemoteReferences         int64 `json:"remote_references"`
	ManagedBackups           int64 `json:"managed_backups"`
	LiveGenerationKeys       int64 `json:"live_generation_keys"`
}

func (e DisabledNocturneEvidence) Validate() error {
	if e.PendingReconciliations < 0 || e.ReconciliationConflicts < 0 ||
		e.CompletedReconciliations < 0 || e.RemoteReferences < 0 ||
		e.ManagedBackups < 0 || e.LiveGenerationKeys < 0 {
		return &Error{Code: CodeInvalidRequest, Reason: "invalid_disabled_nocturne_evidence"}
	}
	return nil
}

func (e DisabledNocturneEvidence) HasRemoteHistory() bool {
	return e.PendingReconciliations != 0 || e.ReconciliationConflicts != 0 ||
		e.CompletedReconciliations != 0 || e.RemoteReferences != 0 ||
		e.ManagedBackups != 0 || e.LiveGenerationKeys != 0
}

type DisabledNocturneEvidenceReader interface {
	ReadDisabledNocturneEvidence(context.Context, DisabledNocturneEvidenceRequest) (DisabledNocturneEvidence, error)
}

// DisabledNocturneVerifier implements both remote and managed-backup steps for
// deployments that have Nocturne disabled. It records not_applicable only from
// an affirmative, zero-footprint storage summary.
type DisabledNocturneVerifier struct {
	reader DisabledNocturneEvidenceReader
	now    func() time.Time
}

func NewDisabledNocturneVerifier(reader DisabledNocturneEvidenceReader, now func() time.Time) (*DisabledNocturneVerifier, error) {
	if reader == nil {
		return nil, errors.New("disabled Nocturne verifier requires a storage evidence reader")
	}
	if now == nil {
		now = time.Now
	}
	return &DisabledNocturneVerifier{reader: reader, now: now}, nil
}

func (v *DisabledNocturneVerifier) Erase(ctx context.Context, request RemoteEraseRequest) (RemoteEraseResult, error) {
	if request.Receipt.ID == "" || request.Receipt.Store != StoreNocturnePaths {
		return RemoteEraseResult{}, &Error{Code: CodeInvalidRequest, Reason: "invalid_disabled_nocturne_remote_request"}
	}
	evidence, digest, err := v.readEvidence(ctx, request.ErasureID, request.LearnerGeneration)
	if err != nil {
		return RemoteEraseResult{}, err
	}
	result := RemoteEraseResult{
		Status: StepNotApplicable, StableReason: "no_nocturne_or_managed_remote_history",
		EvidenceDigest: digest, CompletedAt: v.now().UTC(),
	}
	if evidence.HasRemoteHistory() {
		result.Status = StepUnknown
		result.StableReason = "disabled_nocturne_remote_history_unresolved"
	}
	return result, nil
}

func (v *DisabledNocturneVerifier) VerifyManagedBackups(ctx context.Context, request ManagedBackupVerificationRequest) (ManagedBackupVerificationResult, error) {
	if err := request.Validate(); err != nil {
		return ManagedBackupVerificationResult{}, err
	}
	evidence, digest, err := v.readEvidence(ctx, request.ErasureID, request.LearnerGeneration)
	if err != nil {
		return ManagedBackupVerificationResult{}, err
	}
	result := ManagedBackupVerificationResult{
		Status: StepNotApplicable, StableReason: "no_pre_barrier_managed_backup_or_remote_history",
		EvidenceDigest: digest, CompletedAt: v.now().UTC(),
	}
	if evidence.HasRemoteHistory() {
		result.Status = StepUnknown
		result.StableReason = "disabled_managed_backup_or_remote_history_unresolved"
	}
	return result, nil
}

func (v *DisabledNocturneVerifier) readEvidence(ctx context.Context, erasureID string, generation int64) (DisabledNocturneEvidence, string, error) {
	request := DisabledNocturneEvidenceRequest{ErasureID: erasureID, LearnerGeneration: generation}
	if err := request.Validate(); err != nil {
		return DisabledNocturneEvidence{}, "", err
	}
	evidence, err := v.reader.ReadDisabledNocturneEvidence(ctx, request)
	if err != nil {
		return DisabledNocturneEvidence{}, "", err
	}
	if err := evidence.Validate(); err != nil {
		return DisabledNocturneEvidence{}, "", err
	}
	payload := struct {
		Format     string                   `json:"format"`
		ErasureID  string                   `json:"erasure_id"`
		Generation int64                    `json:"learner_generation"`
		Evidence   DisabledNocturneEvidence `json:"evidence"`
	}{disabledNocturneEvidenceFormat, erasureID, generation, evidence}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return DisabledNocturneEvidence{}, "", err
	}
	digest := sha256.Sum256(encoded)
	return evidence, hex.EncodeToString(digest[:]), nil
}

var _ RemoteEraser = (*DisabledNocturneVerifier)(nil)
var _ ManagedBackupVerifier = (*DisabledNocturneVerifier)(nil)
