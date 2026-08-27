package api

import (
	"encoding/json"
	"time"
)

const (
	OfflineMaxSyncBodyBytes = 8 << 20
	OfflineMaxSyncItems     = 50
)

type Uint63Decimal string
type OfflineTimestamp string

type OfflineSignerKeyStatus string

const (
	OfflineSignerKeyActive     OfflineSignerKeyStatus = "active"
	OfflineSignerKeyVerifyOnly OfflineSignerKeyStatus = "verify_only"
	OfflineSignerKeyRetired    OfflineSignerKeyStatus = "retired"
)

type OfflineActivityType string

const (
	OfflineActivityObjective   OfflineActivityType = "objective"
	OfflineActivityOpen        OfflineActivityType = "open"
	OfflineActivityExplanation OfflineActivityType = "explanation"
)

type OfflineHelpLevel string

const (
	OfflineHelpNone           OfflineHelpLevel = "none"
	OfflineHelpHint           OfflineHelpLevel = "hint"
	OfflineHelpScaffold       OfflineHelpLevel = "scaffold"
	OfflineHelpAnswerRevealed OfflineHelpLevel = "answer_revealed"
)

type OfflinePackTruncatedReason string

const (
	OfflinePackCurrentActivityOnly OfflinePackTruncatedReason = "current_activity_only"
	OfflinePackRequestedLimited    OfflinePackTruncatedReason = "requested_count_limited"
	OfflinePackActivitySizeLimited OfflinePackTruncatedReason = "activity_size_limited"
	OfflinePackSizeLimited         OfflinePackTruncatedReason = "pack_size_limited"
	OfflinePackModelPartial        OfflinePackTruncatedReason = "model_partial"
	OfflinePackRouteExhausted      OfflinePackTruncatedReason = "route_exhausted"
	OfflinePackReviewExhausted     OfflinePackTruncatedReason = "review_exhausted"
)

type OfflineOperationType string

const (
	OfflineAttemptCompleted OfflineOperationType = "offline_attempt_completed"
	OfflineActivitySkipped  OfflineOperationType = "offline_activity_skipped"
)

type OfflineObservationKind string

const (
	OfflineActivityPresented OfflineObservationKind = "activity_presented"
	OfflineAnswerRecorded    OfflineObservationKind = "answer_recorded"
)

type OfflineSkipReason string

const (
	OfflineUserSkipped         OfflineSkipReason = "user_skipped"
	OfflineExpiredLocally      OfflineSkipReason = "expired_locally"
	OfflineUnreadableLocalItem OfflineSkipReason = "unreadable_local_item"
)

type OfflineResultKind string

const (
	OfflineResultArchived     OfflineResultKind = "archived"
	OfflineResultRetryable    OfflineResultKind = "retryable"
	OfflineResultBlocked      OfflineResultKind = "blocked"
	OfflineResultConflict     OfflineResultKind = "conflict"
	OfflineResultNotProcessed OfflineResultKind = "not_processed"
)

type OfflineArchiveStatus string

const (
	OfflineArchivedSucceeded    OfflineArchiveStatus = "archived_succeeded"
	OfflineArchivedRejected     OfflineArchiveStatus = "archived_rejected"
	OfflineNotArchivedRetryable OfflineArchiveStatus = "not_archived_retryable"
	OfflineNotArchivedBlocked   OfflineArchiveStatus = "not_archived_blocked"
	OfflineIdempotencyConflict  OfflineArchiveStatus = "idempotency_conflict"
	OfflineSequenceConflict     OfflineArchiveStatus = "device_sequence_conflict"
	OfflineNotProcessed         OfflineArchiveStatus = "not_processed"
)

type OfflineAssessmentStatus string

const (
	OfflineAssessmentNotRequested OfflineAssessmentStatus = "not_requested"
	OfflineAssessmentQueued       OfflineAssessmentStatus = "queued"
	OfflineAssessmentProcessing   OfflineAssessmentStatus = "processing"
	OfflineAssessmentPendingRetry OfflineAssessmentStatus = "pending_retry"
	OfflineAssessmentCompleted    OfflineAssessmentStatus = "completed"
	OfflineAssessmentFailed       OfflineAssessmentStatus = "failed"
)

type OfflineEvidenceStatus string

const (
	OfflineEvidenceAccepted          OfflineEvidenceStatus = "accepted"
	OfflineEvidenceProvisional       OfflineEvidenceStatus = "provisional"
	OfflineEvidencePendingEvaluation OfflineEvidenceStatus = "pending_evaluation"
	OfflineEvidenceNotEligible       OfflineEvidenceStatus = "not_eligible"
	OfflineEvidenceNotApplicable     OfflineEvidenceStatus = "not_applicable"
	OfflineEvidenceUnchanged         OfflineEvidenceStatus = "unchanged"
)

type OfflineReasonCode string

const (
	OfflineReasonDuplicateActivity    OfflineReasonCode = "duplicate_activity_submission"
	OfflineReasonStaleKnowledge       OfflineReasonCode = "stale_knowledge_head"
	OfflineReasonExpiredActivity      OfflineReasonCode = "expired_activity"
	OfflineReasonStaleContext         OfflineReasonCode = "stale_context"
	OfflineReasonStalePolicy          OfflineReasonCode = "stale_policy"
	OfflineReasonAnswerRevealed       OfflineReasonCode = "answer_revealed"
	OfflineReasonModelUnavailable     OfflineReasonCode = "model_unavailable"
	OfflineReasonEvaluationInvalid    OfflineReasonCode = "evaluation_invalid"
	OfflineReasonActivityInvalid      OfflineReasonCode = "offline_activity_invalid"
	OfflineReasonContentRedacted      OfflineReasonCode = "content_redacted"
	OfflineReasonPrivacyClearing      OfflineReasonCode = "privacy_clear_in_progress"
	OfflineReasonDeviceRevoked        OfflineReasonCode = "device_revoked"
	OfflineReasonAuthorizationExpired OfflineReasonCode = "authorization_expired"
	OfflineReasonAuthorizationInvalid OfflineReasonCode = "authorization_invalid"
	OfflineReasonVersionConflict      OfflineReasonCode = "version_conflict"
	OfflineReasonIdempotencyConflict  OfflineReasonCode = "idempotency_conflict"
	OfflineReasonSequenceConflict     OfflineReasonCode = "device_sequence_conflict"
	OfflineReasonNotProcessed         OfflineReasonCode = "not_processed"
	OfflineReasonInternalError        OfflineReasonCode = "internal_error"
)

type OfflinePairingBootstrap struct {
	ProtocolVersion   int                           `json:"protocol_version"`
	LearnerGeneration Uint63Decimal                 `json:"learner_generation"`
	ServerBaseURL     string                        `json:"server_base_url"`
	SignerManifest    OfflineSignerManifestEnvelope `json:"signer_manifest"`
}

type OfflinePrepareRequest struct {
	OperationID             string        `json:"operation_id"`
	PayloadSchemaVersion    int           `json:"payload_schema_version"`
	ExpectedSessionVersion  Uint63Decimal `json:"expected_session_version"`
	TrustedManifestRevision Uint63Decimal `json:"trusted_manifest_revision"`
	TrustedManifestDigest   string        `json:"trusted_manifest_digest"`
	RequestedCount          *int          `json:"requested_count,omitempty"`
	RequestedTTLSeconds     *int          `json:"requested_ttl_seconds,omitempty"`
}

type OfflineSignerKey struct {
	KeyID             string                 `json:"key_id"`
	PublicKey         string                 `json:"public_key"`
	Fingerprint       string                 `json:"fingerprint"`
	NotBefore         OfflineTimestamp       `json:"not_before"`
	NotAfter          OfflineTimestamp       `json:"not_after"`
	StatusEffectiveAt OfflineTimestamp       `json:"status_effective_at"`
	Status            OfflineSignerKeyStatus `json:"status"`
}

type OfflineSignerManifestPayload struct {
	ProtocolVersion        int                `json:"protocol_version"`
	ManifestRevision       Uint63Decimal      `json:"manifest_revision"`
	Issuer                 string             `json:"issuer"`
	ServerBaseURL          string             `json:"server_base_url"`
	PreviousManifestDigest string             `json:"previous_manifest_digest"`
	IssuedAt               OfflineTimestamp   `json:"issued_at"`
	Keys                   []OfflineSignerKey `json:"keys"`
}

type OfflineSignerManifestEnvelope struct {
	Payload     OfflineSignerManifestPayload `json:"payload"`
	SignerKeyID string                       `json:"signer_key_id"`
	Signature   string                       `json:"signature"`
}

type OfflineAuthorizationPayload struct {
	ProtocolVersion       int              `json:"protocol_version"`
	Format                string           `json:"format"`
	Issuer                string           `json:"issuer"`
	SignerKeyID           string           `json:"signer_key_id"`
	PackID                string           `json:"pack_id"`
	DeviceID              string           `json:"device_id"`
	CredentialEpoch       Uint63Decimal    `json:"credential_epoch"`
	LearnerGeneration     Uint63Decimal    `json:"learner_generation"`
	ServerOriginDigest    string           `json:"server_origin_digest"`
	OfflineActivityID     string           `json:"offline_activity_id"`
	ActivityRevision      Uint63Decimal    `json:"activity_revision"`
	SubmissionID          string           `json:"submission_id"`
	OperationID           string           `json:"operation_id"`
	DeviceSequence        Uint63Decimal    `json:"device_seq"`
	ExpectedVersion       Uint63Decimal    `json:"expected_version"`
	ActivityPayloadDigest string           `json:"activity_payload_digest"`
	EligibleUntil         OfflineTimestamp `json:"eligible_until"`
	ArchiveUntil          OfflineTimestamp `json:"archive_until"`
}

type OfflineAuthorizationEnvelope struct {
	Payload     OfflineAuthorizationPayload `json:"payload"`
	SignerKeyID string                      `json:"signer_key_id"`
	Signature   string                      `json:"signature"`
}

type OfflineSourceRange struct {
	Start int `json:"start"`
	End   int `json:"end"`
}

type OfflineKnowledgeReference struct {
	KnowledgeRevisionID string             `json:"knowledge_revision_id"`
	NodeID              string             `json:"node_id"`
	NodeRevisionID      string             `json:"node_revision_id"`
	DocumentRevisionID  string             `json:"document_revision_id,omitempty"`
	Range               OfflineSourceRange `json:"range"`
	Slice               string             `json:"slice,omitempty"`
	SliceSHA256         string             `json:"slice_sha256"`
}

type OfflineRubricItem struct {
	RubricItemID         string   `json:"rubric_item_id"`
	Criterion            string   `json:"criterion"`
	RequiredReferenceIDs []string `json:"required_reference_ids,omitempty"`
}

type OfflineObjectiveRule struct {
	AcceptedAnswers []string `json:"accepted_answers"`
	CaseSensitive   bool     `json:"case_sensitive"`
	TrimSpace       bool     `json:"trim_space"`
}

type OfflineRubric struct {
	RubricRevision string                `json:"rubric_revision"`
	Items          []OfflineRubricItem   `json:"items"`
	ObjectiveRule  *OfflineObjectiveRule `json:"objective_rule,omitempty"`
}

type OfflineActivity struct {
	ActivityID              string                      `json:"activity_id"`
	Revision                int64                       `json:"revision"`
	SessionID               string                      `json:"session_id"`
	GoalRevisionID          string                      `json:"goal_revision_id"`
	RouteRevisionID         string                      `json:"route_revision_id"`
	RouteStepID             string                      `json:"route_step_id"`
	KnowledgeRevisionID     string                      `json:"knowledge_revision_id"`
	TargetNodeID            string                      `json:"target_node_id"`
	TargetNodeRevisionID    string                      `json:"target_node_revision_id"`
	KnowledgeReferences     []OfflineKnowledgeReference `json:"knowledge_references"`
	Prompt                  string                      `json:"prompt"`
	Type                    OfflineActivityType         `json:"type"`
	Rubric                  OfflineRubric               `json:"rubric"`
	Difficulty              int                         `json:"difficulty"`
	AllowedHelp             []OfflineHelpLevel          `json:"allowed_help"`
	ActivityPolicyVersion   string                      `json:"activity_policy_version"`
	AssessmentPolicyVersion string                      `json:"assessment_policy_version"`
	ReviewPolicyVersion     string                      `json:"review_policy_version"`
	SourceProposalID        string                      `json:"source_proposal_id,omitempty"`
	AttachedFreeQuestionID  string                      `json:"attached_free_question_id,omitempty"`
	AttachedFreeAnswerID    string                      `json:"attached_free_answer_id,omitempty"`
	Review                  bool                        `json:"review"`
	CreatedAt               OfflineTimestamp            `json:"created_at"`
}

type OfflinePackItem struct {
	Activity              OfflineActivity              `json:"activity"`
	ActivityPayloadDigest string                       `json:"activity_payload_digest"`
	Authorization         OfflineAuthorizationEnvelope `json:"authorization"`
}

type OfflinePackPayload struct {
	ProtocolVersion   int                        `json:"protocol_version"`
	PackID            string                     `json:"pack_id"`
	Revision          Uint63Decimal              `json:"revision"`
	DeviceID          string                     `json:"device_id"`
	LearnerGeneration Uint63Decimal              `json:"learner_generation"`
	ParentSessionID   string                     `json:"parent_session_id"`
	IssuedAt          OfflineTimestamp           `json:"issued_at"`
	EligibleUntil     OfflineTimestamp           `json:"eligible_until"`
	ArchiveUntil      OfflineTimestamp           `json:"archive_until"`
	Truncated         bool                       `json:"truncated"`
	TruncatedReason   OfflinePackTruncatedReason `json:"truncated_reason,omitempty"`
	Items             []OfflinePackItem          `json:"items"`
}

type OfflinePackEnvelope struct {
	Payload     OfflinePackPayload `json:"payload"`
	SignerKeyID string             `json:"signer_key_id"`
	Signature   string             `json:"signature"`
}

type OfflinePrepareResponseSignaturePayload struct {
	ProtocolVersion  int              `json:"protocol_version"`
	OperationID      string           `json:"operation_id"`
	RequestHash      string           `json:"request_hash"`
	Replayed         bool             `json:"replayed"`
	PackDigest       string           `json:"pack_digest"`
	ManifestRevision Uint63Decimal    `json:"manifest_revision"`
	ManifestDigest   string           `json:"manifest_digest"`
	ResponseAt       OfflineTimestamp `json:"response_at"`
}

type OfflinePrepareResponseSignatureEnvelope struct {
	Payload     OfflinePrepareResponseSignaturePayload `json:"payload"`
	SignerKeyID string                                 `json:"signer_key_id"`
	Signature   string                                 `json:"signature"`
}

type OfflinePrepareResponse struct {
	OperationID       string                                  `json:"operation_id"`
	Replayed          bool                                    `json:"replayed"`
	Pack              OfflinePackEnvelope                     `json:"pack"`
	ManifestChain     []OfflineSignerManifestEnvelope         `json:"manifest_chain"`
	ResponseSignature OfflinePrepareResponseSignatureEnvelope `json:"response_signature"`
}

type OfflineObservation struct {
	Kind       OfflineObservationKind `json:"kind"`
	OccurredAt *OfflineTimestamp      `json:"occurred_at"`
}

type OfflineAttemptPayload struct {
	Answer       string               `json:"answer"`
	AnswerSHA256 string               `json:"answer_sha256"`
	Help         OfflineHelpLevel     `json:"help"`
	Observations []OfflineObservation `json:"observations"`
}

type OfflineSkipPayload struct {
	Reason OfflineSkipReason `json:"reason"`
}

type OfflineOperation struct {
	OperationID          string                      `json:"operation_id"`
	DeviceID             string                      `json:"device_id"`
	DeviceSequence       Uint63Decimal               `json:"device_seq"`
	SubmissionID         string                      `json:"submission_id"`
	PayloadSchemaVersion int                         `json:"payload_schema_version"`
	AggregateType        string                      `json:"aggregate_type"`
	AggregateID          string                      `json:"aggregate_id"`
	ExpectedVersion      Uint63Decimal               `json:"expected_version"`
	OfflineActivityID    string                      `json:"offline_activity_id"`
	ActivityRevision     Uint63Decimal               `json:"activity_revision"`
	Authorization        OfflineAuthorizationPayload `json:"authorization"`
	Signature            string                      `json:"signature"`
	OccurredAt           *OfflineTimestamp           `json:"occurred_at"`
	OperationType        OfflineOperationType        `json:"operation_type"`
	Payload              json.RawMessage             `json:"payload"`
}

type OfflineSyncRequest struct {
	SyncRequestID        string             `json:"sync_request_id"`
	PayloadSchemaVersion int                `json:"payload_schema_version"`
	Operations           []OfflineOperation `json:"operations"`
}

type OfflineIngestReceipt struct {
	ReceiptID              string               `json:"receipt_id"`
	ArchivedAt             OfflineTimestamp     `json:"archived_at"`
	AggregateVersion       Uint63Decimal        `json:"aggregate_version"`
	FirstEventSequence     Uint63Decimal        `json:"first_event_seq"`
	LastEventSequence      Uint63Decimal        `json:"last_event_seq"`
	ProjectionAsOfEventSeq Uint63Decimal        `json:"projection_as_of_event_seq"`
	ArchiveStatus          OfflineArchiveStatus `json:"archive_status"`
}

type OfflineStatusTicket struct {
	TicketID    string           `json:"ticket_id"`
	OperationID string           `json:"operation_id"`
	Revision    Uint63Decimal    `json:"revision"`
	UpdatedAt   OfflineTimestamp `json:"updated_at"`
}

type OfflineSyncItemResult struct {
	ResultKind       OfflineResultKind       `json:"result_kind"`
	OperationID      string                  `json:"operation_id"`
	DeviceSequence   Uint63Decimal           `json:"device_seq"`
	SubmissionID     string                  `json:"submission_id"`
	ArchiveStatus    OfflineArchiveStatus    `json:"archive_status"`
	AssessmentStatus OfflineAssessmentStatus `json:"assessment_status,omitempty"`
	EvidenceStatus   OfflineEvidenceStatus   `json:"evidence_status,omitempty"`
	ReasonCodes      []OfflineReasonCode     `json:"reason_codes"`
	Replayed         bool                    `json:"replayed"`
	IngestReceipt    *OfflineIngestReceipt   `json:"ingest_receipt,omitempty"`
	StatusTicket     *OfflineStatusTicket    `json:"status_ticket,omitempty"`
	AssessmentID     string                  `json:"assessment_id,omitempty"`
	EvidenceID       string                  `json:"evidence_id,omitempty"`
}

type OfflineSyncResponse struct {
	SyncRequestID string                  `json:"sync_request_id"`
	Results       []OfflineSyncItemResult `json:"results"`
}

type OfflineOperationStatus struct {
	OperationID      string                  `json:"operation_id"`
	SubmissionID     string                  `json:"submission_id"`
	ArchiveStatus    OfflineArchiveStatus    `json:"archive_status"`
	AssessmentStatus OfflineAssessmentStatus `json:"assessment_status"`
	EvidenceStatus   OfflineEvidenceStatus   `json:"evidence_status"`
	ReasonCodes      []OfflineReasonCode     `json:"reason_codes"`
	IngestReceipt    OfflineIngestReceipt    `json:"ingest_receipt"`
	StatusTicket     OfflineStatusTicket     `json:"status_ticket"`
	AssessmentID     string                  `json:"assessment_id,omitempty"`
	EvidenceID       string                  `json:"evidence_id,omitempty"`
}

type OfflinePurgeTask struct {
	ErasureID         string           `json:"erasure_id"`
	DeviceID          string           `json:"device_id"`
	OldGeneration     int64            `json:"old_generation"`
	CurrentGeneration int64            `json:"current_generation"`
	ChallengeRevision int64            `json:"challenge_revision"`
	Challenge         string           `json:"challenge"`
	IssuedAt          OfflineTimestamp `json:"issued_at"`
	Status            string           `json:"status"`
}

type OfflinePurgeAckRequest struct {
	ChallengeRevision    int64  `json:"challenge_revision"`
	Challenge            string `json:"challenge"`
	Outcome              string `json:"outcome"`
	ManagedObjectsAbsent *bool  `json:"managed_objects_absent,omitempty"`
	FailureCode          string `json:"failure_code,omitempty"`
}

type OfflinePurgeAckResponse struct {
	ErasureID         string           `json:"erasure_id"`
	DeviceID          string           `json:"device_id"`
	SourceGeneration  int64            `json:"source_generation"`
	CurrentGeneration int64            `json:"current_generation"`
	ChallengeRevision int64            `json:"challenge_revision"`
	Status            string           `json:"status"`
	UpdatedAt         OfflineTimestamp `json:"updated_at"`
	StableReason      string           `json:"stable_reason"`
}

type OfflineAssessmentSummary struct {
	AssessmentID        string        `json:"assessment_id"`
	AttemptID           string        `json:"attempt_id"`
	ActivityID          string        `json:"activity_id"`
	ActivityRevision    Uint63Decimal `json:"activity_revision"`
	SubmissionID        string        `json:"submission_id"`
	AggregateVersion    Uint63Decimal `json:"aggregate_version"`
	DispositionVersion  Uint63Decimal `json:"disposition_version"`
	Disposition         string        `json:"disposition"`
	Confidence          int           `json:"confidence"`
	Confirmable         bool          `json:"confirmable"`
	AllowedDecisions    []string      `json:"allowed_decisions"`
	AttemptReceivedAt   time.Time     `json:"attempt_received_at"`
	AssessmentCreatedAt time.Time     `json:"assessment_created_at"`
}

type OfflineAssessmentPage struct {
	Metadata   ProjectionMetadata         `json:"metadata"`
	Items      []OfflineAssessmentSummary `json:"items"`
	NextCursor string                     `json:"next_cursor,omitempty"`
}

type OfflineAssessmentView struct {
	Metadata         ProjectionMetadata `json:"metadata"`
	SubmissionID     string             `json:"submission_id"`
	AggregateVersion Uint63Decimal      `json:"aggregate_version"`
	Activity         Activity           `json:"activity"`
	Attempt          Attempt            `json:"attempt"`
	Assessment       AssessmentArtifact `json:"assessment"`
	Decision         AssessmentDecision `json:"decision"`
	Confirmable      bool               `json:"confirmable"`
	AllowedDecisions []string           `json:"allowed_decisions"`
}

const (
	MaxOfflineAssessmentDecisionReasonRunes = 4000
	MaxOfflineAssessmentRubricItemIDRunes   = 200
	MaxOfflineAssessmentMisconceptionRunes  = 32000
)

type OfflineAssessmentDecisionBase struct {
	OperationID                string        `json:"operation_id"`
	PayloadSchemaVersion       int           `json:"payload_schema_version"`
	AttemptID                  string        `json:"attempt_id"`
	ExpectedVersion            Uint63Decimal `json:"expected_version"`
	Kind                       string        `json:"kind"`
	ExpectedDispositionVersion Uint63Decimal `json:"expected_disposition_version"`
}

type OfflineAssessmentDecisionRequest interface{ offlineAssessmentDecision() }

type OfflineAssessmentConfirmRequest struct {
	OfflineAssessmentDecisionBase
}

func (OfflineAssessmentConfirmRequest) offlineAssessmentDecision() {}

type OfflineAssessmentOverrideItem struct {
	RubricItemID           string `json:"rubric_item_id"`
	Conclusion             string `json:"conclusion"`
	MisconceptionCandidate string `json:"misconception_candidate,omitempty"`
}

type OfflineAssessmentOverrideRequest struct {
	OfflineAssessmentDecisionBase
	Reason string                          `json:"reason"`
	Items  []OfflineAssessmentOverrideItem `json:"items"`
}

func (OfflineAssessmentOverrideRequest) offlineAssessmentDecision() {}

type OfflineAssessmentVoidRequest struct {
	OfflineAssessmentDecisionBase
	Reason string `json:"reason"`
}

func (OfflineAssessmentVoidRequest) offlineAssessmentDecision() {}

type OfflineAssessmentDecisionReceipt struct {
	OperationID                 string             `json:"operation_id"`
	AssessmentID                string             `json:"assessment_id"`
	AttemptID                   string             `json:"attempt_id"`
	SubmissionID                string             `json:"submission_id"`
	Replayed                    bool               `json:"replayed"`
	AggregateVersion            Uint63Decimal      `json:"aggregate_version"`
	FirstEventSequence          Uint63Decimal      `json:"first_event_seq"`
	LastEventSequence           Uint63Decimal      `json:"last_event_seq"`
	ProjectionAsOfEventSequence Uint63Decimal      `json:"projection_as_of_event_seq"`
	Decision                    AssessmentDecision `json:"decision"`
}
