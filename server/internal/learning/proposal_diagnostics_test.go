package learning

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
)

func TestProposalFailurePreservesFixedReason(t *testing.T) {
	for _, reason := range []string{"reference_hash_mismatch", "activity_target_reference_missing", "invalid_knowledge_quote", "unknown_risk_flag"} {
		if got := proposalFailureCategory(&Error{Code: CodeProposalRejected, Reason: reason}); got != reason {
			t.Errorf("reason %q collapsed to %q", reason, got)
		}
	}
	if got := proposalFailureCategory(&Error{Code: CodeProposalRejected, Reason: "private-source-and-key"}); got != "domain_invalid" {
		t.Fatalf("unknown reason escaped classification: %q", got)
	}
}

func TestProposalFailureLogsClassificationWithoutModelBody(t *testing.T) {
	var logs bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })
	repo := &proposalTestRepository{claim: ProposalClaim{State: "claimed", LeaseToken: "private-lease"}}
	model := &proposalTestModel{results: []proposalModelResult{{raw: json.RawMessage(`{"route":[{"node_revision_id":"outside-node","teaching_intent":"private-model-body","completion_condition":"pass"}]}`)}}}
	service := newProposalTestService(t, routeProposalStore(), repo, model)
	_, err := service.Propose(context.Background(), "device", routeProposalRequest())
	var failure *Error
	if !errors.As(err, &failure) || failure.Reason != "invalid_route_step" || repo.failedCategory != "invalid_route_step" {
		t.Fatalf("rejection lost reason: %v, stored=%q", err, repo.failedCategory)
	}
	if !strings.Contains(logs.String(), `"error_category":"invalid_route_step"`) || strings.Contains(logs.String(), "private-") || strings.Contains(logs.String(), "outside-node") {
		t.Fatalf("unsafe or missing rejection diagnostic: %s", &logs)
	}
}

func TestProposalRequestRejectionLogsFixedReasonBeforeModel(t *testing.T) {
	var logs bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })
	repo := &proposalTestRepository{claim: ProposalClaim{State: "claimed", LeaseToken: "private-lease"}}
	model := &proposalTestModel{}
	service := newProposalTestService(t, routeProposalStore(), repo, model)
	request := routeProposalRequest()
	request.NodeRevisionIDs = []string{"private-node", "private-node"}
	_, err := service.Propose(context.Background(), "device", request)
	if ErrorCode(err) != CodeProposalRejected || model.calls != 0 || repo.completed != nil {
		t.Fatalf("invalid request reached model or was accepted: %v", err)
	}
	if !strings.Contains(logs.String(), `"error_category":"duplicate_node_revision"`) || strings.Contains(logs.String(), "private-") {
		t.Fatalf("early rejection lacks fixed safe diagnostic: %s", &logs)
	}
}
