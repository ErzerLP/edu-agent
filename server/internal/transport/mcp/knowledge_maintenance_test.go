package mcp

import (
	"bytes"
	"context"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/knowledge"
	"github.com/edu-agent/edu-agent/server/internal/privacy"
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
	for _, name := range []string{"knowledge.maintenance.propose", "knowledge.maintenance.list", "knowledge.maintenance.get"} {
		descriptor, ok := descriptorByToolName(name)
		if !ok || len(descriptor.PrivacyOwners) != 2 || descriptor.PrivacyOwners[0] != privacy.OwnerKnowledge || descriptor.PrivacyOwners[1] != privacy.OwnerLearning {
			t.Fatalf("proposal descriptor %s owners=%v", name, descriptor.PrivacyOwners)
		}
	}
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

func TestKnowledgeMaintenanceMCPProposalResponsesRequireKnowledgeAndLearningPermits(t *testing.T) {
	const secret = "private MCP proposal candidate"
	for _, owner := range []privacy.OwnerKind{privacy.OwnerKnowledge, privacy.OwnerLearning} {
		t.Run(string(owner), func(t *testing.T) {
			handler, _, service, _, _, _ := newProtocolFixture(t, []string{"knowledge:read"})
			service.proposal = mcpMaintenanceProposal()
			service.proposal.CandidateSnapshot = []knowledge.ImportDocument{{Path: "private.md", Markdown: secret}}
			server := httptest.NewServer(handler)
			defer server.Close()
			responses := &responseRecordingTransport{}
			session := connectSDKClientWithTransport(t, server.URL, bearerTransport{token: testToken, base: responses})

			generated := make(chan struct{})
			release := make(chan struct{})
			var generatedOnce sync.Once
			var releaseOnce sync.Once
			unblock := func() { releaseOnce.Do(func() { close(release) }) }
			defer unblock()
			handler.beforeResponseWrite = func(ctx context.Context) {
				invocation, ok := invocationFromContext(ctx)
				if !ok || invocation.Descriptor.Name != "knowledge.maintenance.get" {
					return
				}
				generatedOnce.Do(func() { close(generated) })
				select {
				case <-ctx.Done():
				case <-release:
				}
			}
			callResult := make(chan error, 1)
			go func() {
				_, err := session.CallTool(context.Background(), &sdkmcp.CallToolParams{Name: "knowledge.maintenance.get", Arguments: map[string]any{"proposal_id": maintenanceProposalID}})
				callResult <- err
			}()
			select {
			case <-generated:
			case <-time.After(time.Second):
				t.Fatal("proposal result was not buffered before response write")
			}
			drainResult := make(chan error, 1)
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				drainResult <- handler.readPermits.CloseAndDrain(ctx, 2, owner)
			}()

			timedOut := false
			var callErr error
			select {
			case callErr = <-callResult:
			case <-time.After(200 * time.Millisecond):
				timedOut = true
				unblock()
				callErr = <-callResult
			}
			if err := <-drainResult; err != nil {
				t.Fatal(err)
			}
			body := responses.lastBody()
			if timedOut {
				t.Fatalf("closing %s did not cancel the MCP proposal response; err=%v body=%s", owner, callErr, body)
			}
			if callErr == nil || !bytes.Contains(body, []byte("content_redacted")) || bytes.Contains(body, []byte(secret)) {
				t.Fatalf("closing %s MCP response err=%v body=%s", owner, callErr, body)
			}
		})
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
