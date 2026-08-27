package privacy

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
)

const (
	PolicyVersion              = "privacy-erasure-v1"
	ManagedBackupPolicyVersion = "managed-backup-key-destruction-v1"

	CodeInvalidRequest              = "invalid_request"
	CodeNotFound                    = "not_found"
	CodeIdempotencyConflict         = "idempotency_conflict"
	CodeErasureInProgress           = "erasure_in_progress"
	CodeContentRedacted             = "content_redacted"
	CodePrivacyClearInProgress      = "privacy_clear_in_progress"
	CodeReceiptNotCurrent           = "receipt_not_current"
	CodeVerificationFailed          = "verification_failed"
	CodeUnsupportedReceiptStore     = "unsupported_receipt_store"
	CodeMigrationLeaseConflict      = "migration_lease_conflict"
	CodeOfflineChallengeInvalid     = "purge_challenge_invalid"
	CodeOfflineChallengeUnavailable = "offline_challenge_unavailable"
	CodeOfflinePurgeNotCurrent       = "purge_not_current"
	CodeOfflinePurgeAckConflict      = "purge_ack_conflict"
)

type Error struct {
	Code   string `json:"code"`
	Reason string `json:"reason,omitempty"`
	Cause  error  `json:"-"`
}

func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	if e.Cause != nil {
		return fmt.Sprintf("privacy operation failed: code=%s reason=%s: %v", e.Code, e.Reason, e.Cause)
	}
	return fmt.Sprintf("privacy operation failed: code=%s reason=%s", e.Code, e.Reason)
}
func (e *Error) Unwrap() error { return e.Cause }
func ErrorCode(err error) string {
	var target *Error
	if errors.As(err, &target) {
		return target.Code
	}
	return ""
}

type OwnerKind string

const (
	OwnerIdentity  OwnerKind = "identity"
	OwnerKnowledge OwnerKind = "knowledge"
	OwnerLearning  OwnerKind = "learning"
	OwnerTutoring  OwnerKind = "tutoring"
	OwnerMemory    OwnerKind = "memory"
	OwnerOutbox    OwnerKind = "outbox"
)

var AllOwners = []OwnerKind{OwnerIdentity, OwnerKnowledge, OwnerLearning, OwnerTutoring, OwnerMemory, OwnerOutbox}

func (o OwnerKind) Valid() bool {
	for _, candidate := range AllOwners {
		if candidate == o {
			return true
		}
	}
	return false
}

type ErasureStatus string

const (
	StatusBarrierCommitted ErasureStatus = "barrier_committed"
	StatusLocalScrubbed    ErasureStatus = "local_scrubbed"
	StatusRemoteDraining   ErasureStatus = "remote_draining"
	StatusRemotePurged     ErasureStatus = "remote_purged"
	StatusVerified         ErasureStatus = "verified"
	StatusPartial          ErasureStatus = "partial"
	StatusBlocked          ErasureStatus = "blocked"
)

type StepStatus string

const (
	StepPending       StepStatus = "pending"
	StepSucceeded     StepStatus = "succeeded"
	StepPartial       StepStatus = "partial"
	StepFailed        StepStatus = "failed"
	StepUnknown       StepStatus = "unknown"
	StepNotApplicable StepStatus = "not_applicable"
	StepUnsupported   StepStatus = "unsupported"
)

func CanTransitionErasure(from, to ErasureStatus) bool {
	switch from {
	case StatusBarrierCommitted:
		return to == StatusLocalScrubbed || to == StatusPartial || to == StatusBlocked
	case StatusLocalScrubbed:
		return to == StatusRemoteDraining || to == StatusRemotePurged || to == StatusPartial || to == StatusBlocked
	case StatusRemoteDraining:
		return to == StatusRemotePurged || to == StatusPartial || to == StatusBlocked
	case StatusRemotePurged:
		return to == StatusVerified || to == StatusPartial || to == StatusBlocked
	case StatusPartial:
		return to == StatusLocalScrubbed || to == StatusRemoteDraining || to == StatusRemotePurged ||
			to == StatusVerified || to == StatusBlocked
	case StatusBlocked:
		return to == StatusPartial
	default:
		return false
	}
}

func CanTransitionStep(from, to StepStatus) bool {
	switch from {
	case StepPending:
		return to == StepSucceeded || to == StepPartial || to == StepFailed || to == StepUnknown ||
			to == StepNotApplicable || to == StepUnsupported
	case StepPartial, StepFailed, StepUnknown:
		return to == StepSucceeded || to == StepPartial || to == StepFailed || to == StepUnknown ||
			to == StepNotApplicable
	default:
		return false
	}
}

type StoreKind string

const (
	StoreIdentityMetadata          StoreKind = "identity_metadata"
	StoreKnowledgeContent          StoreKind = "knowledge_content"
	StoreKnowledgeIndex            StoreKind = "knowledge_index"
	StoreKnowledgeArtifacts        StoreKind = "knowledge_artifacts"
	StoreLearningEventPayload      StoreKind = "learning_event_payload"
	StoreLearningTypedPayload      StoreKind = "learning_typed_payload"
	StoreTutoringPayload           StoreKind = "tutoring_payload"
	StoreInboxOutbox               StoreKind = "inbox_outbox"
	StoreProjectionGenerations     StoreKind = "projection_generations"
	StoreMemoryCandidateDelivery   StoreKind = "memory_candidate_delivery"
	StoreProcessCache              StoreKind = "process_cache"
	StoreNocturnePaths             StoreKind = "nocturne_paths"
	StoreNocturneOrphanHistory     StoreKind = "nocturne_orphan_history"
	StoreNocturneSnapshotChangeset StoreKind = "nocturne_snapshot_changeset"
	StoreManagedBackup             StoreKind = "managed_backup"
	StoreExternalProvider          StoreKind = "external_provider"
	StoreOfflineDeviceCache        StoreKind = "offline_device_cache"
)

var ReceiptSlots = []StoreKind{
	StoreIdentityMetadata, StoreKnowledgeContent, StoreKnowledgeIndex, StoreKnowledgeArtifacts,
	StoreLearningEventPayload, StoreLearningTypedPayload, StoreTutoringPayload, StoreInboxOutbox,
	StoreProjectionGenerations, StoreMemoryCandidateDelivery, StoreProcessCache, StoreNocturnePaths,
	StoreNocturneOrphanHistory, StoreNocturneSnapshotChangeset, StoreManagedBackup, StoreExternalProvider,
	StoreOfflineDeviceCache,
}

var LocalManagedSlots = []StoreKind{
	StoreIdentityMetadata, StoreKnowledgeContent, StoreKnowledgeIndex, StoreKnowledgeArtifacts,
	StoreLearningEventPayload, StoreLearningTypedPayload, StoreTutoringPayload, StoreInboxOutbox,
	StoreProjectionGenerations, StoreMemoryCandidateDelivery, StoreProcessCache,
}

func (s StoreKind) Valid() bool {
	for _, candidate := range ReceiptSlots {
		if candidate == s {
			return true
		}
	}
	return false
}

func OwnerForStore(store StoreKind) (OwnerKind, bool) {
	switch store {
	case StoreIdentityMetadata:
		return OwnerIdentity, true
	case StoreKnowledgeContent, StoreKnowledgeIndex, StoreKnowledgeArtifacts:
		return OwnerKnowledge, true
	case StoreLearningEventPayload, StoreLearningTypedPayload, StoreProjectionGenerations:
		return OwnerLearning, true
	case StoreTutoringPayload:
		return OwnerTutoring, true
	case StoreInboxOutbox:
		return OwnerOutbox, true
	case StoreMemoryCandidateDelivery:
		return OwnerMemory, true
	default:
		return "", false
	}
}

func StoresForOwner(owner OwnerKind) []StoreKind {
	stores := make([]StoreKind, 0, 3)
	for _, store := range LocalManagedSlots {
		if candidate, ok := OwnerForStore(store); ok && candidate == owner {
			stores = append(stores, store)
		}
	}
	return stores
}

const (
	ReasonLearnerRequest  ReasonCode = "learner_request"
	ReasonAccountClosure  ReasonCode = "account_closure"
	ReasonOperatorRequest ReasonCode = "operator_request"
)

type ReasonCode string

func ValidReasonCode(value string) bool {
	switch ReasonCode(value) {
	case ReasonLearnerRequest, ReasonAccountClosure, ReasonOperatorRequest:
		return true
	default:
		return false
	}
}

type ErasureRequest struct {
	DeviceID                         string    `json:"device_id"`
	OperationID                      string    `json:"operation_id"`
	ReasonCode                       string    `json:"reason_code"`
	ActorDeviceID                    string    `json:"actor_device_id"`
	RequestedAt                      time.Time `json:"requested_at"`
	ManagedBackupUnrecoverableAfter  time.Time `json:"managed_backup_unrecoverable_after"`
	ExpectedCurrentLearnerGeneration int64     `json:"expected_current_learner_generation,omitempty"`
}

func (r ErasureRequest) Validate() error {
	if uuid.Validate(r.DeviceID) != nil || uuid.Validate(r.OperationID) != nil || uuid.Validate(r.ActorDeviceID) != nil {
		return &Error{Code: CodeInvalidRequest, Reason: "invalid_operation_identity"}
	}
	if !ValidReasonCode(r.ReasonCode) {
		return &Error{Code: CodeInvalidRequest, Reason: "invalid_reason_code"}
	}
	if r.RequestedAt.IsZero() || r.ManagedBackupUnrecoverableAfter.IsZero() || r.ManagedBackupUnrecoverableAfter.Before(r.RequestedAt) || r.ManagedBackupUnrecoverableAfter.After(r.RequestedAt.Add(30*24*time.Hour)) {
		return &Error{Code: CodeInvalidRequest, Reason: "invalid_backup_deadline"}
	}
	if r.ExpectedCurrentLearnerGeneration < 0 {
		return &Error{Code: CodeInvalidRequest, Reason: "invalid_expected_generation"}
	}
	return nil
}

func (r ErasureRequest) OperationHash() (string, error) {
	if err := r.Validate(); err != nil {
		return "", err
	}
	canonical := struct {
		ActorDeviceID                    string `json:"actor_device_id"`
		BackupPolicyVersion              string `json:"backup_policy_version"`
		DeviceID                         string `json:"device_id"`
		ExpectedCurrentLearnerGeneration int64  `json:"expected_current_learner_generation"`
		OperationID                      string `json:"operation_id"`
		PolicyVersion                    string `json:"policy_version"`
		ReasonCode                       string `json:"reason_code"`
	}{
		ActorDeviceID: r.ActorDeviceID, BackupPolicyVersion: ManagedBackupPolicyVersion, DeviceID: r.DeviceID,
		ExpectedCurrentLearnerGeneration: r.ExpectedCurrentLearnerGeneration,
		OperationID:                      r.OperationID, PolicyVersion: PolicyVersion,
		ReasonCode: strings.TrimSpace(r.ReasonCode),
	}
	encoded, err := json.Marshal(canonical)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}

type RedactionPayload struct {
	ErasureID               string `json:"erasure_id"`
	Generation              int64  `json:"generation"`
	RedactedThroughEventSeq int64  `json:"redacted_through_event_seq"`
	PolicyVersion           string `json:"policy_version"`
	ReasonCode              string `json:"reason_code"`
}

func (p RedactionPayload) Validate() error {
	if uuid.Validate(p.ErasureID) != nil || p.Generation < 2 || p.RedactedThroughEventSeq < 0 || p.PolicyVersion == "" || p.ReasonCode == "" {
		return &Error{Code: CodeInvalidRequest, Reason: "invalid_redaction_payload"}
	}
	return nil
}

type MigrationLeaseRequest struct {
	OperationID    string `json:"operation_id"`
	BackupIdentity string `json:"backup_identity"`
}

func (r MigrationLeaseRequest) Validate() error {
	if uuid.Validate(r.OperationID) != nil || len(r.BackupIdentity) != 64 || r.BackupIdentity != strings.ToLower(r.BackupIdentity) {
		return &Error{Code: CodeInvalidRequest, Reason: "invalid_migration_lease_request"}
	}
	identity, err := hex.DecodeString(r.BackupIdentity)
	if err != nil || len(identity) != sha256.Size {
		return &Error{Code: CodeInvalidRequest, Reason: "invalid_migration_backup_identity"}
	}
	return nil
}

type MigrationLease struct {
	OperationID string    `json:"operation_id"`
	AcquiredAt  time.Time `json:"acquired_at"`
	Replayed    bool      `json:"replayed"`
}

type OfflineDeviceChildStatus string

const (
	OfflineDeviceChildPending   OfflineDeviceChildStatus = "pending"
	OfflineDeviceChildSucceeded OfflineDeviceChildStatus = "succeeded"
	OfflineDeviceChildUnknown   OfflineDeviceChildStatus = "unknown"
	OfflineDeviceChildFailed    OfflineDeviceChildStatus = "failed"
)

func CanTransitionOfflineDeviceChild(from, to OfflineDeviceChildStatus) bool {
	switch from {
	case OfflineDeviceChildPending:
		return to == OfflineDeviceChildSucceeded || to == OfflineDeviceChildUnknown || to == OfflineDeviceChildFailed
	case OfflineDeviceChildUnknown, OfflineDeviceChildFailed:
		return to == OfflineDeviceChildPending
	default:
		return false
	}
}

type OfflinePurgeChallenge struct {
	ErasureID           string                   `json:"erasure_id"`
	DeviceID            string                   `json:"device_id"`
	OldGeneration       int64                    `json:"old_generation"`
	CurrentGeneration   int64                    `json:"current_generation"`
	ChallengeRevision   int64                    `json:"challenge_revision"`
	Challenge            string                   `json:"challenge"`
	IssuedAt             time.Time                `json:"issued_at"`
	Status               OfflineDeviceChildStatus `json:"status"`
}

type OfflinePurgeOutcome string

const (
	OfflinePurgeOutcomeSucceeded OfflinePurgeOutcome = "succeeded"
	OfflinePurgeOutcomeFailed    OfflinePurgeOutcome = "failed"
)

type OfflinePurgeFailureCode string

const (
	OfflinePurgeFailureProfileBusy        OfflinePurgeFailureCode = "profile_busy"
	OfflinePurgeFailureKeyDelete          OfflinePurgeFailureCode = "key_delete_failed"
	OfflinePurgeFailurePathDelete         OfflinePurgeFailureCode = "path_delete_failed"
	OfflinePurgeFailureVerification       OfflinePurgeFailureCode = "verification_failed"
)

func (c OfflinePurgeFailureCode) Valid() bool {
	switch c {
	case OfflinePurgeFailureProfileBusy, OfflinePurgeFailureKeyDelete, OfflinePurgeFailurePathDelete, OfflinePurgeFailureVerification:
		return true
	default:
		return false
	}
}

type OfflineDevicePurgeAcknowledgment struct {
	ChallengeRevision   int64                   `json:"challenge_revision"`
	Challenge           string                  `json:"challenge"`
	Outcome             OfflinePurgeOutcome     `json:"outcome"`
	ManagedObjectsAbsent *bool                    `json:"managed_objects_absent,omitempty"`
	FailureCode         OfflinePurgeFailureCode `json:"failure_code,omitempty"`
}

func (a OfflineDevicePurgeAcknowledgment) Validate() error {
	if a.ChallengeRevision <= 0 || len(a.Challenge) != 43 {
		return &Error{Code: CodeInvalidRequest, Reason: "invalid_offline_device_purge_acknowledgment"}
	}
	if decoded, err := base64.RawURLEncoding.DecodeString(a.Challenge); err != nil || len(decoded) != sha256.Size || base64.RawURLEncoding.EncodeToString(decoded) != a.Challenge {
		return &Error{Code: CodeInvalidRequest, Reason: "invalid_offline_device_purge_challenge"}
	}
	switch a.Outcome {
	case OfflinePurgeOutcomeSucceeded:
		if a.ManagedObjectsAbsent == nil || !*a.ManagedObjectsAbsent || a.FailureCode != "" {
			return &Error{Code: CodeInvalidRequest, Reason: "invalid_offline_device_purge_success"}
		}
	case OfflinePurgeOutcomeFailed:
		if a.ManagedObjectsAbsent != nil || !a.FailureCode.Valid() {
			return &Error{Code: CodeInvalidRequest, Reason: "invalid_offline_device_purge_failure"}
		}
	default:
		return &Error{Code: CodeInvalidRequest, Reason: "invalid_offline_device_purge_outcome"}
	}
	return nil
}

type OfflineDeviceChildReceipt struct {
	ErasureID         string                   `json:"erasure_id"`
	DeviceID          string                   `json:"device_id"`
	SourceGeneration  int64                    `json:"source_generation"`
	CurrentGeneration int64                    `json:"current_generation"`
	ChallengeRevision int64                    `json:"challenge_revision"`
	Status             OfflineDeviceChildStatus `json:"status"`
	UpdatedAt          time.Time                `json:"updated_at"`
	StableReason       string                   `json:"stable_reason"`
}

type OfflineDeviceCounts struct {
	Pending   int `json:"pending"`
	Succeeded int `json:"succeeded"`
	Unknown   int `json:"unknown"`
	Failed    int `json:"failed"`
}

type OfflineDeviceReceiptPage struct {
	ErasureID       string                      `json:"erasure_id"`
	ReceiptRevision int64                       `json:"receipt_revision"`
	Items           []OfflineDeviceChildReceipt `json:"items"`
	NextCursor      string                      `json:"next_cursor,omitempty"`
}

type StepReceipt struct {
	ID                 string     `json:"receipt_id"`
	Store              StoreKind  `json:"store"`
	Version            int64      `json:"version"`
	Status             StepStatus `json:"status"`
	StableReason       string     `json:"stable_reason"`
	VerificationMethod string     `json:"verification_method"`
	StartedAt          time.Time  `json:"started_at"`
	CompletedAt        *time.Time `json:"completed_at,omitempty"`
	EvidenceDigest     string               `json:"evidence_digest,omitempty"`
	DeviceCounts       *OfflineDeviceCounts `json:"device_counts,omitempty"`
	ChildrenTruncated  *bool                `json:"children_truncated,omitempty"`
	NextCursor         string               `json:"next_cursor,omitempty"`
}

type ErasureReceipt struct {
	ErasureID               string                      `json:"erasure_id"`
	Status                  ErasureStatus               `json:"status"`
	SummaryVersion          int64                       `json:"summary_version"`
	LearnerGeneration       int64                       `json:"learner_generation"`
	RedactedThroughEventSeq int64                       `json:"redacted_through_event_seq"`
	PolicyVersion           string                      `json:"policy_version"`
	ReasonCode              string                      `json:"reason_code"`
	RequestedAt             time.Time                   `json:"requested_at"`
	UpdatedAt               time.Time                   `json:"updated_at"`
	Steps                   []StepReceipt `json:"steps"`
}

func (r *ErasureReceipt) SortSteps() {
	order := make(map[StoreKind]int, len(ReceiptSlots))
	for index, store := range ReceiptSlots {
		order[store] = index
	}
	sort.Slice(r.Steps, func(i, j int) bool { return order[r.Steps[i].Store] < order[r.Steps[j].Store] })
}
