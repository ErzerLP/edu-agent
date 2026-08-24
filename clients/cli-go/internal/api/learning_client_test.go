package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const (
	learningSessionID     = "10000000-0000-4000-8000-000000000010"
	learningGoalRevision  = "20000000-0000-4000-8000-000000000010"
	learningRouteRevision = "30000000-0000-4000-8000-000000000010"
	learningKnowledgeID   = "40000000-0000-4000-8000-000000000010"
	learningStepID        = "50000000-0000-4000-8000-000000000010"
	learningNodeID        = "50000000-0000-4000-8000-000000000011"
	learningNodeRevision  = "50000000-0000-4000-8000-000000000012"
	learningActivityID    = "60000000-0000-4000-8000-000000000010"
	learningAttemptID     = "70000000-0000-4000-8000-000000000010"
	learningAssessmentID  = "80000000-0000-4000-8000-000000000010"
)

func TestClientDecodesEveryResumableSessionState(t *testing.T) {
	t.Parallel()
	states := []string{"GoalReady", "Diagnostic", "RouteActive", "ActivityIssued", "AwaitingResponse", "Evaluating", "Feedback", "FreeQuestion", "FreeAnswer", "Completed"}
	for _, state := range states {
		state := state
		t.Run(state, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Path != "/v1/tutoring/sessions/current" {
					t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
				}
				writeAPIJSON(t, w, http.StatusOK, learningSessionView(state))
			}))
			defer server.Close()
			view, err := NewClient(server.URL, "token", time.Second, nil).CurrentSession(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if view.Session.State != state {
				t.Fatalf("state=%s", view.Session.State)
			}
			if state == "Completed" && view.WorkItem != nil {
				t.Fatal("completed view retained work item")
			}
		})
	}
}

func TestClientRejectsMismatchedFrameScopedFreeAnswer(t *testing.T) {
	t.Parallel()
	view := learningSessionView("FreeAnswer")
	view.WorkItem.FreeAnswer.FreeQuestionID = "a0000000-0000-4000-8000-000000000099"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeAPIJSON(t, w, http.StatusOK, view)
	}))
	defer server.Close()
	_, err := NewClient(server.URL, "token", time.Second, nil).CurrentSession(t.Context())
	var protocolErr *ProtocolError
	if !strings.Contains(errorString(err), "invalid_success_response") || !asProtocol(err, &protocolErr) {
		t.Fatalf("error=%v", err)
	}
}

func TestLearningMutationRetryUsesIdenticalBody(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	var bodies [][]byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/tutoring/sessions/"+learningSessionID+"/actions" {
			t.Fatalf("path=%s", r.URL.Path)
		}
		data, _ := io.ReadAll(r.Body)
		bodies = append(bodies, append([]byte(nil), data...))
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusGatewayTimeout)
			_, _ = io.WriteString(w, "proxy response deliberately ignored")
			return
		}
		writeAPIJSON(t, w, http.StatusCreated, learningOperationResult())
	}))
	defer server.Close()
	client := NewClient(server.URL, "token", time.Second, nil)
	request := ActionNoFieldsRequest{SessionOperation: SessionOperation{
		OperationID: "e0000000-0000-4000-8000-000000000010", PayloadSchemaVersion: 1,
		AggregateType: "session", AggregateID: learningSessionID, ExpectedVersion: 4,
	}, Action: "present_activity"}
	if _, err := client.ApplySessionAction(t.Context(), learningSessionID, request); err != nil {
		t.Fatal(err)
	}
	if len(bodies) != 2 || !bytes.Equal(bodies[0], bodies[1]) {
		t.Fatalf("mutation retry body changed: %q %q", bodies[0], bodies[1])
	}
}

func TestInvalidLearningRequestDoesNotReachHTTP(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
	defer server.Close()
	client := NewClient(server.URL, "token", time.Second, nil)
	_, err := client.CreateProposal(t.Context(), TutoringProposalRequest{
		RequestID: "e0000000-0000-4000-8000-000000000010", ProposalType: "activity",
		AggregateType: "session", AggregateID: learningSessionID, AggregateVersion: 1,
		KnowledgeRevisionID: learningKnowledgeID, NodeRevisionIDs: []string{learningNodeRevision},
		Input: map[string]any{"schema_version": "wrong", "work_item": map[string]any{}, "retrieval": map[string]any{}},
	})
	if err == nil || calls.Load() != 0 {
		t.Fatalf("err=%v calls=%d", err, calls.Load())
	}
}

func learningSessionView(state string) SessionView {
	now := time.Date(2026, 8, 24, 10, 0, 0, 0, time.UTC)
	metadata := ProjectionMetadata{
		AsOfEventSeq: 20, ProjectionVersion: "projection-v1", MasteryReducerVersion: "mastery-v1",
		AssessmentPolicyVersion: "assessment-v1", ReviewPolicyVersion: "review-v1",
		KnowledgeRevisionID: learningKnowledgeID, Generation: "d0000000-0000-4000-8000-000000000010", ReasonCodes: []string{},
	}
	goal := GoalRevision{
		GoalRevisionID: learningGoalRevision, GoalID: "20000000-0000-4000-8000-000000000011", Revision: 1,
		Text: "Understand the imported material", Source: "go-cli-m1", ActorDeviceID: "10000000-0000-4000-8000-000000000001", CreatedAt: now,
	}
	route := RouteRevision{
		RouteRevisionID: learningRouteRevision, RouteID: "30000000-0000-4000-8000-000000000011", Revision: 1,
		GoalRevisionID: learningGoalRevision, KnowledgeRevisionID: learningKnowledgeID, RoutePolicyVersion: "route-v1",
		SourceProposalID: "30000000-0000-4000-8000-000000000012", CreatedAt: now,
		Steps: []RouteStep{{RouteStepID: learningStepID, Ordinal: 0, NodeID: learningNodeID, NodeRevisionID: learningNodeRevision, TeachingIntent: "Explain the central idea", CompletionCondition: "Answer one prompt"}},
	}
	reference := KnowledgeReference{
		KnowledgeRevisionID: learningKnowledgeID, NodeID: learningNodeID, NodeRevisionID: learningNodeRevision,
		DocumentRevisionID: "40000000-0000-4000-8000-000000000011", Range: LearningSourceRange{Start: 0, End: 12},
		Slice: "canonical text", SliceSHA256: strings.Repeat("a", 64),
	}
	activity := Activity{
		ActivityID: learningActivityID, Revision: 1, SessionID: learningSessionID, GoalRevisionID: learningGoalRevision,
		RouteRevisionID: learningRouteRevision, RouteStepID: learningStepID, KnowledgeRevisionID: learningKnowledgeID,
		TargetNodeID: learningNodeID, TargetNodeRevisionID: learningNodeRevision, KnowledgeReferences: []KnowledgeReference{reference},
		Prompt: "State the central idea.", Type: "open", Rubric: Rubric{RubricRevision: "rubric-v1", Items: []RubricItem{{RubricItemID: "criterion-1", Criterion: "Names the idea"}}},
		Difficulty: 2, AllowedHelp: []string{"none", "hint"}, ActivityPolicyVersion: "activity-v1",
		AssessmentPolicyVersion: "assessment-v1", ReviewPolicyVersion: "review-v1", Review: false, CreatedAt: now,
	}
	attempt := Attempt{
		AttemptID: learningAttemptID, SessionID: learningSessionID, ActivityID: learningActivityID, ActivityRevision: 1,
		AnswerPayloadID: "70000000-0000-4000-8000-000000000011", Answer: "The central idea", AnswerSHA256: strings.Repeat("b", 64),
		Help: "none", ActorDeviceID: "10000000-0000-4000-8000-000000000001", ReceivedAt: now,
	}
	assessmentItem := AssessmentItem{
		RubricItemID: "criterion-1", Conclusion: "pass", AnswerQuote: "central idea", AnswerRange: LearningSourceRange{Start: 4, End: 16},
		AnswerQuoteSHA256: strings.Repeat("c", 64), KnowledgeReferenceID: learningNodeRevision,
		KnowledgeQuote: "canonical text", KnowledgeRange: LearningSourceRange{Start: 0, End: 12}, KnowledgeQuoteSHA256: strings.Repeat("d", 64),
	}
	assessment := AssessmentArtifact{
		AssessmentID: learningAssessmentID, SessionID: learningSessionID, AttemptID: learningAttemptID, ActivityID: learningActivityID,
		ActivityRevision: 1, Items: []AssessmentItem{assessmentItem}, RubricComplete: true, Confidence: 900, RiskFlags: []string{},
		ModelID: "fake-model", ModelParameters: map[string]any{}, PromptRevision: "assessment-prompt-v1", ProposalInputHash: strings.Repeat("e", 64),
		Attempts: 1, AttemptCategories: []string{"initial"}, CreatedAt: now,
	}
	decision := AssessmentDecision{
		DecisionID: "90000000-0000-4000-8000-000000000010", AssessmentID: learningAssessmentID, Version: 1,
		Disposition: "provisional", Items: []AssessmentItem{assessmentItem}, ActorDeviceID: "10000000-0000-4000-8000-000000000001", CreatedAt: now,
	}
	question := FreeQuestion{
		FreeQuestionID: "a0000000-0000-4000-8000-000000000010", SessionID: learningSessionID,
		FocusFrameID: "b0000000-0000-4000-8000-000000000010", SessionAggregateVersion: 8,
		Text: "How does this connect?", KnowledgeRevisionID: learningKnowledgeID, References: []FrozenReference{},
		ActorDeviceID: "10000000-0000-4000-8000-000000000001", ReceivedAt: now,
	}
	answer := FreeAnswer{
		FreeAnswerID: "c0000000-0000-4000-8000-000000000010", SessionID: learningSessionID,
		FocusFrameID: question.FocusFrameID, FreeQuestionID: question.FreeQuestionID, Text: "It connects through the canonical section.",
		KnowledgeRevisionID: learningKnowledgeID, References: []FrozenReference{}, SourceProposalID: "c0000000-0000-4000-8000-000000000011", ReceivedAt: now,
	}
	item := &SessionWorkItem{AllowedActions: []string{}, AllowedAssessmentDecisions: []string{}, GoalRevision: &goal}
	focus := FocusContext{GoalRevisionID: learningGoalRevision}
	switch state {
	case "GoalReady":
		item.AllowedActions = []string{"start_diagnostic", "switch_goal"}
	case "Diagnostic":
		item.AllowedActions = []string{"apply_route", "switch_goal"}
	case "RouteActive":
		item.RouteRevision = &route
		item.AllowedActions = []string{"issue_activity", "record_exposure", "ask_free_question", "complete_session", "switch_goal"}
		focus = FocusContext{GoalRevisionID: learningGoalRevision, RouteRevisionID: learningRouteRevision, RouteStepID: learningStepID, KnowledgeRevisionID: learningKnowledgeID, FocusNodeRevisionID: learningNodeRevision}
	case "ActivityIssued", "AwaitingResponse":
		item.RouteRevision, item.Activity = &route, &activity
		item.AllowedActions = []string{"present_activity", "ask_free_question", "end_activity", "switch_goal"}
		if state == "AwaitingResponse" {
			item.AllowedActions = []string{"submit_attempt", "ask_free_question", "end_activity", "switch_goal"}
		}
		focus = FocusContext{GoalRevisionID: learningGoalRevision, RouteRevisionID: learningRouteRevision, RouteStepID: learningStepID, KnowledgeRevisionID: learningKnowledgeID, FocusNodeRevisionID: learningNodeRevision, ActivityID: learningActivityID}
	case "Evaluating":
		item.RouteRevision, item.Activity, item.Attempt = &route, &activity, &attempt
		item.AllowedActions = []string{"record_assessment", "end_activity", "switch_goal"}
		focus = FocusContext{GoalRevisionID: learningGoalRevision, RouteRevisionID: learningRouteRevision, RouteStepID: learningStepID, KnowledgeRevisionID: learningKnowledgeID, FocusNodeRevisionID: learningNodeRevision, ActivityID: learningActivityID, AttemptID: learningAttemptID}
	case "Feedback":
		item.RouteRevision, item.Activity, item.Attempt = &route, &activity, &attempt
		item.Assessment, item.AssessmentDecision = &assessment, &decision
		item.AllowedAssessmentDecisions = []string{"override", "void"}
		focus = FocusContext{GoalRevisionID: learningGoalRevision, RouteRevisionID: learningRouteRevision, RouteStepID: learningStepID, KnowledgeRevisionID: learningKnowledgeID, FocusNodeRevisionID: learningNodeRevision, ActivityID: learningActivityID, AttemptID: learningAttemptID}
	case "FreeQuestion":
		item.FreeQuestion = &question
		item.AllowedActions = []string{"record_free_answer", "resume_focus", "switch_goal"}
	case "FreeAnswer":
		item.FreeQuestion, item.FreeAnswer = &question, &answer
		item.AllowedActions = []string{"ask_free_question", "convert_free_answer_to_quiz", "resume_focus", "switch_goal"}
	case "Completed":
		item = nil
	}
	return SessionView{
		Metadata:            metadata,
		Session:             TutoringSession{SessionID: learningSessionID, State: state, AggregateVersion: 8, Focus: focus, AttachedQuiz: false, CompletedRoute: state == "Completed"},
		EstimatedActiveTime: ActiveTimeEstimate{DurationSeconds: 300, Estimated: true, AlgorithmVersion: "active-time-v1", SampleCount: 5},
		WorkItem:            item,
	}
}

func TestClientDecodesVersionConflict(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeAPIJSON(t, w, http.StatusConflict, ErrorResponse{
			Error:    ErrorBody{Code: "version_conflict", Message: "changed", RequestID: "request-version"},
			Conflict: &LearningConflict{AggregateType: "session", AggregateID: learningSessionID, ExpectedVersion: 4, CurrentVersion: 5, AsOfEventSeq: 20},
		})
	}))
	defer server.Close()
	_, err := NewClient(server.URL, "token", time.Second, nil).ApplySessionAction(t.Context(), learningSessionID, ActionNoFieldsRequest{
		SessionOperation: SessionOperation{OperationID: "e0000000-0000-4000-8000-000000000010", PayloadSchemaVersion: 1, AggregateType: "session", AggregateID: learningSessionID, ExpectedVersion: 4},
		Action:           "present_activity",
	})
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Code != "version_conflict" || apiErr.Conflict == nil {
		t.Fatalf("error=%T %v", err, err)
	}
}

func learningOperationResult() SessionOperationResult {
	return SessionOperationResult{
		Status: "succeeded", AggregateType: "session", AggregateID: learningSessionID, AggregateVersion: 5,
		FirstEventSeq: 20, LastEventSeq: 20, ProjectionAsOfEventSeq: 20,
		Result: TutoringSession{SessionID: learningSessionID, State: "GoalReady", AggregateVersion: 5, Focus: FocusContext{}, AttachedQuiz: false, CompletedRoute: false},
	}
}

func writeAPIJSON(t *testing.T, w http.ResponseWriter, status int, value any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Fatal(err)
	}
}

func errorString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func asProtocol(err error, target **ProtocolError) bool {
	if err == nil {
		return false
	}
	value, ok := err.(*ProtocolError)
	if ok {
		*target = value
	}
	return ok
}
