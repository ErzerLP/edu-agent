package learning

import (
	"context"
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/platform/outbox"
)

type offlineEvaluationStoreStub struct {
	snapshot   OfflineEvaluationSnapshot
	artifact   *AssessmentArtifact
	canApply   bool
	completed  *OfflineEvaluationCompletion
	retry      string
	failed     string
	processing int
}

func (s *offlineEvaluationStoreStub) OfflineEvaluationCanApply(context.Context, outbox.Message) (outbox.ApplyDecision, error) {
	if s.canApply {
		return outbox.ApplyDecision{Apply: true}, nil
	}
	return outbox.ApplyDecision{Apply: false, TerminalDisposition: "superseded"}, nil
}
func (s *offlineEvaluationStoreStub) BeginOfflineEvaluation(context.Context, outbox.Message) (OfflineEvaluationSnapshot, error) {
	s.processing++
	return s.snapshot, nil
}
func (s *offlineEvaluationStoreStub) SaveOfflineEvaluationArtifact(_ context.Context, _ OfflineEvaluationSnapshot, artifact AssessmentArtifact) error {
	copy := artifact
	s.artifact = &copy
	return nil
}
func (s *offlineEvaluationStoreStub) CompleteOfflineEvaluation(_ context.Context, _ OfflineEvaluationSnapshot, completion OfflineEvaluationCompletion, _ OperationResult) error {
	copy := completion
	s.completed = &copy
	s.canApply = false
	return nil
}
func (s *offlineEvaluationStoreStub) MarkOfflineEvaluationRetry(_ context.Context, _ OfflineEvaluationSnapshot, category string) error {
	s.retry = category
	return nil
}
func (s *offlineEvaluationStoreStub) MarkOfflineEvaluationFailed(_ context.Context, _ OfflineEvaluationSnapshot, category string) error {
	s.failed = category
	return nil
}

type transientOfflineEvaluationModelError struct{ category string }

func (e transientOfflineEvaluationModelError) Error() string    { return e.category }
func (e transientOfflineEvaluationModelError) Category() string { return e.category }
func (transientOfflineEvaluationModelError) Retryable() bool    { return true }

func TestOfflineEvaluationConsumerAppliesFrozenAssessmentExactlyOnce(t *testing.T) {
	authority, store, message, artifact := offlineEvaluationWorkerFixture(t)
	output := struct {
		Assessment struct {
			Items          []AssessmentItem `json:"items"`
			RubricComplete bool             `json:"rubric_complete"`
			Confidence     int              `json:"confidence"`
			RiskFlags      []RiskFlag       `json:"risk_flags"`
		} `json:"assessment"`
	}{}
	output.Assessment.Items = artifact.Items
	output.Assessment.RubricComplete = artifact.RubricComplete
	output.Assessment.Confidence = artifact.Confidence
	output.Assessment.RiskFlags = []RiskFlag{}
	raw, err := json.Marshal(output)
	if err != nil {
		t.Fatal(err)
	}
	model := &proposalTestModel{results: []proposalModelResult{{raw: raw}}}
	service := newProposalTestService(t, authority, &proposalTestRepository{}, model)
	consumer, err := NewOfflineEvaluationConsumer(service, store)
	if err != nil {
		t.Fatal(err)
	}
	if err := consumer.Apply(context.Background(), message); err != nil {
		t.Fatal(err)
	}
	if authority.commits != 1 || authority.lastCommit.Operation.AggregateType != "offline_attempt" {
		t.Fatalf("commit = %#v, commits=%d", authority.lastCommit, authority.commits)
	}
	if authority.lastCommit.Operation.OperationID != store.snapshot.Task.JobID {
		t.Fatalf("operation id = %q", authority.lastCommit.Operation.OperationID)
	}
	if store.artifact == nil || store.completed == nil || store.completed.Artifact.ID != store.snapshot.Task.FutureAssessmentID || store.completed.Invalid {
		t.Fatalf("artifact=%#v completion=%#v", store.artifact, store.completed)
	}
	decision, err := consumer.CanApply(context.Background(), message)
	if err != nil || decision.Apply || decision.TerminalDisposition != "superseded" {
		t.Fatalf("replay decision=%#v err=%v", decision, err)
	}
}

func TestOfflineEvaluationConsumerCompletesLowConfidenceAssessmentProvisionally(t *testing.T) {
	authority, store, message, artifact := offlineEvaluationWorkerFixture(t)
	artifact.Confidence = 100
	output := struct {
		Assessment struct {
			Items          []AssessmentItem `json:"items"`
			RubricComplete bool             `json:"rubric_complete"`
			Confidence     int              `json:"confidence"`
			RiskFlags      []RiskFlag       `json:"risk_flags"`
		} `json:"assessment"`
	}{}
	output.Assessment.Items = artifact.Items
	output.Assessment.RubricComplete = true
	output.Assessment.Confidence = artifact.Confidence
	output.Assessment.RiskFlags = []RiskFlag{}
	raw, err := json.Marshal(output)
	if err != nil {
		t.Fatal(err)
	}
	service := newProposalTestService(t, authority, &proposalTestRepository{}, &proposalTestModel{results: []proposalModelResult{{raw: raw}}})
	consumer, err := NewOfflineEvaluationConsumer(service, store)
	if err != nil {
		t.Fatal(err)
	}
	if err := consumer.Apply(context.Background(), message); err != nil {
		t.Fatal(err)
	}
	if store.completed == nil || store.completed.Decision.Disposition != DispositionProvisional || store.completed.Evidence != nil || len(store.completed.Reasons) != 1 || store.completed.Reasons[0] != "low_confidence" {
		t.Fatalf("provisional completion=%#v", store.completed)
	}
}

func TestOfflineEvaluationConsumerRetriesInvalidModelSchema(t *testing.T) {
	authority, store, message, _ := offlineEvaluationWorkerFixture(t)
	model := &proposalTestModel{results: []proposalModelResult{{raw: json.RawMessage(`{}`)}, {raw: json.RawMessage(`{}`)}}}
	service := newProposalTestService(t, authority, &proposalTestRepository{}, model)
	consumer, err := NewOfflineEvaluationConsumer(service, store)
	if err != nil {
		t.Fatal(err)
	}
	if err := consumer.Apply(context.Background(), message); err == nil {
		t.Fatal("expected invalid model schema error")
	}
	if store.retry != "schema_error" || store.completed != nil || authority.commits != 0 {
		t.Fatalf("retry=%q completion=%#v commits=%d", store.retry, store.completed, authority.commits)
	}
}

func TestOfflineEvaluationConsumerConvergesExpiredSchemaRetryProvisionally(t *testing.T) {
	authority, store, message, _ := offlineEvaluationWorkerFixture(t)
	store.snapshot.RetryExpired = true
	store.snapshot.LastErrorCategory = "schema_error"
	service := newProposalTestService(t, authority, &proposalTestRepository{}, &proposalTestModel{})
	consumer, err := NewOfflineEvaluationConsumer(service, store)
	if err != nil {
		t.Fatal(err)
	}
	if err := consumer.Apply(context.Background(), message); err != nil {
		t.Fatal(err)
	}
	if store.failed != "" || store.completed == nil || store.completed.Decision.Disposition != DispositionProvisional || store.completed.Evidence != nil || !slices.Contains(store.completed.Reasons, "schema_error") {
		t.Fatalf("failed=%q completion=%#v", store.failed, store.completed)
	}
}

func TestOfflineEvaluationConsumerConvergesWithoutModelProvisionally(t *testing.T) {
	authority, store, message, _ := offlineEvaluationWorkerFixture(t)
	service := newProposalTestService(t, authority, &proposalTestRepository{}, nil)
	consumer, err := NewOfflineEvaluationConsumer(service, store)
	if err != nil {
		t.Fatal(err)
	}
	if err := consumer.Apply(context.Background(), message); err != nil {
		t.Fatal(err)
	}
	if store.retry != "" || store.failed != "" || store.completed == nil || store.completed.Decision.Disposition != DispositionProvisional || store.completed.Evidence != nil || !slices.Equal(store.completed.Reasons, []string{OfflineReasonModelUnavailable}) {
		t.Fatalf("retry=%q failed=%q completion=%#v", store.retry, store.failed, store.completed)
	}
	if store.artifact == nil || !slices.Equal(store.artifact.AttemptCategories, []string{OfflineReasonModelUnavailable}) || authority.commits != 1 {
		t.Fatalf("artifact=%#v commits=%d", store.artifact, authority.commits)
	}
}

func TestOfflineEvaluationConsumerKeepsTransientModelFailureRetryable(t *testing.T) {
	authority, store, message, _ := offlineEvaluationWorkerFixture(t)
	model := &proposalTestModel{results: []proposalModelResult{{err: transientOfflineEvaluationModelError{category: "timeout"}}, {err: transientOfflineEvaluationModelError{category: "timeout"}}}}
	service := newProposalTestService(t, authority, &proposalTestRepository{}, model)
	consumer, err := NewOfflineEvaluationConsumer(service, store)
	if err != nil {
		t.Fatal(err)
	}
	if err := consumer.Apply(context.Background(), message); err == nil {
		t.Fatal("expected retryable apply error")
	}
	if store.retry != "upstream_error" || store.completed != nil || authority.commits != 0 {
		t.Fatalf("retry=%q completion=%#v commits=%d", store.retry, store.completed, authority.commits)
	}
}

func offlineEvaluationWorkerFixture(t *testing.T) (*proposalTestStore, *offlineEvaluationStoreStub, outbox.Message, AssessmentArtifact) {
	t.Helper()
	activity, attempt, artifact := assessmentFixture()
	activity.ID = "11111111-1111-4111-8111-111111111111"
	activity.SessionID = "22222222-2222-4222-8222-222222222222"
	activity.KnowledgeRevisionID = "33333333-3333-4333-8333-333333333333"
	activity.TargetNodeID = "44444444-4444-4444-8444-444444444444"
	activity.TargetNodeRevisionID = "55555555-5555-4555-8555-555555555555"
	activity.References[0].KnowledgeRevisionID = activity.KnowledgeRevisionID
	activity.References[0].NodeID = activity.TargetNodeID
	activity.References[0].NodeRevisionID = activity.TargetNodeRevisionID
	activity.References[0].DocumentRevisionID = "66666666-6666-4666-8666-666666666666"
	activity.Rubric.Items[0].RequiredReferenceIDs = []string{activity.TargetNodeRevisionID}
	attempt.ID = "77777777-7777-4777-8777-777777777777"
	attempt.ActivityID = activity.ID
	attempt.ActorDeviceID = "dddddddd-dddd-4ddd-8ddd-dddddddddddd"
	artifact.Items = append([]AssessmentItem(nil), artifact.Items...)
	artifact.Items[0].KnowledgeReferenceID = activity.TargetNodeRevisionID
	submissionID := "88888888-8888-4888-8888-888888888888"
	futureID := "99999999-9999-4999-8999-999999999999"
	jobID := "cccccccc-cccc-4ccc-8ccc-cccccccccccc"
	message := outbox.Message{
		ID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", BusinessType: "learning.offline-evaluation",
		AggregateID: submissionID, Revision: 1, Generation: 3,
		IdempotencyKey: "learning.offline-evaluation:" + submissionID,
		LeaseToken:     "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", LeaseExpiresAt: ptrEvaluationTime(time.Now().UTC().Add(time.Minute)),
	}
	message.Payload, _ = json.Marshal(OfflineEvaluationTask{JobID: jobID, SubmissionID: submissionID, FutureAssessmentID: futureID, LearnerGeneration: 3})
	authority := &proposalTestStore{activity: activity, attempt: attempt, aggregateVer: 0}
	store := &offlineEvaluationStoreStub{canApply: true, snapshot: OfflineEvaluationSnapshot{
		Task:     OfflineEvaluationTask{JobID: jobID, SubmissionID: submissionID, FutureAssessmentID: futureID, LearnerGeneration: 3},
		Activity: activity, Attempt: attempt, AggregateVersion: 0, AttemptCount: 1,
		RetryDeadline: time.Now().UTC().Add(time.Hour), Now: time.Now().UTC(), LeaseToken: message.LeaseToken,
	}}
	return authority, store, message, artifact
}

func ptrEvaluationTime(value time.Time) *time.Time { return &value }
