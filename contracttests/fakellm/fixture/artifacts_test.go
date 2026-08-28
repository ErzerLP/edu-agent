package fixture

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
)

const (
	fixtureRequestID           = "30000000-0000-4000-8000-000000000001"
	fixtureSessionID           = "30000000-0000-4000-8000-000000000002"
	fixtureGoalRevisionID      = "30000000-0000-4000-8000-000000000003"
	fixtureRouteRevisionID     = "30000000-0000-4000-8000-000000000004"
	fixtureRouteStepID         = "30000000-0000-4000-8000-000000000005"
	fixtureKnowledgeRevisionID = "30000000-0000-4000-8000-000000000006"
	fixtureNodeID              = "30000000-0000-4000-8000-000000000007"
	fixtureNodeRevisionID      = "30000000-0000-4000-8000-000000000008"
	fixtureDocumentRevisionID  = "30000000-0000-4000-8000-000000000009"
	fixtureActivityID          = "30000000-0000-4000-8000-000000000010"
	fixtureAttemptID           = "30000000-0000-4000-8000-000000000011"
	fixtureFreeQuestionID      = "30000000-0000-4000-8000-000000000012"
	fixtureFreeAnswerID        = "30000000-0000-4000-8000-000000000013"
	fixtureFocusFrameID        = "30000000-0000-4000-8000-000000000014"
	fixtureSecondNodeID        = "30000000-0000-4000-8000-000000000015"
	fixtureSecondNodeRevision  = "30000000-0000-4000-8000-000000000016"
)

func TestEveryProposalArtifactIsGroundedInValidatedReferences(t *testing.T) {
	for _, kind := range []RequestKind{KindRoute, KindActivity, KindAssessment, KindFreeAnswer, KindExplanation} {
		kind := kind
		t.Run(string(kind), func(t *testing.T) {
			request := validProposalFixture(t, kind)
			raw, err := renderArtifact(kind, request, Scenario{Kind: ScenarioAccepted})
			if err != nil {
				t.Fatal(err)
			}
			contextValue := decodeProposalFixtureContext(t, request)
			first := contextValue.Retrieval.Hits[0]
			switch kind {
			case KindRoute:
				var output struct {
					Route []routeStep `json:"route"`
				}
				decodeFixtureOutput(t, raw, &output)
				if len(output.Route) != len(request.NodeRevisionIDs) || !strings.Contains(output.Route[0].TeachingIntent, first.Slice) || !strings.Contains(output.Route[0].CompletionCondition, first.Slice) {
					t.Fatalf("route=%+v", output.Route)
				}
			case KindActivity:
				var output struct {
					Activity activity `json:"activity"`
				}
				decodeFixtureOutput(t, raw, &output)
				if !strings.Contains(output.Activity.Prompt, first.Slice) || len(output.Activity.KnowledgeReferences) != len(request.NodeRevisionIDs) || output.Activity.KnowledgeReferences[0].SliceSHA256 != first.SliceSHA256 || output.Activity.KnowledgeReferences[0].Range != first.Range {
					t.Fatalf("activity=%+v", output.Activity)
				}
			case KindAssessment:
				var output struct {
					Assessment struct {
						Items          []assessmentItem `json:"items"`
						RubricComplete bool             `json:"rubric_complete"`
						Confidence     int              `json:"confidence"`
						RiskFlags      []string         `json:"risk_flags"`
					} `json:"assessment"`
				}
				decodeFixtureOutput(t, raw, &output)
				if len(output.Assessment.Items) != 1 || output.Assessment.Items[0].KnowledgeReferenceID != first.NodeRevisionID || output.Assessment.Items[0].KnowledgeQuote != first.Slice || output.Assessment.Items[0].KnowledgeQuoteSHA256 != first.SliceSHA256 || output.Assessment.Items[0].AnswerQuote != "candidate answer" {
					t.Fatalf("assessment=%+v", output.Assessment.Items)
				}
			case KindFreeAnswer, KindExplanation:
				var output struct {
					Text struct {
						Text                string           `json:"text"`
						KnowledgeReferences []modelReference `json:"knowledge_references"`
					} `json:"text"`
				}
				decodeFixtureOutput(t, raw, &output)
				if !strings.Contains(output.Text.Text, first.Slice) || len(output.Text.KnowledgeReferences) != len(request.NodeRevisionIDs) || output.Text.KnowledgeReferences[0].SliceSHA256 != first.SliceSHA256 || output.Text.KnowledgeReferences[0].Range != first.Range {
					t.Fatalf("text=%+v", output.Text)
				}
			}
		})
	}
}

func TestRouteStepLimitKeepsStableCanonicalPrefix(t *testing.T) {
	request := validProposalFixture(t, KindRoute)
	raw, err := renderArtifact(KindRoute, request, Scenario{Kind: ScenarioAccepted, RouteStepLimit: 1})
	if err != nil {
		t.Fatal(err)
	}
	var output struct {
		Route []routeStep `json:"route"`
	}
	decodeFixtureOutput(t, raw, &output)
	if len(output.Route) != 1 || output.Route[0].NodeRevisionID != request.NodeRevisionIDs[0] || !strings.Contains(output.Route[0].TeachingIntent, "Stable Concept Verification") {
		t.Fatalf("route=%+v", output.Route)
	}
	controller := NewController()
	if err := controller.Configure(KindRoute, Scenario{Kind: ScenarioAccepted, RouteStepLimit: -1}); err == nil {
		t.Fatal("negative route_step_limit was accepted")
	}
}

func TestCanonicalIntentIsStableNormalizedAndBounded(t *testing.T) {
	request := validProposalFixture(t, KindRoute)
	contextValue := decodeProposalFixtureContext(t, request)
	longSlice := "\t\n Stable Concept Verification \r\n premise to consequence " + strings.Repeat("filler ", 100) + "SECRET_BEYOND_BUDGET"
	contextValue.Retrieval.Hits = contextValue.Retrieval.Hits[:1]
	contextValue.Retrieval.Hits[0].Slice = longSlice
	contextValue.Retrieval.Hits[0].Range.End = contextValue.Retrieval.Hits[0].Range.Start + len(longSlice)
	contextValue.Retrieval.Hits[0].SliceSHA256 = sha256Hex(longSlice)
	request.NodeRevisionIDs = request.NodeRevisionIDs[:1]
	request.Input = encodeProposalFixtureContext(t, contextValue)

	first, err := renderArtifact(KindRoute, request, Scenario{Kind: ScenarioAccepted})
	if err != nil {
		t.Fatal(err)
	}
	second, err := renderArtifact(KindRoute, request, Scenario{Kind: ScenarioAccepted})
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Fatal("route output is not stable")
	}
	var output struct {
		Route []routeStep `json:"route"`
	}
	decodeFixtureOutput(t, first, &output)
	step := output.Route[0]
	for label, value := range map[string]string{"teaching intent": step.TeachingIntent, "completion condition": step.CompletionCondition} {
		if !utf8.ValidString(value) || strings.ContainsAny(value, "\x00\r\n\t") || strings.Contains(value, "  ") || !strings.Contains(value, "Stable Concept Verification") || strings.Contains(value, "SECRET_BEYOND_BUDGET") {
			t.Fatalf("%s was not normalized and bounded: %q", label, value)
		}
	}
}

func TestProposalContextFailsClosedOnReferenceAndSchemaErrors(t *testing.T) {
	tests := []struct {
		name   string
		kind   RequestKind
		mutate func(*proposalRequest, *proposalContext)
	}{
		{name: "missing hit", kind: KindRoute, mutate: func(_ *proposalRequest, value *proposalContext) { value.Retrieval.Hits = value.Retrieval.Hits[:1] }},
		{name: "duplicate hit", kind: KindRoute, mutate: func(_ *proposalRequest, value *proposalContext) { value.Retrieval.Hits[1] = value.Retrieval.Hits[0] }},
		{name: "stale retrieval knowledge", kind: KindActivity, mutate: func(_ *proposalRequest, value *proposalContext) {
			value.Retrieval.KnowledgeRevisionID = fixtureDocumentRevisionID
		}},
		{name: "invalid document authority", kind: KindActivity, mutate: func(_ *proposalRequest, value *proposalContext) {
			value.Retrieval.Hits[0].DocumentRevisionID = "not-a-uuid"
		}},
		{name: "invalid node authority", kind: KindExplanation, mutate: func(_ *proposalRequest, value *proposalContext) { value.Retrieval.Hits[0].NodeID = "not-a-uuid" }},
		{name: "stale node revision set", kind: KindFreeAnswer, mutate: func(_ *proposalRequest, value *proposalContext) {
			value.Retrieval.Hits[0].NodeRevisionID = fixtureFreeAnswerID
		}},
		{name: "hash mismatch", kind: KindRoute, mutate: func(_ *proposalRequest, value *proposalContext) {
			value.Retrieval.Hits[0].SliceSHA256 = strings.Repeat("0", 64)
		}},
		{name: "range length mismatch", kind: KindActivity, mutate: func(_ *proposalRequest, value *proposalContext) { value.Retrieval.Hits[0].Range.End++ }},
		{name: "range boundary", kind: KindExplanation, mutate: func(_ *proposalRequest, value *proposalContext) {
			value.Retrieval.Hits[0].Range.Start = value.Retrieval.Hits[0].Range.End
		}},
		{name: "duplicate requested node", kind: KindRoute, mutate: func(request *proposalRequest, _ *proposalContext) {
			request.NodeRevisionIDs[1] = request.NodeRevisionIDs[0]
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := validProposalFixture(t, test.kind)
			contextValue := decodeProposalFixtureContext(t, request)
			test.mutate(&request, &contextValue)
			request.Input = encodeProposalFixtureContext(t, contextValue)
			if _, err := renderArtifact(test.kind, request, Scenario{Kind: ScenarioAccepted}); err == nil {
				t.Fatal("invalid context was accepted")
			}
		})
	}

	reference := validContextReferences()[0]
	reference.Slice = string([]byte{0xff})
	if err := validateContextReference(reference, fixtureKnowledgeRevisionID); err == nil {
		t.Fatal("invalid UTF-8 canonical slice was accepted")
	}

	request := validProposalFixture(t, KindRoute)
	var raw map[string]any
	if err := json.Unmarshal(request.Input, &raw); err != nil {
		t.Fatal(err)
	}
	raw["unknown_context_field"] = true
	encoded, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	request.Input = encoded
	if _, err := renderArtifact(KindRoute, request, Scenario{Kind: ScenarioAccepted}); err == nil {
		t.Fatal("unknown context field was accepted")
	}
}

func TestEveryProposalKindRejectsStaleWorkItemAuthority(t *testing.T) {
	tests := []struct {
		name   string
		kind   RequestKind
		mutate func(*proposalRequest, *proposalContext)
	}{
		{name: "route goal", kind: KindRoute, mutate: func(_ *proposalRequest, value *proposalContext) {
			value.WorkItem.GoalRevision.GoalRevisionID = fixtureDocumentRevisionID
		}},
		{name: "activity route step", kind: KindActivity, mutate: func(_ *proposalRequest, value *proposalContext) {
			value.WorkItem.RouteRevision.Steps[0].NodeRevisionID = fixtureSecondNodeRevision
		}},
		{name: "assessment activity", kind: KindAssessment, mutate: func(_ *proposalRequest, value *proposalContext) {
			value.WorkItem.Activity.ActivityID = fixtureFreeAnswerID
		}},
		{name: "assessment attempt", kind: KindAssessment, mutate: func(_ *proposalRequest, value *proposalContext) {
			value.WorkItem.Attempt.AttemptID = fixtureFreeAnswerID
		}},
		{name: "free answer question", kind: KindFreeAnswer, mutate: func(_ *proposalRequest, value *proposalContext) {
			value.WorkItem.FreeQuestion.FocusFrameID = fixtureFreeAnswerID
		}},
		{name: "explanation route", kind: KindExplanation, mutate: func(_ *proposalRequest, value *proposalContext) {
			value.WorkItem.RouteRevision.RouteRevisionID = fixtureFreeAnswerID
		}},
		{name: "assessment reference document", kind: KindAssessment, mutate: func(_ *proposalRequest, value *proposalContext) {
			value.WorkItem.Activity.KnowledgeReferences[0].DocumentRevisionID = fixtureFreeAnswerID
		}},
		{name: "assessment target node", kind: KindAssessment, mutate: func(_ *proposalRequest, value *proposalContext) {
			value.WorkItem.Activity.TargetNodeID = fixtureSecondNodeID
		}},
		{name: "attached activity free answer", kind: KindActivity, mutate: func(request *proposalRequest, value *proposalContext) {
			request.TutoringState = "FreeAnswer"
			request.FreeQuestionID = fixtureFreeQuestionID
			request.FreeAnswerID = fixtureFreeAnswerID
			request.FocusFrameID = fixtureFocusFrameID
			value.WorkItem.FreeQuestion = &contextFreeQuestion{
				FreeQuestionID: fixtureFreeQuestionID, SessionID: fixtureSessionID,
				FocusFrameID: fixtureFocusFrameID, KnowledgeRevisionID: fixtureKnowledgeRevisionID,
			}
			value.WorkItem.FreeAnswer = &contextFreeAnswer{
				FreeAnswerID: fixtureSecondNodeRevision, SessionID: fixtureSessionID,
				FocusFrameID: fixtureFocusFrameID, FreeQuestionID: fixtureFreeQuestionID,
				KnowledgeRevisionID: fixtureKnowledgeRevisionID,
			}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := validProposalFixture(t, test.kind)
			contextValue := decodeProposalFixtureContext(t, request)
			test.mutate(&request, &contextValue)
			request.Input = encodeProposalFixtureContext(t, contextValue)
			if _, err := renderArtifact(test.kind, request, Scenario{Kind: ScenarioAccepted}); err == nil {
				t.Fatal("stale work-item authority was accepted")
			}
		})
	}
}

func validProposalFixture(t *testing.T, kind RequestKind) proposalRequest {
	t.Helper()
	references := validContextReferences()
	workItem := contextWorkItem{
		AllowedActions:             []string{},
		AllowedAssessmentDecisions: []string{},
		GoalRevision:               &contextGoalRevision{GoalRevisionID: fixtureGoalRevisionID},
		RouteRevision: &contextRouteRevision{
			RouteRevisionID: fixtureRouteRevisionID, GoalRevisionID: fixtureGoalRevisionID,
			KnowledgeRevisionID: fixtureKnowledgeRevisionID,
			Steps:               []contextRouteStep{{RouteStepID: fixtureRouteStepID, NodeID: fixtureNodeID, NodeRevisionID: fixtureNodeRevisionID}},
		},
	}
	request := proposalRequest{
		RequestID: fixtureRequestID, ProposalType: kind, AggregateType: "session", AggregateID: fixtureSessionID,
		AggregateVersion: 7, GoalRevisionID: fixtureGoalRevisionID, RouteRevisionID: fixtureRouteRevisionID,
		RouteStepID: fixtureRouteStepID, FocusNodeRevisionID: fixtureNodeRevisionID,
		TutoringState: "RouteActive", KnowledgeRevisionID: fixtureKnowledgeRevisionID,
		NodeRevisionIDs: []string{fixtureNodeRevisionID, fixtureSecondNodeRevision},
	}
	switch kind {
	case KindRoute:
	case KindActivity:
	case KindAssessment:
		request.ActivityID = fixtureActivityID
		request.AttemptID = fixtureAttemptID
		request.TutoringState = "Evaluating"
		workItem.Activity = &contextActivity{
			ActivityID: fixtureActivityID, SessionID: fixtureSessionID, GoalRevisionID: fixtureGoalRevisionID,
			RouteRevisionID: fixtureRouteRevisionID, RouteStepID: fixtureRouteStepID,
			KnowledgeRevisionID: fixtureKnowledgeRevisionID, TargetNodeID: fixtureNodeID,
			TargetNodeRevisionID: fixtureNodeRevisionID, KnowledgeReferences: references,
			Rubric: contextRubric{RubricRevision: "rubric-v1", Items: []contextRubricItem{{
				RubricItemID: "rubric-item-1", Criterion: "supported answer", RequiredReferenceIDs: []string{fixtureNodeRevisionID},
			}}},
		}
		workItem.Attempt = &contextAttempt{AttemptID: fixtureAttemptID, SessionID: fixtureSessionID, ActivityID: fixtureActivityID, Answer: "candidate answer"}
	case KindFreeAnswer:
		request.FreeQuestionID = fixtureFreeQuestionID
		request.FocusFrameID = fixtureFocusFrameID
		request.TutoringState = "FreeQuestion"
		workItem.FreeQuestion = &contextFreeQuestion{
			FreeQuestionID: fixtureFreeQuestionID, SessionID: fixtureSessionID, FocusFrameID: fixtureFocusFrameID,
			KnowledgeRevisionID: fixtureKnowledgeRevisionID,
		}
	case KindExplanation:
	default:
		t.Fatalf("unsupported fixture kind %q", kind)
	}
	contextValue := proposalContext{SchemaVersion: proposalContextSchemaVersion, WorkItem: workItem}
	contextValue.Retrieval.KnowledgeRevisionID = fixtureKnowledgeRevisionID
	contextValue.Retrieval.Hits = references
	request.Input = encodeProposalFixtureContext(t, contextValue)
	return request
}

func validContextReferences() []contextReference {
	firstSlice := "Stable Concept Verification"
	secondSlice := "A second canonical concept"
	return []contextReference{
		{
			KnowledgeRevisionID: fixtureKnowledgeRevisionID, DocumentRevisionID: fixtureDocumentRevisionID,
			NodeID: fixtureNodeID, NodeRevisionID: fixtureNodeRevisionID,
			Range: sourceRange{Start: 10, End: 10 + len(firstSlice)}, Slice: firstSlice, SliceSHA256: sha256Hex(firstSlice),
		},
		{
			KnowledgeRevisionID: fixtureKnowledgeRevisionID, DocumentRevisionID: fixtureDocumentRevisionID,
			NodeID: fixtureSecondNodeID, NodeRevisionID: fixtureSecondNodeRevision,
			Range: sourceRange{Start: 100, End: 100 + len(secondSlice)}, Slice: secondSlice, SliceSHA256: sha256Hex(secondSlice),
		},
	}
}

func decodeProposalFixtureContext(t *testing.T, request proposalRequest) proposalContext {
	t.Helper()
	var value proposalContext
	if err := decodeStrict(request.Input, &value); err != nil {
		t.Fatal(err)
	}
	return value
}

func encodeProposalFixtureContext(t *testing.T, value proposalContext) json.RawMessage {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func decodeFixtureOutput(t *testing.T, raw []byte, target any) {
	t.Helper()
	if err := decodeStrict(raw, target); err != nil {
		t.Fatal(err)
	}
}
