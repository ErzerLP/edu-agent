package mcp

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/knowledge"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	maintenanceProposalID = "b0000000-0000-4000-8000-000000000001"
	maintenanceBaseID     = "b0000000-0000-4000-8000-000000000002"
)

func mcpMaintenanceProposal() knowledge.Proposal {
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	return knowledge.Proposal{
		ID: maintenanceProposalID, RequestID: "b0000000-0000-4000-8000-000000000003",
		Kind: knowledge.ProposalCandidate, Status: knowledge.ProposalOpen, BaseRevisionID: maintenanceBaseID,
		IdentityImpact: knowledge.IdentityImpact{}, LineageImpact: knowledge.LineageImpact{},
		EvidenceImpact: knowledge.AcceptedEvidenceImpact{}, Risk: knowledge.ProposalRisk{},
		KnowledgeGeneration: 1, CanonicalizerVersion: knowledge.CanonicalizerVersion,
		IdentityPolicyVersion: knowledge.IdentityPolicyVersion, DiffVersion: knowledge.MaintenanceDiffVersion,
		RiskVersion: knowledge.MaintenanceRiskVersion, AutoPolicyVersion: knowledge.MaintenanceAutoPolicy,
		CreatedByDeviceID: testDeviceID, CreatedAt: now, UpdatedAt: now,
	}
}

func TestKnowledgeMaintenanceMCPInjectsActorSupportsExactSurface(t *testing.T) {
	handler, _, service, _, _, _ := newProtocolFixture(t, []string{"knowledge:read", "knowledge:write"})
	service.proposal = mcpMaintenanceProposal()
	service.page = knowledge.ProposalPage{Items: []knowledge.Proposal{service.proposal}}
	server := newMCPTestServer(t, handler)
	session := connectSDKClient(t, server.URL, testToken)

	tools, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range tools.Tools {
		lower := strings.ToLower(tool.Name)
		if strings.Contains(lower, "approve") || strings.Contains(lower, "reject") || strings.Contains(lower, "rollback") || strings.Contains(lower, "finalize") || strings.Contains(lower, "adjudicate") {
			t.Fatalf("forbidden knowledge finalization descriptor registered: %s", tool.Name)
		}
	}

	source := map[string]any{"kind": "url", "locator": "https://example.test/source", "sha256": strings.Repeat("a", 64)}
	proposalResult, err := session.CallTool(context.Background(), &sdkmcp.CallToolParams{Name: "knowledge.maintenance.propose", Arguments: map[string]any{
		"request_id": service.proposal.RequestID, "base_revision_id": maintenanceBaseID,
		"sources": []any{source}, "candidate_snapshot": []any{},
	}})
	if err != nil || proposalResult.IsError || service.create.ActorDeviceID != testDeviceID {
		t.Fatalf("propose result=%+v err=%v actor=%q", proposalResult, err, service.create.ActorDeviceID)
	}

	listResult, err := session.CallTool(context.Background(), &sdkmcp.CallToolParams{Name: "knowledge.maintenance.list", Arguments: map[string]any{"status": "open", "limit": 10}})
	if err != nil || listResult.IsError || service.list.Status != "open" || service.list.Limit != 10 {
		t.Fatalf("list result=%+v err=%v command=%+v", listResult, err, service.list)
	}
	getResult, err := session.CallTool(context.Background(), &sdkmcp.CallToolParams{Name: "knowledge.maintenance.get", Arguments: map[string]any{"proposal_id": maintenanceProposalID}})
	if err != nil || getResult.IsError || service.getID != maintenanceProposalID {
		t.Fatalf("get result=%+v err=%v id=%q", getResult, err, service.getID)
	}
}

func TestKnowledgeMaintenanceMCPRejectsActorAndMapsErrors(t *testing.T) {
	handler, _, service, _, _, _ := newProtocolFixture(t, []string{"knowledge:read", "knowledge:write"})
	service.proposal = mcpMaintenanceProposal()
	server := newMCPTestServer(t, handler)
	session := connectSDKClient(t, server.URL, testToken)

	result, err := session.CallTool(context.Background(), &sdkmcp.CallToolParams{Name: "knowledge.maintenance.propose", Arguments: map[string]any{
		"request_id": service.proposal.RequestID, "base_revision_id": maintenanceBaseID,
		"sources":            []any{map[string]any{"kind": "url", "locator": "https://example.test", "sha256": strings.Repeat("a", 64)}},
		"candidate_snapshot": []any{}, "actor_device_id": "b0000000-0000-4000-8000-000000000099",
	}})
	if err == nil && result != nil && !result.IsError {
		t.Fatalf("actor field was accepted: %+v", result)
	}
	if service.create.RequestID != "" {
		t.Fatalf("actor injection reached service: %+v", service.create)
	}

	service.err = &knowledge.Error{Code: knowledge.CodeProposalStale}
	result, err = session.CallTool(context.Background(), &sdkmcp.CallToolParams{Name: "knowledge.maintenance.get", Arguments: map[string]any{"proposal_id": maintenanceProposalID}})
	if err != nil || result == nil || !result.IsError || !strings.Contains(result.Content[0].(*sdkmcp.TextContent).Text, `"code":"operation_conflict"`) {
		t.Fatalf("conflict result=%+v err=%v", result, err)
	}
}

func newMCPTestServer(t *testing.T, handler *Handler) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return server
}
