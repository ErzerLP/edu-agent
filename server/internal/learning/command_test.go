package learning

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/edu-agent/edu-agent/server/internal/tutoring"
)

func TestCoordinatorCommitsStateMachineTransitionBatch(t *testing.T) {
	sessionID := "10000000-0000-4000-8000-000000000020"
	store := &proposalTestStore{session: tutoring.Session{ID: sessionID, State: tutoring.StateGoalReady, AggregateVer: 1, Context: tutoring.FocusContext{GoalRevisionID: "goal-revision"}}}
	service := newProposalTestService(t, store, &proposalTestRepository{}, nil)
	command := ActionCommand{Operation: OperationEnvelope{OperationID: "10000000-0000-4000-8000-000000000021", PayloadSchemaVersion: 1, AggregateType: "session", AggregateID: sessionID, ExpectedVersion: 1, Payload: json.RawMessage(`{"action":"start_diagnostic"}`)}, Action: tutoring.ActionStartDiagnostic}
	if _, err := service.ApplyAction(context.Background(), "90000000-0000-4000-8000-000000000001", sessionID, command); err != nil {
		t.Fatal(err)
	}
	batch := store.lastCommit.Batch
	if store.commits != 1 || batch.Session == nil || batch.Session.State != tutoring.StateDiagnostic || batch.TutoringState != string(tutoring.StateDiagnostic) {
		t.Fatalf("transition batch = %#v", batch)
	}
	if len(batch.Events) != 1 || batch.Events[0].Type != EventTutoringStateChanged {
		t.Fatalf("transition events = %#v", batch.Events)
	}
	if len(store.lastCommit.Expectations) != 1 || store.lastCommit.Expectations[0].ExpectedVersion != 1 || store.lastCommit.DeviceID != "90000000-0000-4000-8000-000000000001" {
		t.Fatalf("commit boundary = %#v", store.lastCommit)
	}
}

func TestCoordinatorRejectsEveryProvisionalFeedbackExit(t *testing.T) {
	sessionID := "10000000-0000-4000-8000-000000000080"
	attemptID := "10000000-0000-4000-8000-000000000081"
	assessmentID := "10000000-0000-4000-8000-000000000082"
	for index, action := range []tutoring.Action{tutoring.ActionAcknowledgeFeedback, tutoring.ActionEndActivity, tutoring.ActionSwitchGoal} {
		t.Run(string(action), func(t *testing.T) {
			store := &proposalTestStore{
				session:    tutoring.Session{ID: sessionID, State: tutoring.StateFeedback, AggregateVer: 4, Context: tutoring.FocusContext{AttemptID: &attemptID}},
				assessment: AssessmentArtifact{ID: assessmentID, SessionID: sessionID, AttemptID: attemptID},
				decision:   AssessmentDecision{ID: "decision", AssessmentID: assessmentID, Version: 1, Disposition: DispositionProvisional},
			}
			service := newProposalTestService(t, store, &proposalTestRepository{}, nil)
			command := ActionCommand{
				Operation:      OperationEnvelope{OperationID: fmt.Sprintf("10000000-0000-4000-8000-%012d", 90+index), PayloadSchemaVersion: 1, AggregateType: "session", AggregateID: sessionID, ExpectedVersion: 4, Payload: json.RawMessage(`{}`)},
				Action:         action,
				GoalRevisionID: "10000000-0000-4000-8000-000000000083",
			}
			_, err := service.ApplyAction(context.Background(), "90000000-0000-4000-8000-000000000001", sessionID, command)
			var domain *Error
			if !errors.As(err, &domain) || domain.Code != CodeAssessmentDispositionConflict || domain.CurrentDisposition != string(DispositionProvisional) || store.commits != 0 {
				t.Fatalf("action=%s err=%v commits=%d", action, err, store.commits)
			}
		})
	}
}

func TestCoordinatorVoidsAssessmentAndInvalidatesEvidence(t *testing.T) {
	sessionID := "10000000-0000-4000-8000-000000000030"
	evidenceID := "10000000-0000-4000-8000-000000000031"
	activity := Activity{ID: "activity", Revision: 1, SessionID: sessionID}
	attempt := Attempt{ID: "attempt", SessionID: sessionID, ActivityID: activity.ID, ActivityRevision: activity.Revision}
	store := &proposalTestStore{session: assessmentFeedbackSession(activity, attempt, 4), assessment: AssessmentArtifact{ID: "10000000-0000-4000-8000-000000000032", SessionID: sessionID, ActivityID: activity.ID, AttemptID: attempt.ID, ActivityRevision: activity.Revision}, decision: AssessmentDecision{ID: "decision", AssessmentID: "10000000-0000-4000-8000-000000000032", Version: 1, Disposition: DispositionAccepted, ProducedEvidenceID: &evidenceID}, activity: activity, attempt: attempt}
	service := newProposalTestService(t, store, &proposalTestRepository{}, nil)
	command := AssessmentDecisionCommand{Operation: OperationEnvelope{OperationID: "10000000-0000-4000-8000-000000000033", PayloadSchemaVersion: 1, AggregateType: "session", AggregateID: sessionID, ExpectedVersion: 4, Payload: json.RawMessage(`{"kind":"void"}`)}, Kind: "void", ExpectedDispositionVersion: 1, Reason: "invalid source"}
	if _, err := service.Decide(context.Background(), "90000000-0000-4000-8000-000000000001", store.assessment.ID, command); err != nil {
		t.Fatal(err)
	}
	batch := store.lastCommit.Batch
	if len(batch.Invalidations) != 1 || batch.Invalidations[0].EvidenceID != evidenceID || batch.Disposition != DispositionVoided {
		t.Fatalf("compensation batch = %#v", batch)
	}
	if batch.Session == nil || batch.Session.ID != sessionID || batch.Session.AggregateVer != 4 {
		t.Fatalf("decision omitted session authority snapshot: %#v", batch.Session)
	}
	if len(batch.Events) != 3 || batch.Events[0].Type != EventAssessmentVoided || batch.Events[1].Type != EventEvidenceInvalidated || batch.Events[2].Type != EventTutoringStateChanged {
		t.Fatalf("compensation events = %#v", batch.Events)
	}
}

func TestCoordinatorReturnsCurrentDispositionForReplacementValidationConflict(t *testing.T) {
	activity, attempt, artifact := assessmentFixture()
	sessionID := "10000000-0000-4000-8000-000000000034"
	activity.ID = "10000000-0000-4000-8000-000000000035"
	activity.SessionID = sessionID
	activity.Type = ActivityObjective
	attempt.ID = "10000000-0000-4000-8000-000000000036"
	attempt.SessionID = sessionID
	attempt.ActivityID = activity.ID
	artifact.ID = "10000000-0000-4000-8000-000000000037"
	artifact.SessionID = sessionID
	artifact.ActivityID = activity.ID
	artifact.AttemptID = attempt.ID
	current := AssessmentDecision{ID: "10000000-0000-4000-8000-000000000038", AssessmentID: artifact.ID, Version: 1, Disposition: DispositionAccepted, Items: artifact.Items}
	store := &proposalTestStore{
		session: assessmentFeedbackSession(activity, attempt, 4), activity: activity, attempt: attempt,
		assessment: artifact, decision: current,
	}
	service := newProposalTestService(t, store, &proposalTestRepository{}, nil)
	command := AssessmentDecisionCommand{
		Operation:                  coordinatorOperation("10000000-0000-4000-8000-000000000039", sessionID, 4),
		Kind:                       "override",
		ExpectedDispositionVersion: 1,
		Reason:                     "manual correction",
		Items:                      artifact.Items,
	}
	_, err := service.Decide(context.Background(), "90000000-0000-4000-8000-000000000001", artifact.ID, command)
	var domain *Error
	if !errors.As(err, &domain) || domain.Code != CodeAssessmentDispositionConflict || domain.CurrentDisposition != string(DispositionAccepted) || store.commits != 0 {
		t.Fatalf("err=%v commits=%d", err, store.commits)
	}
}

func TestA101DecideRequiresCurrentFeedbackAssessmentChainWithoutCommit(t *testing.T) {
	activity, attempt, artifact := assessmentFixture()
	artifact.SessionID = "10000000-0000-4000-8000-000000000040"
	activity.SessionID = artifact.SessionID
	attempt.SessionID = artifact.SessionID
	decision := AssessmentDecision{ID: "decision", AssessmentID: artifact.ID, Version: 1, Disposition: DispositionProvisional, Items: artifact.Items}
	for index, test := range []struct {
		name         string
		assessmentID string
		mutate       func(*proposalTestStore)
	}{
		{name: "historical assessment URL", assessmentID: "10000000-0000-4000-8000-000000000041"},
		{name: "session not in feedback", assessmentID: artifact.ID, mutate: func(store *proposalTestStore) { store.session.State = tutoring.StateEvaluating }},
		{name: "attempt assessment ownership", assessmentID: artifact.ID, mutate: func(store *proposalTestStore) { store.assessment.AttemptID = "10000000-0000-4000-8000-000000000042" }},
		{name: "decision assessment ownership", assessmentID: artifact.ID, mutate: func(store *proposalTestStore) { store.decision.AssessmentID = "10000000-0000-4000-8000-000000000043" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := &proposalTestStore{
				session: assessmentFeedbackSession(activity, attempt, 5), activity: activity, attempt: attempt,
				assessment: artifact, decision: decision,
			}
			if test.mutate != nil {
				test.mutate(store)
			}
			service := newProposalTestService(t, store, &proposalTestRepository{}, nil)
			command := AssessmentDecisionCommand{Operation: coordinatorOperation(fmt.Sprintf("10000000-0000-4000-8000-%012d", 50+index), artifact.SessionID, 5), Kind: "confirm", ExpectedDispositionVersion: 1}
			_, err := service.Decide(context.Background(), "90000000-0000-4000-8000-000000000001", test.assessmentID, command)
			if ErrorCode(err) != CodeActivityStateConflict || store.commits != 0 || len(store.lastCommit.Batch.Decisions) != 0 || len(store.lastCommit.Batch.Evidence) != 0 {
				t.Fatalf("err=%v commits=%d batch=%+v", err, store.commits, store.lastCommit.Batch)
			}
		})
	}
}

func TestAuthorityReplayPrecedesMutableStateReads(t *testing.T) {
	sessionID := "10000000-0000-4000-8000-000000000050"
	archived := OperationResult{Status: "succeeded", Replayed: true, Archived: true, AggregateType: "session", AggregateID: sessionID, AggregateVersion: 9}
	store := &proposalTestStore{lookupResult: archived, lookupFound: true}
	service := newProposalTestService(t, store, &proposalTestRepository{}, nil)
	command := ActionCommand{Operation: OperationEnvelope{OperationID: "10000000-0000-4000-8000-000000000051", PayloadSchemaVersion: 1, AggregateType: "session", AggregateID: sessionID, ExpectedVersion: 1, Payload: json.RawMessage(`{}`)}, Action: tutoring.ActionStartDiagnostic}
	result, err := service.ApplyAction(context.Background(), "90000000-0000-4000-8000-000000000001", sessionID, command)
	if err != nil || result.AggregateVersion != 9 || store.loads != 0 || store.commits != 0 {
		t.Fatalf("replay=%+v err=%v loads=%d commits=%d", result, err, store.loads, store.commits)
	}
}

func TestTransitionDraftsUsesFixedCompleteCanonicalBatch(t *testing.T) {
	sessionID := "10000000-0000-4000-8000-000000000060"
	transition := tutoring.Transition{
		Before: tutoring.StateEvaluating, After: tutoring.StateFeedback,
		Events:  []string{string(EventAssessmentRecorded), string(EventTutoringStateChanged)},
		Session: tutoring.Session{ID: sessionID, State: tutoring.StateFeedback, Context: tutoring.FocusContext{GoalRevisionID: "goal"}},
	}
	payloads := map[EventType][]json.RawMessage{
		EventAssessmentRecorded:             {mustJSON(AssessmentArtifact{ID: "assessment"})},
		EventAssessmentAccepted:             {mustJSON(AssessmentProjectionEvent{AssessmentID: "assessment", NodeRevisionID: "node"})},
		EventEvidenceAccepted:               {mustJSON(AcceptedEvidence{ID: "evidence", AssessmentID: "assessment", NodeRevisionID: "node"})},
		EventMisconceptionHypothesisRevised: {mustJSON(MisconceptionHypothesis{ID: "misconception", NodeRevisionID: "node"})},
	}
	events := transitionDrafts(sessionID, transition, payloads)
	want := []EventType{EventAssessmentRecorded, EventAssessmentAccepted, EventEvidenceAccepted, EventMisconceptionHypothesisRevised, EventTutoringStateChanged}
	if len(events) != len(want) {
		t.Fatalf("events=%v", events)
	}
	for index := range want {
		if events[index].Type != want[index] {
			t.Fatalf("event[%d]=%s want %s", index, events[index].Type, want[index])
		}
	}
	var projected SessionProjection
	if err := json.Unmarshal(events[len(events)-1].Payload, &projected); err != nil || projected.Session.ID != sessionID || projected.Session.State != tutoring.StateFeedback || projected.Session.Context.GoalRevisionID != "goal" {
		t.Fatalf("terminal session projection=%+v err=%v", projected, err)
	}
}

func TestFocusEventsCarryReplayableSessionAndFrame(t *testing.T) {
	sessionID := "10000000-0000-4000-8000-000000000070"
	frame := &tutoring.FocusFrame{ID: "frame", SessionID: sessionID, SavedState: tutoring.StateRouteActive, Context: tutoring.FocusContext{GoalRevisionID: "goal", FocusNodeRevisionID: "node"}, SavedAggregateVersion: 3}
	transition := tutoring.Transition{Before: tutoring.StateRouteActive, Intermediate: []tutoring.State{tutoring.StateFocusSuspended}, After: tutoring.StateFreeQuestion, Events: []string{string(EventFocusSuspended), string(EventFreeQuestionAsked), string(EventTutoringStateChanged)}, Session: tutoring.Session{ID: sessionID, State: tutoring.StateFreeQuestion, Context: frame.Context, ActiveFrame: frame}}
	events := transitionDrafts(sessionID, transition, map[EventType][]json.RawMessage{EventFreeQuestionAsked: {mustJSON(tutoring.FreeQuestion{ID: "question", SessionID: sessionID, FocusFrameID: frame.ID})}})
	for _, index := range []int{0, 2} {
		var projected SessionProjection
		if err := json.Unmarshal(events[index].Payload, &projected); err != nil || projected.Session.ActiveFrame == nil || projected.Session.ActiveFrame.ID != frame.ID || projected.Session.Context.FocusNodeRevisionID != "node" {
			t.Fatalf("focus event[%d] projection=%+v err=%v", index, projected, err)
		}
	}
}

func TestAssessmentAuthorityComesFromFrozenActivityReference(t *testing.T) {
	frozen := KnowledgeReference{
		KnowledgeRevisionID: "knowledge", NodeID: "node", NodeRevisionID: "node-revision",
		DocumentRevisionID: "document-revision", Range: SourceRange{Start: 2, End: 8},
		Slice: "source", SliceSHA256: SHA256([]byte("source")),
	}
	activity := Activity{KnowledgeRevisionID: frozen.KnowledgeRevisionID, References: []KnowledgeReference{frozen}}
	artifact := AssessmentArtifact{Items: []AssessmentItem{{RubricItemID: "rubric", KnowledgeReferenceID: frozen.NodeRevisionID}}}
	owners, err := assessmentAuthority(activity, artifact)
	if err != nil {
		t.Fatal(err)
	}
	want := knowledgeOwner(frozen)
	if len(owners) != 1 || owners[0] != want {
		t.Fatalf("assessment owners=%+v want=%+v", owners, want)
	}
}

func TestCoordinatorPreservesProjectionMetadataOnQueries(t *testing.T) {
	metadata := ProjectionMetadata{GenerationID: "10000000-0000-4000-8000-000000000040", AsOfEventSequence: 42, ProjectionVersion: ProjectionVersion, MasteryReducerVersion: MasteryReducerVersion}
	store := &proposalTestStore{timeline: TimelinePage{Metadata: metadata, Items: []TimelineItem{}}}
	service := newProposalTestService(t, store, &proposalTestRepository{}, nil)
	page, err := service.Timeline(context.Background(), TimelineQuery{Page: CursorPageRequest{Limit: 10}})
	if err != nil {
		t.Fatal(err)
	}
	if page.Metadata.GenerationID != metadata.GenerationID || page.Metadata.AsOfEventSequence != 42 || page.Metadata.MasteryReducerVersion != MasteryReducerVersion {
		t.Fatalf("query metadata changed: %#v", page.Metadata)
	}
}
