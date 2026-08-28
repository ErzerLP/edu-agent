package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/learning"
	"github.com/edu-agent/edu-agent/server/internal/privacy"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

const mcpCarryoverProposalID = "bb000000-0000-4000-8000-000000000001"

func mcpCarryoverProposal(reason string) learning.EvidenceCarryoverProposal {
	now := time.Date(2026, 8, 28, 11, 0, 0, 0, time.UTC)
	proposal := learning.EvidenceCarryoverProposal{
		ID: mcpCarryoverProposalID, KnowledgeProposalID: "bb000000-0000-4000-8000-000000000002",
		Status:                    learning.EvidenceCarryoverOpen,
		SourceEvidenceID:          "bb000000-0000-4000-8000-000000000003",
		SourceKnowledgeRevisionID: "bb000000-0000-4000-8000-000000000004",
		SourceNodeRevisionID:      "bb000000-0000-4000-8000-000000000005",
		TargetKnowledgeRevisionID: "bb000000-0000-4000-8000-000000000006",
		Candidates: []learning.EvidenceCarryoverCandidate{{
			KnowledgeRevisionID: "bb000000-0000-4000-8000-000000000006",
			NodeID:              "bb000000-0000-4000-8000-000000000007",
			NodeRevisionID:      "bb000000-0000-4000-8000-000000000008",
			DocumentRevisionID:  "bb000000-0000-4000-8000-000000000009",
		}},
		KnowledgeBasisHash: strings.Repeat("a", 64), EvidenceFingerprint: strings.Repeat("b", 64),
		CandidateFingerprint: strings.Repeat("c", 64), BasisFingerprint: strings.Repeat("d", 64),
		KnowledgeGeneration: 1, LearningGeneration: 1, PolicyVersion: learning.EvidenceCarryoverPolicyVersion,
		CreatedAt: now, UpdatedAt: now,
	}
	if reason != "" {
		proposal.Status = learning.EvidenceCarryoverApproved
		proposal.Decision = &learning.EvidenceCarryoverDecision{
			ID: "bb000000-0000-4000-8000-00000000000a", OperationID: "bb000000-0000-4000-8000-00000000000b",
			RequestedDecision: "approve", Outcome: "approved", Reason: reason, ActorDeviceID: testDeviceID,
			EventID: "bb000000-0000-4000-8000-00000000000c", EventSequence: 1, CreatedAt: now,
		}
	}
	return proposal
}

func TestEvidenceCarryoverMCPRegistersOnlyReadOnlyListAndGet(t *testing.T) {
	handler, _, _, service, _, _ := newProtocolFixture(t, []string{"learning:read", "knowledge:read", "memory:read"})
	service.carryover = mcpCarryoverProposal("")
	service.carryoverPage = learning.EvidenceCarryoverPage{Items: []learning.EvidenceCarryoverProposal{service.carryover}}
	server := newMCPTestServer(t, handler)
	session := connectSDKClient(t, server.URL, testToken)

	tools, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, tool := range tools.Tools {
		seen[tool.Name] = true
		lower := strings.ToLower(tool.Name)
		if strings.Contains(lower, "evidence_carryover") && (strings.Contains(lower, "approve") || strings.Contains(lower, "reject") || strings.Contains(lower, "decision") || strings.Contains(lower, "finalize") || strings.Contains(lower, "rollback")) {
			t.Fatalf("forbidden carryover mutation descriptor registered: %s", tool.Name)
		}
	}
	if !seen["learning.evidence_carryover.list"] || !seen["learning.evidence_carryover.get"] {
		t.Fatalf("carryover read descriptors missing: %v", seen)
	}

	list, err := session.CallTool(context.Background(), &sdkmcp.CallToolParams{Name: "learning.evidence_carryover.list", Arguments: map[string]any{"status": "all", "limit": 10}})
	if err != nil || list.IsError || service.carryoverList.Status != "" || service.carryoverList.Limit != 10 {
		t.Fatalf("list result=%+v err=%v command=%+v", list, err, service.carryoverList)
	}
	get, err := session.CallTool(context.Background(), &sdkmcp.CallToolParams{Name: "learning.evidence_carryover.get", Arguments: map[string]any{"proposal_id": mcpCarryoverProposalID}})
	if err != nil || get.IsError || service.carryoverGetID != mcpCarryoverProposalID {
		t.Fatalf("get result=%+v err=%v id=%q", get, err, service.carryoverGetID)
	}
	if service.carryoverDecisionCalls != 0 {
		t.Fatalf("MCP invoked carryover decision service %d times", service.carryoverDecisionCalls)
	}
}

func TestEvidenceCarryoverMCPValidatesPayloadAndReturnsStructuredErrors(t *testing.T) {
	handler, _, _, service, _, _ := newProtocolFixture(t, []string{"learning:read", "knowledge:read", "memory:read"})
	service.carryover = mcpCarryoverProposal("")
	server := newMCPTestServer(t, handler)
	session := connectSDKClient(t, server.URL, testToken)

	for name, test := range map[string]struct {
		tool      string
		arguments map[string]any
	}{
		"invalid_status": {"learning.evidence_carryover.list", map[string]any{"status": "invalid"}},
		"invalid_limit":  {"learning.evidence_carryover.list", map[string]any{"limit": 101}},
		"actor":          {"learning.evidence_carryover.get", map[string]any{"proposal_id": mcpCarryoverProposalID, "actor_device_id": testDeviceID}},
		"invalid_id":     {"learning.evidence_carryover.get", map[string]any{"proposal_id": "not-a-uuid"}},
	} {
		result, err := session.CallTool(context.Background(), &sdkmcp.CallToolParams{Name: test.tool, Arguments: test.arguments})
		if err == nil && result != nil && !result.IsError {
			t.Fatalf("%s payload accepted: %+v", name, result)
		}
	}
	if service.calls != 0 {
		t.Fatalf("invalid MCP payload reached service: calls=%d", service.calls)
	}

	service.err = &learning.Error{Code: learning.CodeOperationConflict}
	result, err := session.CallTool(context.Background(), &sdkmcp.CallToolParams{Name: "learning.evidence_carryover.get", Arguments: map[string]any{"proposal_id": mcpCarryoverProposalID}})
	if err != nil || result == nil || !result.IsError {
		t.Fatalf("structured error result=%+v err=%v", result, err)
	}
	structured, ok := result.StructuredContent.(map[string]any)
	if !ok {
		t.Fatalf("structured error is %T, want JSON object", result.StructuredContent)
	}
	failure, ok := structured["error"].(map[string]any)
	if !ok || failure["code"] != learning.CodeOperationConflict {
		t.Fatalf("structured error=%v", structured)
	}
	encoded, err := json.Marshal(result.StructuredContent)
	if err != nil || len(encoded) == 0 || encoded[0] == '"' || !bytes.Contains(encoded, []byte(`"error"`)) {
		t.Fatalf("structured error was encoded as scalar/base64: %s err=%v", encoded, err)
	}
}

func TestEvidenceCarryoverMCPPrivacyBarrierPrecedesResponseWrite(t *testing.T) {
	const secret = "private carryover reason"
	handler, _, _, service, _, _ := newProtocolFixture(t, []string{"learning:read", "knowledge:read", "memory:read"})
	service.carryover = mcpCarryoverProposal(secret)
	server := httptest.NewServer(handler)
	defer server.Close()
	responses := &responseRecordingTransport{}
	session := connectSDKClientWithTransport(t, server.URL, bearerTransport{token: testToken, base: responses})

	generated := make(chan struct{})
	var once sync.Once
	handler.beforeResponseWrite = func(ctx context.Context) {
		invocation, ok := invocationFromContext(ctx)
		if !ok || invocation.Descriptor.Name != "learning.evidence_carryover.get" {
			return
		}
		once.Do(func() { close(generated) })
		<-ctx.Done()
	}
	callResult := make(chan error, 1)
	go func() {
		_, err := session.CallTool(context.Background(), &sdkmcp.CallToolParams{Name: "learning.evidence_carryover.get", Arguments: map[string]any{"proposal_id": mcpCarryoverProposalID}})
		callResult <- err
	}()
	select {
	case <-generated:
	case <-time.After(5 * time.Second):
		t.Fatal("carryover result was not buffered before response write")
	}
	drainResult := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		drainResult <- handler.readPermits.CloseAndDrain(ctx, 2, privacy.OwnerKnowledge, privacy.OwnerLearning)
	}()
	select {
	case err := <-callResult:
		body := responses.lastBody()
		if err == nil || !bytes.Contains(body, []byte(learning.CodeContentRedacted)) || bytes.Contains(body, []byte(secret)) {
			t.Fatalf("privacy response error=%v body=%s", err, body)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("carryover call did not finish after privacy cancellation")
	}
	if err := <-drainResult; err != nil {
		t.Fatalf("CloseAndDrain: %v", err)
	}
}
