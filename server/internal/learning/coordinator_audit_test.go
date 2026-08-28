package learning

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/tutoring"
)

func coordinatorOperation(id, sessionID string, version int64) OperationEnvelope {
	return OperationEnvelope{OperationID: id, PayloadSchemaVersion: 1, AggregateType: "session", AggregateID: sessionID, ExpectedVersion: version, Payload: json.RawMessage(`{}`)}
}

func assessmentFeedbackSession(activity Activity, attempt Attempt, version int64) tutoring.Session {
	activityID, attemptID := activity.ID, attempt.ID
	return tutoring.Session{
		ID: activity.SessionID, State: tutoring.StateFeedback, AggregateVer: version,
		Context: tutoring.FocusContext{
			GoalRevisionID: activity.GoalRevisionID, RouteRevisionID: activity.RouteRevisionID,
			RouteStepID: activity.RouteStepID, KnowledgeRevisionID: activity.KnowledgeRevisionID,
			FocusNodeRevisionID: activity.TargetNodeRevisionID, ActivityID: &activityID, AttemptID: &attemptID,
		},
	}
}

func frozenSessionRequest(session tutoring.Session, kind ProposalType, knowledgeRevision string, nodes []string) ProposalRequest {
	request := ProposalRequest{
		RequestID: "71000000-0000-4000-8000-000000000001", Type: kind,
		AggregateType: "session", AggregateID: session.ID, AggregateVersion: session.AggregateVer,
		GoalRevisionID: session.Context.GoalRevisionID, RouteRevisionID: session.Context.RouteRevisionID,
		RouteStepID: session.Context.RouteStepID, FocusNodeRevisionID: session.Context.FocusNodeRevisionID,
		KnowledgeRevisionID: knowledgeRevision, NodeRevisionIDs: append([]string(nil), nodes...),
		TutoringState: string(session.State), Input: json.RawMessage(`{"frozen":true}`),
	}
	if session.Context.ActivityID != nil {
		request.ActivityID = *session.Context.ActivityID
	}
	if session.Context.AttemptID != nil {
		request.AttemptID = *session.Context.AttemptID
	}
	if session.ActiveFrame != nil {
		request.FocusFrameID = session.ActiveFrame.ID
	}
	return request
}

func frozenRouteProposal(session tutoring.Session, knowledgeRevision string, nodes ...string) ProposalArtifact {
	request := frozenSessionRequest(session, ProposalRoute, knowledgeRevision, nodes)
	hash, _ := HashJSON(request)
	artifact := ProposalArtifact{
		ID: "71000000-0000-4000-8000-000000000002", SchemaVersion: ProposalSchemaVersion,
		InputHash: hash, Type: ProposalRoute, AggregateType: "session", AggregateID: session.ID,
		AggregateVersion: session.AggregateVer, GoalRevisionID: request.GoalRevisionID,
		RouteRevisionID: request.RouteRevisionID, KnowledgeRevisionID: knowledgeRevision,
		FrozenRequest: request, ModelID: "strict-fake", ModelParameters: map[string]any{},
		PromptRevision: TutorPromptRevision, AttemptCategories: []string{"success"},
	}
	for _, node := range nodes {
		artifact.Route = append(artifact.Route, RouteProposalStep{NodeRevisionID: node, TeachingIntent: "teach", CompletionCondition: "pass"})
	}
	return artifact
}

func TestApplyActionMapsPersistedFocusInvalidationWithoutCommit(t *testing.T) {
	sessionID := "72500000-0000-4000-8000-000000000001"
	store := &proposalTestStore{session: tutoring.Session{ID: sessionID, State: tutoring.StateRouteActive, AggregateVer: 7, FocusFrameInvalidated: true}}
	service := newProposalTestService(t, store, &proposalTestRepository{}, nil)
	command := ActionCommand{Operation: coordinatorOperation("72500000-0000-4000-8000-000000000002", sessionID, 7), Action: tutoring.ActionResumeFocus}
	if _, err := service.ApplyAction(context.Background(), "72500000-0000-4000-8000-000000000099", sessionID, command); ErrorCode(err) != CodeFocusFrameInvalidated {
		t.Fatalf("resume error=%v code=%q", err, ErrorCode(err))
	}
	if store.commits != 0 || store.archives != 1 {
		t.Fatalf("persisted invalidation changed commits=%d archives=%d", store.commits, store.archives)
	}
}

func TestCoordinatorObjectiveAssessmentWorksWithoutModelAndRevealedAnswerNeverCreatesEvidence(t *testing.T) {
	sessionID := "72000000-0000-4000-8000-000000000001"
	activityID := "72000000-0000-4000-8000-000000000002"
	attemptID := "72000000-0000-4000-8000-000000000003"
	ref, _ := (proposalResolver{}).Resolve(context.Background(), "knowledge", "node")
	activity := Activity{
		ID: activityID, Revision: 1, SessionID: sessionID, GoalRevisionID: "goal", RouteRevisionID: "route", RouteStepID: "step", KnowledgeRevisionID: "knowledge", TargetNodeID: "node", TargetNodeRevisionID: "node",
		Type: ActivityObjective, Rubric: Rubric{Revision: "r1", Items: []RubricItem{{ID: "item", Criterion: "correct"}}, ObjectiveRule: &ObjectiveRule{AcceptedAnswers: []string{"Paris"}, TrimSpace: true}},
		AllowedHelp: []HelpLevel{HelpNone, HelpAnswerRevealed}, References: []KnowledgeReference{ref},
	}
	baseSession := tutoring.Session{ID: sessionID, State: tutoring.StateEvaluating, AggregateVer: 4, Context: tutoring.FocusContext{GoalRevisionID: "goal", RouteRevisionID: "route", RouteStepID: "step", KnowledgeRevisionID: "knowledge", FocusNodeRevisionID: "node", ActivityID: &activityID, AttemptID: &attemptID}}
	for _, test := range []struct {
		name            string
		help            HelpLevel
		wantEvidence    int
		wantDisposition Disposition
	}{
		{name: "deterministic accepted", help: HelpNone, wantEvidence: 1, wantDisposition: DispositionAccepted},
		{name: "answer revealed provisional", help: HelpAnswerRevealed, wantEvidence: 0, wantDisposition: DispositionProvisional},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := &proposalTestStore{session: baseSession, activity: activity, attempt: Attempt{ID: attemptID, SessionID: sessionID, ActivityID: activityID, ActivityRevision: 1, Answer: " Paris ", Help: test.help}}
			service := newProposalTestService(t, store, &proposalTestRepository{}, nil)
			command := ActionCommand{Operation: coordinatorOperation("72000000-0000-4000-8000-000000000010", sessionID, 4), Action: tutoring.ActionRecordAssessment}
			if _, err := service.ApplyAction(context.Background(), "72000000-0000-4000-8000-000000000099", sessionID, command); err != nil {
				t.Fatal(err)
			}
			batch := store.lastCommit.Batch
			if batch.Assessment == nil || batch.Assessment.ModelID != "deterministic-objective" || batch.Disposition != test.wantDisposition || len(batch.Evidence) != test.wantEvidence {
				t.Fatalf("objective batch=%+v", batch)
			}
			if test.wantEvidence == 1 {
				owner, ok := batch.Authority.Evidence[batch.Evidence[0].ID]
				if !ok || owner.SessionID != sessionID || owner.KnowledgeRevisionID != ref.KnowledgeRevisionID || owner.NodeID != ref.NodeID || owner.NodeRevisionID != ref.NodeRevisionID || owner.DocumentRevisionID != ref.DocumentRevisionID {
					t.Fatalf("evidence authority=%+v frozen_reference=%+v", owner, ref)
				}
			}
			if test.help == HelpAnswerRevealed && len(batch.Events) > 1 && batch.Events[1].Type != EventAssessmentMarkedProvisional {
				t.Fatalf("revealed answer events=%v", batch.Events)
			}
		})
	}
}

func TestCoordinatorRejectsHelpOutsideFrozenActivityPolicy(t *testing.T) {
	sessionID, activityID := "72100000-0000-4000-8000-000000000001", "72100000-0000-4000-8000-000000000002"
	store := &proposalTestStore{
		session:  tutoring.Session{ID: sessionID, State: tutoring.StateAwaitingResponse, AggregateVer: 2, Context: tutoring.FocusContext{ActivityID: &activityID}},
		activity: Activity{ID: activityID, Revision: 1, AllowedHelp: []HelpLevel{HelpNone}},
	}
	service := newProposalTestService(t, store, &proposalTestRepository{}, nil)
	command := ActionCommand{Operation: coordinatorOperation("72100000-0000-4000-8000-000000000003", sessionID, 2), Action: tutoring.ActionSubmitAttempt, Answer: "answer", Help: HelpHint}
	if _, err := service.ApplyAction(context.Background(), "72100000-0000-4000-8000-000000000099", sessionID, command); ErrorCode(err) != CodeInvalidRequest || store.commits != 0 {
		t.Fatalf("disallowed help err=%v commits=%d", err, store.commits)
	}
}

func TestCoordinatorRouteLineageAdvanceAndCompletion(t *testing.T) {
	sessionID := "73000000-0000-4000-8000-000000000001"
	deviceID := "73000000-0000-4000-8000-000000000099"
	store := &proposalTestStore{
		session: tutoring.Session{ID: sessionID, State: tutoring.StateDiagnostic, AggregateVer: 3, Context: tutoring.FocusContext{GoalRevisionID: "goal-revision"}},
		goal:    GoalRevision{ID: "goal-revision", GoalID: "goal", Revision: 1, ActorDeviceID: deviceID},
	}
	store.proposal = frozenRouteProposal(store.session, "knowledge", "node-1", "node-2")
	service := newProposalTestService(t, store, &proposalTestRepository{}, nil)
	apply := ActionCommand{Operation: coordinatorOperation("73000000-0000-4000-8000-000000000010", sessionID, 3), Action: tutoring.ActionApplyRoute, ProposalID: store.proposal.ID}
	if _, err := service.ApplyAction(context.Background(), deviceID, sessionID, apply); err != nil {
		t.Fatal(err)
	}
	first := *store.lastCommit.Batch.RouteRevision
	if first.Revision != 1 || first.RouteID == "" || len(first.Steps) != 2 {
		t.Fatalf("first route=%+v", first)
	}
	for _, step := range first.Steps {
		owner, ok := store.lastCommit.Batch.Authority.RouteSteps[step.ID]
		if !ok || owner.KnowledgeRevisionID != "knowledge" || owner.NodeID != step.NodeID || owner.NodeRevisionID != step.NodeRevisionID || owner.DocumentRevisionID != "document" {
			t.Fatalf("route step authority step=%+v owner=%+v", step, owner)
		}
	}

	store.route = first
	store.session = *store.lastCommit.Batch.Session
	store.session.AggregateVer = 5
	store.proposal = frozenRouteProposal(store.session, "knowledge", "node-1", "node-2")
	apply.Operation = coordinatorOperation("73000000-0000-4000-8000-000000000011", sessionID, 5)
	if _, err := service.ApplyAction(context.Background(), deviceID, sessionID, apply); err != nil {
		t.Fatal(err)
	}
	second := *store.lastCommit.Batch.RouteRevision
	if second.RouteID != first.RouteID || second.Revision != 2 {
		t.Fatalf("route lineage first=%+v second=%+v", first, second)
	}

	store.route = second
	store.session = *store.lastCommit.Batch.Session
	store.session.State = tutoring.StateFeedback
	store.session.AggregateVer = 7
	attemptID := "73000000-0000-4000-8000-000000000020"
	assessmentID := "73000000-0000-4000-8000-000000000021"
	store.session.Context.AttemptID = &attemptID
	store.assessment = AssessmentArtifact{ID: assessmentID, SessionID: sessionID, AttemptID: attemptID}
	store.decision = AssessmentDecision{AssessmentID: assessmentID, Disposition: DispositionAccepted}
	ack := ActionCommand{Operation: coordinatorOperation("73000000-0000-4000-8000-000000000012", sessionID, 7), Action: tutoring.ActionAcknowledgeFeedback}
	if _, err := service.ApplyAction(context.Background(), deviceID, sessionID, ack); err != nil {
		t.Fatal(err)
	}
	advanced := *store.lastCommit.Batch.Session
	if advanced.State != tutoring.StateRouteActive || advanced.Context.RouteStepID != second.Steps[1].ID || advanced.Context.FocusNodeRevisionID != second.Steps[1].NodeRevisionID {
		t.Fatalf("advanced session=%+v", advanced)
	}

	store.session = advanced
	store.session.State = tutoring.StateFeedback
	store.session.AggregateVer = 10
	store.session.Context.AttemptID = &attemptID
	ack.Operation = coordinatorOperation("73000000-0000-4000-8000-000000000013", sessionID, 10)
	if _, err := service.ApplyAction(context.Background(), deviceID, sessionID, ack); err != nil {
		t.Fatal(err)
	}
	completed := store.lastCommit.Batch
	if completed.Session.State != tutoring.StateCompleted || len(completed.Events) != 3 || completed.Events[0].Type != EventTutoringStateChanged || completed.Events[1].Type != EventTutoringStateChanged || completed.Events[2].Type != EventLearningCompleted {
		t.Fatalf("route completion=%+v events=%v", completed.Session, completed.Events)
	}
}

func appendCoordinatorEvents(target []LearningEvent, drafts []EventDraft) []LearningEvent {
	for _, item := range drafts {
		sequence := int64(len(target) + 1)
		target = append(target, LearningEvent{EventSequence: sequence, ID: "event", Type: item.Type, SchemaVersion: EventSchemaVersion, AggregateType: item.AggregateType, AggregateID: item.AggregateID, AggregateVersion: sequence, Payload: item.Payload})
	}
	return target
}

func TestCoordinatorFocusAndCompletionReplayEndToEnd(t *testing.T) {
	sessionID := "74000000-0000-4000-8000-000000000001"
	original := tutoring.Session{ID: sessionID, State: tutoring.StateRouteActive, AggregateVer: 3, Context: tutoring.FocusContext{GoalRevisionID: "goal", RouteRevisionID: "route", RouteStepID: "step", KnowledgeRevisionID: "knowledge", FocusNodeRevisionID: "node"}}
	store := &proposalTestStore{session: original}
	service := newProposalTestService(t, store, &proposalTestRepository{}, nil)
	ask := ActionCommand{Operation: coordinatorOperation("74000000-0000-4000-8000-000000000010", sessionID, 3), Action: tutoring.ActionAskFreeQuestion, Question: "Why?"}
	if _, err := service.ApplyAction(context.Background(), "74000000-0000-4000-8000-000000000099", sessionID, ask); err != nil {
		t.Fatal(err)
	}
	events := appendCoordinatorEvents(nil, store.lastCommit.Batch.Events)
	askedProjection, err := Replay(events, NewEventRegistry(), "generation")
	if err != nil {
		t.Fatal(err)
	}
	askedProjected := askedProjection.Sessions[sessionID].Session
	if askedProjected.State != tutoring.StateFreeQuestion || askedProjected.ActiveFrame == nil || askedProjected.ActiveFrame.SavedState != tutoring.StateRouteActive || askedProjected.ActiveFrame.Context != original.Context {
		t.Fatalf("ask replay=%+v", askedProjected)
	}
	asked := *store.lastCommit.Batch.Session
	store.session = asked
	store.session.AggregateVer = int64(len(events))
	resume := ActionCommand{Operation: coordinatorOperation("74000000-0000-4000-8000-000000000011", sessionID, store.session.AggregateVer), Action: tutoring.ActionResumeFocus}
	if _, err := service.ApplyAction(context.Background(), "74000000-0000-4000-8000-000000000099", sessionID, resume); err != nil {
		t.Fatal(err)
	}
	events = appendCoordinatorEvents(events, store.lastCommit.Batch.Events)
	projection, err := Replay(events, NewEventRegistry(), "generation")
	if err != nil {
		t.Fatal(err)
	}
	projected := projection.Sessions[sessionID].Session
	if projected.State != tutoring.StateRouteActive || projected.ActiveFrame != nil || projected.Context != original.Context {
		t.Fatalf("focus replay=%+v", projected)
	}

	store = &proposalTestStore{session: original}
	service = newProposalTestService(t, store, &proposalTestRepository{}, nil)
	complete := ActionCommand{Operation: coordinatorOperation("74000000-0000-4000-8000-000000000012", sessionID, 3), Action: tutoring.ActionCompleteSession}
	if _, err := service.ApplyAction(context.Background(), "74000000-0000-4000-8000-000000000099", sessionID, complete); err != nil {
		t.Fatal(err)
	}
	projection, err = Replay(appendCoordinatorEvents(nil, store.lastCommit.Batch.Events), NewEventRegistry(), "generation")
	if err != nil || projection.Sessions[sessionID].Session.State != tutoring.StateCompleted {
		t.Fatalf("completion replay=%+v err=%v", projection.Sessions[sessionID], err)
	}
}

func TestCoordinatorSwitchGoalUsesTargetHeadWithoutFakeGoalEvent(t *testing.T) {
	sessionID := "75000000-0000-4000-8000-000000000001"
	deviceID := "75000000-0000-4000-8000-000000000099"
	store := &proposalTestStore{
		session: tutoring.Session{ID: sessionID, State: tutoring.StateRouteActive, AggregateVer: 5, Context: tutoring.FocusContext{GoalRevisionID: "old"}},
		goal:    GoalRevision{ID: "target-revision", GoalID: "target-goal", Revision: 2, ActorDeviceID: "other-device"}, aggregateVer: 2,
	}
	service := newProposalTestService(t, store, &proposalTestRepository{}, nil)
	command := ActionCommand{Operation: coordinatorOperation("75000000-0000-4000-8000-000000000010", sessionID, 5), Action: tutoring.ActionSwitchGoal, GoalRevisionID: "target-revision"}
	if _, err := service.ApplyAction(context.Background(), deviceID, sessionID, command); err != nil {
		t.Fatal(err)
	}
	commit := store.lastCommit
	if len(commit.Expectations) != 2 || commit.Batch.Session.Context.GoalRevisionID != "target-revision" || len(commit.Batch.Events) != 1 || commit.Batch.Events[0].Type != EventTutoringStateChanged {
		t.Fatalf("switch-goal commit=%+v", commit)
	}
}

func TestCoordinatorAttachedQuizRequiresOwnedImmutableFreeRecords(t *testing.T) {
	sessionID := "75500000-0000-4000-8000-000000000001"
	frame := &tutoring.FocusFrame{ID: "frame", SessionID: sessionID, SavedState: tutoring.StateRouteActive, Context: tutoring.FocusContext{GoalRevisionID: "goal-revision", RouteRevisionID: "route", RouteStepID: "step", KnowledgeRevisionID: "knowledge", FocusNodeRevisionID: "node"}, SavedAggregateVersion: 7, CreatedEventSequence: 1}
	session := tutoring.Session{ID: sessionID, State: tutoring.StateFreeAnswer, AggregateVer: 8, Context: frame.Context, ActiveFrame: frame}
	question := tutoring.FreeQuestion{ID: "question", SessionID: sessionID, FocusFrameID: frame.ID, SessionAggregateVer: 8, KnowledgeRevisionID: "knowledge"}
	answer := tutoring.FreeAnswer{ID: "answer", SessionID: sessionID, FocusFrameID: frame.ID, FreeQuestionID: question.ID, KnowledgeRevisionID: "knowledge"}
	request := frozenSessionRequest(session, ProposalActivity, "knowledge", []string{"node"})
	request.FreeQuestionID, request.FreeAnswerID = question.ID, answer.ID
	hash, _ := HashJSON(request)
	ref, _ := (proposalResolver{}).Resolve(context.Background(), "knowledge", "node")
	artifact := ProposalArtifact{
		ID: "proposal", SchemaVersion: ProposalSchemaVersion, InputHash: hash, Type: ProposalActivity,
		AggregateType: "session", AggregateID: sessionID, AggregateVersion: session.AggregateVer,
		GoalRevisionID: request.GoalRevisionID, RouteRevisionID: request.RouteRevisionID,
		KnowledgeRevisionID: "knowledge", FrozenRequest: request,
		Activity: &ActivityProposal{Prompt: "quiz", Type: ActivityObjective, Rubric: Rubric{Revision: "r1", Items: []RubricItem{{ID: "item", Criterion: "correct", RequiredReferenceIDs: []string{"node"}}}, ObjectiveRule: &ObjectiveRule{AcceptedAnswers: []string{"yes"}}}, Difficulty: 1, AllowedHelp: []HelpLevel{HelpNone}, References: []KnowledgeReference{ref}},
	}
	newStore := func() *proposalTestStore {
		return &proposalTestStore{
			session: session, proposal: artifact,
			goal:         GoalRevision{ID: "goal-revision"},
			route:        RouteRevision{ID: "route", GoalRevisionID: "goal-revision", KnowledgeRevisionID: "knowledge", Steps: []RouteStep{{ID: "step", NodeID: "node", NodeRevisionID: "node"}}},
			freeQuestion: question, freeAnswer: answer,
		}
	}
	for _, test := range []struct {
		name, questionID, answerID string
		wantCode                   string
	}{
		{name: "arbitrary strings rejected", questionID: "not-question", answerID: "not-answer", wantCode: CodeStaleProposal},
		{name: "owned records attached", questionID: question.ID, answerID: answer.ID},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := newStore()
			service := newProposalTestService(t, store, &proposalTestRepository{}, nil)
			command := ActionCommand{Operation: coordinatorOperation("75500000-0000-4000-8000-000000000010", sessionID, 8), Action: tutoring.ActionConvertFreeAnswerToQuiz, ProposalID: artifact.ID, Question: test.questionID, Answer: test.answerID}
			_, err := service.ApplyAction(context.Background(), "75500000-0000-4000-8000-000000000099", sessionID, command)
			if test.wantCode != "" {
				if ErrorCode(err) != test.wantCode || store.commits != 0 {
					t.Fatalf("invalid attachment err=%v commits=%d", err, store.commits)
				}
				return
			}
			if err != nil || store.lastCommit.Batch.Activity == nil || store.lastCommit.Batch.Activity.AttachedFreeQuestionID != question.ID || store.lastCommit.Batch.Activity.AttachedFreeAnswerID != answer.ID {
				t.Fatalf("attached quiz=%+v err=%v", store.lastCommit.Batch.Activity, err)
			}
		})
	}
}

func TestCoordinatorExposureCanonicalizesClientReferences(t *testing.T) {
	sessionID := "75600000-0000-4000-8000-000000000001"
	store := &proposalTestStore{session: tutoring.Session{ID: sessionID, State: tutoring.StateRouteActive, AggregateVer: 2, Context: tutoring.FocusContext{KnowledgeRevisionID: "knowledge"}}}
	service := newProposalTestService(t, store, &proposalTestRepository{}, nil)
	command := ActionCommand{Operation: coordinatorOperation("75600000-0000-4000-8000-000000000010", sessionID, 2), Action: tutoring.ActionRecordExposure, ExposureKind: "reading", ExposureText: "text", References: []KnowledgeReference{{KnowledgeRevisionID: "knowledge", NodeRevisionID: "node", Range: SourceRange{Start: 1, End: 2}, SliceSHA256: SHA256([]byte("forged"))}}}
	if _, err := service.ApplyAction(context.Background(), "75600000-0000-4000-8000-000000000099", sessionID, command); ErrorCode(err) != CodeProposalRejected || store.commits != 0 {
		t.Fatalf("forged exposure err=%v commits=%d", err, store.commits)
	}
}

func TestCoordinatorConfirmCreatesEvidenceOnlyForFullySupportedArtifact(t *testing.T) {
	activity, attempt, artifact := assessmentFixture()
	artifact.SessionID = "session-1"
	artifact.Confidence = 849
	activity.SessionID = artifact.SessionID
	attempt.SessionID = artifact.SessionID
	store := &proposalTestStore{
		session:  assessmentFeedbackSession(activity, attempt, 5),
		activity: activity, attempt: attempt, assessment: artifact,
		decision: AssessmentDecision{ID: "provisional", AssessmentID: artifact.ID, Version: 1, Disposition: DispositionProvisional, Items: artifact.Items},
	}
	service := newProposalTestService(t, store, &proposalTestRepository{}, nil)
	command := AssessmentDecisionCommand{Operation: coordinatorOperation("75900000-0000-4000-8000-000000000010", artifact.SessionID, 5), Kind: "confirm", ExpectedDispositionVersion: 1}
	if _, err := service.Decide(context.Background(), "75900000-0000-4000-8000-000000000099", artifact.ID, command); err != nil {
		t.Fatal(err)
	}
	batch := store.lastCommit.Batch
	if len(batch.Evidence) != 1 || batch.Disposition != DispositionAccepted || len(batch.Events) != 3 || batch.Events[0].Type != EventAssessmentAccepted || batch.Events[1].Type != EventEvidenceAccepted || batch.Events[2].Type != EventTutoringStateChanged {
		t.Fatalf("confirm batch=%+v", batch)
	}

	store = &proposalTestStore{
		session: store.session, activity: activity, attempt: attempt,
		assessment: AssessmentArtifact{ID: artifact.ID, SessionID: artifact.SessionID, ActivityID: artifact.ActivityID, AttemptID: artifact.AttemptID, ActivityRevision: 1, Confidence: 0},
		decision:   AssessmentDecision{ID: "unsafe", AssessmentID: artifact.ID, Version: 1, Disposition: DispositionProvisional},
	}
	service = newProposalTestService(t, store, &proposalTestRepository{}, nil)
	if _, err := service.Decide(context.Background(), "75900000-0000-4000-8000-000000000099", artifact.ID, command); ErrorCode(err) != CodeAssessmentDispositionConflict || store.commits != 0 {
		t.Fatalf("unsafe confirm err=%v commits=%d", err, store.commits)
	}
}

func TestCoordinatorOverrideRecomputesMisconceptionAndCanonicalOrdinals(t *testing.T) {
	activity, attempt, artifact := assessmentFixture()
	artifact.SessionID = "session-1"
	activity.SessionID = artifact.SessionID
	attempt.SessionID = artifact.SessionID
	artifact.Items[0].Conclusion = ConclusionFail
	artifact.Items[0].MisconceptionCandidate = "Queue is LIFO"
	oldEvidence := AcceptedEvidence{ID: "old-evidence", AssessmentID: artifact.ID, ActivityID: activity.ID, ActivityRevision: 1, NodeRevisionID: activity.TargetNodeRevisionID, Outcome: OutcomeFail, Help: HelpNone, ReceivedAt: time.Now().Add(-time.Hour), Misconceptions: []MisconceptionCandidate{{RubricItemID: "item-1", Text: "Queue is LIFO"}}, RubricOutcomes: []RubricOutcome{{RubricItemID: "item-1", Conclusion: ConclusionFail}}}
	oldHypothesis := ReduceNode(activity.TargetNodeRevisionID, []AcceptedEvidence{oldEvidence}, map[string]bool{}, nil).Misconceptions[0]
	oldHypothesis.Revision = 1
	oldEvidenceID := oldEvidence.ID
	store := &proposalTestStore{
		session:  assessmentFeedbackSession(activity, attempt, 6),
		activity: activity, attempt: attempt, assessment: artifact,
		decision: AssessmentDecision{ID: "old-decision", AssessmentID: artifact.ID, Version: 1, Disposition: DispositionAccepted, Items: artifact.Items, ProducedEvidenceID: &oldEvidenceID},
		evidence: []AcceptedEvidence{oldEvidence}, misconceptions: []MisconceptionHypothesis{oldHypothesis},
	}
	replacement := artifact.Items[0]
	replacement.Conclusion = ConclusionPass
	replacement.MisconceptionCandidate = ""
	service := newProposalTestService(t, store, &proposalTestRepository{}, nil)
	command := AssessmentDecisionCommand{Operation: coordinatorOperation("76000000-0000-4000-8000-000000000010", artifact.SessionID, 6), Kind: "override", ExpectedDispositionVersion: 1, Reason: "corrected", Items: []AssessmentItem{replacement}}
	if _, err := service.Decide(context.Background(), "76000000-0000-4000-8000-000000000099", artifact.ID, command); err != nil {
		t.Fatal(err)
	}
	batch := store.lastCommit.Batch
	want := []EventType{EventAssessmentOverridden, EventEvidenceInvalidated, EventEvidenceAccepted, EventMisconceptionHypothesisRevised, EventTutoringStateChanged}
	if len(batch.Events) != len(want) {
		t.Fatalf("override events=%v", batch.Events)
	}
	for index := range want {
		if batch.Events[index].Type != want[index] {
			t.Fatalf("event[%d]=%s want %s", index, batch.Events[index].Type, want[index])
		}
	}
	if len(batch.Evidence) != 1 || len(batch.Evidence[0].Misconceptions) != 0 || len(batch.Misconceptions) != 1 || batch.Misconceptions[0].Status != MisconceptionResolved || batch.Misconceptions[0].Revision != 2 {
		t.Fatalf("override recomputation evidence=%+v misconceptions=%+v", batch.Evidence, batch.Misconceptions)
	}
}

func TestCoordinatorActivityTargetIsRouteOwnedAndReferenceOrderIndependent(t *testing.T) {
	sessionID := "76100000-0000-4000-8000-000000000001"
	session := tutoring.Session{ID: sessionID, State: tutoring.StateRouteActive, AggregateVer: 4, Context: tutoring.FocusContext{GoalRevisionID: "goal", RouteRevisionID: "route", RouteStepID: "step", KnowledgeRevisionID: "knowledge", FocusNodeRevisionID: "target"}}
	request := frozenSessionRequest(session, ProposalActivity, "knowledge", []string{"helper", "target"})
	hash, _ := HashJSON(request)
	helper, _ := (proposalResolver{}).Resolve(context.Background(), "knowledge", "helper")
	target, _ := (proposalResolver{}).Resolve(context.Background(), "knowledge", "target")
	proposal := ProposalArtifact{
		ID: "proposal", SchemaVersion: ProposalSchemaVersion, InputHash: hash, Type: ProposalActivity,
		AggregateType: "session", AggregateID: sessionID, AggregateVersion: session.AggregateVer,
		GoalRevisionID: "goal", RouteRevisionID: "route", KnowledgeRevisionID: "knowledge", FrozenRequest: request,
		Activity: &ActivityProposal{Prompt: "question", Type: ActivityObjective, Rubric: Rubric{Revision: "r1", Items: []RubricItem{{ID: "item", Criterion: "correct", RequiredReferenceIDs: []string{"target"}}}, ObjectiveRule: &ObjectiveRule{AcceptedAnswers: []string{"yes"}}}, Difficulty: 1, AllowedHelp: []HelpLevel{HelpNone}, References: []KnowledgeReference{helper, target}},
	}
	newStore := func() *proposalTestStore {
		proposalCopy := proposal
		activityCopy := *proposal.Activity
		activityCopy.References = append([]KnowledgeReference(nil), proposal.Activity.References...)
		proposalCopy.Activity = &activityCopy
		return &proposalTestStore{session: session, proposal: proposalCopy, goal: GoalRevision{ID: "goal"}, route: RouteRevision{ID: "route", GoalRevisionID: "goal", KnowledgeRevisionID: "knowledge", Steps: []RouteStep{{ID: "step", NodeID: "node", NodeRevisionID: "target"}}}}
	}
	store := newStore()
	service := newProposalTestService(t, store, &proposalTestRepository{}, nil)
	command := ActionCommand{Operation: coordinatorOperation("76100000-0000-4000-8000-000000000010", sessionID, 4), Action: tutoring.ActionIssueActivity, ProposalID: proposal.ID}
	if _, err := service.ApplyAction(context.Background(), "device-b", sessionID, command); err != nil {
		t.Fatal(err)
	}
	activity := *store.lastCommit.Batch.Activity
	if activity.TargetNodeID != "node" || activity.TargetNodeRevisionID != "target" || activity.References[0].NodeRevisionID != "helper" {
		t.Fatalf("frozen activity target=%+v references=%+v", activity, activity.References)
	}
	evidence := service.makeEvidence(activity, Attempt{Help: HelpNone}, AssessmentArtifact{}, AssessmentDecision{}, nil, OutcomePass, time.Now())
	if evidence.NodeRevisionID != "target" {
		t.Fatalf("evidence scored helper reference: %+v", evidence)
	}

	store = newStore()
	store.proposal.Activity.References = []KnowledgeReference{helper}
	service = newProposalTestService(t, store, &proposalTestRepository{}, nil)
	if _, err := service.ApplyAction(context.Background(), "device-b", sessionID, command); ErrorCode(err) != CodeProposalRejected || store.commits != 0 {
		t.Fatalf("missing target err=%v commits=%d", err, store.commits)
	}

	store = newStore()
	store.route.Steps[0].NodeRevisionID = "other-route-node"
	service = newProposalTestService(t, store, &proposalTestRepository{}, nil)
	if _, err := service.ApplyAction(context.Background(), "device-b", sessionID, command); ErrorCode(err) != CodeStaleProposal || store.commits != 0 {
		t.Fatalf("cross-route target err=%v commits=%d", err, store.commits)
	}
}

func TestCoordinatorGoalLineageAndCrossDeviceSharing(t *testing.T) {
	goalID := "76200000-0000-4000-8000-000000000001"
	previousID := "76200000-0000-4000-8000-000000000002"
	previous := previousID
	operation := func(id string, version int64) OperationEnvelope {
		return OperationEnvelope{OperationID: id, PayloadSchemaVersion: 1, AggregateType: "goal", AggregateID: goalID, ExpectedVersion: version, Payload: json.RawMessage(`{}`)}
	}
	for _, test := range []struct {
		name     string
		version  int64
		previous *string
		loaded   GoalRevision
	}{
		{name: "revision one has previous", previous: &previous},
		{name: "later revision missing previous", version: 1},
		{name: "cross goal previous", version: 1, previous: &previous, loaded: GoalRevision{ID: previousID, GoalID: "other-goal", Revision: 1}},
		{name: "skipped previous", version: 2, previous: &previous, loaded: GoalRevision{ID: previousID, GoalID: goalID, Revision: 1}},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := &proposalTestStore{goal: test.loaded}
			service := newProposalTestService(t, store, &proposalTestRepository{}, nil)
			command := GoalCommand{Operation: operation("76200000-0000-4000-8000-000000000010", test.version), GoalID: goalID, Text: "learn", Source: "user", PreviousRevisionID: test.previous}
			if _, err := service.CreateGoal(context.Background(), "device-b", command); ErrorCode(err) != CodeInvalidRequest || store.commits != 0 {
				t.Fatalf("lineage err=%v commits=%d", err, store.commits)
			}
		})
	}

	store := &proposalTestStore{goal: GoalRevision{ID: previousID, GoalID: goalID, Revision: 1, ActorDeviceID: "device-a"}}
	service := newProposalTestService(t, store, &proposalTestRepository{}, nil)
	valid := GoalCommand{Operation: operation("76200000-0000-4000-8000-000000000011", 1), GoalID: goalID, Text: "learn more", Source: "user", PreviousRevisionID: &previous}
	if _, err := service.CreateGoal(context.Background(), "device-b", valid); err != nil || store.commits != 1 {
		t.Fatalf("cross-device goal revision err=%v commits=%d", err, store.commits)
	}

	sessionID := "76200000-0000-4000-8000-000000000003"
	store = &proposalTestStore{goal: GoalRevision{ID: previousID, GoalID: goalID, Revision: 1, ActorDeviceID: "device-a"}}
	service = newProposalTestService(t, store, &proposalTestRepository{}, nil)
	sessionCommand := SessionCommand{Operation: coordinatorOperation("76200000-0000-4000-8000-000000000012", sessionID, 0), GoalRevisionID: previousID}
	if _, err := service.CreateSession(context.Background(), "device-b", sessionCommand); err != nil || store.commits != 1 {
		t.Fatalf("cross-device session err=%v commits=%d", err, store.commits)
	}
}

func TestCoordinatorRejectionFenceTurnsConcurrentStateChangeIntoConflict(t *testing.T) {
	sessionID := "76300000-0000-4000-8000-000000000001"
	advanced := tutoring.Session{ID: sessionID, State: tutoring.StateDiagnostic, AggregateVer: 4}
	store := &proposalTestStore{session: tutoring.Session{ID: sessionID, State: tutoring.StateRouteActive, AggregateVer: 3}, authorityAsOf: 91, advanceOnArchive: &advanced}
	service := newProposalTestService(t, store, &proposalTestRepository{}, nil)
	command := ActionCommand{Operation: coordinatorOperation("76300000-0000-4000-8000-000000000010", sessionID, 3), Action: tutoring.ActionStartDiagnostic}
	if _, err := service.ApplyAction(context.Background(), "device", sessionID, command); ErrorCode(err) != CodeVersionConflict {
		t.Fatalf("concurrent rejection err=%v", err)
	}
	if store.lastArchivedError.CurrentVersion != 4 || store.lastArchivedError.AsOfEventSequence != 91 || store.lastArchivedError.Code != CodeVersionConflict {
		t.Fatalf("archived concurrent rejection=%+v", store.lastArchivedError)
	}

	store = &proposalTestStore{session: advanced, authorityAsOf: 92}
	service = newProposalTestService(t, store, &proposalTestRepository{}, nil)
	command.Operation = coordinatorOperation("76300000-0000-4000-8000-000000000011", sessionID, 3)
	if _, err := service.ApplyAction(context.Background(), "device", sessionID, command); ErrorCode(err) != CodeVersionConflict || store.lastArchivedError.AsOfEventSequence != 92 {
		t.Fatalf("early version conflict err=%v archived=%+v", err, store.lastArchivedError)
	}
}

func TestCoordinatorRejectsUnknownRecordExposureKind(t *testing.T) {
	sessionID := "76400000-0000-4000-8000-000000000001"
	store := &proposalTestStore{session: tutoring.Session{ID: sessionID, State: tutoring.StateRouteActive, AggregateVer: 2}}
	service := newProposalTestService(t, store, &proposalTestRepository{}, nil)
	command := ActionCommand{Operation: coordinatorOperation("76400000-0000-4000-8000-000000000010", sessionID, 2), Action: tutoring.ActionRecordExposure, ExposureKind: "video", ExposureText: "watched"}
	if _, err := service.ApplyAction(context.Background(), "device", sessionID, command); ErrorCode(err) != CodeInvalidRequest || store.commits != 0 {
		t.Fatalf("invalid exposure err=%v commits=%d", err, store.commits)
	}
}
