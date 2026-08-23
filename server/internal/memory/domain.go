package memory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

const (
	AdmissionPolicyVersion = "memory-admission-v1"
	MaxContentRunes        = 32000
	MaxContentBytes        = 256 << 10
	MaxReasonRunes         = 1000
	MaxReferenceBytes      = 500
)

const (
	CodeInvalidRequest          = "invalid_request"
	CodeNotFound                = "not_found"
	CodeIdempotencyConflict     = "idempotency_conflict"
	CodeCandidateConflict       = "candidate_conflict"
	CodeMemoryConflict          = "memory_conflict"
	CodeInvalidMemoryTransition = "invalid_memory_transition"
	CodeMemoryPolicyRejected    = "memory_policy_rejected"
	CodeMemoryUnavailable       = "memory_unavailable"
	CodeDeliveryConflict        = "delivery_conflict"
	CodePrivacyClearInProgress  = "privacy_clear_in_progress"
	CodeContentRedacted         = "content_redacted"
	CodeStaleCursor             = "stale_cursor"
)

type Error struct {
	Code             string `json:"code"`
	Reason           string `json:"reason,omitempty"`
	CandidateID      string `json:"candidate_id,omitempty"`
	ExpectedRevision int64  `json:"expected_revision,omitempty"`
	CurrentRevision  int64  `json:"current_revision,omitempty"`
	Cause            error  `json:"-"`
}

func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	if e.Cause != nil {
		return fmt.Sprintf("memory operation failed: code=%s reason=%s: %v", e.Code, e.Reason, e.Cause)
	}
	return fmt.Sprintf("memory operation failed: code=%s reason=%s", e.Code, e.Reason)
}
func (e *Error) Unwrap() error { return e.Cause }
func ErrorCode(err error) string {
	var target *Error
	if errors.As(err, &target) {
		return target.Code
	}
	return ""
}

type SourceKind string

const (
	SourceUserStatement      SourceKind = "user_statement"
	SourceModelInference     SourceKind = "model_inference"
	SourceLongTermBackground SourceKind = "long_term_background"
	SourceGeneratedSummary   SourceKind = "generated_summary"
)

type Sensitivity string

const (
	SensitivityNonSensitive Sensitivity = "non_sensitive"
	SensitivitySensitive    Sensitivity = "sensitive"
)

type Stability string

const (
	StabilityTransient Stability = "transient"
	StabilityStable    Stability = "stable"
)

type Category string

const (
	CategoryInteractionPreference Category = "interaction_preference"
	CategoryTimeConstraint        Category = "time_constraint"
	CategoryPersonalContext       Category = "personal_context"
	CategoryGeneratedSummary      Category = "generated_summary"

	CategoryRawChat          Category = "raw_chat"
	CategoryCompleteAttempt  Category = "complete_attempt"
	CategoryQuestionOrRubric Category = "question_or_rubric"
	CategoryGoal             Category = "goal"
	CategoryRoute            Category = "route"
	CategoryMastery          Category = "mastery"
	CategoryEvidence         Category = "evidence"
	CategoryMisconception    Category = "misconception"
	CategoryReviewQueue      Category = "review_queue"
	CategorySyncState        Category = "sync_state"
	CategoryDeviceToken      Category = "device_token"
	CategoryModelSecret      Category = "model_secret"
	CategoryNocturneSecret   Category = "nocturne_secret"
)

type CandidateStatus string

const (
	CandidatePending  CandidateStatus = "pending_review"
	CandidateAdmitted CandidateStatus = "admitted"
	CandidateRejected CandidateStatus = "rejected"
	CandidateExpired  CandidateStatus = "expired"
)

type Decision string

const (
	DecisionAdmit  Decision = "admit"
	DecisionReject Decision = "reject"
	DecisionExpire Decision = "expire"
)

type RecordStatus string

const (
	RecordQueued              RecordStatus = "queued"
	RecordApplied             RecordStatus = "applied"
	RecordPermanentlyRejected RecordStatus = "permanently_rejected"
	RecordSuperseded          RecordStatus = "superseded"
	RecordDeletePending       RecordStatus = "delete_pending"
	RecordDeleted             RecordStatus = "deleted"
)

type DeliveryPublicStatus string

const (
	DeliveryQueued   DeliveryPublicStatus = "queued"
	DeliveryApplied  DeliveryPublicStatus = "applied"
	DeliveryRejected DeliveryPublicStatus = "rejected"
)

type DeliveryStatus string

const (
	DeliveryStatusQueued              DeliveryStatus = "queued"
	DeliveryStatusApplied             DeliveryStatus = "applied"
	DeliveryStatusPermanentlyRejected DeliveryStatus = "permanently_rejected"
	DeliveryStatusFenced              DeliveryStatus = "fenced"
	DeliveryStatusExpiryReconciling   DeliveryStatus = "expiry_reconciling"
	DeliveryStatusExpired             DeliveryStatus = "expired"
	DeliveryStatusDeletePending       DeliveryStatus = "delete_pending"
	DeliveryStatusDeleted             DeliveryStatus = "deleted"
)

type AttemptState string

const (
	AttemptPrepared    AttemptState = "prepared"
	AttemptSent        AttemptState = "sent"
	AttemptUnknown     AttemptState = "unknown"
	AttemptReconciling AttemptState = "reconciling"
	AttemptConfirmed   AttemptState = "confirmed"
	AttemptFenced      AttemptState = "fenced"
	AttemptFailed      AttemptState = "failed"
)

type ReconciliationStatus string

const (
	ReconciliationPending         ReconciliationStatus = "pending"
	ReconciliationReconciling     ReconciliationStatus = "reconciling"
	ReconciliationDeletePending   ReconciliationStatus = "delete_pending"
	ReconciliationAbsenceVerified ReconciliationStatus = "absence_verified"
	ReconciliationVerified        ReconciliationStatus = "verified"
	ReconciliationConflict        ReconciliationStatus = "conflict"
)

type ReceiptStatus string

const (
	ReceiptPending       ReceiptStatus = "pending"
	ReceiptSucceeded     ReceiptStatus = "succeeded"
	ReceiptPartial       ReceiptStatus = "partial"
	ReceiptFailed        ReceiptStatus = "failed"
	ReceiptUnknown       ReceiptStatus = "unknown"
	ReceiptNotApplicable ReceiptStatus = "not_applicable"
	ReceiptUnsupported   ReceiptStatus = "unsupported"
)

type DeliveryKind string

const (
	DeliveryAdmit      DeliveryKind = "admit"
	DeliveryCorrection DeliveryKind = "correction"
	DeliveryDelete     DeliveryKind = "delete"
	DeliveryErasure    DeliveryKind = "erasure"
)

type SourceReference struct {
	EventID        string   `json:"event_id,omitempty"`
	OperationID    string   `json:"operation_id,omitempty"`
	ModelID        string   `json:"model_id,omitempty"`
	PromptRevision string   `json:"prompt_revision,omitempty"`
	SourceHashes   []string `json:"source_hashes,omitempty"`
}

type Candidate struct {
	ID              string          `json:"candidate_id"`
	URI             string          `json:"candidate_uri"`
	LogicalMemoryID string          `json:"logical_memory_id,omitempty"`
	PayloadID       string          `json:"payload_id"`
	ContentHash     string          `json:"content_sha256"`
	Source          SourceKind      `json:"source_kind"`
	SourceReference SourceReference `json:"source_reference"`
	ProposerID      string          `json:"proposer_id"`
	Reason          string          `json:"reason"`
	Category        Category        `json:"category"`
	Sensitivity     Sensitivity     `json:"sensitivity"`
	Stability       Stability       `json:"stability"`
	ValidUntil      time.Time       `json:"valid_until"`
	PolicyVersion   string          `json:"admission_policy_version"`
	Status          CandidateStatus `json:"status"`
	Revision        int64           `json:"revision"`
	CreatedAt       time.Time       `json:"created_at"`
}

type CandidateDecision struct {
	ID          string    `json:"decision_id"`
	CandidateID string    `json:"candidate_id"`
	Revision    int64     `json:"revision"`
	Decision    Decision  `json:"decision"`
	Reason      string    `json:"reason"`
	ActorID     string    `json:"actor_id,omitempty"`
	ActorKind   string    `json:"actor_kind"`
	OperationID string    `json:"operation_id,omitempty"`
	RequestHash string    `json:"request_hash,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
}

type Record struct {
	LogicalMemoryID    string       `json:"logical_memory_id"`
	ID                 string       `json:"record_revision_id"`
	Revision           int64        `json:"revision"`
	RecordGeneration   int64        `json:"record_generation"`
	LearnerGeneration  int64        `json:"learner_generation"`
	CandidateID        string       `json:"candidate_id"`
	PreviousRevisionID string       `json:"previous_record_revision_id,omitempty"`
	ExternalURI        string       `json:"external_uri"`
	ExternalURIDigest  string       `json:"external_uri_sha256"`
	ExternalNodeID     string       `json:"external_node_id,omitempty"`
	ExternalMemoryID   int64        `json:"external_memory_id,omitempty"`
	ContentHash        string       `json:"content_sha256"`
	Status             RecordStatus `json:"status"`
	DeliveryID         string       `json:"delivery_id"`
	ReceiptID          string       `json:"receipt_id"`
	CreatedAt          time.Time    `json:"created_at"`
	AppliedAt          *time.Time   `json:"applied_at,omitempty"`
	SupersededAt       *time.Time   `json:"superseded_at,omitempty"`
	DeletedAt          *time.Time   `json:"deleted_at,omitempty"`
}

type Delivery struct {
	ID                string               `json:"delivery_id"`
	Kind              DeliveryKind         `json:"kind"`
	LogicalMemoryID   string               `json:"logical_memory_id"`
	RecordRevisionID  string               `json:"record_revision_id"`
	RecordRevision    int64                `json:"record_revision"`
	LearnerGeneration int64                `json:"learner_generation"`
	RecordGeneration  int64                `json:"record_generation"`
	PayloadID         string               `json:"payload_id"`
	PayloadHash       string               `json:"payload_sha256"`
	ExternalURI       string               `json:"external_uri"`
	AttemptState      AttemptState         `json:"attempt_state"`
	Status            DeliveryStatus       `json:"status"`
	PublicStatus      DeliveryPublicStatus `json:"public_status"`
	Disposition       string               `json:"disposition,omitempty"`
	ValidUntil        time.Time            `json:"valid_until"`
	AttemptCount      int                  `json:"attempt_count"`
	LastCategory      string               `json:"last_category,omitempty"`
	ReceiptID         string               `json:"receipt_id"`
	CreatedAt         time.Time            `json:"created_at"`
	UpdatedAt         time.Time            `json:"updated_at"`
}

type Attempt struct {
	ID             string       `json:"attempt_id"`
	DeliveryID     string       `json:"delivery_id"`
	AttemptToken   string       `json:"attempt_token"`
	State          AttemptState `json:"state"`
	LeaseToken     string       `json:"-"`
	LeaseExpiresAt time.Time    `json:"-"`
	BootEpoch      string       `json:"boot_epoch,omitempty"`
	SentAt         *time.Time   `json:"sent_at,omitempty"`
	UnknownAt      *time.Time   `json:"unknown_at,omitempty"`
	ResultDigest   string       `json:"result_digest,omitempty"`
	ErrorCategory  string       `json:"error_category,omitempty"`
	CreatedAt      time.Time    `json:"created_at"`
	UpdatedAt      time.Time    `json:"updated_at"`
}

type PolicyRejection struct {
	DeliveryID       string
	OutboxLeaseToken string
	ReceiptID        string
	Reason           string
	ErrorCategory    string
	At               time.Time
}

type ExpiryReconciliation struct {
	ID                string               `json:"reconciliation_id"`
	DeliveryID        string               `json:"delivery_id"`
	ErasureDeliveryID string               `json:"erasure_delivery_id,omitempty"`
	LogicalMemoryID   string               `json:"logical_memory_id"`
	ExternalURI       string               `json:"external_uri"`
	ContentHash       string               `json:"content_sha256"`
	AttemptToken      string               `json:"attempt_token"`
	SentBootEpoch     string               `json:"sent_boot_epoch"`
	LearnerGeneration int64                `json:"learner_generation"`
	RecordGeneration  int64                `json:"record_generation"`
	Status            ReconciliationStatus `json:"status"`
	LeaseToken        string               `json:"-"`
	LeaseExpiresAt    *time.Time           `json:"-"`
	Reason            string               `json:"reason,omitempty"`
	CreatedAt         time.Time            `json:"created_at"`
	UpdatedAt         time.Time            `json:"updated_at"`
}

type Receipt struct {
	ID                 string        `json:"receipt_id"`
	DeliveryID         string        `json:"delivery_id"`
	Version            int64         `json:"version"`
	Status             ReceiptStatus `json:"status"`
	Reason             string        `json:"reason"`
	VerificationMethod string        `json:"verification_method"`
	EvidenceDigest     string        `json:"evidence_digest,omitempty"`
	CreatedAt          time.Time     `json:"created_at"`
}

type Generation struct {
	LearnerGeneration int64     `json:"learner_generation"`
	MemoryGeneration  int64     `json:"memory_generation"`
	ReadOpen          bool      `json:"read_open"`
	WriteOpen         bool      `json:"write_open"`
	UpdatedAt         time.Time `json:"updated_at"`
}

type OperationKind string

const (
	OperationCreateCandidate   OperationKind = "create_candidate"
	OperationCandidateDecision OperationKind = "candidate_decision"
	OperationRecordDelete      OperationKind = "record_delete"
	OperationDeliveryReplay    OperationKind = "delivery_replay"
	OperationPrivacyErasure    OperationKind = "privacy_erasure"
)

type Operation struct {
	DeviceID    string        `json:"device_id"`
	OperationID string        `json:"operation_id"`
	RequestHash string        `json:"request_hash"`
	Kind        OperationKind `json:"operation_kind"`
}

type MaintenanceAuthorization struct {
	ErasureID               string
	ReceiptID               string
	TargetLearnerGeneration int64
}

func (a MaintenanceAuthorization) Validate() error {
	if !validUUID(a.ErasureID) || !validUUID(a.ReceiptID) || a.TargetLearnerGeneration < 2 {
		return invalid("invalid_maintenance_authorization")
	}
	return nil
}

type GenerationStamp struct {
	LearnerGeneration int64 `json:"learner_generation"`
	MemoryGeneration  int64 `json:"memory_generation"`
}

type OutboxIntent struct {
	DeliveryID        string `json:"delivery_id"`
	PayloadHash       string `json:"payload_hash"`
	RecordRevision    int64  `json:"record_revision"`
	LearnerGeneration int64  `json:"learner_generation"`
	RecordGeneration  int64  `json:"record_generation"`
}

type CandidateView struct {
	Candidate       Candidate       `json:"candidate"`
	ContentStatus   string          `json:"content_status"`
	ProposedContent string          `json:"proposed_content,omitempty"`
	ReadGeneration  GenerationStamp `json:"read_generation"`
}

type OperationResult struct {
	Candidate CandidateView `json:"candidate"`
	Record    *Record       `json:"record,omitempty"`
	Delivery  *Delivery     `json:"delivery,omitempty"`
	Replayed  bool          `json:"replayed"`
}

type CandidatePage struct {
	Items          []CandidateView `json:"items"`
	NextCursor     string          `json:"next_cursor,omitempty"`
	ReadGeneration GenerationStamp `json:"read_generation"`
}

type RecordView struct {
	Record         Record          `json:"record"`
	Delivery       Delivery        `json:"delivery"`
	Receipt        Receipt         `json:"receipt"`
	ReadGeneration GenerationStamp `json:"read_generation"`
}

type RecordDetail struct {
	Record         Record              `json:"record"`
	Delivery       Delivery            `json:"delivery"`
	Receipt        Receipt             `json:"receipt"`
	ReadGeneration GenerationStamp     `json:"read_generation"`
	ContentStatus  ExportContentStatus `json:"content_status"`
	Content        string              `json:"content,omitempty"`
}

type RecordPage struct {
	Items          []Record        `json:"items"`
	NextCursor     string          `json:"next_cursor,omitempty"`
	ReadGeneration GenerationStamp `json:"read_generation"`
}

type ExportContentStatus string

const (
	ExportContentAvailable   ExportContentStatus = "available"
	ExportContentDegraded    ExportContentStatus = "degraded"
	ExportContentUnavailable ExportContentStatus = "unavailable"
	ExportContentRedacted    ExportContentStatus = "redacted"
)

// ExportItem is the public record export DTO. It deliberately excludes remote
// credentials, raw upstream responses, attempt tokens, and lease state.
type ExportItem struct {
	Record         Record               `json:"record"`
	DeliveryStatus DeliveryPublicStatus `json:"delivery_status"`
	Receipt        Receipt              `json:"receipt"`
	ContentStatus  ExportContentStatus  `json:"content_status"`
	Content        string               `json:"content,omitempty"`
}

type ExportPage struct {
	Items          []ExportItem    `json:"items"`
	NextCursor     string          `json:"next_cursor,omitempty"`
	ReadGeneration GenerationStamp `json:"read_generation"`
	Degraded       bool            `json:"degraded"`
	ReasonCodes    []string        `json:"reason_codes"`
}

type PageRequest struct {
	Cursor string
	Limit  int
}

func (c Candidate) Validate() error {
	if !validUUID(c.ID) || c.URI != CandidateURI(c.ID) || !validUUID(c.PayloadID) || !validHash(c.ContentHash) || !validUUID(c.ProposerID) {
		return invalid("invalid_candidate_identity")
	}
	if c.LogicalMemoryID != "" && !validUUID(c.LogicalMemoryID) {
		return invalid("invalid_logical_memory_id")
	}
	if !validSource(c.Source) || !validCategory(c.Category) || !validSensitivity(c.Sensitivity) || !validStability(c.Stability) || !validCandidateStatus(c.Status) {
		return invalid("invalid_candidate_enum")
	}
	if !supportedSourceCategory(c.Source, c.Category) {
		return invalid("invalid_source_category")
	}
	if c.PolicyVersion != AdmissionPolicyVersion || !validText(c.Reason, MaxReasonRunes, MaxReferenceBytes*4) || c.CreatedAt.IsZero() || !isUTC(c.CreatedAt) || c.ValidUntil.IsZero() || !isUTC(c.ValidUntil) || !c.ValidUntil.After(c.CreatedAt) {
		return invalid("invalid_candidate_metadata")
	}
	if c.Revision < 1 || (c.Status == CandidatePending && c.Revision != 1) || (c.Status != CandidatePending && c.Revision < 2) {
		return invalid("invalid_candidate_revision")
	}
	if err := c.SourceReference.Validate(c.Source); err != nil {
		return err
	}
	return nil
}

func (r SourceReference) Validate(source SourceKind) error {
	if r.EventID != "" && !validUUID(r.EventID) || r.OperationID != "" && !validUUID(r.OperationID) {
		return invalid("invalid_source_reference")
	}
	if len(r.ModelID) > MaxReferenceBytes || len(r.PromptRevision) > MaxReferenceBytes || !utf8.ValidString(r.ModelID) || !utf8.ValidString(r.PromptRevision) {
		return invalid("invalid_source_reference")
	}
	for _, hash := range r.SourceHashes {
		if !validHash(hash) {
			return invalid("invalid_source_hash")
		}
	}
	if source == SourceGeneratedSummary || source == SourceModelInference {
		if strings.TrimSpace(r.ModelID) == "" || strings.TrimSpace(r.PromptRevision) == "" || len(r.SourceHashes) == 0 {
			return invalid("model_provenance_required")
		}
	}
	return nil
}

func (d CandidateDecision) Validate() error {
	if !validUUID(d.ID) || !validUUID(d.CandidateID) || d.Revision < 2 || !validDecision(d.Decision) || !validText(d.Reason, MaxReasonRunes, MaxReferenceBytes*4) || d.CreatedAt.IsZero() || !isUTC(d.CreatedAt) {
		return invalid("invalid_candidate_decision")
	}
	if d.ActorKind != "device" && d.ActorKind != "model" && d.ActorKind != "system" {
		return invalid("invalid_decision_actor_kind")
	}
	if d.ActorKind != "system" && !validUUID(d.ActorID) || d.ActorKind == "system" && d.ActorID != "" {
		return invalid("invalid_decision_actor")
	}
	if d.OperationID != "" && !validUUID(d.OperationID) || d.RequestHash != "" && !validHash(d.RequestHash) {
		return invalid("invalid_decision_operation")
	}
	return nil
}

func (r Record) Validate() error {
	if !validUUID(r.LogicalMemoryID) || !validUUID(r.ID) || !validUUID(r.CandidateID) || !validUUID(r.DeliveryID) || !validUUID(r.ReceiptID) || !validHash(r.ContentHash) || !validHash(r.ExternalURIDigest) {
		return invalid("invalid_record_identity")
	}
	if r.PreviousRevisionID != "" && !validUUID(r.PreviousRevisionID) || r.Revision < 1 || r.RecordGeneration < 1 || r.LearnerGeneration < 1 || !validRecordStatus(r.Status) || r.CreatedAt.IsZero() || !isUTC(r.CreatedAt) {
		return invalid("invalid_record_metadata")
	}
	if r.ExternalURI != DeterministicExternalURI(r.LogicalMemoryID) || SHA256String(r.ExternalURI) != r.ExternalURIDigest || r.ExternalMemoryID < 0 {
		return invalid("invalid_external_reference")
	}
	return nil
}

func (d Delivery) Validate() error {
	if !validUUID(d.ID) || !validUUID(d.LogicalMemoryID) || !validUUID(d.RecordRevisionID) || !validUUID(d.PayloadID) || !validUUID(d.ReceiptID) || !validHash(d.PayloadHash) {
		return invalid("invalid_delivery_identity")
	}
	if !validDeliveryKind(d.Kind) || !validDeliveryStatus(d.Status) || !validAttemptState(d.AttemptState) || !validPublicStatus(d.PublicStatus) || d.RecordRevision < 1 || d.LearnerGeneration < 1 || d.RecordGeneration < 1 || d.AttemptCount < 0 {
		return invalid("invalid_delivery_metadata")
	}
	if d.ExternalURI != DeterministicExternalURI(d.LogicalMemoryID) || d.ValidUntil.IsZero() || !isUTC(d.ValidUntil) || d.CreatedAt.IsZero() || !isUTC(d.CreatedAt) || d.UpdatedAt.IsZero() || !isUTC(d.UpdatedAt) {
		return invalid("invalid_delivery_time_or_uri")
	}
	return nil
}

func (a Attempt) Validate() error {
	if !validUUID(a.ID) || !validUUID(a.DeliveryID) || !validUUID(a.AttemptToken) || !validUUID(a.LeaseToken) || !validAttemptState(a.State) || a.LeaseExpiresAt.IsZero() || !isUTC(a.LeaseExpiresAt) || a.CreatedAt.IsZero() || !isUTC(a.CreatedAt) || a.UpdatedAt.IsZero() || !isUTC(a.UpdatedAt) {
		return invalid("invalid_attempt")
	}
	if a.ResultDigest != "" && !validHash(a.ResultDigest) {
		return invalid("invalid_attempt_result_digest")
	}
	return nil
}

func (r ExpiryReconciliation) Validate() error {
	if !validUUID(r.ID) || !validUUID(r.DeliveryID) || !validUUID(r.LogicalMemoryID) || !validUUID(r.AttemptToken) || !validHash(r.ContentHash) || r.ExternalURI != DeterministicExternalURI(r.LogicalMemoryID) || r.LearnerGeneration < 1 || r.RecordGeneration < 1 || !validReconciliationStatus(r.Status) || r.CreatedAt.IsZero() || !isUTC(r.CreatedAt) || r.UpdatedAt.IsZero() || !isUTC(r.UpdatedAt) {
		return invalid("invalid_expiry_reconciliation")
	}
	return nil
}

func (r Receipt) Validate() error {
	if !validUUID(r.ID) || !validUUID(r.DeliveryID) || r.Version < 1 || !validReceiptStatus(r.Status) || !validText(r.Reason, MaxReasonRunes, MaxReferenceBytes*4) || !validText(r.VerificationMethod, MaxReferenceBytes, MaxReferenceBytes) || r.CreatedAt.IsZero() || !isUTC(r.CreatedAt) || r.EvidenceDigest != "" && !validHash(r.EvidenceDigest) {
		return invalid("invalid_receipt")
	}
	return nil
}

func (g Generation) Validate() error {
	if g.LearnerGeneration < 1 || g.MemoryGeneration < 1 || g.UpdatedAt.IsZero() || !isUTC(g.UpdatedAt) {
		return invalid("invalid_generation")
	}
	return nil
}

func (o Operation) Validate() error {
	if !validUUID(o.DeviceID) || !validUUID(o.OperationID) || !validHash(o.RequestHash) || !validOperationKind(o.Kind) {
		return invalid("invalid_operation")
	}
	return nil
}

type AdmissionContentDisposition string

const (
	AdmissionContentAutomatic AdmissionContentDisposition = "automatic"
	AdmissionContentReview    AdmissionContentDisposition = "review"
	AdmissionContentForbidden AdmissionContentDisposition = "forbidden"
)

var (
	admissionWhitespacePattern = regexp.MustCompile(`\s+`)
	admissionEmailPattern      = regexp.MustCompile(`(?i)\b[a-z0-9._%+\-]+@[a-z0-9.\-]+\.[a-z]{2,}\b`)
	admissionLongNumberPattern = regexp.MustCompile(`\b(?:[0-9][ -]?){13,19}\b`)
	admissionCNIDPattern       = regexp.MustCompile(`(?i)\b[0-9]{17}[0-9x]\b`)
	admissionTokenPattern      = regexp.MustCompile(`(?i)\b(?:akia[0-9a-z]{16}|gh[pousr]_[0-9a-z]{20,}|eyj[0-9a-z_-]{8,}\.[0-9a-z_-]{8,}\.[0-9a-z_-]{8,})\b`)
	admissionTranscriptPattern = regexp.MustCompile(`(?im)^\s*(?:user|assistant|system|用户|助手|系统)\s*[:：]`)
	admissionClockPattern      = regexp.MustCompile(`(?i)\b(?:[01]?[0-9]|2[0-3]):[0-5][0-9]\b|\b(?:1[0-2]|[1-9])\s*(?:am|pm)\b`)
)

var admissionSensitiveMarkers = []string{
	"password", "passwd", "api key", "api-key", "apikey", "access token", "refresh token",
	"bearer token", "authorization:", "client_secret", "private key", "secret key", "session token",
	"social security", "ssn", "credit card", "bank account", "passport number", "home address",
	"密码", "口令", "密钥", "令牌", "访问令牌", "刷新令牌", "身份证", "手机号", "电话号码",
	"银行卡", "信用卡", "护照号", "家庭住址", "详细住址", "邮箱地址",
}

var admissionForbiddenTruthMarkers = []string{
	"answer key", "complete answer", "full answer", "model answer", "worked solution", "grading rubric",
	"grading criteria", "scoring rubric", "chat transcript", "conversation transcript", "conversation history",
	"raw chat", "learning goal", "study goal", "learning route", "learning path", "mastery state",
	"mastery score", "review queue", "sync state", "device token", "misconception hypothesis",
	"参考答案", "完整答案", "全部答案", "答案解析", "标准答案", "评分标准", "评分量规", "评分细则",
	"聊天记录", "对话记录", "完整对话", "学习目标", "学习路线", "学习路径", "掌握度",
	"掌握状态", "复习队列", "同步状态", "设备令牌", "错因假设", "误区假设",
}

var admissionPreferenceIntents = []string{
	"i prefer", "i like", "i want", "my preference", "please ", "prefer ", "keep ", "use ",
	"give me", "show me", "avoid ", "respond ", "answer ", "explain ", "ask ", "remember ",
	"我偏好", "我喜欢", "我希望", "我的偏好", "请用", "请保持", "请给", "请先", "回答时",
	"解释时", "提问时", "不要", "记住",
}

var admissionPreferenceTargets = []string{
	"concise", "shorter", "brief", "detailed", "example", "step by step", "steps", "bullet",
	"hint", "explanation", "answer", "question", "tone", "language", "analogy", "worked example",
	"简洁", "简短", "详细", "例子", "示例", "分步", "步骤", "要点", "提示", "解释", "回答",
	"提问", "语气", "语言", "中文", "英文", "类比",
}

var admissionTimeIntents = []string{
	"i am available", "i'm available", "i can study", "i only have", "available to study", "study time",
	"time constraint", "free to study", "我有空", "有空学习", "我可以学习", "我能学习", "我只能学习", "学习时间",
	"可学习", "有时间学习", "有时间", "只有", "限时",
}

var admissionTimeTokens = []string{
	"monday", "tuesday", "wednesday", "thursday", "friday", "saturday", "sunday", "weekday",
	"weekend", "morning", "afternoon", "evening", "tonight", "tomorrow", "minutes", "hours",
	"before ", "after ", "daily", "weekly", "周一", "周二", "周三", "周四", "周五", "周六", "周日",
	"工作日", "周末", "早上", "上午", "中午", "下午", "晚上", "今晚", "明天", "分钟", "小时",
	"每天", "每周", "之前", "之后",
}

// ClassifyAdmissionContent is a deliberately narrow, deterministic policy classifier.
// It only proves a small set of compact preference/time-constraint shapes and fails closed.
func ClassifyAdmissionContent(category Category, content string) AdmissionContentDisposition {
	normalized := strings.ToLower(strings.TrimSpace(content))
	flat := admissionWhitespacePattern.ReplaceAllString(normalized, " ")
	if admissionTranscriptPattern.MatchString(content) || admissionEmailPattern.MatchString(normalized) ||
		admissionLongNumberPattern.MatchString(normalized) || admissionCNIDPattern.MatchString(normalized) ||
		admissionTokenPattern.MatchString(normalized) || containsAny(flat, admissionSensitiveMarkers) ||
		containsAny(flat, admissionForbiddenTruthMarkers) {
		return AdmissionContentForbidden
	}
	if utf8.RuneCountInString(content) > 512 || strings.Count(content, "\n") > 1 {
		return AdmissionContentReview
	}
	switch category {
	case CategoryInteractionPreference:
		if containsAny(flat, admissionPreferenceIntents) && containsAny(flat, admissionPreferenceTargets) {
			return AdmissionContentAutomatic
		}
	case CategoryTimeConstraint:
		if containsAny(flat, admissionTimeIntents) && (containsAny(flat, admissionTimeTokens) || admissionClockPattern.MatchString(flat)) {
			return AdmissionContentAutomatic
		}
	}
	return AdmissionContentReview
}

func EvaluateAdmission(candidate Candidate, content string, now time.Time) CandidateStatus {
	disposition := ClassifyAdmissionContent(candidate.Category, content)
	if disposition == AdmissionContentForbidden {
		return CandidateRejected
	}
	if candidate.Source == SourceUserStatement &&
		(candidate.Category == CategoryInteractionPreference || candidate.Category == CategoryTimeConstraint) &&
		candidate.Stability == StabilityStable && candidate.Sensitivity == SensitivityNonSensitive &&
		candidate.PolicyVersion == AdmissionPolicyVersion && now.UTC().Before(candidate.ValidUntil) &&
		disposition == AdmissionContentAutomatic {
		return CandidateAdmitted
	}
	return CandidatePending
}

func ValidateProposedContent(category Category, content string) error {
	if !validCategory(category) {
		return invalid("invalid_category")
	}
	if forbiddenCategory(category) {
		return &Error{Code: CodeMemoryPolicyRejected, Reason: "forbidden_business_truth"}
	}
	if !utf8.ValidString(content) || strings.TrimSpace(content) == "" || strings.IndexByte(content, 0) >= 0 || len(content) > MaxContentBytes || utf8.RuneCountInString(content) > MaxContentRunes {
		return invalid("invalid_content")
	}
	return nil
}

type DeliveryPolicy struct {
	CandidateID       string
	Source            SourceKind
	SourceReference   SourceReference
	Category          Category
	Sensitivity       Sensitivity
	Stability         Stability
	PolicyVersion     string
	ContentHash       string
	AdmissionDecision CandidateDecision
}

// ValidateDeliveryPayload is the adapter-side policy gate and must run after loading
// the private payload and immutable admission provenance by delivery ID.
func ValidateDeliveryPayload(policy DeliveryPolicy, content, expectedHash string) error {
	if err := ValidateProposedContent(policy.Category, content); err != nil {
		return err
	}
	if !validUUID(policy.CandidateID) || !supportedSourceCategory(policy.Source, policy.Category) ||
		!validSensitivity(policy.Sensitivity) || policy.Stability != StabilityStable ||
		policy.PolicyVersion != AdmissionPolicyVersion {
		return &Error{Code: CodeMemoryPolicyRejected, Reason: "delivery_policy_metadata_rejected"}
	}
	if err := policy.SourceReference.Validate(policy.Source); err != nil {
		return &Error{Code: CodeMemoryPolicyRejected, Reason: "delivery_source_provenance_invalid", Cause: err}
	}
	if !validHash(policy.ContentHash) || !validHash(expectedHash) || policy.ContentHash != expectedHash ||
		SHA256String(content) != policy.ContentHash {
		return &Error{Code: CodeMemoryPolicyRejected, Reason: "delivery_payload_hash_mismatch"}
	}
	decision := policy.AdmissionDecision
	if err := decision.Validate(); err != nil || decision.CandidateID != policy.CandidateID || decision.Decision != DecisionAdmit {
		return &Error{Code: CodeMemoryPolicyRejected, Reason: "delivery_admission_provenance_invalid", Cause: err}
	}
	disposition := ClassifyAdmissionContent(policy.Category, content)
	if disposition == AdmissionContentForbidden {
		return &Error{Code: CodeMemoryPolicyRejected, Reason: "delivery_content_forbidden"}
	}
	if decision.ActorKind == "system" {
		if policy.Source != SourceUserStatement || !remoteWritableCategory(policy.Category) ||
			policy.Sensitivity != SensitivityNonSensitive || disposition != AdmissionContentAutomatic {
			return &Error{Code: CodeMemoryPolicyRejected, Reason: "automatic_admission_content_unproven"}
		}
		return nil
	}
	if decision.ActorKind != "device" || decision.ActorID == "" || decision.OperationID == "" || decision.RequestHash == "" {
		return &Error{Code: CodeMemoryPolicyRejected, Reason: "manual_admission_provenance_required"}
	}
	return nil
}

func CanTransitionCandidate(from CandidateStatus, decision Decision) bool {
	return from == CandidatePending && (decision == DecisionAdmit || decision == DecisionReject || decision == DecisionExpire)
}

func CandidateStatusForDecision(decision Decision) CandidateStatus {
	switch decision {
	case DecisionAdmit:
		return CandidateAdmitted
	case DecisionReject:
		return CandidateRejected
	case DecisionExpire:
		return CandidateExpired
	default:
		return ""
	}
}

func CanTransitionRecord(from, to RecordStatus) bool {
	switch from {
	case RecordQueued:
		return to == RecordApplied || to == RecordPermanentlyRejected || to == RecordDeletePending
	case RecordApplied:
		return to == RecordSuperseded || to == RecordDeletePending
	case RecordDeletePending:
		return to == RecordDeleted
	default:
		return false
	}
}

func CanTransitionDelivery(from, to DeliveryStatus) bool {
	switch from {
	case DeliveryStatusQueued:
		return to == DeliveryStatusApplied || to == DeliveryStatusPermanentlyRejected ||
			to == DeliveryStatusFenced || to == DeliveryStatusExpiryReconciling ||
			to == DeliveryStatusExpired
	case DeliveryStatusApplied:
		return to == DeliveryStatusFenced || to == DeliveryStatusDeleted
	case DeliveryStatusFenced:
		return to == DeliveryStatusDeleted
	case DeliveryStatusExpiryReconciling:
		return to == DeliveryStatusExpired || to == DeliveryStatusFenced || to == DeliveryStatusDeleted
	case DeliveryStatusDeletePending:
		return to == DeliveryStatusDeleted || to == DeliveryStatusFenced
	default:
		return false
	}
}

func CanTransitionAttempt(from, to AttemptState) bool {
	switch from {
	case AttemptPrepared:
		return to == AttemptSent || to == AttemptFenced || to == AttemptFailed
	case AttemptSent:
		return to == AttemptUnknown || to == AttemptReconciling || to == AttemptConfirmed || to == AttemptFenced
	case AttemptUnknown:
		return to == AttemptReconciling || to == AttemptConfirmed || to == AttemptFenced
	case AttemptReconciling:
		return to == AttemptConfirmed || to == AttemptFenced
	default:
		return false
	}
}

func PublicDeliveryStatus(status DeliveryStatus) DeliveryPublicStatus {
	switch status {
	case DeliveryStatusApplied:
		return DeliveryApplied
	case DeliveryStatusPermanentlyRejected, DeliveryStatusFenced, DeliveryStatusExpired, DeliveryStatusDeleted:
		return DeliveryRejected
	default:
		return DeliveryQueued
	}
}

func CandidateURI(id string) string { return "candidate://" + id }
func DeterministicExternalURI(logicalMemoryID string) string {
	return "nocturne://core/edu-agent/" + logicalMemoryID
}
func SHA256String(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}
func CanonicalRequestHash(value any) (string, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	var normalized any
	decoder := json.NewDecoder(strings.NewReader(string(encoded)))
	decoder.UseNumber()
	if err := decoder.Decode(&normalized); err != nil {
		return "", err
	}
	encoded, err = json.Marshal(normalized)
	if err != nil {
		return "", err
	}
	return SHA256String(string(encoded)), nil
}

func validUUID(value string) bool {
	parsed, err := uuid.Parse(value)
	return err == nil && parsed.String() == strings.ToLower(value)
}
func validHash(value string) bool {
	if len(value) != 64 || value != strings.ToLower(value) {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size
}
func isUTC(value time.Time) bool { return value.Location() == time.UTC }
func validText(value string, maxRunes, maxBytes int) bool {
	return utf8.ValidString(value) && strings.TrimSpace(value) != "" && utf8.RuneCountInString(value) <= maxRunes && len(value) <= maxBytes
}
func invalid(reason string) error { return &Error{Code: CodeInvalidRequest, Reason: reason} }
func validOperationKind(v OperationKind) bool {
	return v == OperationCreateCandidate || v == OperationCandidateDecision || v == OperationRecordDelete || v == OperationDeliveryReplay || v == OperationPrivacyErasure
}
func validSource(v SourceKind) bool {
	return v == SourceUserStatement || v == SourceModelInference || v == SourceLongTermBackground || v == SourceGeneratedSummary
}
func validSensitivity(v Sensitivity) bool {
	return v == SensitivityNonSensitive || v == SensitivitySensitive
}
func validStability(v Stability) bool { return v == StabilityTransient || v == StabilityStable }
func validCandidateStatus(v CandidateStatus) bool {
	return v == CandidatePending || v == CandidateAdmitted || v == CandidateRejected || v == CandidateExpired
}
func validDecision(v Decision) bool {
	return v == DecisionAdmit || v == DecisionReject || v == DecisionExpire
}
func validRecordStatus(v RecordStatus) bool {
	return v == RecordQueued || v == RecordApplied || v == RecordPermanentlyRejected || v == RecordSuperseded || v == RecordDeletePending || v == RecordDeleted
}
func validPublicStatus(v DeliveryPublicStatus) bool {
	return v == DeliveryQueued || v == DeliveryApplied || v == DeliveryRejected
}
func validAttemptState(v AttemptState) bool {
	return v == AttemptPrepared || v == AttemptSent || v == AttemptUnknown || v == AttemptReconciling || v == AttemptConfirmed || v == AttemptFenced || v == AttemptFailed
}
func validDeliveryKind(v DeliveryKind) bool {
	return v == DeliveryAdmit || v == DeliveryCorrection || v == DeliveryDelete || v == DeliveryErasure
}
func validDeliveryStatus(v DeliveryStatus) bool {
	return v == DeliveryStatusQueued || v == DeliveryStatusApplied || v == DeliveryStatusPermanentlyRejected || v == DeliveryStatusFenced || v == DeliveryStatusExpiryReconciling || v == DeliveryStatusExpired || v == DeliveryStatusDeletePending || v == DeliveryStatusDeleted
}
func validReconciliationStatus(v ReconciliationStatus) bool {
	return v == ReconciliationPending || v == ReconciliationReconciling || v == ReconciliationDeletePending || v == ReconciliationAbsenceVerified || v == ReconciliationVerified || v == ReconciliationConflict
}
func validReceiptStatus(v ReceiptStatus) bool {
	return v == ReceiptPending || v == ReceiptSucceeded || v == ReceiptPartial || v == ReceiptFailed || v == ReceiptUnknown || v == ReceiptNotApplicable || v == ReceiptUnsupported
}
func validCategory(v Category) bool {
	switch v {
	case CategoryInteractionPreference, CategoryTimeConstraint, CategoryPersonalContext, CategoryGeneratedSummary,
		CategoryRawChat, CategoryCompleteAttempt, CategoryQuestionOrRubric, CategoryGoal, CategoryRoute,
		CategoryMastery, CategoryEvidence, CategoryMisconception, CategoryReviewQueue, CategorySyncState,
		CategoryDeviceToken, CategoryModelSecret, CategoryNocturneSecret:
		return true
	default:
		return false
	}
}
func remoteWritableCategory(v Category) bool {
	return v == CategoryInteractionPreference || v == CategoryTimeConstraint || v == CategoryPersonalContext
}

func supportedSourceCategory(source SourceKind, category Category) bool {
	if !validSource(source) {
		return false
	}
	if source == SourceGeneratedSummary {
		return category == CategoryGeneratedSummary
	}
	return category != CategoryGeneratedSummary && remoteWritableCategory(category)
}

func containsAny(value string, markers []string) bool {
	for _, marker := range markers {
		if strings.Contains(value, marker) {
			return true
		}
	}
	return false
}

func forbiddenCategory(v Category) bool {
	switch v {
	case CategoryRawChat, CategoryCompleteAttempt, CategoryQuestionOrRubric, CategoryGoal, CategoryRoute,
		CategoryMastery, CategoryEvidence, CategoryMisconception, CategoryReviewQueue, CategorySyncState,
		CategoryDeviceToken, CategoryModelSecret, CategoryNocturneSecret:
		return true
	default:
		return false
	}
}

type AttemptTransition struct {
	AttemptID     string
	AttemptToken  string
	LeaseToken    string
	From          AttemptState
	To            AttemptState
	BootEpoch     string
	ResultDigest  string
	ErrorCategory string
	At            time.Time
}

type AttemptOutcomeKind string

const (
	AttemptOutcomeApplied             AttemptOutcomeKind = "applied"
	AttemptOutcomePermanentlyRejected AttemptOutcomeKind = "permanently_rejected"
	AttemptOutcomeFenced              AttemptOutcomeKind = "fenced"
	AttemptOutcomeDeleted             AttemptOutcomeKind = "deleted"
)

type AttemptOutcome struct {
	AttemptID          string
	AttemptToken       string
	LeaseToken         string
	From               AttemptState
	Kind               AttemptOutcomeKind
	ReceiptID          string
	ReceiptStatus      ReceiptStatus
	Reason             string
	VerificationMethod string
	EvidenceDigest     string
	ExternalNodeID     string
	ExternalMemoryID   int64
	ResultDigest       string
	ErrorCategory      string
	At                 time.Time
}

type ReconciliationTransition struct {
	ReconciliationID string
	LeaseToken       string
	From             ReconciliationStatus
	To               ReconciliationStatus
	At               time.Time
}

type ReconciliationResult string

const (
	ReconciliationAbsenceResult  ReconciliationResult = "absence_verified"
	ReconciliationDeleteResult   ReconciliationResult = "delete_verified"
	ReconciliationConflictResult ReconciliationResult = "conflict"
)

type ReconciliationFinalization struct {
	ReconciliationID string
	LeaseToken       string
	From             ReconciliationStatus
	Result           ReconciliationResult
	ReceiptID        string
	Reason           string
	EvidenceDigest   string
	At               time.Time
}

type AttemptPersistence interface {
	ClaimAttempt(context.Context, string, time.Time, time.Duration) (Attempt, error)
	TransitionAttempt(context.Context, AttemptTransition) (Attempt, error)
	FinalizeAttempt(context.Context, AttemptOutcome) (Attempt, error)
	FenceDelivery(context.Context, string, string, time.Time) error
	ClaimExpiryReconciliation(context.Context, time.Time, time.Duration) (ExpiryReconciliation, error)
	TransitionExpiryReconciliation(context.Context, ReconciliationTransition) (ExpiryReconciliation, error)
	FinalizeExpiryReconciliation(context.Context, ReconciliationFinalization) (ExpiryReconciliation, error)
}

type ReplayPlan struct {
	Operation  Operation
	DeliveryID string
}

type Store interface {
	CreateCandidate(context.Context, CreatePlan) (OperationResult, error)
	DecideCandidate(context.Context, DecisionPlan) (OperationResult, error)
	DeleteRecord(context.Context, DeletePlan) (OperationResult, error)
	ReplayDelivery(context.Context, ReplayPlan) (OperationResult, error)
	Candidate(context.Context, string) (CandidateView, error)
	Record(context.Context, string) (RecordView, error)
	ListCandidates(context.Context, PageRequest) (CandidatePage, error)
	ListRecords(context.Context, PageRequest) (RecordPage, error)
	ExpireCandidates(context.Context, time.Time, int) (int, error)
	ExpireDeliveries(context.Context, time.Time, int) (int, error)
}
