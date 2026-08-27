package learning

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/edu-agent/edu-agent/server/internal/platform/outbox"
)

// OfflineEvaluationTask is the immutable outbox contract written by offline ingest.
type OfflineEvaluationTask struct {
	JobID              string `json:"job_id"`
	SubmissionID       string `json:"submission_id"`
	FutureAssessmentID string `json:"future_assessment_id"`
	LearnerGeneration  int64  `json:"learner_generation"`
}

// OfflineEvaluationSnapshot contains the frozen authority required by one worker attempt.
type OfflineEvaluationSnapshot struct {
	Task              OfflineEvaluationTask
	Attempt           Attempt
	Activity          Activity
	AggregateVersion  int64
	AttemptCount      int
	RetryDeadline     time.Time
	RetryExpired      bool
	LastErrorCategory string
	Now               time.Time
	LeaseToken        string
	ModelArtifact     *AssessmentArtifact
}

// OfflineEvaluationCompletion is the immutable model result and domain disposition to commit.
type OfflineEvaluationCompletion struct {
	Artifact AssessmentArtifact
	Decision AssessmentDecision
	Evidence *AcceptedEvidence
	Reasons  []string
	Invalid  bool
}

// OfflineEvaluationStore owns the durable worker lifecycle and status projection.
type OfflineEvaluationStore interface {
	OfflineEvaluationCanApply(context.Context, outbox.Message) (outbox.ApplyDecision, error)
	BeginOfflineEvaluation(context.Context, outbox.Message) (OfflineEvaluationSnapshot, error)
	SaveOfflineEvaluationArtifact(context.Context, OfflineEvaluationSnapshot, AssessmentArtifact) error
	MarkOfflineEvaluationRetry(context.Context, OfflineEvaluationSnapshot, string) error
	MarkOfflineEvaluationFailed(context.Context, OfflineEvaluationSnapshot, string) error
	CompleteOfflineEvaluation(context.Context, OfflineEvaluationSnapshot, OfflineEvaluationCompletion, OperationResult) error
}

// OfflineEvaluationConsumer evaluates Open Activity attempts from frozen offline inputs.
type OfflineEvaluationConsumer struct {
	service *Service
	store   OfflineEvaluationStore
}

func NewOfflineEvaluationConsumer(service *Service, store OfflineEvaluationStore) (*OfflineEvaluationConsumer, error) {
	if service == nil || service.authority == nil || store == nil {
		return nil, fmt.Errorf("offline evaluation dependencies are required")
	}
	return &OfflineEvaluationConsumer{service: service, store: store}, nil
}

func (c *OfflineEvaluationConsumer) CanApply(ctx context.Context, message outbox.Message) (outbox.ApplyDecision, error) {
	return c.store.OfflineEvaluationCanApply(ctx, message)
}

func (c *OfflineEvaluationConsumer) Apply(ctx context.Context, message outbox.Message) error {
	snapshot, err := c.store.BeginOfflineEvaluation(ctx, message)
	if err != nil {
		return err
	}
	artifact := snapshot.ModelArtifact
	invalid := false
	invalidReasons := []string(nil)
	if artifact != nil {
		for _, category := range artifact.AttemptCategories {
			if category == "retry_deadline_exceeded" {
				invalid, invalidReasons = true, []string{offlineEvaluationFallbackReason(artifact.AttemptCategories)}
				break
			}
		}
		if !invalid {
			for _, risk := range artifact.RiskFlags {
				if risk == RiskRetryExhausted {
					invalid, invalidReasons = true, []string{offlineEvaluationFallbackReason(artifact.AttemptCategories)}
					break
				}
			}
		}
	}
	if artifact == nil {
		generated, reasons, generationErr := c.generate(ctx, snapshot)
		if generationErr != nil {
			category, permanent := classifyOfflineEvaluationError(generationErr)
			if permanent {
				if markErr := c.store.MarkOfflineEvaluationFailed(ctx, snapshot, category); markErr != nil {
					return markErr
				}
			} else if markErr := c.store.MarkOfflineEvaluationRetry(ctx, snapshot, category); markErr != nil {
				return markErr
			}
			return offlineEvaluationError{category: category, permanent: permanent, cause: generationErr}
		}
		artifact = &generated
		invalid = len(reasons) > 0
		invalidReasons = reasons
		if err := c.store.SaveOfflineEvaluationArtifact(ctx, snapshot, generated); err != nil {
			return err
		}
	}
	completion, err := c.completion(snapshot, *artifact, invalid, invalidReasons)
	if err != nil {
		category, permanent := classifyOfflineEvaluationError(err)
		if permanent {
			if markErr := c.store.MarkOfflineEvaluationFailed(ctx, snapshot, category); markErr != nil {
				return markErr
			}
		} else if markErr := c.store.MarkOfflineEvaluationRetry(ctx, snapshot, category); markErr != nil {
			return markErr
		}
		return offlineEvaluationError{category: category, permanent: permanent, cause: err}
	}
	request, err := c.commitRequest(snapshot, completion)
	if err != nil {
		return err
	}
	result, err := c.service.authority.Commit(ctx, request)
	if err != nil {
		category, permanent := classifyOfflineEvaluationError(err)
		if permanent {
			if markErr := c.store.MarkOfflineEvaluationFailed(ctx, snapshot, category); markErr != nil {
				return markErr
			}
		} else if markErr := c.store.MarkOfflineEvaluationRetry(ctx, snapshot, category); markErr != nil {
			return markErr
		}
		return offlineEvaluationError{category: category, permanent: permanent, cause: err}
	}
	return c.store.CompleteOfflineEvaluation(ctx, snapshot, completion, result)
}

func (c *OfflineEvaluationConsumer) generate(ctx context.Context, snapshot OfflineEvaluationSnapshot) (AssessmentArtifact, []string, error) {
	input := struct {
		Activity Activity `json:"activity"`
		Attempt  Attempt  `json:"attempt"`
	}{Activity: snapshot.Activity, Attempt: snapshot.Attempt}
	encodedInput, err := json.Marshal(input)
	if err != nil {
		return AssessmentArtifact{}, nil, fmt.Errorf("encode offline evaluation input: %w", err)
	}
	request := ProposalRequest{
		RequestID: snapshot.Task.JobID, Type: ProposalAssessment,
		AggregateType: "offline_attempt", AggregateID: snapshot.Task.SubmissionID,
		AggregateVersion:    snapshot.AggregateVersion,
		GoalRevisionID:      snapshot.Activity.GoalRevisionID,
		RouteRevisionID:     snapshot.Activity.RouteRevisionID,
		RouteStepID:         snapshot.Activity.RouteStepID,
		FocusNodeRevisionID: snapshot.Activity.TargetNodeRevisionID,
		ActivityID:          snapshot.Activity.ID, AttemptID: snapshot.Attempt.ID,
		KnowledgeRevisionID: snapshot.Activity.KnowledgeRevisionID,
		NodeRevisionIDs:     offlineEvaluationNodeRevisionIDs(snapshot.Activity),
		Input:               encodedInput,
	}
	inputHash, err := HashJSON(request)
	if err != nil {
		return AssessmentArtifact{}, nil, err
	}
	if snapshot.RetryExpired {
		category := offlineEvaluationFallbackReason([]string{snapshot.LastErrorCategory})
		categories := []string{category, "retry_deadline_exceeded"}
		return c.fallbackArtifact(snapshot, inputHash, categories), []string{category}, nil
	}
	if c.service.model == nil {
		return c.fallbackArtifact(snapshot, inputHash, []string{OfflineReasonModelUnavailable}), []string{OfflineReasonModelUnavailable}, nil
	}
	categories := make([]string, 0, 2)
	var lastDecodeErr error
	for attempt := 1; attempt <= 2; attempt++ {
		output, modelErr := c.service.model.Generate(ctx, request)
		if modelErr != nil {
			category := modelCategory(modelErr)
			categories = append(categories, category)
			if retryableModelCategory(category) {
				if attempt < 2 {
					continue
				}
				return AssessmentArtifact{}, nil, offlineEvaluationError{category: category, cause: modelErr}
			}
			fallbackReason := offlineEvaluationFallbackReason(categories)
			return c.fallbackArtifact(snapshot, inputHash, categories), []string{fallbackReason}, nil
		}
		successCategories := append(append([]string(nil), categories...), "success")
		artifact, decodeErr := c.service.decodeProposal(ctx, request, inputHash, output, successCategories)
		if decodeErr == nil && artifact.Assessment != nil {
			value := *artifact.Assessment
			value.ID = snapshot.Task.FutureAssessmentID
			value.SessionID = snapshot.Activity.SessionID
			value.ActivityID = snapshot.Activity.ID
			value.ActivityRevision = snapshot.Activity.Revision
			value.AttemptID = snapshot.Attempt.ID
			value.CreatedAt = snapshot.Now
			value.EvidenceEligibility = snapshot.Attempt.EvidenceEligibility
			value.EvidenceIneligibleReason = snapshot.Attempt.EvidenceIneligibleReason
			return value, nil, nil
		}
		category := proposalFailureCategory(decodeErr)
		if category == "schema_mismatch" || category == "invalid_response" {
			category = "schema_error"
		}
		categories = append(categories, category)
		lastDecodeErr = decodeErr
	}
	category := "schema_error"
	if len(categories) > 0 && categories[len(categories)-1] != "" {
		category = categories[len(categories)-1]
	}
	return AssessmentArtifact{}, nil, offlineEvaluationError{category: category, cause: lastDecodeErr}
}

func (c *OfflineEvaluationConsumer) fallbackArtifact(snapshot OfflineEvaluationSnapshot, inputHash string, categories []string) AssessmentArtifact {
	return AssessmentArtifact{
		ID: snapshot.Task.FutureAssessmentID, SessionID: snapshot.Activity.SessionID,
		AttemptID: snapshot.Attempt.ID, ActivityID: snapshot.Activity.ID,
		ActivityRevision: snapshot.Activity.Revision, RubricComplete: false, Confidence: 0,
		RiskFlags: []RiskFlag{RiskRetryExhausted}, ModelID: c.service.modelID,
		ModelParameters: cloneMap(c.service.modelParameters), PromptRevision: c.service.promptRevision,
		ProposalInputHash: inputHash, Attempts: len(categories), AttemptCategories: append([]string(nil), categories...),
		EvidenceEligibility:      snapshot.Attempt.EvidenceEligibility,
		EvidenceIneligibleReason: snapshot.Attempt.EvidenceIneligibleReason,
		CreatedAt:                snapshot.Now,
	}
}

func (c *OfflineEvaluationConsumer) completion(snapshot OfflineEvaluationSnapshot, artifact AssessmentArtifact, invalid bool, invalidReasons []string) (OfflineEvaluationCompletion, error) {
	acceptance, err := EvaluateAssessment(snapshot.Activity, snapshot.Attempt, artifact)
	if err != nil {
		if invalid || isInvalidModelArtifactError(err) {
			reasons := []string{"evaluation_invalid"}
			if len(invalidReasons) > 0 {
				reasons = nil
			}
			acceptance = Acceptance{Disposition: DispositionProvisional, Reasons: reasons}
			invalid = true
		} else {
			return OfflineEvaluationCompletion{}, err
		}
	}
	if invalid {
		acceptance = Acceptance{Disposition: DispositionProvisional}
	}
	if snapshot.Attempt.EvidenceIneligibleReason != "" {
		acceptance = Acceptance{Disposition: DispositionProvisional, Reasons: []string{snapshot.Attempt.EvidenceIneligibleReason}}
	}
	reasons := append([]string(nil), acceptance.Reasons...)
	reasons = append(reasons, invalidReasons...)
	reasons = unique(reasons)
	decision := AssessmentDecision{
		ID:           deterministicOfflineEvaluationID(snapshot.Task.SubmissionID, "decision"),
		AssessmentID: artifact.ID, Version: 1, Disposition: acceptance.Disposition,
		Items: append([]AssessmentItem(nil), artifact.Items...), ActorDeviceID: snapshot.Attempt.ActorDeviceID,
		CreatedAt: artifact.CreatedAt,
	}
	completion := OfflineEvaluationCompletion{Artifact: artifact, Decision: decision, Reasons: reasons, Invalid: invalid}
	if acceptance.Disposition != DispositionAccepted {
		return completion, nil
	}
	if snapshot.Attempt.Help == HelpAnswerRevealed || !snapshot.Attempt.EvidenceEligibility {
		completion.Decision.Disposition = DispositionProvisional
		completion.Reasons = unique(append(completion.Reasons, snapshot.Attempt.EvidenceIneligibleReason))
		return completion, nil
	}
	evidence := c.service.makeEvidence(snapshot.Activity, snapshot.Attempt, artifact, decision, artifact.Items, acceptance.Outcome, artifact.CreatedAt)
	evidence.ID = deterministicOfflineEvaluationID(snapshot.Task.SubmissionID, "evidence")
	decision.ProducedEvidenceID = &evidence.ID
	completion.Decision = decision
	completion.Evidence = &evidence
	return completion, nil
}

func (c *OfflineEvaluationConsumer) commitRequest(snapshot OfflineEvaluationSnapshot, completion OfflineEvaluationCompletion) (CommitRequest, error) {
	owners, err := assessmentAuthority(snapshot.Activity, completion.Artifact)
	if err != nil {
		return CommitRequest{}, err
	}
	batch := CommandBatch{
		Assessment: &completion.Artifact, Decisions: []AssessmentDecision{completion.Decision},
		Disposition: completion.Decision.Disposition,
		Authority:   AuthorityProvenance{AssessmentItems: owners},
	}
	batch.Events = append(batch.Events, offlineEvaluationDraft(EventAssessmentRecorded, snapshot, completion.Artifact))
	projection := AssessmentProjectionEvent{
		AssessmentID: completion.Artifact.ID, NodeRevisionID: snapshot.Activity.TargetNodeRevisionID,
		Reasons: completion.Reasons, Decision: completion.Decision,
	}
	if completion.Decision.Disposition == DispositionAccepted && completion.Evidence != nil {
		owner, ownerErr := evidenceAuthority(snapshot.Activity, *completion.Evidence)
		if ownerErr != nil {
			return CommitRequest{}, ownerErr
		}
		batch.Evidence = []AcceptedEvidence{*completion.Evidence}
		batch.Authority.Evidence = map[string]EvidenceOwner{completion.Evidence.ID: owner}
		batch.Events = append(batch.Events,
			offlineEvaluationDraft(EventAssessmentAccepted, snapshot, projection),
			offlineEvaluationDraft(EventEvidenceAccepted, snapshot, *completion.Evidence),
		)
	} else {
		batch.Events = append(batch.Events, offlineEvaluationDraft(EventAssessmentMarkedProvisional, snapshot, projection))
	}
	payload := struct {
		JobID      string             `json:"job_id"`
		Assessment AssessmentArtifact `json:"assessment"`
		Decision   AssessmentDecision `json:"decision"`
		EvidenceID string             `json:"evidence_id,omitempty"`
	}{JobID: snapshot.Task.JobID, Assessment: completion.Artifact, Decision: completion.Decision}
	if completion.Evidence != nil {
		payload.EvidenceID = completion.Evidence.ID
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return CommitRequest{}, fmt.Errorf("encode offline evaluation commit: %w", err)
	}
	requestHash, err := HashJSON(payload)
	if err != nil {
		return CommitRequest{}, err
	}
	return CommitRequest{
		DeviceID: snapshot.Attempt.ActorDeviceID,
		Operation: OperationEnvelope{
			OperationID: snapshot.Task.JobID, PayloadSchemaVersion: 1,
			AggregateType: "offline_attempt", AggregateID: snapshot.Task.SubmissionID,
			ExpectedVersion: snapshot.AggregateVersion, Payload: encoded,
		},
		RequestHash:  requestHash,
		Expectations: []AggregateExpectation{{Type: "offline_attempt", ID: snapshot.Task.SubmissionID, ExpectedVersion: snapshot.AggregateVersion}},
		Batch:        batch, ReceivedAt: completion.Artifact.CreatedAt,
	}, nil
}

func offlineEvaluationNodeRevisionIDs(activity Activity) []string {
	values := make([]string, 0, len(activity.References))
	for _, reference := range activity.References {
		values = append(values, reference.NodeRevisionID)
	}
	return unique(values)
}

func offlineEvaluationDraft(kind EventType, snapshot OfflineEvaluationSnapshot, payload any) EventDraft {
	return EventDraft{
		Type: kind, AggregateType: "offline_attempt", AggregateID: snapshot.Task.SubmissionID,
		Payload: mustJSON(payload), ParentSessionID: snapshot.Activity.SessionID, Source: "offline",
		ArchiveDisposition: "succeeded", EvidenceDisposition: string(OfflineEvidencePendingEvaluation),
		GoalRevisionID: snapshot.Activity.GoalRevisionID, RouteRevisionID: snapshot.Activity.RouteRevisionID,
		KnowledgeRevisionID: snapshot.Activity.KnowledgeRevisionID, ActivityID: snapshot.Activity.ID,
		ActivityRevision: snapshot.Activity.Revision,
	}
}

func deterministicOfflineEvaluationID(submissionID, kind string) string {
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte("edu-agent/offline-evaluation/"+submissionID+"/"+kind)).String()
}

func isInvalidModelArtifactError(err error) bool {
	var domain *Error
	return errors.As(err, &domain) && (domain.Code == CodeProposalRejected || domain.Code == CodeKnowledgeReferenceInvalid)
}

func offlineEvaluationFallbackReason(categories []string) string {
	for index := len(categories) - 1; index >= 0; index-- {
		switch categories[index] {
		case "schema_error", "schema_mismatch", "invalid_response":
			return "schema_error"
		}
	}
	return "model_unavailable"
}

func classifyOfflineEvaluationError(err error) (string, bool) {
	var classified offlineEvaluationError
	if errors.As(err, &classified) {
		return classified.Category(), classified.Permanent()
	}
	var domain *Error
	if errors.As(err, &domain) {
		switch domain.Code {
		case CodeVersionConflict:
			return "version_conflict", false
		case CodeProposalRejected, CodeKnowledgeReferenceInvalid, CodeInvalidRequest:
			return "invalid_frozen_input", true
		}
	}
	return "storage_error", false
}

type offlineEvaluationError struct {
	category  string
	permanent bool
	cause     error
}

func (e offlineEvaluationError) Error() string {
	if e.cause == nil {
		return e.category
	}
	return fmt.Sprintf("%s: %v", e.category, e.cause)
}
func (e offlineEvaluationError) Unwrap() error    { return e.cause }
func (e offlineEvaluationError) Category() string { return strings.TrimSpace(e.category) }
func (e offlineEvaluationError) Permanent() bool  { return e.permanent }
