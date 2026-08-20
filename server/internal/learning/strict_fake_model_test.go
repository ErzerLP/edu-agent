package learning

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/tutoring"
)

type strictProjectionStore struct {
	*proposalTestStore
	projection Projection
	events     []LearningEvent
	registry   *EventRegistry
}

func newStrictProjectionStore(base *proposalTestStore) *strictProjectionStore {
	return &strictProjectionStore{proposalTestStore: base, projection: EmptyProjection("incremental-generation"), registry: NewEventRegistry()}
}

func (s *strictProjectionStore) Commit(ctx context.Context, request CommitRequest) (OperationResult, error) {
	versions := make(map[string]int64, len(request.Expectations))
	for _, expectation := range request.Expectations {
		versions[expectation.Type+"\x00"+expectation.ID] = expectation.ExpectedVersion
	}
	canonical := make([]LearningEvent, 0, len(request.Batch.Events))
	for ordinal, draft := range request.Batch.Events {
		key := draft.AggregateType + "\x00" + draft.AggregateID
		current, ok := versions[key]
		if !ok {
			current = request.Operation.ExpectedVersion
		}
		current++
		versions[key] = current
		sequence := s.projection.Metadata.AsOfEventSequence + int64(ordinal) + 1
		canonical = append(canonical, LearningEvent{
			EventSequence: sequence, ID: fmt.Sprintf("strict-event-%d", sequence), Type: draft.Type,
			SchemaVersion: EventSchemaVersion, AggregateType: draft.AggregateType, AggregateID: draft.AggregateID,
			AggregateVersion: current,
			DeviceID:         request.DeviceID, OperationID: request.Operation.OperationID, OperationOrdinal: ordinal,
			ReceivedAt: request.ReceivedAt, OccurredAt: request.Operation.OccurredAt,
			PayloadID: fmt.Sprintf("strict-payload-%d", sequence), PayloadHash: SHA256(draft.Payload), Payload: append(json.RawMessage(nil), draft.Payload...),
		})
	}
	for _, event := range canonical {
		if err := ApplyEvent(&s.projection, s.registry, event); err != nil {
			return OperationResult{}, fmt.Errorf("apply strict incremental event: %w", err)
		}
	}
	s.events = append(s.events, canonical...)
	return s.proposalTestStore.Commit(ctx, request)
}

type strictAssessmentResolver struct {
	crossRevisionNode string
	resolved          map[string]string
}

func (r *strictAssessmentResolver) Resolve(ctx context.Context, knowledgeRevisionID, nodeRevisionID string) (KnowledgeReference, error) {
	resolved, err := (proposalResolver{}).Resolve(ctx, knowledgeRevisionID, nodeRevisionID)
	if err == nil && nodeRevisionID == r.crossRevisionNode {
		resolved.KnowledgeRevisionID = "other-knowledge-revision"
	}
	if err == nil {
		if r.resolved == nil {
			r.resolved = map[string]string{}
		}
		r.resolved[nodeRevisionID] = resolved.KnowledgeRevisionID
	}
	return resolved, err
}

type strictAssessmentConfig struct {
	requestNodeIDs []string
	resolver       KnowledgeReferenceResolver
}

type strictAssessmentFixture struct {
	store    *strictProjectionStore
	repo     *proposalTestRepository
	model    *proposalTestModel
	service  *Service
	request  ProposalRequest
	resolver KnowledgeReferenceResolver
}

func newStrictAssessmentFixture(t *testing.T, results []proposalModelResult, configure func(*strictAssessmentConfig)) strictAssessmentFixture {
	t.Helper()
	activity, attempt, _ := assessmentFixture()
	activity.GoalRevisionID = "goal-revision"
	activity.RouteRevisionID = "route-revision"
	activity.RouteStepID = "route-step"
	attempt.SessionID = activity.SessionID
	config := strictAssessmentConfig{requestNodeIDs: []string{activity.TargetNodeRevisionID}, resolver: &strictAssessmentResolver{}}
	if configure != nil {
		configure(&config)
	}
	activityID, attemptID := activity.ID, attempt.ID
	session := tutoring.Session{
		ID: activity.SessionID, State: tutoring.StateEvaluating, AggregateVer: 7,
		Context: tutoring.FocusContext{
			GoalRevisionID: "goal-revision", RouteRevisionID: "route-revision", RouteStepID: "route-step",
			KnowledgeRevisionID: activity.KnowledgeRevisionID, FocusNodeRevisionID: activity.TargetNodeRevisionID,
			ActivityID: &activityID, AttemptID: &attemptID,
		},
	}
	request := frozenSessionRequest(session, ProposalAssessment, activity.KnowledgeRevisionID, config.requestNodeIDs)
	request.RequestID = "81000000-0000-4000-8000-000000000001"
	request.Input = json.RawMessage(`{"assessment":"strict-fake-e2e"}`)
	store := newStrictProjectionStore(&proposalTestStore{
		session: session, activity: activity, attempt: attempt,
		goal:  GoalRevision{ID: "goal-revision"},
		route: RouteRevision{ID: "route-revision", GoalRevisionID: "goal-revision", KnowledgeRevisionID: activity.KnowledgeRevisionID, Steps: []RouteStep{{ID: "route-step", Ordinal: 0, NodeID: activity.TargetNodeID, NodeRevisionID: activity.TargetNodeRevisionID}}},
	})
	repo := &proposalTestRepository{claim: ProposalClaim{State: "claimed", LeaseToken: "strict-lease"}}
	model := &proposalTestModel{results: results}
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	ids := []string{"10000000-0000-4000-8000-000000000010", "10000000-0000-4000-8000-000000000011"}
	idIndex := 0
	service, err := NewService(store, repo, config.resolver, ServiceOptions{Now: func() time.Time { return now }, NewUUID: func() string { value := ids[idIndex%len(ids)]; idIndex++; return value }, Model: model, ModelID: "strict-fake", ModelParameters: map[string]any{"temperature": 0}, PromptRevision: "test-prompt"})
	if err != nil {
		t.Fatal(err)
	}
	return strictAssessmentFixture{store: store, repo: repo, model: model, service: service, request: request, resolver: config.resolver}
}

func strictAssessmentRaw(t *testing.T, mutate func(*AssessmentArtifact)) json.RawMessage {
	t.Helper()
	_, _, artifact := assessmentFixture()
	artifact.Items = append([]AssessmentItem(nil), artifact.Items...)
	if mutate != nil {
		mutate(&artifact)
	}
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
	output.Assessment.RiskFlags = artifact.RiskFlags
	raw, err := json.Marshal(output)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func strictProjectionFingerprint(t *testing.T, projection Projection) string {
	t.Helper()
	fingerprint, err := ProjectionFingerprint(projection)
	if err != nil {
		t.Fatal(err)
	}
	return fingerprint
}

func strictFullReplay(t *testing.T, events []LearningEvent) Projection {
	t.Helper()
	projection, err := Replay(events, NewEventRegistry(), "full-replay-generation")
	if err != nil {
		t.Fatal(err)
	}
	return projection
}

func strictErrorHasReason(err error, want string) bool {
	for err != nil {
		if domain, ok := err.(*Error); ok && domain.Reason == want {
			return true
		}
		err = errors.Unwrap(err)
	}
	return false
}

func assertStrictProjectionConvergence(t *testing.T, fixture strictAssessmentFixture, disposition Disposition) {
	t.Helper()
	incremental := fixture.store.projection
	full := strictFullReplay(t, fixture.store.events)
	incrementalFingerprint := strictProjectionFingerprint(t, incremental)
	fullFingerprint := strictProjectionFingerprint(t, full)
	if incrementalFingerprint != fullFingerprint {
		t.Fatalf("incremental/full projection mismatch: incremental=%s full=%s", incrementalFingerprint, fullFingerprint)
	}
	if incremental.Metadata.AsOfEventSequence != full.Metadata.AsOfEventSequence || !reflect.DeepEqual(incremental.Timeline, full.Timeline) {
		t.Fatalf("incremental/full event projection mismatch: incremental_as_of=%d full_as_of=%d incremental_timeline=%+v full_timeline=%+v", incremental.Metadata.AsOfEventSequence, full.Metadata.AsOfEventSequence, incremental.Timeline, full.Timeline)
	}
	incrementalSession, incrementalSessionFound := incremental.Sessions[fixture.store.session.ID]
	fullSession, fullSessionFound := full.Sessions[fixture.store.session.ID]
	if !incrementalSessionFound || !fullSessionFound || incrementalSession.Session.State != fullSession.Session.State || !reflect.DeepEqual(incremental.Sessions, full.Sessions) {
		t.Fatalf("incremental/full session state mismatch: incremental=%+v full=%+v", incremental.Sessions, full.Sessions)
	}
	if !reflect.DeepEqual(incremental.Evidence, full.Evidence) || !reflect.DeepEqual(incremental.Pending, full.Pending) || !reflect.DeepEqual(incremental.Nodes, full.Nodes) {
		t.Fatalf("incremental/full derived projection mismatch: incremental evidence=%+v pending=%+v nodes=%+v full evidence=%+v pending=%+v nodes=%+v", incremental.Evidence, incremental.Pending, incremental.Nodes, full.Evidence, full.Pending, full.Nodes)
	}
	eventCount := len(fixture.store.events)
	if eventCount != len(fixture.store.lastCommit.Batch.Events) || incremental.Metadata.AsOfEventSequence != int64(eventCount) || len(incremental.Timeline) != eventCount || full.Metadata.AsOfEventSequence != int64(eventCount) || len(full.Timeline) != eventCount {
		t.Fatalf("canonical event accounting: captured=%d drafts=%d incremental_as_of=%d incremental_timeline=%d full_as_of=%d full_timeline=%d", eventCount, len(fixture.store.lastCommit.Batch.Events), incremental.Metadata.AsOfEventSequence, len(incremental.Timeline), full.Metadata.AsOfEventSequence, len(full.Timeline))
	}
	session, ok := full.Sessions[fixture.store.session.ID]
	if !ok || session.Session.State != tutoring.StateFeedback {
		t.Fatalf("full replay session=%+v found=%v", session.Session, ok)
	}
	target := fixture.store.activity.TargetNodeRevisionID
	node, nodeFound := full.Nodes[target]
	if !nodeFound {
		t.Fatalf("full replay missing target node %q", target)
	}
	if disposition == DispositionAccepted {
		if len(full.Evidence) != 1 || len(full.Pending) != 0 || node.Mastery.ValidEvidenceCount != 1 || node.Mastery.State != MasteryLearning {
			t.Fatalf("accepted replay evidence=%d pending=%d node=%+v", len(full.Evidence), len(full.Pending), node)
		}
	} else if len(full.Evidence) != 0 || len(full.Pending) != 1 || node.Mastery.ValidEvidenceCount != 0 || node.Mastery.State != MasteryProvisional {
		t.Fatalf("provisional replay evidence=%d pending=%d node=%+v", len(full.Evidence), len(full.Pending), node)
	}

	if eventCount < 2 {
		t.Fatalf("projection sensitivity requires multiple canonical events, got %d", eventCount)
	}
	missingEvent := strictFullReplay(t, fixture.store.events[:eventCount-1])
	if strictProjectionFingerprint(t, missingEvent) == incrementalFingerprint {
		t.Fatal("projection fingerprint did not detect a missing event")
	}
	changedState := strictFullReplay(t, fixture.store.events)
	changedSession := changedState.Sessions[fixture.store.session.ID]
	changedSession.Session.State = tutoring.StateEvaluating
	changedState.Sessions[fixture.store.session.ID] = changedSession
	if strictProjectionFingerprint(t, changedState) == fullFingerprint {
		t.Fatal("projection fingerprint did not detect a changed tutoring state")
	}
}

func TestStrictFakeAssessmentModelFailureAndDecodeMatrix(t *testing.T) {
	baseActivity, baseAttempt, _ := assessmentFixture()
	answerLength := len(baseAttempt.Answer)
	knowledgeLength := len(baseActivity.References[0].Slice)
	tests := []struct {
		name                  string
		results               func(*testing.T) []proposalModelResult
		configure             func(*strictAssessmentConfig)
		wantCode              string
		wantCategory          string
		wantReason            string
		wantResolvedKnowledge string
		wantRawKnowledgeRef   string
		wantAttempts          []string
		wantCalls             int
	}{
		{name: "timeout exhausted on second attempt", results: func(*testing.T) []proposalModelResult {
			return []proposalModelResult{{err: proposalModelError("timeout")}, {err: proposalModelError("timeout")}}
		}, wantCode: CodeModelUnavailable, wantCategory: "timeout", wantAttempts: []string{"timeout", "timeout"}, wantCalls: 2},
		{name: "rate limit exhausted on second attempt", results: func(*testing.T) []proposalModelResult {
			return []proposalModelResult{{err: proposalModelError("rate_limited")}, {err: proposalModelError("rate_limited")}}
		}, wantCode: CodeModelUnavailable, wantCategory: "rate_limited", wantAttempts: []string{"rate_limited", "rate_limited"}, wantCalls: 2},
		{name: "non transient model category is not retried", results: func(*testing.T) []proposalModelResult {
			return []proposalModelResult{{err: proposalModelError("invalid_response")}}
		}, wantCode: CodeProposalRejected, wantCategory: "invalid_response", wantAttempts: []string{"invalid_response"}, wantCalls: 1},
		{name: "malformed JSON", results: func(*testing.T) []proposalModelResult {
			return []proposalModelResult{{raw: json.RawMessage(`{"assessment":`)}}
		}, wantCode: CodeProposalRejected, wantCategory: "schema_mismatch", wantAttempts: []string{"schema_mismatch"}, wantCalls: 1},
		{name: "recursive schema mismatch", results: func(*testing.T) []proposalModelResult {
			return []proposalModelResult{{raw: json.RawMessage(`{"assessment":{"items":[],"rubric_complete":false,"confidence":0,"risk_flags":[],"invented":true}}`)}}
		}, wantCode: CodeProposalRejected, wantCategory: "schema_mismatch", wantAttempts: []string{"schema_mismatch"}, wantCalls: 1},
		{name: "unknown risk is a domain failure and is not retried", results: func(t *testing.T) []proposalModelResult {
			return []proposalModelResult{{raw: strictAssessmentRaw(t, func(value *AssessmentArtifact) { value.RiskFlags = []RiskFlag{"invented_risk"} })}}
		}, wantCode: CodeProposalRejected, wantCategory: "domain_invalid", wantAttempts: []string{"domain_invalid"}, wantCalls: 1},
		{name: "unknown rubric item", results: func(t *testing.T) []proposalModelResult {
			return []proposalModelResult{{raw: strictAssessmentRaw(t, func(value *AssessmentArtifact) { value.Items[0].RubricItemID = "unknown-rubric" })}}
		}, wantCode: CodeProposalRejected, wantCategory: "domain_invalid", wantAttempts: []string{"domain_invalid"}, wantCalls: 1},
		{name: "duplicate rubric item", results: func(t *testing.T) []proposalModelResult {
			return []proposalModelResult{{raw: strictAssessmentRaw(t, func(value *AssessmentArtifact) { value.Items = append(value.Items, value.Items[0]) })}}
		}, wantCode: CodeProposalRejected, wantCategory: "domain_invalid", wantAttempts: []string{"domain_invalid"}, wantCalls: 1},
		{name: "unknown knowledge reference", results: func(t *testing.T) []proposalModelResult {
			return []proposalModelResult{{raw: strictAssessmentRaw(t, func(value *AssessmentArtifact) { value.Items[0].KnowledgeReferenceID = "unknown-node-revision" })}}
		}, wantCode: CodeProposalRejected, wantCategory: "domain_invalid", wantReason: "node_not_allowed", wantRawKnowledgeRef: "unknown-node-revision", wantAttempts: []string{"domain_invalid"}, wantCalls: 1},
		{name: "cross knowledge revision", configure: func(config *strictAssessmentConfig) {
			config.requestNodeIDs = append(config.requestNodeIDs, "cross-node-revision")
			config.resolver = &strictAssessmentResolver{crossRevisionNode: "cross-node-revision"}
		}, results: func(t *testing.T) []proposalModelResult {
			return []proposalModelResult{{raw: strictAssessmentRaw(t, func(value *AssessmentArtifact) {
				value.Items[0].KnowledgeReferenceID = "cross-node-revision"
			})}}
		}, wantCode: CodeProposalRejected, wantCategory: "domain_invalid", wantReason: "knowledge_reference_mismatch", wantResolvedKnowledge: "other-knowledge-revision", wantRawKnowledgeRef: "cross-node-revision", wantAttempts: []string{"domain_invalid"}, wantCalls: 1},
		{name: "answer quote range out of bounds", results: func(t *testing.T) []proposalModelResult {
			return []proposalModelResult{{raw: strictAssessmentRaw(t, func(value *AssessmentArtifact) { value.Items[0].AnswerRange.End = answerLength + 1 })}}
		}, wantCode: CodeProposalRejected, wantCategory: "domain_invalid", wantAttempts: []string{"domain_invalid"}, wantCalls: 1},
		{name: "answer quote hash mismatch", results: func(t *testing.T) []proposalModelResult {
			return []proposalModelResult{{raw: strictAssessmentRaw(t, func(value *AssessmentArtifact) { value.Items[0].AnswerQuoteSHA256 = strings.Repeat("0", 64) })}}
		}, wantCode: CodeProposalRejected, wantCategory: "domain_invalid", wantAttempts: []string{"domain_invalid"}, wantCalls: 1},
		{name: "knowledge quote range out of bounds", results: func(t *testing.T) []proposalModelResult {
			return []proposalModelResult{{raw: strictAssessmentRaw(t, func(value *AssessmentArtifact) { value.Items[0].KnowledgeRange.End = knowledgeLength + 1 })}}
		}, wantCode: CodeProposalRejected, wantCategory: "domain_invalid", wantAttempts: []string{"domain_invalid"}, wantCalls: 1},
		{name: "knowledge quote hash mismatch", results: func(t *testing.T) []proposalModelResult {
			return []proposalModelResult{{raw: strictAssessmentRaw(t, func(value *AssessmentArtifact) { value.Items[0].KnowledgeQuoteSHA256 = strings.Repeat("0", 64) })}}
		}, wantCode: CodeProposalRejected, wantCategory: "domain_invalid", wantAttempts: []string{"domain_invalid"}, wantCalls: 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			results := test.results(t)
			fixture := newStrictAssessmentFixture(t, results, test.configure)
			beforeFingerprint := strictProjectionFingerprint(t, fixture.store.projection)
			beforeActivity, activityHashErr := HashJSON(fixture.store.activity)
			if activityHashErr != nil {
				t.Fatal(activityHashErr)
			}
			beforeState, stateHashErr := HashJSON(fixture.store.session)
			if stateHashErr != nil {
				t.Fatal(stateHashErr)
			}
			if test.wantRawKnowledgeRef != "" {
				var output struct {
					Assessment struct {
						Items []AssessmentItem `json:"items"`
					} `json:"assessment"`
				}
				if err := json.Unmarshal(results[0].raw, &output); err != nil {
					t.Fatal(err)
				}
				if len(output.Assessment.Items) != 1 || output.Assessment.Items[0].KnowledgeReferenceID != test.wantRawKnowledgeRef {
					t.Fatalf("model raw knowledge reference=%q want=%q", output.Assessment.Items[0].KnowledgeReferenceID, test.wantRawKnowledgeRef)
				}
			}
			_, err := fixture.service.Propose(context.Background(), "strict-device", fixture.request)
			if ErrorCode(err) != test.wantCode {
				t.Fatalf("Propose error=%v code=%q want=%q", err, ErrorCode(err), test.wantCode)
			}
			if test.wantReason != "" && !strictErrorHasReason(err, test.wantReason) {
				t.Fatalf("Propose error=%v does not contain domain reason %q", err, test.wantReason)
			}
			if test.wantResolvedKnowledge != "" {
				resolver, ok := fixture.resolver.(*strictAssessmentResolver)
				if !ok || resolver.resolved["cross-node-revision"] != test.wantResolvedKnowledge {
					t.Fatalf("cross reference resolver result=%v want=%q", resolver.resolved, test.wantResolvedKnowledge)
				}
			}
			if fixture.model.calls != test.wantCalls || fixture.repo.completed != nil || fixture.repo.failedCategory != test.wantCategory {
				t.Fatalf("calls=%d completed=%v failure=%q", fixture.model.calls, fixture.repo.completed != nil, fixture.repo.failedCategory)
			}
			if got, want := strings.Join(fixture.repo.failedAttempts, ","), strings.Join(test.wantAttempts, ","); got != want {
				t.Fatalf("durable attempts=%v want=%v", fixture.repo.failedAttempts, test.wantAttempts)
			}
			if fixture.store.commits != 0 || fixture.store.archives != 0 || len(fixture.store.lastCommit.Batch.Events) != 0 || len(fixture.store.evidence) != 0 {
				t.Fatalf("hard failure mutated authority: commits=%d archives=%d events=%d evidence=%d", fixture.store.commits, fixture.store.archives, len(fixture.store.lastCommit.Batch.Events), len(fixture.store.evidence))
			}
			if len(fixture.store.events) != 0 || fixture.store.projection.Metadata.AsOfEventSequence != 0 || len(fixture.store.projection.Timeline) != 0 {
				t.Fatalf("hard failure changed incremental projection: events=%d as_of=%d timeline=%d", len(fixture.store.events), fixture.store.projection.Metadata.AsOfEventSequence, len(fixture.store.projection.Timeline))
			}
			if afterActivity, hashErr := HashJSON(fixture.store.activity); hashErr != nil || afterActivity != beforeActivity {
				t.Fatalf("proposal failure changed frozen activity: before=%s after=%s err=%v", beforeActivity, afterActivity, hashErr)
			}
			if afterState, hashErr := HashJSON(fixture.store.session); hashErr != nil || afterState != beforeState {
				t.Fatalf("hard failure changed tutoring state: before=%s after=%s err=%v", beforeState, afterState, hashErr)
			}
			if after := strictProjectionFingerprint(t, fixture.store.projection); after != beforeFingerprint {
				t.Fatalf("hard failure projection fingerprint changed: before=%s after=%s", beforeFingerprint, after)
			}
		})
	}
}

func TestStrictFakeAssessmentEndToEndAcceptanceMatrix(t *testing.T) {
	type matrixCase struct {
		name        string
		mutate      func(*AssessmentArtifact)
		firstError  error
		disposition Disposition
	}
	tests := []matrixCase{
		{name: "accepted open assessment", disposition: DispositionAccepted},
		{name: "timeout then success records second attempt categories", firstError: proposalModelError("timeout"), disposition: DispositionAccepted},
		{name: "rate limit then success records second attempt categories", firstError: proposalModelError("rate_limited"), disposition: DispositionAccepted},
		{name: "low confidence", mutate: func(value *AssessmentArtifact) { value.Confidence = 849 }, disposition: DispositionProvisional},
		{name: "missing answer quote support", mutate: func(value *AssessmentArtifact) {
			value.Items[0].AnswerRange = SourceRange{}
			value.Items[0].AnswerQuote = ""
			value.Items[0].AnswerQuoteSHA256 = ""
		}, disposition: DispositionProvisional},
		{name: "missing knowledge support", mutate: func(value *AssessmentArtifact) {
			value.Items[0].KnowledgeReferenceID = ""
			value.Items[0].KnowledgeRange = SourceRange{}
			value.Items[0].KnowledgeQuote = ""
			value.Items[0].KnowledgeQuoteSHA256 = ""
		}, disposition: DispositionProvisional},
		{name: "insufficient knowledge quote support", mutate: func(value *AssessmentArtifact) {
			value.Items[0].KnowledgeRange = SourceRange{}
			value.Items[0].KnowledgeQuote = ""
			value.Items[0].KnowledgeQuoteSHA256 = ""
		}, disposition: DispositionProvisional},
	}
	for _, risk := range []RiskFlag{RiskIncompleteRubric, RiskInsufficientAnswerEvidence, RiskInsufficientKnowledgeSupport, RiskConflictingEvidence, RiskAmbiguousRubric, RiskUnsafeContent, RiskSchemaRepaired, RiskStaleContext, RiskRetryExhausted} {
		risk := risk
		tests = append(tests, matrixCase{name: "known risk " + string(risk), mutate: func(value *AssessmentArtifact) { value.RiskFlags = []RiskFlag{risk} }, disposition: DispositionProvisional})
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			results := []proposalModelResult{{raw: strictAssessmentRaw(t, test.mutate)}}
			wantCategories := []string{"success"}
			if test.firstError != nil {
				results = append([]proposalModelResult{{err: test.firstError}}, results...)
				wantCategories = []string{modelCategory(test.firstError), "success"}
			}
			fixture := newStrictAssessmentFixture(t, results, nil)
			proposal, err := fixture.service.Propose(context.Background(), "strict-device", fixture.request)
			if err != nil {
				t.Fatal(err)
			}
			if fixture.model.calls != len(results) || fixture.repo.completed == nil || fixture.store.commits != 0 {
				t.Fatalf("proposal boundary calls=%d completed=%v commits=%d", fixture.model.calls, fixture.repo.completed != nil, fixture.store.commits)
			}
			frozenHash, hashErr := HashJSON(proposal.FrozenRequest)
			if hashErr != nil || proposal.SchemaVersion != ProposalSchemaVersion || proposal.ID != "10000000-0000-4000-8000-000000000010" || proposal.InputHash != frozenHash || proposal.ModelID != "strict-fake" || proposal.PromptRevision != "test-prompt" || proposal.Assessment == nil {
				t.Fatalf("proposal identity/schema/metadata invalid: proposal=%+v hashErr=%v", proposal, hashErr)
			}
			if got, want := strings.Join(proposal.AttemptCategories, ","), strings.Join(wantCategories, ","); got != want {
				t.Fatalf("proposal attempts=%v want=%v", proposal.AttemptCategories, wantCategories)
			}
			assessment := proposal.Assessment
			if assessment.ID != "10000000-0000-4000-8000-000000000011" || assessment.ID == proposal.ID || assessment.ProposalInputHash != proposal.InputHash || assessment.Attempts != len(wantCategories) || assessment.ModelID != proposal.ModelID || assessment.PromptRevision != proposal.PromptRevision {
				t.Fatalf("assessment identity/hash/metadata invalid: %+v", assessment)
			}

			fixture.store.proposal = proposal
			command := ActionCommand{Operation: coordinatorOperation("81000000-0000-4000-8000-000000000010", fixture.store.session.ID, fixture.store.session.AggregateVer), Action: tutoring.ActionRecordAssessment, ProposalID: proposal.ID}
			if _, err := fixture.service.ApplyAction(context.Background(), "strict-device", fixture.store.session.ID, command); err != nil {
				t.Fatal(err)
			}
			batch := fixture.store.lastCommit.Batch
			if fixture.store.commits != 1 || batch.Assessment == nil || batch.Assessment.ID != assessment.ID || batch.Disposition != test.disposition || batch.Session == nil || batch.Session.State != tutoring.StateFeedback {
				t.Fatalf("assessment commit=%+v commits=%d", batch, fixture.store.commits)
			}
			if len(batch.Events) < 3 || batch.Events[0].Type != EventAssessmentRecorded || batch.Events[len(batch.Events)-1].Type != EventTutoringStateChanged {
				t.Fatalf("assessment events=%v", batch.Events)
			}
			if test.disposition == DispositionAccepted {
				if len(batch.Evidence) != 1 || batch.Evidence[0].NodeRevisionID != fixture.store.activity.TargetNodeRevisionID || batch.Events[1].Type != EventAssessmentAccepted || batch.Events[2].Type != EventEvidenceAccepted {
					t.Fatalf("accepted evidence/events=%+v / %v", batch.Evidence, batch.Events)
				}
			} else if len(batch.Evidence) != 0 || batch.Events[1].Type != EventAssessmentMarkedProvisional {
				t.Fatalf("provisional evidence/events=%+v / %v", batch.Evidence, batch.Events)
			}
			assertStrictProjectionConvergence(t, fixture, test.disposition)
		})
	}
}

func TestStrictFakeAssessmentRejectsStaleProposalBeforeApply(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*strictAssessmentFixture, *ProposalArtifact)
	}{
		{name: "frozen input hash changed", mutate: func(_ *strictAssessmentFixture, proposal *ProposalArtifact) {
			proposal.InputHash = strings.Repeat("0", 64)
		}},
		{name: "frozen context changed", mutate: func(fixture *strictAssessmentFixture, _ *ProposalArtifact) { fixture.store.session.AggregateVer++ }},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newStrictAssessmentFixture(t, []proposalModelResult{{raw: strictAssessmentRaw(t, nil)}}, nil)
			proposal, err := fixture.service.Propose(context.Background(), "strict-device", fixture.request)
			if err != nil {
				t.Fatal(err)
			}
			test.mutate(&fixture, &proposal)
			fixture.store.proposal = proposal
			before := strictProjectionFingerprint(t, fixture.store.projection)
			beforeState, stateHashErr := HashJSON(fixture.store.session)
			if stateHashErr != nil {
				t.Fatal(stateHashErr)
			}
			command := ActionCommand{Operation: coordinatorOperation("81000000-0000-4000-8000-000000000020", fixture.store.session.ID, fixture.store.session.AggregateVer), Action: tutoring.ActionRecordAssessment, ProposalID: proposal.ID}
			if _, err := fixture.service.ApplyAction(context.Background(), "strict-device", fixture.store.session.ID, command); ErrorCode(err) != CodeStaleProposal {
				t.Fatalf("ApplyAction error=%v code=%q", err, ErrorCode(err))
			}
			if fixture.store.commits != 0 || len(fixture.store.lastCommit.Batch.Events) != 0 || len(fixture.store.evidence) != 0 {
				t.Fatalf("stale apply mutated authority: commits=%d events=%d evidence=%d", fixture.store.commits, len(fixture.store.lastCommit.Batch.Events), len(fixture.store.evidence))
			}
			if len(fixture.store.events) != 0 || fixture.store.projection.Metadata.AsOfEventSequence != 0 || len(fixture.store.projection.Timeline) != 0 {
				t.Fatalf("stale apply changed incremental projection: events=%d as_of=%d timeline=%d", len(fixture.store.events), fixture.store.projection.Metadata.AsOfEventSequence, len(fixture.store.projection.Timeline))
			}
			if afterState, hashErr := HashJSON(fixture.store.session); hashErr != nil || afterState != beforeState {
				t.Fatalf("stale apply changed tutoring state: before=%s after=%s err=%v", beforeState, afterState, hashErr)
			}
			if after := strictProjectionFingerprint(t, fixture.store.projection); after != before {
				t.Fatalf("stale apply projection fingerprint changed: before=%s after=%s", before, after)
			}
		})
	}
}
