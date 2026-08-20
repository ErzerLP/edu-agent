package learning

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/edu-agent/edu-agent/server/internal/tutoring"
)

func TestProposalGenerationRejectsStaleAggregateBeforeModel(t *testing.T) {
	request := routeProposalRequest()
	store := routeProposalStore()
	store.session.AggregateVer = request.AggregateVersion + 1
	repo := &proposalTestRepository{claim: ProposalClaim{State: "claimed", LeaseToken: "lease"}}
	model := &proposalTestModel{results: []proposalModelResult{{raw: json.RawMessage(`{"route":[{"node_revision_id":"node-revision","teaching_intent":"teach","completion_condition":"pass"}]}`)}}}
	service := newProposalTestService(t, store, repo, model)
	_, err := service.Propose(context.Background(), "device", request)
	if ErrorCode(err) != CodeStaleProposal || model.calls != 0 || repo.failedCategory != "stale_context" || len(repo.failedAttempts) != 1 || repo.failedAttempts[0] != "stale_context" {
		t.Fatalf("stale generation err=%v calls=%d category=%q attempts=%v", err, model.calls, repo.failedCategory, repo.failedAttempts)
	}
}

func TestProposalGenerationEnforcesNodeAllowListWithoutRetry(t *testing.T) {
	store := routeProposalStore()
	repo := &proposalTestRepository{claim: ProposalClaim{State: "claimed", LeaseToken: "lease"}}
	model := &proposalTestModel{results: []proposalModelResult{{raw: json.RawMessage(`{"route":[{"node_revision_id":"outside-node","teaching_intent":"teach","completion_condition":"pass"}]}`)}}}
	service := newProposalTestService(t, store, repo, model)
	_, err := service.Propose(context.Background(), "device", routeProposalRequest())
	if ErrorCode(err) != CodeProposalRejected || model.calls != 1 || repo.failedCategory != "domain_invalid" || len(repo.failedAttempts) != 1 || repo.failedAttempts[0] != "domain_invalid" {
		t.Fatalf("allow-list err=%v calls=%d category=%q attempts=%v", err, model.calls, repo.failedCategory, repo.failedAttempts)
	}
}

func TestProposalPermanentModelFailureDoesNotRetry(t *testing.T) {
	store := routeProposalStore()
	repo := &proposalTestRepository{claim: ProposalClaim{State: "claimed", LeaseToken: "lease"}}
	model := &proposalTestModel{results: []proposalModelResult{{err: proposalModelError("unauthorized")}}}
	service := newProposalTestService(t, store, repo, model)
	_, err := service.Propose(context.Background(), "device", routeProposalRequest())
	if ErrorCode(err) != CodeProposalRejected || model.calls != 1 || repo.failedCategory != "unauthorized" || len(repo.failedAttempts) != 1 {
		t.Fatalf("permanent failure err=%v calls=%d category=%q attempts=%v", err, model.calls, repo.failedCategory, repo.failedAttempts)
	}
}

func TestProposalFailedReplayPreservesUnavailableCategory(t *testing.T) {
	store := &proposalTestStore{}
	repo := &proposalTestRepository{claim: ProposalClaim{State: "failed", Category: "not_configured"}}
	model := &proposalTestModel{}
	service := newProposalTestService(t, store, repo, model)
	_, err := service.Propose(context.Background(), "device", routeProposalRequest())
	if ErrorCode(err) != CodeModelUnavailable || model.calls != 0 {
		t.Fatalf("failed replay err=%v calls=%d", err, model.calls)
	}
}

func TestProposalNilModelInitialAndReplayRemainUnavailable(t *testing.T) {
	store := routeProposalStore()
	repo := &proposalTestRepository{claim: ProposalClaim{State: "claimed", LeaseToken: "lease"}}
	service := newProposalTestService(t, store, repo, nil)
	request := routeProposalRequest()
	if _, err := service.Propose(context.Background(), "device", request); ErrorCode(err) != CodeModelUnavailable || repo.failedCategory != "not_configured" {
		t.Fatalf("initial nil-model proposal err=%v category=%q", err, repo.failedCategory)
	}
	repo.claim = ProposalClaim{State: "failed", Category: repo.failedCategory}
	if _, err := service.Propose(context.Background(), "device", request); ErrorCode(err) != CodeModelUnavailable {
		t.Fatalf("replayed nil-model proposal err=%v", err)
	}
}

func TestProposalApplyRejectsFrozenTamperAndArtifactNodeEscape(t *testing.T) {
	sessionID := routeProposalRequest().AggregateID
	base := tutoring.Session{ID: sessionID, State: tutoring.StateDiagnostic, AggregateVer: 7, Context: tutoring.FocusContext{GoalRevisionID: "goal-revision"}}
	operation := coordinatorOperation("77000000-0000-4000-8000-000000000001", sessionID, 7)
	command := ActionCommand{Operation: operation, Action: tutoring.ActionApplyRoute, ProposalID: "proposal"}

	for _, test := range []struct {
		name   string
		mutate func(*ProposalArtifact)
		code   string
	}{
		{name: "frozen request hash", code: CodeStaleProposal, mutate: func(value *ProposalArtifact) { value.FrozenRequest.Input = json.RawMessage(`{"tampered":true}`) }},
		{name: "artifact node escape", code: CodeProposalRejected, mutate: func(value *ProposalArtifact) { value.Route[0].NodeRevisionID = "outside-node" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			artifact := frozenRouteProposal(base, routeProposalRequest().KnowledgeRevisionID, "node-revision")
			artifact.ID = "proposal"
			test.mutate(&artifact)
			store := &proposalTestStore{session: base, goal: GoalRevision{ID: "goal-revision", GoalID: "goal", Revision: 1}, proposal: artifact}
			service := newProposalTestService(t, store, &proposalTestRepository{}, nil)
			_, err := service.ApplyAction(context.Background(), "77000000-0000-4000-8000-000000000099", sessionID, command)
			if ErrorCode(err) != test.code || store.commits != 0 {
				t.Fatalf("apply err=%v commits=%d", err, store.commits)
			}
		})
	}
}
