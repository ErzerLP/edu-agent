package learning

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

type offlineAssessmentAuthorityStub struct {
	AuthorityStore
	view       OfflineAssessmentView
	viewErr    error
	lookup     OperationResult
	lookupErr  error
	found      bool
	commits    int
	lastCommit CommitRequest
}

func (s *offlineAssessmentAuthorityStub) ListOfflineAssessments(context.Context, string, OfflineAssessmentQuery) (OfflineAssessmentPage, error) {
	return OfflineAssessmentPage{}, nil
}

func (s *offlineAssessmentAuthorityStub) OfflineAssessment(context.Context, string, string) (OfflineAssessmentView, error) {
	return s.view, s.viewErr
}

func (s *offlineAssessmentAuthorityStub) LookupOperation(context.Context, OperationLookup) (OperationResult, error, bool) {
	return s.lookup, s.lookupErr, s.found
}

func (s *offlineAssessmentAuthorityStub) ArchiveRejection(context.Context, OperationRejection) (OperationResult, error) {
	panic("offline assessment decisions do not archive tutoring rejections")
}

func (s *offlineAssessmentAuthorityStub) Commit(_ context.Context, request CommitRequest) (OperationResult, error) {
	s.commits++
	s.lastCommit = request
	return OperationResult{
		Status: "succeeded", Archived: true, AggregateType: "offline_attempt", AggregateID: request.Operation.AggregateID,
		AggregateVersion:   request.Operation.ExpectedVersion + int64(len(request.Batch.Events)),
		FirstEventSequence: 21, LastEventSequence: 21 + int64(len(request.Batch.Events)) - 1,
		ProjectionAsOf: 21 + int64(len(request.Batch.Events)) - 1, Result: append(json.RawMessage(nil), request.Batch.TypedResult...),
	}, nil
}

func TestOfflineAssessmentDecisionUsesAttemptAggregateWithoutTutoringFeedback(t *testing.T) {
	view := offlineAssessmentServiceFixture()
	store := &offlineAssessmentAuthorityStub{view: view}
	service := &Service{
		authority: store,
		now:       func() time.Time { return time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC) },
		newUUID:   func() string { return "89000000-0000-4000-8000-000000000001" },
	}
	command := OfflineAssessmentDecisionCommand{
		OperationID: "88000000-0000-4000-8000-000000000001", PayloadSchemaVersion: 1,
		AttemptID: view.Attempt.ID, ExpectedVersion: 2, Kind: "override", ExpectedDispositionVersion: 1,
		Reason: "reviewed offline result", Items: []OfflineAssessmentOverrideItem{{RubricItemID: "item-1", Conclusion: ConclusionPass}},
	}
	receipt, err := service.DecideOfflineAssessment(t.Context(), view.Attempt.ActorDeviceID, view.Assessment.ID, command)
	if err != nil {
		t.Fatal(err)
	}
	if store.commits != 1 || receipt.OperationID != command.OperationID || receipt.Decision.ID != command.OperationID || receipt.Decision.Disposition != DispositionOverridden || receipt.Decision.ProducedEvidenceID == nil {
		t.Fatalf("receipt=%+v commits=%d", receipt, store.commits)
	}
	commit := store.lastCommit
	if commit.Operation.AggregateType != "offline_attempt" || commit.Operation.AggregateID != view.SubmissionID || commit.Operation.ExpectedVersion != command.ExpectedVersion || len(commit.Expectations) != 1 || commit.Expectations[0].Type != "offline_attempt" {
		t.Fatalf("offline decision aggregate=%+v expectations=%+v", commit.Operation, commit.Expectations)
	}
	if commit.Batch.Session != nil || commit.Batch.Assessment != nil || len(commit.Batch.Decisions) != 1 || len(commit.Batch.Evidence) != 1 || commit.Batch.OfflineStatusUpdate == nil || commit.Batch.OfflineStatusUpdate.Evidence != OfflineEvidenceAccepted {
		t.Fatalf("offline decision batch crossed ownership or missed projection facts: %+v", commit.Batch)
	}
	for _, event := range commit.Batch.Events {
		if event.AggregateType != "offline_attempt" || event.AggregateID != view.SubmissionID || event.Source != "offline" || event.Type == EventTutoringStateChanged {
			t.Fatalf("offline decision event crossed tutoring state: %+v", event)
		}
	}
	decision := commit.Batch.Decisions[0]
	if len(decision.Items) != 1 || decision.Items[0].AnswerQuote != view.Assessment.Items[0].AnswerQuote || decision.Items[0].KnowledgeQuote != view.Assessment.Items[0].KnowledgeQuote {
		t.Fatalf("override did not preserve immutable assessment source: %+v", decision.Items)
	}
}

func TestOfflineAssessmentDecisionRejectsExpectedAttemptVersionConflict(t *testing.T) {
	view := offlineAssessmentServiceFixture()
	store := &offlineAssessmentAuthorityStub{view: view}
	service := &Service{authority: store}
	command := OfflineAssessmentDecisionCommand{
		OperationID: "88000000-0000-4000-8000-000000000004", PayloadSchemaVersion: 1,
		AttemptID: view.Attempt.ID, ExpectedVersion: 3, Kind: "void", ExpectedDispositionVersion: 1, Reason: "invalid assessment",
	}
	_, err := service.DecideOfflineAssessment(t.Context(), view.Attempt.ActorDeviceID, view.Assessment.ID, command)
	var conflict *Error
	if !errors.As(err, &conflict) || conflict.Code != CodeVersionConflict || conflict.AggregateType != "offline_attempt" || conflict.AggregateID != view.SubmissionID || conflict.ExpectedVersion != 3 || conflict.CurrentVersion != 2 || conflict.AsOfEventSequence != view.Metadata.AsOfEventSequence || store.commits != 0 {
		t.Fatalf("version conflict=%+v commits=%d", conflict, store.commits)
	}
}

func TestOfflineAssessmentDecisionCannotUpgradeIneligibleAttempt(t *testing.T) {
	view := offlineAssessmentServiceFixture()
	view.Attempt.EvidenceEligibility = false
	view.Attempt.EvidenceIneligibleReason = "stale_knowledge"
	store := &offlineAssessmentAuthorityStub{view: view}
	service := &Service{authority: store}
	command := OfflineAssessmentDecisionCommand{
		OperationID: "88000000-0000-4000-8000-000000000005", PayloadSchemaVersion: 1,
		AttemptID: view.Attempt.ID, ExpectedVersion: 2, Kind: "override", ExpectedDispositionVersion: 1,
		Reason: "reviewed offline result", Items: []OfflineAssessmentOverrideItem{{RubricItemID: "item-1", Conclusion: ConclusionPass}},
	}
	_, err := service.DecideOfflineAssessment(t.Context(), view.Attempt.ActorDeviceID, view.Assessment.ID, command)
	var conflict *Error
	if !errors.As(err, &conflict) || conflict.Code != CodeAssessmentDispositionConflict || conflict.Reason != view.Attempt.EvidenceIneligibleReason || store.commits != 0 {
		t.Fatalf("eligibility conflict=%+v commits=%d", conflict, store.commits)
	}
}

func TestOfflineAssessmentDecisionReplayPrecedesDispositionValidation(t *testing.T) {
	view := offlineAssessmentServiceFixture()
	view.Decision.Disposition = DispositionVoided
	operationID := "88000000-0000-4000-8000-000000000002"
	decision := AssessmentDecision{
		ID: operationID, AssessmentID: view.Assessment.ID, Version: 2, Disposition: DispositionVoided,
		Items: view.Assessment.Items, ActorDeviceID: view.Attempt.ActorDeviceID, CreatedAt: time.Now().UTC(),
	}
	encoded, _ := json.Marshal(decision)
	store := &offlineAssessmentAuthorityStub{
		view: view, found: true,
		lookup: OperationResult{Replayed: true, AggregateVersion: 3, FirstEventSequence: 21, LastEventSequence: 21, ProjectionAsOf: 21, Result: encoded},
	}
	service := &Service{authority: store}
	command := OfflineAssessmentDecisionCommand{
		OperationID: operationID, PayloadSchemaVersion: 1, AttemptID: view.Attempt.ID,
		ExpectedVersion: 2, Kind: "void", ExpectedDispositionVersion: 1, Reason: "invalid assessment",
	}
	receipt, err := service.DecideOfflineAssessment(t.Context(), view.Attempt.ActorDeviceID, view.Assessment.ID, command)
	if err != nil || !receipt.Replayed || store.commits != 0 || receipt.Decision.ID != operationID {
		t.Fatalf("replay receipt=%+v commits=%d err=%v", receipt, store.commits, err)
	}
}

func TestOfflineAssessmentDecisionContentConflictIsStable(t *testing.T) {
	view := offlineAssessmentServiceFixture()
	store := &offlineAssessmentAuthorityStub{
		view: view, found: true, lookupErr: &Error{Code: CodeIdempotencyConflict, Reason: "operation_content_mismatch"},
	}
	service := &Service{authority: store}
	command := OfflineAssessmentDecisionCommand{
		OperationID: "88000000-0000-4000-8000-000000000003", PayloadSchemaVersion: 1,
		AttemptID: view.Attempt.ID, ExpectedVersion: 2, Kind: "void", ExpectedDispositionVersion: 1, Reason: "invalid assessment",
	}
	if _, err := service.DecideOfflineAssessment(t.Context(), view.Attempt.ActorDeviceID, view.Assessment.ID, command); ErrorCode(err) != CodeIdempotencyConflict || store.commits != 0 {
		t.Fatalf("content conflict err=%v commits=%d", err, store.commits)
	}
}

func TestOfflineAssessmentDecisionLimitsUseUnicodeCodePoints(t *testing.T) {
	base := OfflineAssessmentDecisionCommand{
		OperationID: "88000000-0000-4000-8000-000000000006", PayloadSchemaVersion: 1,
		AttemptID: "82000000-0000-4000-8000-000000000001", ExpectedVersion: 2,
		Kind: "void", ExpectedDispositionVersion: 1,
	}
	for _, test := range []struct {
		name   string
		reason string
		valid  bool
	}{
		{name: "multibyte reason at boundary", reason: strings.Repeat("界", MaxOfflineAssessmentDecisionReasonRunes), valid: true},
		{name: "multibyte reason over boundary", reason: strings.Repeat("界", MaxOfflineAssessmentDecisionReasonRunes+1)},
	} {
		t.Run(test.name, func(t *testing.T) {
			command := base
			command.Reason = test.reason
			err := validateOfflineAssessmentDecisionCommand("90000000-0000-4000-8000-000000000001", "81000000-0000-4000-8000-000000000001", command)
			if test.valid && err != nil {
				t.Fatalf("boundary reason rejected: %v", err)
			}
			if !test.valid && ErrorCode(err) != CodeInvalidRequest {
				t.Fatalf("overlong reason err=%v", err)
			}
		})
	}

	boundaryRubricID := strings.Repeat("项", MaxOfflineAssessmentRubricItemIDRunes)
	artifact := AssessmentArtifact{Items: []AssessmentItem{{RubricItemID: boundaryRubricID}}}
	command := OfflineAssessmentDecisionCommand{Kind: "override", Items: []OfflineAssessmentOverrideItem{{
		RubricItemID: boundaryRubricID, Conclusion: ConclusionPartial,
		MisconceptionCandidate: strings.Repeat("误", MaxOfflineAssessmentMisconceptionRunes),
	}}}
	if _, err := offlineAssessmentDecisionItems(artifact, command); err != nil {
		t.Fatalf("boundary override item rejected: %v", err)
	}
	command.Items[0].RubricItemID = strings.Repeat("项", MaxOfflineAssessmentRubricItemIDRunes+1)
	if _, err := offlineAssessmentDecisionItems(artifact, command); ErrorCode(err) != CodeInvalidRequest {
		t.Fatalf("overlong rubric item ID err=%v", err)
	}
	command.Items[0].RubricItemID = boundaryRubricID
	command.Items[0].MisconceptionCandidate = strings.Repeat("误", MaxOfflineAssessmentMisconceptionRunes+1)
	if _, err := offlineAssessmentDecisionItems(artifact, command); ErrorCode(err) != CodeInvalidRequest {
		t.Fatalf("overlong misconception err=%v", err)
	}
}

func offlineAssessmentServiceFixture() OfflineAssessmentView {
	activity, attempt, artifact := assessmentFixture()
	activity.ID = "85000000-0000-4000-8000-000000000001"
	activity.SessionID = "86000000-0000-4000-8000-000000000001"
	activity.GoalRevisionID = "86000000-0000-4000-8000-000000000002"
	activity.RouteRevisionID = "86000000-0000-4000-8000-000000000003"
	activity.RouteStepID = "86000000-0000-4000-8000-000000000004"
	activity.KnowledgeRevisionID = "86000000-0000-4000-8000-000000000005"
	activity.TargetNodeID = "86000000-0000-4000-8000-000000000006"
	activity.TargetNodeRevisionID = "86000000-0000-4000-8000-000000000007"
	activity.References[0].KnowledgeRevisionID = activity.KnowledgeRevisionID
	activity.References[0].NodeID = activity.TargetNodeID
	activity.References[0].NodeRevisionID = activity.TargetNodeRevisionID
	activity.References[0].DocumentRevisionID = "86000000-0000-4000-8000-000000000008"
	activity.Rubric.Items[0].RequiredReferenceIDs = []string{activity.TargetNodeRevisionID}

	attempt.ID = "82000000-0000-4000-8000-000000000001"
	attempt.SessionID = activity.SessionID
	attempt.ActivityID = activity.ID
	attempt.ActorDeviceID = "90000000-0000-4000-8000-000000000001"
	attempt.OfflineSubmissionID = "83000000-0000-4000-8000-000000000001"
	attempt.ArchiveDisposition = "offline_succeeded"
	attempt.EvidenceEligibility = true

	artifact.ID = "81000000-0000-4000-8000-000000000001"
	artifact.SessionID = activity.SessionID
	artifact.AttemptID = attempt.ID
	artifact.ActivityID = activity.ID
	artifact.Items[0].KnowledgeReferenceID = activity.TargetNodeRevisionID
	artifact.EvidenceEligibility = true
	artifact.CreatedAt = attempt.ReceivedAt
	decision := AssessmentDecision{
		ID: "87000000-0000-4000-8000-000000000001", AssessmentID: artifact.ID, Version: 1,
		Disposition: DispositionProvisional, Items: artifact.Items, ActorDeviceID: attempt.ActorDeviceID, CreatedAt: attempt.ReceivedAt,
	}
	return OfflineAssessmentView{
		Metadata: ProjectionMetadata{AsOfEventSequence: 20}, SubmissionID: attempt.OfflineSubmissionID,
		AggregateVersion: "2", Activity: activity, Attempt: attempt, Assessment: artifact,
		Decision: decision, AllowedDecisions: []string{"override", "void"},
	}
}
