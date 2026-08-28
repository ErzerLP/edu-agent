package learning

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/tutoring"
)

type offlinePrepareTestResolver map[string]KnowledgeReference

func (r offlinePrepareTestResolver) Resolve(_ context.Context, knowledgeRevisionID, nodeRevisionID string) (KnowledgeReference, error) {
	value, ok := r[nodeRevisionID]
	if !ok || value.KnowledgeRevisionID != knowledgeRevisionID {
		return KnowledgeReference{}, fmt.Errorf("unknown offline prepare node %s", nodeRevisionID)
	}
	return value, nil
}

type offlinePrepareTestModel struct {
	calls    int
	failFrom int
}

func (m *offlinePrepareTestModel) Generate(_ context.Context, request ProposalRequest) (json.RawMessage, error) {
	m.calls++
	if m.failFrom > 0 && m.calls >= m.failFrom {
		return nil, proposalModelError("unavailable")
	}
	value := map[string]any{"activity": map[string]any{
		"prompt": "practice " + request.FocusNodeRevisionID,
		"type":   "objective",
		"rubric": map[string]any{
			"rubric_revision": "offline-r1",
			"items": []any{map[string]any{
				"rubric_item_id": "item-1", "criterion": "answer correctly",
				"required_reference_ids": []string{request.FocusNodeRevisionID},
			}},
			"objective_rule": map[string]any{"accepted_answers": []string{"ok"}, "case_sensitive": false, "trim_space": true},
		},
		"difficulty":           1,
		"allowed_help":         []string{"none"},
		"knowledge_references": []any{map[string]any{"node_revision_id": request.FocusNodeRevisionID}},
	}}
	encoded, err := json.Marshal(value)
	return encoded, err
}

func TestGenerateOfflinePrepareRouteActiveBoundedCounts(t *testing.T) {
	for _, count := range []int{1, 5, 20} {
		t.Run(fmt.Sprintf("count_%d", count), func(t *testing.T) {
			service, model, request := offlinePrepareGeneratorFixture(t, count, false, 0)
			artifact, err := service.GenerateOfflinePrepare(t.Context(), request)
			if err != nil {
				t.Fatal(err)
			}
			if len(artifact.Activities) != count || artifact.ModelPartial || model.calls != count {
				t.Fatalf("activities=%d partial=%v model_calls=%d", len(artifact.Activities), artifact.ModelPartial, model.calls)
			}
			seen := map[string]bool{}
			for index, activity := range artifact.Activities {
				if seen[activity.ID] || activity.RouteStepID != request.Route.Steps[index].ID || activity.TargetNodeRevisionID != request.Route.Steps[index].NodeRevisionID {
					t.Fatalf("activity[%d]=%+v", index, activity)
				}
				seen[activity.ID] = true
			}
		})
	}
}

func TestGenerateOfflinePrepareAwaitingResponseKeepsCurrentActivityFirst(t *testing.T) {
	service, model, request := offlinePrepareGeneratorFixture(t, 5, true, 0)
	currentID := request.CurrentActivity.ID
	artifact, err := service.GenerateOfflinePrepare(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if len(artifact.Activities) != 5 || artifact.Activities[0].ID != currentID || model.calls != 4 {
		t.Fatalf("activities=%d first=%s model_calls=%d", len(artifact.Activities), artifact.Activities[0].ID, model.calls)
	}
	if artifact.Activities[1].RouteStepID != request.Route.Steps[1].ID {
		t.Fatalf("first generated route step=%s", artifact.Activities[1].RouteStepID)
	}
}

func TestGenerateOfflinePrepareStopsWithStableModelPartial(t *testing.T) {
	service, model, request := offlinePrepareGeneratorFixture(t, 5, false, 3)
	artifact, err := service.GenerateOfflinePrepare(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if len(artifact.Activities) != 2 || !artifact.ModelPartial || model.calls != 4 {
		t.Fatalf("activities=%d partial=%v model_calls=%d", len(artifact.Activities), artifact.ModelPartial, model.calls)
	}
}

func TestGenerateOfflinePrepareStopsAtRouteEndWithoutModelPartial(t *testing.T) {
	service, model, request := offlinePrepareGeneratorFixture(t, 5, false, 0)
	request.Route.Steps = request.Route.Steps[:3]
	artifact, err := service.GenerateOfflinePrepare(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if len(artifact.Activities) != 3 || artifact.ModelPartial || model.calls != 3 {
		t.Fatalf("activities=%d partial=%v model_calls=%d", len(artifact.Activities), artifact.ModelPartial, model.calls)
	}
}

func offlinePrepareGeneratorFixture(t *testing.T, count int, awaiting bool, failFrom int) (*Service, *offlinePrepareTestModel, OfflinePrepareGenerationRequest) {
	t.Helper()
	const (
		deviceID    = "81000000-0000-4000-8000-000000000001"
		operationID = "81000000-0000-4000-8000-000000000002"
		sessionID   = "81000000-0000-4000-8000-000000000003"
		goalID      = "81000000-0000-4000-8000-000000000004"
		routeID     = "81000000-0000-4000-8000-000000000005"
		knowledgeID = "81000000-0000-4000-8000-000000000006"
		documentID  = "81000000-0000-4000-8000-000000000007"
	)
	resolver := offlinePrepareTestResolver{}
	route := RouteRevision{ID: routeID, RouteID: "81000000-0000-4000-8000-000000000008", Revision: 1, GoalRevisionID: goalID, KnowledgeRevisionID: knowledgeID, PolicyVersion: RoutePolicyVersion}
	for index := 0; index < 20; index++ {
		stepID := fmt.Sprintf("82000000-0000-4000-8000-%012x", index+1)
		nodeID := fmt.Sprintf("83000000-0000-4000-8000-%012x", index+1)
		nodeRevisionID := fmt.Sprintf("84000000-0000-4000-8000-%012x", index+1)
		route.Steps = append(route.Steps, RouteStep{ID: stepID, Ordinal: index, NodeID: nodeID, NodeRevisionID: nodeRevisionID, TeachingIntent: "teach", CompletionCondition: "pass"})
		resolver[nodeRevisionID] = KnowledgeReference{KnowledgeRevisionID: knowledgeID, NodeID: nodeID, NodeRevisionID: nodeRevisionID, DocumentRevisionID: documentID, Range: SourceRange{Start: 0, End: 5}, Slice: "topic", SliceSHA256: SHA256([]byte("topic"))}
	}
	model := &offlinePrepareTestModel{failFrom: failFrom}
	now := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	idIndex := 1
	newID := func() string {
		value := fmt.Sprintf("85000000-0000-4000-8000-%012x", idIndex)
		idIndex++
		return value
	}
	service, err := NewService(&proposalTestStore{}, &proposalTestRepository{}, resolver, ServiceOptions{Now: func() time.Time { return now }, NewUUID: newID, Model: model, ModelID: "strict-fake", ModelParameters: map[string]any{}, PromptRevision: "test-offline"})
	if err != nil {
		t.Fatal(err)
	}
	state := tutoring.StateRouteActive
	request := OfflinePrepareGenerationRequest{
		DeviceID: deviceID, OperationID: operationID, Count: count, SessionID: sessionID,
		SessionState: string(state), ExpectedSessionVersion: 7, GoalRevisionID: goalID,
		Route: route, RouteStepID: route.Steps[0].ID, KnowledgeRevisionID: knowledgeID,
	}
	if awaiting {
		state = tutoring.StateAwaitingResponse
		request.SessionState = string(state)
		ref := resolver[route.Steps[0].NodeRevisionID]
		request.CurrentActivity = &Activity{
			ID: "86000000-0000-4000-8000-000000000001", Revision: 1, SessionID: sessionID,
			GoalRevisionID: goalID, RouteRevisionID: route.ID, RouteStepID: route.Steps[0].ID,
			KnowledgeRevisionID: knowledgeID, TargetNodeID: route.Steps[0].NodeID,
			TargetNodeRevisionID: route.Steps[0].NodeRevisionID, References: []KnowledgeReference{ref},
			Prompt: "current", Type: ActivityObjective,
			Rubric:     Rubric{Revision: "current-r1", Items: []RubricItem{{ID: "item-1", Criterion: "correct"}}, ObjectiveRule: &ObjectiveRule{AcceptedAnswers: []string{"ok"}}},
			Difficulty: 1, AllowedHelp: []HelpLevel{HelpNone}, ActivityPolicyVersion: ActivityPolicyVersion,
			AssessmentPolicyVersion: AssessmentPolicyVersion, ReviewPolicyVersion: ReviewPolicyVersion, CreatedAt: now,
		}
	}
	return service, model, request
}
