package learning

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
)

type feedbackModel struct {
	results  []json.RawMessage
	requests []ProposalRequest
	feedback []string
}

func (m *feedbackModel) Generate(ctx context.Context, request ProposalRequest) (json.RawMessage, error) {
	m.requests = append(m.requests, request)
	m.feedback = append(m.feedback, ProposalRetryReason(ctx))
	return m.results[len(m.requests)-1], nil
}

func TestProposalRepairFeedbackPreservesFrozenRequestAndFullValidation(t *testing.T) {
	invalid := json.RawMessage(`{"route":[{"node_revision_id":"node-revision","teaching_intent":"teach","completion_condition":"pass","invented_state":"mastered"}]}`)
	valid := json.RawMessage(`{"route":[{"node_revision_id":"node-revision","teaching_intent":"teach","completion_condition":"pass"}]}`)
	for _, retry := range []struct {
		name     string
		raw      json.RawMessage
		accepted bool
	}{
		{"repair", valid, true}, {"still_malformed", invalid, false},
		{"scope_escape", json.RawMessage(`{"route":[{"node_revision_id":"unselected-node","teaching_intent":"teach","completion_condition":"pass"}]}`), false},
	} {
		t.Run(retry.name, func(t *testing.T) {
			repo := &proposalTestRepository{claim: ProposalClaim{State: "claimed", LeaseToken: "lease"}}
			model := &feedbackModel{results: []json.RawMessage{invalid, retry.raw}}
			store := routeProposalStore()
			service := newProposalTestService(t, store, repo, model)
			artifact, err := service.Propose(t.Context(), "device", routeProposalRequest())
			if len(model.requests) != 2 || !reflect.DeepEqual(model.requests[0], model.requests[1]) || !reflect.DeepEqual(model.feedback, []string{"", "schema_mismatch"}) || store.commits != 0 {
				t.Fatalf("unbounded retry or frozen input changed: calls=%d feedback=%v", len(model.requests), model.feedback)
			}
			if retry.accepted {
				if err != nil || repo.completed == nil || string(artifact.FrozenRequest.Input) != `{"goal":"fractions"}` || !reflect.DeepEqual(artifact.AttemptCategories, []string{"schema_mismatch", "success"}) {
					t.Fatalf("repair failed: %v categories=%v", err, artifact.AttemptCategories)
				}
			} else if ErrorCode(err) != CodeProposalRejected || repo.completed != nil || len(repo.failedAttempts) != 2 {
				t.Fatalf("retry output bypassed validation: %v", err)
			}
		})
	}
}
