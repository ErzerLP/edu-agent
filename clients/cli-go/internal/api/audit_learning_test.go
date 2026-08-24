package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestProposalResponseStrictlyBindsFrozenRequest(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		mutate func(*TutoringProposal)
	}{
		{name: "input hash", mutate: func(value *TutoringProposal) { value.InputHash = strings.Repeat("0", 64) }},
		{name: "frozen input", mutate: func(value *TutoringProposal) { value.FrozenRequest.Input["schema_version"] = "changed" }},
		{name: "proposal type", mutate: func(value *TutoringProposal) { value.ProposalType = "activity" }},
		{name: "aggregate ID", mutate: func(value *TutoringProposal) { value.AggregateID = "10000000-0000-4000-8000-000000000099" }},
		{name: "aggregate version", mutate: func(value *TutoringProposal) { value.AggregateVersion++ }},
		{name: "knowledge revision", mutate: func(value *TutoringProposal) { value.KnowledgeRevisionID = "40000000-0000-4000-8000-000000000099" }},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			request := auditSessionProposalRequest()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				proposal := auditProposal(request)
				test.mutate(&proposal)
				writeAPIJSON(t, w, http.StatusCreated, proposal)
			}))
			defer server.Close()
			_, err := NewClient(server.URL, "token", time.Second, nil).CreateProposal(t.Context(), request)
			var protocolErr *ProtocolError
			if !errors.As(err, &protocolErr) || protocolErr.Category != "invalid_success_response" {
				t.Fatalf("error=%T %v", err, err)
			}
		})
	}
}

func TestMutationResultUsesEndpointSpecificUnionAndOwnership(t *testing.T) {
	t.Parallel()
	otherSessionID := "10000000-0000-4000-8000-000000000099"
	tests := []struct {
		name     string
		response any
		call     func(*Client) error
	}{
		{
			name: "goal endpoint rejects session result", response: auditSessionOperationResult(learningSessionID),
			call: func(client *Client) error {
				_, err := client.CreateGoal(t.Context(), LearningGoalRequest{OperationID: "e0000000-0000-4000-8000-000000000010", PayloadSchemaVersion: 1, AggregateType: "goal", AggregateID: "20000000-0000-4000-8000-000000000011", Text: "Goal", Source: "go-cli-m1"})
				return err
			},
		},
		{
			name: "session endpoint rejects another aggregate", response: auditSessionOperationResult(otherSessionID),
			call: func(client *Client) error {
				_, err := client.CreateSession(t.Context(), TutoringSessionRequest{OperationID: "e0000000-0000-4000-8000-000000000011", PayloadSchemaVersion: 1, AggregateType: "session", AggregateID: learningSessionID, GoalRevisionID: learningGoalRevision})
				return err
			},
		},
		{
			name: "action endpoint rejects another aggregate", response: auditSessionOperationResult(otherSessionID),
			call: func(client *Client) error {
				_, err := client.ApplySessionAction(t.Context(), learningSessionID, ActionNoFieldsRequest{SessionOperation: SessionOperation{OperationID: "e0000000-0000-4000-8000-000000000012", PayloadSchemaVersion: 1, AggregateType: "session", AggregateID: learningSessionID, ExpectedVersion: 4}, Action: "present_activity"})
				return err
			},
		},
		{
			name: "decision endpoint rejects another assessment", response: auditDecisionOperationResult("80000000-0000-4000-8000-000000000099"),
			call: func(client *Client) error {
				_, err := client.DecideAssessment(t.Context(), learningAssessmentID, AssessmentConfirmRequest{SessionOperation: SessionOperation{OperationID: "e0000000-0000-4000-8000-000000000013", PayloadSchemaVersion: 1, AggregateType: "session", AggregateID: learningSessionID, ExpectedVersion: 8}, Kind: "confirm", ExpectedDispositionVersion: 1})
				return err
			},
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				writeAPIJSON(t, w, http.StatusCreated, test.response)
			}))
			defer server.Close()
			var protocolErr *ProtocolError
			if err := test.call(NewClient(server.URL, "token", time.Second, nil)); !errors.As(err, &protocolErr) {
				t.Fatalf("error=%T %v", err, err)
			}
		})
	}
}

func TestInvalidLearningRequestsNeverReachHTTP(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
	defer server.Close()
	client := NewClient(server.URL, "token", time.Second, nil)
	baseOperation := SessionOperation{OperationID: "e0000000-0000-4000-8000-000000000010", PayloadSchemaVersion: 1, AggregateType: "session", AggregateID: learningSessionID, ExpectedVersion: 8}
	assessmentItem := learningSessionView("Feedback").WorkItem.Assessment.Items[0]
	zeroTime := time.Time{}
	tests := []struct {
		name string
		call func() error
	}{
		{name: "retrieval limits", call: func() error {
			_, err := client.RetrieveKnowledge(t.Context(), KnowledgeRetrievalRequest{Query: "topic", Limits: &KnowledgeQueryLimits{MaxDepth: 9}})
			return err
		}},
		{name: "proposal optional UUID", call: func() error {
			request := auditSessionProposalRequest()
			request.ActivityID = "not-a-uuid"
			_, err := client.CreateProposal(t.Context(), request)
			return err
		}},
		{name: "proposal state", call: func() error {
			request := auditSessionProposalRequest()
			request.TutoringState = "AwaitingResponse"
			_, err := client.CreateProposal(t.Context(), request)
			return err
		}},
		{name: "action path ownership", call: func() error {
			request := ActionNoFieldsRequest{SessionOperation: baseOperation, Action: "present_activity"}
			_, err := client.ApplySessionAction(t.Context(), "10000000-0000-4000-8000-000000000099", request)
			return err
		}},
		{name: "direct exposure SHA", call: func() error {
			request := ActionDirectExposureRequest{SessionOperation: baseOperation, Action: "record_exposure", ExposureKind: "reading", ExposureText: "text", KnowledgeReferences: []KnowledgeReferenceInput{{NodeRevisionID: learningNodeRevision, SliceSHA256: "BAD"}}}
			_, err := client.ApplySessionAction(t.Context(), learningSessionID, request)
			return err
		}},
		{name: "override unassessed", call: func() error {
			assessmentItem.Conclusion = "unassessed"
			_, err := client.DecideAssessment(t.Context(), learningAssessmentID, AssessmentOverrideRequest{SessionOperation: baseOperation, Kind: "override", ExpectedDispositionVersion: 1, Reason: "reason", Items: []AssessmentItem{assessmentItem}})
			return err
		}},
		{name: "timeline filter", call: func() error { _, err := client.Timeline(t.Context(), "", 50, "bad"); return err }},
		{name: "routes limit", call: func() error { _, err := client.Routes(t.Context(), "", 201, true); return err }},
		{name: "evidence filter", call: func() error { _, err := client.Evidence(t.Context(), "", 50, "bad"); return err }},
		{name: "reviews zero time", call: func() error { _, err := client.Reviews(t.Context(), "", 50, &zeroTime); return err }},
		{name: "cursor control", call: func() error { _, err := client.Routes(t.Context(), "bad\ncursor", 50, true); return err }},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			before := calls.Load()
			if err := test.call(); err == nil {
				t.Fatal("expected local validation error")
			}
			if calls.Load() != before {
				t.Fatalf("HTTP calls changed from %d to %d", before, calls.Load())
			}
		})
	}
}

func TestProjectionPageCursorRejectsControlCharacters(t *testing.T) {
	t.Parallel()
	tests := []string{"bad\tcursor", "bad\x7fcursor", "bad\x01cursor"}
	for _, cursor := range tests {
		cursor := cursor
		t.Run(strings.ReplaceAll(cursor, "bad", "control"), func(t *testing.T) {
			t.Parallel()
			view := learningSessionView("RouteActive")
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				writeAPIJSON(t, w, http.StatusOK, RoutesPage{Metadata: view.Metadata, Items: []RouteProjection{}, NextCursor: cursor})
			}))
			defer server.Close()
			_, err := NewClient(server.URL, "token", time.Second, nil).Routes(t.Context(), "", 50, true)
			var protocolErr *ProtocolError
			if !errors.As(err, &protocolErr) || protocolErr.Category != "invalid_success_response" {
				t.Fatalf("error=%T %v", err, err)
			}
		})
	}
}

func TestConflictEnvelopeIsStrictAndRequestBound(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		envelope ErrorResponse
	}{
		{name: "missing version conflict", envelope: ErrorResponse{Error: ErrorBody{Code: "version_conflict", Message: "changed", RequestID: "request-conflict"}}},
		{name: "wrong aggregate", envelope: ErrorResponse{Error: ErrorBody{Code: "version_conflict", Message: "changed", RequestID: "request-conflict"}, Conflict: &LearningConflict{AggregateType: "session", AggregateID: "10000000-0000-4000-8000-000000000099", ExpectedVersion: 4, CurrentVersion: 5, AsOfEventSeq: 20}}},
		{name: "unexpected disposition", envelope: ErrorResponse{Error: ErrorBody{Code: "stale_cursor", Message: "changed", RequestID: "request-conflict"}, CurrentDisposition: "provisional"}},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				writeAPIJSON(t, w, http.StatusConflict, test.envelope)
			}))
			defer server.Close()
			_, err := NewClient(server.URL, "token", time.Second, nil).ApplySessionAction(t.Context(), learningSessionID, ActionNoFieldsRequest{SessionOperation: SessionOperation{OperationID: "e0000000-0000-4000-8000-000000000010", PayloadSchemaVersion: 1, AggregateType: "session", AggregateID: learningSessionID, ExpectedVersion: 4}, Action: "present_activity"})
			var protocolErr *ProtocolError
			if !errors.As(err, &protocolErr) || protocolErr.Category != "malformed_error_response" {
				t.Fatalf("error=%T %v", err, err)
			}
		})
	}
}

func TestAssessmentDispositionConflictIsBoundToEndpoint(t *testing.T) {
	t.Parallel()
	baseOperation := SessionOperation{OperationID: "e0000000-0000-4000-8000-000000000010", PayloadSchemaVersion: 1, AggregateType: "session", AggregateID: learningSessionID, ExpectedVersion: 8}
	tests := []struct {
		name               string
		currentDisposition string
		call               func(*Client) error
		wantAPIError       bool
	}{
		{name: "action permits omitted disposition", call: func(client *Client) error {
			_, err := client.ApplySessionAction(t.Context(), learningSessionID, ActionNoFieldsRequest{SessionOperation: baseOperation, Action: "present_activity"})
			return err
		}, wantAPIError: true},
		{name: "action permits valid disposition", currentDisposition: "provisional", call: func(client *Client) error {
			_, err := client.ApplySessionAction(t.Context(), learningSessionID, ActionNoFieldsRequest{SessionOperation: baseOperation, Action: "present_activity"})
			return err
		}, wantAPIError: true},
		{name: "action rejects invalid disposition", currentDisposition: "unknown", call: func(client *Client) error {
			_, err := client.ApplySessionAction(t.Context(), learningSessionID, ActionNoFieldsRequest{SessionOperation: baseOperation, Action: "present_activity"})
			return err
		}},
		{name: "decision requires disposition", call: func(client *Client) error {
			_, err := client.DecideAssessment(t.Context(), learningAssessmentID, AssessmentConfirmRequest{SessionOperation: baseOperation, Kind: "confirm", ExpectedDispositionVersion: 1})
			return err
		}},
		{name: "decision permits valid disposition", currentDisposition: "accepted", call: func(client *Client) error {
			_, err := client.DecideAssessment(t.Context(), learningAssessmentID, AssessmentConfirmRequest{SessionOperation: baseOperation, Kind: "confirm", ExpectedDispositionVersion: 1})
			return err
		}, wantAPIError: true},
		{name: "other endpoint rejects conflict", currentDisposition: "provisional", call: func(client *Client) error {
			_, err := client.Timeline(t.Context(), "", 50, "")
			return err
		}},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				writeAPIJSON(t, w, http.StatusConflict, ErrorResponse{Error: ErrorBody{Code: "assessment_disposition_conflict", Message: "changed", RequestID: "request-conflict"}, CurrentDisposition: test.currentDisposition})
			}))
			defer server.Close()
			err := test.call(NewClient(server.URL, "token", time.Second, nil))
			var apiErr *APIError
			if test.wantAPIError {
				if !errors.As(err, &apiErr) || apiErr.Code != "assessment_disposition_conflict" || apiErr.CurrentDisposition != test.currentDisposition {
					t.Fatalf("error=%T %v", err, err)
				}
				return
			}
			var protocolErr *ProtocolError
			if !errors.As(err, &protocolErr) || protocolErr.Category != "malformed_error_response" {
				t.Fatalf("error=%T %v", err, err)
			}
		})
	}
}

func TestGoalAggregateProposalAndDirectExposureUnions(t *testing.T) {
	t.Parallel()
	t.Run("goal proposal", func(t *testing.T) {
		request := TutoringProposalRequest{RequestID: "e0000000-0000-4000-8000-000000000020", ProposalType: "route", AggregateType: "goal", AggregateID: "20000000-0000-4000-8000-000000000011", AggregateVersion: 1, GoalRevisionID: learningGoalRevision, KnowledgeRevisionID: learningKnowledgeID, NodeRevisionIDs: []string{learningNodeRevision}, Input: map[string]any{"goal_text": "Goal"}}
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			writeAPIJSON(t, w, http.StatusCreated, auditProposal(request))
		}))
		defer server.Close()
		if _, err := NewClient(server.URL, "token", time.Second, nil).CreateProposal(t.Context(), request); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("direct exposure", func(t *testing.T) {
		var calls atomic.Int32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			calls.Add(1)
			writeAPIJSON(t, w, http.StatusCreated, auditSessionOperationResult(learningSessionID))
		}))
		defer server.Close()
		request := ActionDirectExposureRequest{SessionOperation: SessionOperation{OperationID: "e0000000-0000-4000-8000-000000000021", PayloadSchemaVersion: 1, AggregateType: "session", AggregateID: learningSessionID, ExpectedVersion: 4}, Action: "record_exposure", ExposureKind: "reading", ExposureText: "Canonical text", KnowledgeReferences: []KnowledgeReferenceInput{{KnowledgeRevisionID: learningKnowledgeID, NodeID: learningNodeID, NodeRevisionID: learningNodeRevision, DocumentRevisionID: "40000000-0000-4000-8000-000000000011", Range: &LearningSourceRange{Start: 0, End: 12}, Slice: "canonical text", SliceSHA256: strings.Repeat("a", 64)}}}
		if _, err := NewClient(server.URL, "token", time.Second, nil).ApplySessionAction(t.Context(), learningSessionID, request); err != nil || calls.Load() != 1 {
			t.Fatalf("error=%v calls=%d", err, calls.Load())
		}
	})
}

func auditSessionProposalRequest() TutoringProposalRequest {
	view := learningSessionView("Diagnostic")
	reference := ProposalContextReference{KnowledgeRevisionID: learningKnowledgeID, DocumentRevisionID: "40000000-0000-4000-8000-000000000011", NodeID: learningNodeID, NodeRevisionID: learningNodeRevision, Range: LearningSourceRange{Start: 0, End: 12}, Slice: "canonical text", SliceSHA256: strings.Repeat("a", 64)}
	contextValue := ProposalContext{SchemaVersion: ProposalContextSchemaVersion, WorkItem: *view.WorkItem, Retrieval: ProposalContextRetrieval{KnowledgeRevisionID: learningKnowledgeID, Hits: []ProposalContextReference{reference}}}
	encoded, _ := json.Marshal(contextValue)
	var input map[string]any
	_ = json.Unmarshal(encoded, &input)
	return TutoringProposalRequest{RequestID: "e0000000-0000-4000-8000-000000000020", ProposalType: "route", AggregateType: "session", AggregateID: learningSessionID, AggregateVersion: view.Session.AggregateVersion, GoalRevisionID: learningGoalRevision, TutoringState: "Diagnostic", KnowledgeRevisionID: learningKnowledgeID, NodeRevisionIDs: []string{learningNodeRevision}, Input: input}
}

func auditProposal(request TutoringProposalRequest) TutoringProposal {
	inputHash, _ := hashJSON(request)
	return TutoringProposal{ProposalID: "f0000000-0000-4000-8000-000000000010", SchemaVersion: 1, InputHash: inputHash, ProposalType: request.ProposalType, AggregateType: request.AggregateType, AggregateID: request.AggregateID, AggregateVersion: request.AggregateVersion, GoalRevisionID: request.GoalRevisionID, RouteRevisionID: request.RouteRevisionID, ActivityID: request.ActivityID, AttemptID: request.AttemptID, KnowledgeRevisionID: request.KnowledgeRevisionID, FrozenRequest: request, Route: []RouteProposalStep{{NodeRevisionID: learningNodeRevision, TeachingIntent: "Teach the topic", CompletionCondition: "Answer once"}}, ModelID: "fake-model", ModelParameters: map[string]any{}, PromptRevision: "prompt-v1", AttemptCategories: []string{"initial"}, CreatedAt: time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)}
}

func auditSessionOperationResult(sessionID string) SessionOperationResult {
	return SessionOperationResult{Status: "succeeded", AggregateType: "session", AggregateID: sessionID, AggregateVersion: 5, FirstEventSeq: 20, LastEventSeq: 20, ProjectionAsOfEventSeq: 20, Result: TutoringSession{SessionID: sessionID, State: "GoalReady", AggregateVersion: 5, Focus: FocusContext{GoalRevisionID: learningGoalRevision}}}
}

func auditDecisionOperationResult(assessmentID string) AssessmentDecisionOperationResult {
	decision := learningSessionView("Feedback").WorkItem.AssessmentDecision
	copyDecision := *decision
	copyDecision.AssessmentID = assessmentID
	return AssessmentDecisionOperationResult{Status: "succeeded", AggregateType: "session", AggregateID: learningSessionID, AggregateVersion: 9, FirstEventSeq: 20, LastEventSeq: 20, ProjectionAsOfEventSeq: 20, TutoringState: "Feedback", EvidenceDisposition: copyDecision.Disposition, Result: copyDecision}
}
