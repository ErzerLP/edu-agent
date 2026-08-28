package learning

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

const MaxUint63 uint64 = 1<<63 - 1

type OfflineSubmissionState string

const (
	OfflineSubmissionReserved         OfflineSubmissionState = "reserved"
	OfflineSubmissionClaimedSucceeded OfflineSubmissionState = "claimed_succeeded"
	OfflineSubmissionClaimedRejected  OfflineSubmissionState = "claimed_rejected"
	OfflineSubmissionExpired          OfflineSubmissionState = "expired"
	OfflineSubmissionRevoked          OfflineSubmissionState = "revoked"
)

func CanTransitionOfflineSubmission(from, to OfflineSubmissionState) bool {
	if from != OfflineSubmissionReserved {
		return false
	}
	switch to {
	case OfflineSubmissionClaimedSucceeded, OfflineSubmissionClaimedRejected, OfflineSubmissionExpired, OfflineSubmissionRevoked:
		return true
	default:
		return false
	}
}

type OfflineLocalState string

const (
	OfflineLocalDraft                   OfflineLocalState = "draft"
	OfflineLocalQueued                  OfflineLocalState = "queued"
	OfflineLocalUploading               OfflineLocalState = "uploading"
	OfflineLocalArchivedPendingEvidence OfflineLocalState = "archived_pending_evidence"
	OfflineLocalTerminal                OfflineLocalState = "terminal"
	OfflineLocalConflict                OfflineLocalState = "conflict"
	OfflineLocalBlocked                 OfflineLocalState = "blocked"
	OfflineLocalDiscarded               OfflineLocalState = "discarded"
)

func CanTransitionOfflineLocal(from, to OfflineLocalState, privacyPurge bool) bool {
	if privacyPurge && to == OfflineLocalDiscarded {
		return true
	}
	switch from {
	case OfflineLocalDraft:
		return to == OfflineLocalQueued || to == OfflineLocalDiscarded
	case OfflineLocalQueued:
		return to == OfflineLocalUploading || to == OfflineLocalDiscarded
	case OfflineLocalUploading:
		return to == OfflineLocalQueued || to == OfflineLocalArchivedPendingEvidence || to == OfflineLocalTerminal || to == OfflineLocalConflict || to == OfflineLocalBlocked
	case OfflineLocalArchivedPendingEvidence:
		return to == OfflineLocalTerminal || to == OfflineLocalBlocked
	case OfflineLocalTerminal, OfflineLocalConflict, OfflineLocalBlocked:
		return to == OfflineLocalDiscarded
	default:
		return false
	}
}

type OfflineWorkerStatus string

const (
	OfflineWorkerQueued       OfflineWorkerStatus = "queued"
	OfflineWorkerProcessing   OfflineWorkerStatus = "processing"
	OfflineWorkerPendingRetry OfflineWorkerStatus = "pending_retry"
	OfflineWorkerCompleted    OfflineWorkerStatus = "completed"
	OfflineWorkerFailed       OfflineWorkerStatus = "failed"
)

func CanTransitionOfflineWorker(from, to OfflineWorkerStatus) bool {
	switch from {
	case OfflineWorkerQueued:
		return to == OfflineWorkerProcessing
	case OfflineWorkerProcessing:
		return to == OfflineWorkerPendingRetry || to == OfflineWorkerCompleted || to == OfflineWorkerFailed
	case OfflineWorkerPendingRetry:
		return to == OfflineWorkerProcessing || to == OfflineWorkerCompleted || to == OfflineWorkerFailed
	default:
		return false
	}
}

type OfflineArchiveStatus string

type OfflineAssessmentStatus string

type OfflineEvidenceStatus string

type OfflineResultKind string

const (
	OfflineArchivedSucceeded   OfflineArchiveStatus = "archived_succeeded"
	OfflineArchivedRejected    OfflineArchiveStatus = "archived_rejected"
	OfflineNotArchivedRetry    OfflineArchiveStatus = "not_archived_retryable"
	OfflineNotArchivedBlocked  OfflineArchiveStatus = "not_archived_blocked"
	OfflineIdempotencyConflict OfflineArchiveStatus = "idempotency_conflict"
	OfflineSequenceConflict    OfflineArchiveStatus = "device_sequence_conflict"
	OfflineNotProcessed        OfflineArchiveStatus = "not_processed"

	OfflineAssessmentNotRequested OfflineAssessmentStatus = "not_requested"
	OfflineAssessmentQueued       OfflineAssessmentStatus = "queued"
	OfflineAssessmentProcessing   OfflineAssessmentStatus = "processing"
	OfflineAssessmentPendingRetry OfflineAssessmentStatus = "pending_retry"
	OfflineAssessmentCompleted    OfflineAssessmentStatus = "completed"
	OfflineAssessmentFailed       OfflineAssessmentStatus = "failed"

	OfflineEvidenceAccepted          OfflineEvidenceStatus = "accepted"
	OfflineEvidenceProvisional       OfflineEvidenceStatus = "provisional"
	OfflineEvidencePendingEvaluation OfflineEvidenceStatus = "pending_evaluation"
	OfflineEvidenceNotEligible       OfflineEvidenceStatus = "not_eligible"
	OfflineEvidenceNotApplicable     OfflineEvidenceStatus = "not_applicable"
	OfflineEvidenceUnchanged         OfflineEvidenceStatus = "unchanged"

	OfflineResultArchived     OfflineResultKind = "archived"
	OfflineResultRetryable    OfflineResultKind = "retryable"
	OfflineResultBlocked      OfflineResultKind = "blocked"
	OfflineResultConflict     OfflineResultKind = "conflict"
	OfflineResultNotProcessed OfflineResultKind = "not_processed"
)

const (
	OfflineReasonDuplicateActivity    = "duplicate_activity_submission"
	OfflineReasonStaleKnowledge       = "stale_knowledge_head"
	OfflineReasonExpiredActivity      = "expired_activity"
	OfflineReasonStaleContext         = "stale_context"
	OfflineReasonStalePolicy          = "stale_policy"
	OfflineReasonAnswerRevealed       = "answer_revealed"
	OfflineReasonModelUnavailable     = "model_unavailable"
	OfflineReasonEvaluationInvalid    = "evaluation_invalid"
	OfflineReasonActivityInvalid      = "offline_activity_invalid"
	OfflineReasonContentRedacted      = "content_redacted"
	OfflineReasonPrivacyClearing      = "privacy_clear_in_progress"
	OfflineReasonDeviceRevoked        = "device_revoked"
	OfflineReasonAuthorizationExpired = "authorization_expired"
	OfflineReasonAuthorizationInvalid = "authorization_invalid"
	OfflineReasonVersionConflict      = "version_conflict"
	OfflineReasonIdempotencyConflict  = "idempotency_conflict"
	OfflineReasonSequenceConflict     = "device_sequence_conflict"
	OfflineReasonNotProcessed         = "not_processed"
	OfflineReasonInternalError        = "internal_error"
)

func ValidateOfflineResultCombination(archive OfflineArchiveStatus, assessment OfflineAssessmentStatus, evidence OfflineEvidenceStatus) error {
	valid := false
	switch archive {
	case OfflineArchivedRejected:
		valid = assessment == OfflineAssessmentNotRequested && evidence == OfflineEvidenceUnchanged
	case OfflineArchivedSucceeded:
		switch assessment {
		case OfflineAssessmentNotRequested:
			valid = evidence == OfflineEvidenceProvisional || evidence == OfflineEvidenceNotEligible || evidence == OfflineEvidenceNotApplicable
		case OfflineAssessmentQueued:
			valid = evidence == OfflineEvidencePendingEvaluation
		case OfflineAssessmentCompleted:
			valid = evidence == OfflineEvidenceAccepted || evidence == OfflineEvidenceProvisional || evidence == OfflineEvidenceNotEligible
		case OfflineAssessmentFailed:
			valid = evidence == OfflineEvidenceUnchanged
		}
	case OfflineNotArchivedRetry, OfflineNotProcessed:
		valid = assessment == "" && evidence == ""
	case OfflineNotArchivedBlocked, OfflineIdempotencyConflict, OfflineSequenceConflict:
		valid = assessment == "" && evidence == ""
	}
	if !valid {
		return &Error{Code: CodeInvalidRequest, Reason: "invalid_offline_result_combination"}
	}
	return nil
}

type OfflineOperationType string

const (
	OfflineAttemptCompleted OfflineOperationType = "offline_attempt_completed"
	OfflineActivitySkipped  OfflineOperationType = "offline_activity_skipped"
)

type OfflineObservation struct {
	Kind       string     `json:"kind"`
	OccurredAt *time.Time `json:"occurred_at"`
}

type OfflineAttemptPayload struct {
	Answer       string               `json:"answer"`
	AnswerSHA256 string               `json:"answer_sha256"`
	Help         HelpLevel            `json:"help"`
	Observations []OfflineObservation `json:"observations"`
}

type OfflineSkipPayload struct {
	Reason string `json:"reason"`
}

type OfflineOperationWireV1 struct {
	OperationID          string               `json:"operation_id"`
	DeviceID             string               `json:"device_id"`
	DeviceSequence       string               `json:"device_seq"`
	SubmissionID         string               `json:"submission_id"`
	PayloadSchemaVersion int                  `json:"payload_schema_version"`
	AggregateType        string               `json:"aggregate_type"`
	AggregateID          string               `json:"aggregate_id"`
	ExpectedVersion      string               `json:"expected_version"`
	OfflineActivityID    string               `json:"offline_activity_id"`
	ActivityRevision     string               `json:"activity_revision"`
	Authorization        json.RawMessage      `json:"authorization"`
	Signature            string               `json:"signature"`
	OccurredAt           *time.Time           `json:"occurred_at"`
	OperationType        OfflineOperationType `json:"operation_type"`
	Payload              json.RawMessage      `json:"payload"`
}

type OfflineOperation struct {
	OperationID          string
	DeviceID             string
	CredentialEpoch      int64
	LearnerGeneration    int64
	DeviceSequence       uint64
	SubmissionID         string
	PayloadSchemaVersion int
	AggregateType        string
	AggregateID          string
	ExpectedVersion      int64
	OfflineActivityID    string
	ActivityRevision     int64
	AuthorizationHash    string
	Authorization        json.RawMessage
	AuthorizationSig     []byte
	OccurredAt           *time.Time
	Type                 OfflineOperationType
	Payload              json.RawMessage
	Attempt              *OfflineAttemptPayload
	Skip                 *OfflineSkipPayload
}

func DecodeOfflineOperationWire(input []byte) (OfflineOperation, error) {
	canonical, err := CanonicalizeJCS(input)
	if err != nil {
		return OfflineOperation{}, &Error{Code: CodeInvalidRequest, Reason: "invalid_offline_operation_json", Cause: err}
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(canonical, &fields); err != nil {
		return OfflineOperation{}, &Error{Code: CodeInvalidRequest, Reason: "invalid_offline_operation_json", Cause: err}
	}
	for _, field := range []string{
		"operation_id", "device_id", "device_seq", "submission_id", "payload_schema_version",
		"aggregate_type", "aggregate_id", "expected_version", "offline_activity_id", "activity_revision",
		"authorization", "signature", "occurred_at", "operation_type", "payload",
	} {
		if _, ok := fields[field]; !ok {
			return OfflineOperation{}, &Error{Code: CodeInvalidRequest, Reason: "missing_offline_operation_field"}
		}
	}
	decoder := json.NewDecoder(bytes.NewReader(canonical))
	decoder.DisallowUnknownFields()
	var wire OfflineOperationWireV1
	if err := decoder.Decode(&wire); err != nil {
		return OfflineOperation{}, &Error{Code: CodeInvalidRequest, Reason: "invalid_offline_operation_json", Cause: err}
	}
	deviceSequence, err := ParseUint63Decimal(wire.DeviceSequence)
	if err != nil || deviceSequence == 0 {
		return OfflineOperation{}, &Error{Code: CodeInvalidRequest, Reason: "invalid_device_sequence"}
	}
	expectedVersion, err := ParseUint63Decimal(wire.ExpectedVersion)
	if err != nil {
		return OfflineOperation{}, &Error{Code: CodeInvalidRequest, Reason: "invalid_expected_version"}
	}
	activityRevision, err := ParseUint63Decimal(wire.ActivityRevision)
	if err != nil {
		return OfflineOperation{}, &Error{Code: CodeInvalidRequest, Reason: "invalid_activity_revision"}
	}
	authorization, err := CanonicalizeJCS(wire.Authorization)
	if err != nil || len(authorization) == 0 || authorization[0] != '{' {
		return OfflineOperation{}, &Error{Code: CodeInvalidRequest, Reason: "invalid_offline_authorization"}
	}
	authorizationHash, err := JCSSHA256(authorization)
	if err != nil {
		return OfflineOperation{}, &Error{Code: CodeInvalidRequest, Reason: "invalid_offline_authorization", Cause: err}
	}
	signature, err := base64.RawURLEncoding.DecodeString(wire.Signature)
	if err != nil || len(signature) != 64 || base64.RawURLEncoding.EncodeToString(signature) != wire.Signature {
		return OfflineOperation{}, &Error{Code: CodeInvalidRequest, Reason: "invalid_offline_authorization_signature"}
	}
	payload, err := CanonicalizeJCS(wire.Payload)
	if err != nil || len(payload) == 0 || payload[0] != '{' {
		return OfflineOperation{}, &Error{Code: CodeInvalidRequest, Reason: "invalid_offline_operation_payload"}
	}
	operation := OfflineOperation{
		OperationID: wire.OperationID, DeviceID: wire.DeviceID, DeviceSequence: deviceSequence,
		SubmissionID: wire.SubmissionID, PayloadSchemaVersion: wire.PayloadSchemaVersion,
		AggregateType: wire.AggregateType, AggregateID: wire.AggregateID,
		ExpectedVersion: int64(expectedVersion), OfflineActivityID: wire.OfflineActivityID,
		ActivityRevision: int64(activityRevision), AuthorizationHash: authorizationHash,
		Authorization: authorization, AuthorizationSig: signature, OccurredAt: wire.OccurredAt,
		Type: wire.OperationType, Payload: payload,
	}
	if err := operation.decodePayload(); err != nil {
		return OfflineOperation{}, err
	}
	if err := operation.Validate(); err != nil {
		return OfflineOperation{}, err
	}
	return operation, nil
}

func (o *OfflineOperation) decodePayload() error {
	decoder := json.NewDecoder(bytes.NewReader(o.Payload))
	decoder.DisallowUnknownFields()
	switch o.Type {
	case OfflineAttemptCompleted:
		var attempt OfflineAttemptPayload
		if err := decoder.Decode(&attempt); err != nil {
			return &Error{Code: CodeInvalidRequest, Reason: "invalid_offline_attempt_payload", Cause: err}
		}
		o.Attempt = &attempt
		o.Skip = nil
	case OfflineActivitySkipped:
		var skip OfflineSkipPayload
		if err := decoder.Decode(&skip); err != nil {
			return &Error{Code: CodeInvalidRequest, Reason: "invalid_offline_skip_payload", Cause: err}
		}
		o.Attempt = nil
		o.Skip = &skip
	default:
		return &Error{Code: CodeInvalidRequest, Reason: "invalid_offline_operation_type"}
	}
	return nil
}

func canonicalUUID(value string) bool {
	parsed, err := uuid.Parse(value)
	return err == nil && parsed.String() == value
}

func (o OfflineOperation) Validate() error {
	if !canonicalUUID(o.OperationID) || !canonicalUUID(o.DeviceID) || !canonicalUUID(o.SubmissionID) ||
		!canonicalUUID(o.AggregateID) || !canonicalUUID(o.OfflineActivityID) ||
		o.DeviceSequence == 0 || o.DeviceSequence > MaxUint63 || o.PayloadSchemaVersion != 1 ||
		o.AggregateType != "offline_attempt" || o.AggregateID != o.SubmissionID || o.ExpectedVersion != 0 ||
		o.ActivityRevision != 1 || len(o.AuthorizationHash) != 64 {
		return &Error{Code: CodeInvalidRequest, Reason: "invalid_offline_operation_envelope"}
	}
	authorizationHash, err := hex.DecodeString(o.AuthorizationHash)
	if err != nil || len(authorizationHash) != 32 || o.AuthorizationHash != strings.ToLower(o.AuthorizationHash) {
		return &Error{Code: CodeInvalidRequest, Reason: "invalid_authorization_hash"}
	}
	if len(o.Authorization) > 0 {
		canonical, err := CanonicalizeJCS(o.Authorization)
		if err != nil || len(canonical) == 0 || canonical[0] != '{' {
			return &Error{Code: CodeInvalidRequest, Reason: "invalid_offline_authorization"}
		}
		actualHash, _ := JCSSHA256(canonical)
		if subtle.ConstantTimeCompare([]byte(actualHash), []byte(o.AuthorizationHash)) != 1 {
			return &Error{Code: CodeInvalidRequest, Reason: "authorization_hash_mismatch"}
		}
	}
	if o.OccurredAt != nil {
		_, offset := o.OccurredAt.Zone()
		if offset != 0 {
			return &Error{Code: CodeInvalidRequest, Reason: "occurred_at_not_utc"}
		}
	}
	if len(o.Payload) > 0 && o.Attempt == nil && o.Skip == nil {
		copy := o
		if err := copy.decodePayload(); err != nil {
			return err
		}
		o = copy
	}
	switch o.Type {
	case OfflineAttemptCompleted:
		if o.Attempt == nil || o.Skip != nil || !validHelp(o.Attempt.Help) || len(o.Attempt.Answer) == 0 ||
			len(o.Attempt.Answer) > 262144 || !utf8.ValidString(o.Attempt.Answer) || len(o.Attempt.Observations) > 64 {
			return &Error{Code: CodeInvalidRequest, Reason: "invalid_offline_attempt_payload"}
		}
		answerHash, err := hex.DecodeString(o.Attempt.AnswerSHA256)
		actualHash, actualErr := hex.DecodeString(SHA256([]byte(o.Attempt.Answer)))
		if err != nil || actualErr != nil || len(answerHash) != 32 || subtle.ConstantTimeCompare(answerHash, actualHash) != 1 ||
			o.Attempt.AnswerSHA256 != strings.ToLower(o.Attempt.AnswerSHA256) {
			return &Error{Code: CodeInvalidRequest, Reason: "invalid_offline_attempt_payload"}
		}
		for _, observation := range o.Attempt.Observations {
			if observation.Kind != "activity_presented" && observation.Kind != "answer_recorded" {
				return &Error{Code: CodeInvalidRequest, Reason: "invalid_offline_observation"}
			}
			if observation.OccurredAt != nil {
				_, offset := observation.OccurredAt.Zone()
				if offset != 0 {
					return &Error{Code: CodeInvalidRequest, Reason: "invalid_offline_observation"}
				}
			}
		}
	case OfflineActivitySkipped:
		if o.Attempt != nil || o.Skip == nil || (o.Skip.Reason != "user_skipped" && o.Skip.Reason != "expired_locally" && o.Skip.Reason != "unreadable_local_item") {
			return &Error{Code: CodeInvalidRequest, Reason: "invalid_offline_skip_payload"}
		}
	default:
		return &Error{Code: CodeInvalidRequest, Reason: "invalid_offline_operation_type"}
	}
	return nil
}

func (o OfflineOperation) canonicalPayload() (json.RawMessage, error) {
	if len(o.Payload) > 0 {
		payload, err := CanonicalizeJCS(o.Payload)
		if err != nil || len(payload) == 0 || payload[0] != '{' {
			return nil, &Error{Code: CodeInvalidRequest, Reason: "invalid_offline_operation_payload", Cause: err}
		}
		return payload, nil
	}
	var value any
	switch o.Type {
	case OfflineAttemptCompleted:
		value = o.Attempt
	case OfflineActivitySkipped:
		value = o.Skip
	default:
		return nil, &Error{Code: CodeInvalidRequest, Reason: "invalid_offline_operation_type"}
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return CanonicalizeJCS(encoded)
}

func (o OfflineOperation) CanonicalHash() (string, error) {
	if err := o.Validate(); err != nil {
		return "", err
	}
	payload, err := o.canonicalPayload()
	if err != nil {
		return "", err
	}
	deviceSequence, _ := FormatUint63Decimal(o.DeviceSequence)
	expectedVersion, _ := FormatUint63Decimal(uint64(o.ExpectedVersion))
	activityRevision, _ := FormatUint63Decimal(uint64(o.ActivityRevision))
	authorizationHash, _ := hex.DecodeString(o.AuthorizationHash)
	hashPayload := struct {
		ProtocolVersion      int                  `json:"protocol_version"`
		OperationID          string               `json:"operation_id"`
		DeviceID             string               `json:"device_id"`
		DeviceSequence       string               `json:"device_seq"`
		SubmissionID         string               `json:"submission_id"`
		PayloadSchemaVersion int                  `json:"payload_schema_version"`
		AggregateType        string               `json:"aggregate_type"`
		AggregateID          string               `json:"aggregate_id"`
		ExpectedVersion      string               `json:"expected_version"`
		OfflineActivityID    string               `json:"offline_activity_id"`
		ActivityRevision     string               `json:"activity_revision"`
		AuthorizationHash    string               `json:"authorization_sha256"`
		OccurredAt           *time.Time           `json:"occurred_at"`
		OperationType        OfflineOperationType `json:"operation_type"`
		Payload              json.RawMessage      `json:"payload"`
	}{
		ProtocolVersion: 1, OperationID: o.OperationID, DeviceID: o.DeviceID,
		DeviceSequence: deviceSequence, SubmissionID: o.SubmissionID,
		PayloadSchemaVersion: o.PayloadSchemaVersion, AggregateType: o.AggregateType,
		AggregateID: o.AggregateID, ExpectedVersion: expectedVersion,
		OfflineActivityID: o.OfflineActivityID, ActivityRevision: activityRevision,
		AuthorizationHash: base64.RawURLEncoding.EncodeToString(authorizationHash), OccurredAt: o.OccurredAt,
		OperationType: o.Type, Payload: payload,
	}
	encoded, err := json.Marshal(hashPayload)
	if err != nil {
		return "", err
	}
	return JCSSHA256(encoded)
}

type OfflineActivity struct {
	Activity
	LearnerGeneration int64     `json:"learner_generation"`
	PracticeKind      string    `json:"practice_kind"`
	PayloadSHA256     string    `json:"payload_sha256"`
	IssuedAt          time.Time `json:"issued_at"`
	EligibleUntil     time.Time `json:"eligible_until"`
	ArchiveUntil      time.Time `json:"archive_until"`
}

type OfflineWinnerDecision struct {
	Winner              bool
	EvidenceEligibility bool
	Reason              string
}

func DecideOfflineWinner(existingWinningAttemptID, contenderAttemptID string, otherwiseEligible bool, ineligibleReason string) (OfflineWinnerDecision, error) {
	if uuid.Validate(contenderAttemptID) != nil {
		return OfflineWinnerDecision{}, &Error{Code: CodeInvalidRequest, Reason: "invalid_contender_attempt"}
	}
	if existingWinningAttemptID != "" && uuid.Validate(existingWinningAttemptID) != nil {
		return OfflineWinnerDecision{}, &Error{Code: CodeInvalidRequest, Reason: "invalid_existing_winner"}
	}
	if !otherwiseEligible {
		if ineligibleReason == "" {
			return OfflineWinnerDecision{}, &Error{Code: CodeInvalidRequest, Reason: "missing_ineligibility_reason"}
		}
		return OfflineWinnerDecision{Reason: ineligibleReason}, nil
	}
	if existingWinningAttemptID == "" || existingWinningAttemptID == contenderAttemptID {
		return OfflineWinnerDecision{Winner: true, EvidenceEligibility: true}, nil
	}
	return OfflineWinnerDecision{Reason: OfflineReasonDuplicateActivity}, nil
}

type OfflineIngestRequest struct {
	Operation OfflineOperation
}

type OfflineIngestReceipt struct {
	ReceiptID          string               `json:"receipt_id"`
	ArchivedAt         time.Time            `json:"archived_at"`
	AggregateVersion   string               `json:"aggregate_version"`
	FirstEventSequence string               `json:"first_event_seq"`
	LastEventSequence  string               `json:"last_event_seq"`
	ProjectionAsOf     string               `json:"projection_as_of_event_seq"`
	ArchiveStatus      OfflineArchiveStatus `json:"archive_status"`
}

type OfflineStatusTicket struct {
	TicketID    string    `json:"ticket_id"`
	OperationID string    `json:"operation_id"`
	Revision    string    `json:"revision"`
	UpdatedAt   time.Time `json:"updated_at"`
}

type OfflineIngestResult struct {
	ResultKind       OfflineResultKind       `json:"result_kind"`
	OperationID      string                  `json:"operation_id"`
	DeviceSequence   string                  `json:"device_seq"`
	SubmissionID     string                  `json:"submission_id"`
	ArchiveStatus    OfflineArchiveStatus    `json:"archive_status"`
	AssessmentStatus OfflineAssessmentStatus `json:"assessment_status,omitempty"`
	EvidenceStatus   OfflineEvidenceStatus   `json:"evidence_status,omitempty"`
	ReasonCodes      []string                `json:"reason_codes"`
	Replayed         bool                    `json:"replayed"`
	Receipt          *OfflineIngestReceipt   `json:"ingest_receipt,omitempty"`
	StatusTicket     *OfflineStatusTicket    `json:"status_ticket,omitempty"`
	AssessmentID     string                  `json:"assessment_id,omitempty"`
	EvidenceID       string                  `json:"evidence_id,omitempty"`
}

func (r OfflineIngestResult) Validate() error {
	deviceSequence, deviceSequenceErr := ParseUint63Decimal(r.DeviceSequence)
	if uuid.Validate(r.OperationID) != nil || uuid.Validate(r.SubmissionID) != nil || deviceSequenceErr != nil || deviceSequence == 0 {
		return errors.New("offline ingest result identity is invalid")
	}
	if r.ResultKind == OfflineResultArchived {
		if r.Receipt == nil {
			return errors.New("archived offline ingest result requires a receipt")
		}
		aggregateVersion, aggregateErr := ParseUint63Decimal(r.Receipt.AggregateVersion)
		firstSequence, firstErr := ParseUint63Decimal(r.Receipt.FirstEventSequence)
		lastSequence, lastErr := ParseUint63Decimal(r.Receipt.LastEventSequence)
		projectionAsOf, projectionErr := ParseUint63Decimal(r.Receipt.ProjectionAsOf)
		if aggregateErr != nil || firstErr != nil || lastErr != nil || projectionErr != nil ||
			aggregateVersion < 1 || firstSequence < 1 || lastSequence < firstSequence || projectionAsOf < lastSequence {
			return errors.New("archived offline ingest result requires a receipt")
		}
		if r.StatusTicket != nil {
			revision, revisionErr := ParseUint63Decimal(r.StatusTicket.Revision)
			if revisionErr != nil || revision < 1 {
				return errors.New("offline status ticket revision is invalid")
			}
		}
		return ValidateOfflineResultCombination(r.ArchiveStatus, r.AssessmentStatus, r.EvidenceStatus)
	}
	if r.Receipt != nil || r.StatusTicket != nil || r.AssessmentID != "" || r.EvidenceID != "" {
		return fmt.Errorf("non-archived offline ingest result carries an archive receipt")
	}
	return nil
}

type OfflineIngestStore interface {
	IngestOffline(context.Context, OfflineIngestRequest) (OfflineIngestResult, error)
}
