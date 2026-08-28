package learning

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

const (
	OfflineAssessmentFilterProvisional      = "provisional"
	MaxOfflineAssessmentDecisionReasonRunes = 4000
	MaxOfflineAssessmentRubricItemIDRunes   = 200
	MaxOfflineAssessmentMisconceptionRunes  = MaxProposalTextRunes
)

type OfflineAssessmentQuery struct {
	Status string
	Page   CursorPageRequest
}

type OfflineAssessmentSummary struct {
	AssessmentID        string      `json:"assessment_id"`
	AttemptID           string      `json:"attempt_id"`
	ActivityID          string      `json:"activity_id"`
	ActivityRevision    string      `json:"activity_revision"`
	SubmissionID        string      `json:"submission_id"`
	AggregateVersion    string      `json:"aggregate_version"`
	DispositionVersion  string      `json:"disposition_version"`
	Disposition         Disposition `json:"disposition"`
	Confidence          int         `json:"confidence"`
	Confirmable         bool        `json:"confirmable"`
	AllowedDecisions    []string    `json:"allowed_decisions"`
	AttemptReceivedAt   time.Time   `json:"attempt_received_at"`
	AssessmentCreatedAt time.Time   `json:"assessment_created_at"`
}

type OfflineAssessmentPage struct {
	Metadata   ProjectionMetadata         `json:"metadata"`
	Items      []OfflineAssessmentSummary `json:"items"`
	NextCursor string                     `json:"next_cursor,omitempty"`
}

type OfflineAssessmentView struct {
	Metadata         ProjectionMetadata `json:"metadata"`
	SubmissionID     string             `json:"submission_id"`
	AggregateVersion string             `json:"aggregate_version"`
	Activity         Activity           `json:"activity"`
	Attempt          Attempt            `json:"attempt"`
	Assessment       AssessmentArtifact `json:"assessment"`
	Decision         AssessmentDecision `json:"decision"`
	Confirmable      bool               `json:"confirmable"`
	AllowedDecisions []string           `json:"allowed_decisions"`
}

type OfflineAssessmentOverrideItem struct {
	RubricItemID           string     `json:"rubric_item_id"`
	Conclusion             Conclusion `json:"conclusion"`
	MisconceptionCandidate string     `json:"misconception_candidate,omitempty"`
}

type OfflineAssessmentDecisionCommand struct {
	OperationID                string                          `json:"operation_id"`
	PayloadSchemaVersion       int                             `json:"payload_schema_version"`
	AttemptID                  string                          `json:"attempt_id"`
	ExpectedVersion            int64                           `json:"expected_version"`
	Kind                       string                          `json:"kind"`
	ExpectedDispositionVersion int64                           `json:"expected_disposition_version"`
	Reason                     string                          `json:"reason,omitempty"`
	Items                      []OfflineAssessmentOverrideItem `json:"items,omitempty"`
}

type OfflineAssessmentDecisionReceipt struct {
	OperationID                 string             `json:"operation_id"`
	AssessmentID                string             `json:"assessment_id"`
	AttemptID                   string             `json:"attempt_id"`
	SubmissionID                string             `json:"submission_id"`
	Replayed                    bool               `json:"replayed"`
	AggregateVersion            string             `json:"aggregate_version"`
	FirstEventSequence          string             `json:"first_event_seq"`
	LastEventSequence           string             `json:"last_event_seq"`
	ProjectionAsOfEventSequence string             `json:"projection_as_of_event_seq"`
	Decision                    AssessmentDecision `json:"decision"`
}

type OfflineStatusProjectionUpdate struct {
	SubmissionID string
	Assessment   OfflineAssessmentStatus
	Evidence     OfflineEvidenceStatus
	ReasonCodes  []string
	AssessmentID string
	EvidenceID   string
}

type OfflineAssessmentStore interface {
	OperationArchive
	CommandStore
	ListOfflineAssessments(context.Context, string, OfflineAssessmentQuery) (OfflineAssessmentPage, error)
	OfflineAssessment(context.Context, string, string) (OfflineAssessmentView, error)
}

type OfflineAssessmentService interface {
	ListOfflineAssessments(context.Context, string, OfflineAssessmentQuery) (OfflineAssessmentPage, error)
	OfflineAssessment(context.Context, string, string) (OfflineAssessmentView, error)
	DecideOfflineAssessment(context.Context, string, string, OfflineAssessmentDecisionCommand) (OfflineAssessmentDecisionReceipt, error)
}

func (s *Service) ListOfflineAssessments(ctx context.Context, deviceID string, query OfflineAssessmentQuery) (OfflineAssessmentPage, error) {
	store, err := s.offlineAssessmentStore()
	if err != nil {
		return OfflineAssessmentPage{}, err
	}
	if !canonicalOfflineAssessmentUUID(deviceID) || query.Status != OfflineAssessmentFilterProvisional || query.Page.Limit < 1 || query.Page.Limit > 200 {
		return OfflineAssessmentPage{}, &Error{Code: CodeInvalidRequest, Reason: "invalid_offline_assessment_query"}
	}
	return store.ListOfflineAssessments(ctx, deviceID, query)
}

func (s *Service) OfflineAssessment(ctx context.Context, deviceID, assessmentID string) (OfflineAssessmentView, error) {
	store, err := s.offlineAssessmentStore()
	if err != nil {
		return OfflineAssessmentView{}, err
	}
	if !canonicalOfflineAssessmentUUID(deviceID) || !canonicalOfflineAssessmentUUID(assessmentID) {
		return OfflineAssessmentView{}, &Error{Code: CodeInvalidRequest, Reason: "invalid_offline_assessment_id"}
	}
	return store.OfflineAssessment(ctx, deviceID, assessmentID)
}

func (s *Service) DecideOfflineAssessment(ctx context.Context, deviceID, assessmentID string, command OfflineAssessmentDecisionCommand) (OfflineAssessmentDecisionReceipt, error) {
	store, err := s.offlineAssessmentStore()
	if err != nil {
		return OfflineAssessmentDecisionReceipt{}, err
	}
	if err := validateOfflineAssessmentDecisionCommand(deviceID, assessmentID, command); err != nil {
		return OfflineAssessmentDecisionReceipt{}, err
	}

	// The ownership/generation read precedes replay lookup so an old-generation
	// receipt cannot bypass the current offline content barrier.
	view, err := store.OfflineAssessment(ctx, deviceID, assessmentID)
	if err != nil {
		return OfflineAssessmentDecisionReceipt{}, err
	}
	requestHash, err := HashJSON(struct {
		AssessmentID string                           `json:"assessment_id"`
		Command      OfflineAssessmentDecisionCommand `json:"command"`
	}{AssessmentID: assessmentID, Command: command})
	if err != nil {
		return OfflineAssessmentDecisionReceipt{}, err
	}
	lookup := OperationLookup{DeviceID: deviceID, OperationID: command.OperationID, RequestHash: requestHash}
	if replay, replayErr, found := store.LookupOperation(ctx, lookup); found || replayErr != nil {
		if replayErr != nil {
			return OfflineAssessmentDecisionReceipt{}, replayErr
		}
		return offlineAssessmentReceipt(replay, assessmentID, view.Attempt.ID, view.SubmissionID)
	}

	aggregateVersion, err := parseOfflineAssessmentVersion(view.AggregateVersion)
	if err != nil {
		return OfflineAssessmentDecisionReceipt{}, err
	}
	if command.AttemptID != view.Attempt.ID || command.ExpectedVersion != aggregateVersion {
		return OfflineAssessmentDecisionReceipt{}, &Error{
			Code: CodeVersionConflict, AggregateType: "offline_attempt", AggregateID: view.SubmissionID,
			ExpectedVersion: command.ExpectedVersion, CurrentVersion: aggregateVersion,
			AsOfEventSequence: view.Metadata.AsOfEventSequence,
		}
	}
	if view.Decision.Disposition != DispositionProvisional || !view.Assessment.EvidenceEligibility || !view.Attempt.EvidenceEligibility {
		return OfflineAssessmentDecisionReceipt{}, &Error{Code: CodeAssessmentDispositionConflict, CurrentDisposition: string(view.Decision.Disposition), Reason: view.Attempt.EvidenceIneligibleReason}
	}

	items, err := offlineAssessmentDecisionItems(view.Assessment, command)
	if err != nil {
		return OfflineAssessmentDecisionReceipt{}, err
	}
	effect, err := DecideAssessment(view.Decision, view.Assessment, DecisionCommand{
		Kind: command.Kind, ExpectedVersion: command.ExpectedDispositionVersion,
		Reason: command.Reason, Items: items,
	}, view.Confirmable)
	if err != nil {
		return OfflineAssessmentDecisionReceipt{}, err
	}

	now := s.now().UTC().Truncate(time.Microsecond)
	decision := AssessmentDecision{
		ID: command.OperationID, AssessmentID: assessmentID, Version: view.Decision.Version + 1,
		Disposition: effect.Disposition, Items: effect.Items, Reason: strings.TrimSpace(command.Reason),
		ActorDeviceID: deviceID, CreatedAt: now, ReplacesDecisionID: &view.Decision.ID,
	}
	batch := CommandBatch{Decisions: []AssessmentDecision{decision}, Disposition: decision.Disposition}
	statusUpdate := &OfflineStatusProjectionUpdate{
		SubmissionID: view.SubmissionID, Assessment: OfflineAssessmentCompleted,
		Evidence: OfflineEvidenceProvisional, AssessmentID: assessmentID,
	}
	eventType := EventAssessmentVoided
	if effect.CreateEvidence {
		outcome, outcomeErr := ValidateAssessmentReplacement(view.Activity, view.Attempt, view.Assessment, effect.Items)
		if outcomeErr != nil {
			return OfflineAssessmentDecisionReceipt{}, outcomeErr
		}
		evidence := s.makeEvidence(view.Activity, view.Attempt, view.Assessment, decision, effect.Items, outcome, now)
		decision.ProducedEvidenceID = &evidence.ID
		batch.Decisions[0] = decision
		batch.Evidence = []AcceptedEvidence{evidence}
		owner, ownerErr := evidenceAuthority(view.Activity, evidence)
		if ownerErr != nil {
			return OfflineAssessmentDecisionReceipt{}, ownerErr
		}
		batch.Authority.Evidence = map[string]EvidenceOwner{evidence.ID: owner}
		statusUpdate.Evidence = OfflineEvidenceAccepted
		statusUpdate.EvidenceID = evidence.ID
		eventType = EventAssessmentAccepted
		if decision.Disposition == DispositionOverridden {
			eventType = EventAssessmentOverridden
		}
	}
	projection := AssessmentProjectionEvent{
		AssessmentID: assessmentID, NodeRevisionID: view.Activity.TargetNodeRevisionID, Decision: decision,
	}
	batch.Events = []EventDraft{offlineAssessmentDecisionDraft(eventType, view, projection, string(statusUpdate.Evidence))}
	if len(batch.Evidence) == 1 {
		batch.Events = append(batch.Events, offlineAssessmentDecisionDraft(EventEvidenceAccepted, view, batch.Evidence[0], string(OfflineEvidenceAccepted)))
	}
	batch.TypedResult = mustJSON(decision)
	batch.OfflineStatusUpdate = statusUpdate
	payload, err := json.Marshal(command)
	if err != nil {
		return OfflineAssessmentDecisionReceipt{}, fmt.Errorf("encode offline assessment decision: %w", err)
	}
	result, err := store.Commit(ctx, CommitRequest{
		DeviceID: deviceID,
		Operation: OperationEnvelope{
			OperationID: command.OperationID, PayloadSchemaVersion: 1, AggregateType: "offline_attempt",
			AggregateID: view.SubmissionID, ExpectedVersion: command.ExpectedVersion, Payload: payload,
		},
		RequestHash:  requestHash,
		Expectations: []AggregateExpectation{{Type: "offline_attempt", ID: view.SubmissionID, ExpectedVersion: command.ExpectedVersion}},
		Batch:        batch, ReceivedAt: now,
	})
	if err != nil {
		if ErrorCode(err) == "" && strings.Contains(err.Error(), "privacy") {
			return OfflineAssessmentDecisionReceipt{}, &Error{Code: CodeContentRedacted, Reason: "offline_assessment_generation_closed", Cause: err}
		}
		return OfflineAssessmentDecisionReceipt{}, err
	}
	return offlineAssessmentReceipt(result, assessmentID, view.Attempt.ID, view.SubmissionID)
}

func (s *Service) offlineAssessmentStore() (OfflineAssessmentStore, error) {
	store, ok := s.authority.(OfflineAssessmentStore)
	if !ok {
		return nil, errors.New("offline assessment store is unavailable")
	}
	return store, nil
}

func validateOfflineAssessmentDecisionCommand(deviceID, assessmentID string, command OfflineAssessmentDecisionCommand) error {
	if !canonicalOfflineAssessmentUUID(deviceID) || !canonicalOfflineAssessmentUUID(assessmentID) ||
		!canonicalOfflineAssessmentUUID(command.OperationID) || !canonicalOfflineAssessmentUUID(command.AttemptID) ||
		command.PayloadSchemaVersion != 1 || command.ExpectedVersion < 1 || command.ExpectedDispositionVersion < 1 {
		return &Error{Code: CodeInvalidRequest, Reason: "invalid_offline_assessment_decision"}
	}
	if !utf8.ValidString(command.Reason) || utf8.RuneCountInString(command.Reason) > MaxOfflineAssessmentDecisionReasonRunes || strings.TrimSpace(command.Reason) != command.Reason {
		return &Error{Code: CodeInvalidRequest, Reason: "invalid_offline_assessment_reason"}
	}
	switch command.Kind {
	case "confirm":
		if command.Reason != "" || len(command.Items) != 0 {
			return &Error{Code: CodeInvalidRequest, Reason: "invalid_offline_assessment_confirm"}
		}
	case "override":
		if command.Reason == "" || len(command.Items) == 0 || len(command.Items) > MaxRubricItems {
			return &Error{Code: CodeInvalidRequest, Reason: "invalid_offline_assessment_override"}
		}
	case "void":
		if command.Reason == "" || len(command.Items) != 0 {
			return &Error{Code: CodeInvalidRequest, Reason: "invalid_offline_assessment_void"}
		}
	default:
		return &Error{Code: CodeInvalidRequest, Reason: "invalid_offline_assessment_decision_kind"}
	}
	return nil
}

func offlineAssessmentDecisionItems(artifact AssessmentArtifact, command OfflineAssessmentDecisionCommand) ([]AssessmentItem, error) {
	if command.Kind != "override" {
		return nil, nil
	}
	if len(command.Items) != len(artifact.Items) {
		return nil, &Error{Code: CodeAssessmentDispositionConflict, Reason: "offline_override_rubric_incomplete"}
	}
	byRubric := make(map[string]OfflineAssessmentOverrideItem, len(command.Items))
	for _, item := range command.Items {
		if !utf8.ValidString(item.RubricItemID) || strings.TrimSpace(item.RubricItemID) == "" || utf8.RuneCountInString(item.RubricItemID) > MaxOfflineAssessmentRubricItemIDRunes || byRubric[item.RubricItemID].RubricItemID != "" ||
			(item.Conclusion != ConclusionPass && item.Conclusion != ConclusionPartial && item.Conclusion != ConclusionFail) ||
			!utf8.ValidString(item.MisconceptionCandidate) || utf8.RuneCountInString(item.MisconceptionCandidate) > MaxOfflineAssessmentMisconceptionRunes {
			return nil, &Error{Code: CodeInvalidRequest, Reason: "invalid_offline_override_item"}
		}
		byRubric[item.RubricItemID] = item
	}
	result := make([]AssessmentItem, len(artifact.Items))
	for index, source := range artifact.Items {
		replacement, ok := byRubric[source.RubricItemID]
		if !ok {
			return nil, &Error{Code: CodeAssessmentDispositionConflict, Reason: "offline_override_rubric_mismatch"}
		}
		result[index] = source
		result[index].Conclusion = replacement.Conclusion
		result[index].MisconceptionCandidate = strings.TrimSpace(replacement.MisconceptionCandidate)
	}
	return result, nil
}

func offlineAssessmentDecisionDraft(eventType EventType, view OfflineAssessmentView, payload any, evidenceDisposition string) EventDraft {
	return EventDraft{
		Type: eventType, AggregateType: "offline_attempt", AggregateID: view.SubmissionID,
		Payload: mustJSON(payload), ParentSessionID: view.Activity.SessionID, Source: "offline",
		ArchiveDisposition: "succeeded", EvidenceDisposition: evidenceDisposition,
		GoalRevisionID: view.Activity.GoalRevisionID, RouteRevisionID: view.Activity.RouteRevisionID,
		KnowledgeRevisionID: view.Activity.KnowledgeRevisionID, ActivityID: view.Activity.ID,
		ActivityRevision: view.Activity.Revision,
	}
}

func offlineAssessmentReceipt(result OperationResult, assessmentID, attemptID, submissionID string) (OfflineAssessmentDecisionReceipt, error) {
	var decision AssessmentDecision
	if err := json.Unmarshal(result.Result, &decision); err != nil || decision.ID == "" || decision.AssessmentID != assessmentID {
		return OfflineAssessmentDecisionReceipt{}, &Error{Code: CodeProjectionUnavailable, Reason: "offline_assessment_receipt_invalid", Cause: err}
	}
	aggregateVersion, err := FormatUint63Decimal(uint64(result.AggregateVersion))
	if err != nil {
		return OfflineAssessmentDecisionReceipt{}, err
	}
	first, err := FormatUint63Decimal(uint64(result.FirstEventSequence))
	if err != nil {
		return OfflineAssessmentDecisionReceipt{}, err
	}
	last, err := FormatUint63Decimal(uint64(result.LastEventSequence))
	if err != nil {
		return OfflineAssessmentDecisionReceipt{}, err
	}
	projection, err := FormatUint63Decimal(uint64(result.ProjectionAsOf))
	if err != nil {
		return OfflineAssessmentDecisionReceipt{}, err
	}
	return OfflineAssessmentDecisionReceipt{
		OperationID: decision.ID, AssessmentID: assessmentID, AttemptID: attemptID, SubmissionID: submissionID,
		Replayed: result.Replayed, AggregateVersion: aggregateVersion, FirstEventSequence: first,
		LastEventSequence: last, ProjectionAsOfEventSequence: projection, Decision: decision,
	}, nil
}

func parseOfflineAssessmentVersion(value string) (int64, error) {
	parsed, err := ParseUint63Decimal(value)
	if err != nil || parsed > uint64(^uint64(0)>>1) {
		return 0, &Error{Code: CodeProjectionUnavailable, Reason: "offline_assessment_version_invalid", Cause: err}
	}
	return int64(parsed), nil
}

func canonicalOfflineAssessmentUUID(value string) bool {
	parsed, err := uuid.Parse(value)
	return err == nil && parsed.String() == value
}
