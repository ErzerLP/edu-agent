package postgresstore

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/learning"
	"github.com/edu-agent/edu-agent/server/internal/tutoring"
)

func TestOfflinePrepareClaimEnvelopePreservesFrozenGenerationPlan(t *testing.T) {
	request, generation := offlinePrepareClaimFixture()
	frozenBytes, err := canonicalOfflineStoreValue(generation)
	if err != nil {
		t.Fatal(err)
	}
	body, err := encodeOfflinePrepareClaimRequest(request, generation)
	if err != nil {
		t.Fatal(err)
	}
	generation.Route.Steps[0].TeachingIntent = "changed after first claim"
	generation.CurrentActivity.Prompt = "changed after first claim"
	requestHash, err := request.Request.CanonicalHash()
	if err != nil {
		t.Fatal(err)
	}
	recovered, err := recoverOfflinePrepareGenerationPlan(body, nil, request, requestHash)
	if err != nil {
		t.Fatal(err)
	}
	if recovered == nil {
		t.Fatal("frozen generation plan was omitted")
	}
	recoveredBytes, err := canonicalOfflineStoreValue(*recovered)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(recoveredBytes, frozenBytes) {
		t.Fatalf("generation plan changed\nfrozen=%s\nrecovered=%s", frozenBytes, recoveredBytes)
	}
	reencoded, err := encodeOfflinePrepareClaimRequest(request, *recovered)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(reencoded, body) {
		t.Fatalf("claim envelope bytes changed\nfirst=%s\nreencoded=%s", body, reencoded)
	}
}

func TestOfflinePrepareClaimEnvelopeIsClosed(t *testing.T) {
	request, generation := offlinePrepareClaimFixture()
	body, err := encodeOfflinePrepareClaimRequest(request, generation)
	if err != nil {
		t.Fatal(err)
	}
	var value map[string]any
	if err := json.Unmarshal(body, &value); err != nil {
		t.Fatal(err)
	}
	value["future_plan"] = true
	invalid, err := canonicalOfflineStoreValue(value)
	if err != nil {
		t.Fatal(err)
	}
	requestHash, err := request.Request.CanonicalHash()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := recoverOfflinePrepareGenerationPlan(invalid, nil, request, requestHash); err == nil {
		t.Fatal("claim envelope with an unknown field was accepted")
	}
}

func TestLegacyOfflinePrepareClaimWithoutArtifactFailsClosed(t *testing.T) {
	request, _ := offlinePrepareClaimFixture()
	legacy, err := canonicalOfflineStoreValue(request.Request)
	if err != nil {
		t.Fatal(err)
	}
	requestHash, err := request.Request.CanonicalHash()
	if err != nil {
		t.Fatal(err)
	}
	_, recoverErr := recoverOfflinePrepareGenerationPlan(legacy, nil, request, requestHash)
	var domainErr *learning.Error
	if !errors.As(recoverErr, &domainErr) || domainErr.Code != learning.CodeOfflinePrepareUnavailable || domainErr.Reason != "offline_prepare_claim_plan_missing" {
		t.Fatalf("legacy claim error=%v", recoverErr)
	}
	plan, err := recoverOfflinePrepareGenerationPlan(legacy, []byte(`{"protocol_version":1}`), request, requestHash)
	if err != nil || plan != nil {
		t.Fatalf("legacy artifact recovery plan=%+v err=%v", plan, err)
	}
}

func TestOfflinePreparePublishAuthorityRejectsFrozenPlanAfterRouteChange(t *testing.T) {
	_, generation := offlinePrepareClaimFixture()
	current := learning.CloneActivity(*generation.CurrentActivity)
	artifact := learning.OfflinePrepareArtifact{
		ProtocolVersion: 1, SessionID: generation.SessionID, SessionState: generation.SessionState,
		ExpectedSessionVersion: generation.ExpectedSessionVersion, GoalRevisionID: generation.GoalRevisionID,
		RouteRevisionID: generation.Route.ID, RouteStepID: generation.RouteStepID,
		KnowledgeRevisionID: generation.KnowledgeRevisionID, Activities: []learning.Activity{current},
	}
	authority := offlinePrepareAuthority{
		generation: 1, credentialEpoch: 1,
		session: tutoring.Session{
			ID: generation.SessionID, State: tutoring.StateAwaitingResponse, AggregateVer: generation.ExpectedSessionVersion,
			Context: tutoring.FocusContext{
				GoalRevisionID: generation.GoalRevisionID, RouteRevisionID: generation.Route.ID,
				RouteStepID: generation.RouteStepID, KnowledgeRevisionID: generation.KnowledgeRevisionID,
				FocusNodeRevisionID: generation.Route.Steps[0].NodeRevisionID,
			},
		},
		route: generation.Route, currentActivity: &current,
	}
	authority.route.Steps[0].NodeRevisionID = "a2000000-0000-4000-8000-000000000001"
	err := validateOfflinePrepareArtifactAuthority(artifact, authority)
	var domainErr *learning.Error
	if !errors.As(err, &domainErr) || domainErr.Code != learning.CodeOfflinePrepareUnavailable || domainErr.Reason != "offline_prepare_route_changed" {
		t.Fatalf("changed route publish validation error=%v", err)
	}
}

func offlinePrepareClaimFixture() (learning.OfflinePrepareStoreRequest, learning.OfflinePrepareGenerationRequest) {
	const (
		deviceID       = "a1000000-0000-4000-8000-000000000001"
		operationID    = "a1000000-0000-4000-8000-000000000002"
		sessionID      = "a1000000-0000-4000-8000-000000000003"
		goalID         = "a1000000-0000-4000-8000-000000000004"
		routeID        = "a1000000-0000-4000-8000-000000000005"
		routeRootID    = "a1000000-0000-4000-8000-000000000006"
		stepID         = "a1000000-0000-4000-8000-000000000007"
		knowledgeID    = "a1000000-0000-4000-8000-000000000008"
		nodeID         = "a1000000-0000-4000-8000-000000000009"
		nodeRevisionID = "a1000000-0000-4000-8000-00000000000a"
		documentID     = "a1000000-0000-4000-8000-00000000000b"
		activityID     = "a1000000-0000-4000-8000-00000000000c"
	)
	count := 5
	request := learning.OfflinePrepareStoreRequest{
		DeviceID: deviceID,
		Request: learning.OfflinePrepareRequest{
			OperationID: operationID, PayloadSchemaVersion: 1, ExpectedSessionVersion: "7",
			TrustedManifestRevision: "1", TrustedManifestDigest: learning.OfflineZeroDigest,
			RequestedCount: &count,
		},
		Count: count, TTL: 24 * time.Hour,
	}
	route := learning.RouteRevision{
		ID: routeID, RouteID: routeRootID, Revision: 1, GoalRevisionID: goalID,
		KnowledgeRevisionID: knowledgeID, PolicyVersion: learning.RoutePolicyVersion,
		Steps: []learning.RouteStep{{
			ID: stepID, Ordinal: 0, NodeID: nodeID, NodeRevisionID: nodeRevisionID,
			TeachingIntent: "teach", CompletionCondition: "pass",
		}},
	}
	reference := learning.KnowledgeReference{
		KnowledgeRevisionID: knowledgeID, NodeID: nodeID, NodeRevisionID: nodeRevisionID,
		DocumentRevisionID: documentID, Range: learning.SourceRange{Start: 0, End: 5},
		Slice: "topic", SliceSHA256: learning.SHA256([]byte("topic")),
	}
	activity := &learning.Activity{
		ID: activityID, Revision: 1, SessionID: sessionID, GoalRevisionID: goalID,
		RouteRevisionID: routeID, RouteStepID: stepID, KnowledgeRevisionID: knowledgeID,
		TargetNodeID: nodeID, TargetNodeRevisionID: nodeRevisionID, References: []learning.KnowledgeReference{reference},
		Prompt: "current question", Type: learning.ActivityObjective,
		Rubric:     learning.Rubric{Revision: "r1", Items: []learning.RubricItem{{ID: "item-1", Criterion: "correct"}}, ObjectiveRule: &learning.ObjectiveRule{AcceptedAnswers: []string{"ok"}}},
		Difficulty: 1, AllowedHelp: []learning.HelpLevel{learning.HelpNone},
		ActivityPolicyVersion: learning.ActivityPolicyVersion, AssessmentPolicyVersion: learning.AssessmentPolicyVersion,
		ReviewPolicyVersion: learning.ReviewPolicyVersion, CreatedAt: time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC),
	}
	generation := learning.OfflinePrepareGenerationRequest{
		DeviceID: deviceID, OperationID: operationID, Count: count, SessionID: sessionID,
		SessionState: string(tutoring.StateAwaitingResponse), ExpectedSessionVersion: 7,
		GoalRevisionID: goalID, Route: route, RouteStepID: stepID,
		KnowledgeRevisionID: knowledgeID, CurrentActivity: activity,
	}
	return request, generation
}
