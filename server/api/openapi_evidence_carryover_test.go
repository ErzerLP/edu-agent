package api_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/learning"
	"github.com/getkin/kin-openapi/openapi3"
)

func TestEvidenceCarryoverOpenAPIContracts(t *testing.T) {
	document, err := openapi3.NewLoader().LoadFromFile("openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if err := document.Validate(context.Background()); err != nil {
		t.Fatal(err)
	}
	contracts := []struct {
		path, method, scope string
		codes               []string
	}{
		{"/v1/learning/evidence-carryovers", "get", "learning:read", []string{"200", "400", "401", "403", "409", "429", "500", "503"}},
		{"/v1/learning/evidence-carryovers/{proposalID}", "get", "learning:read", []string{"200", "400", "401", "403", "404", "429", "500", "503"}},
		{"/v1/learning/evidence-carryovers/{proposalID}/approve", "post", "learning:approve", []string{"200", "400", "401", "403", "404", "409", "413", "422", "429", "500", "503"}},
		{"/v1/learning/evidence-carryovers/{proposalID}/reject", "post", "learning:approve", []string{"200", "400", "401", "403", "404", "409", "413", "422", "429", "500", "503"}},
	}
	for _, contract := range contracts {
		item := document.Paths.Find(contract.path)
		var operation *openapi3.Operation
		if contract.method == "get" {
			operation = item.Get
		} else {
			operation = item.Post
		}
		if operation == nil || operation.Extensions["x-required-scope"] != contract.scope {
			t.Fatalf("%s %s scope=%v", contract.method, contract.path, operation)
		}
		for _, code := range contract.codes {
			if operation.Responses.Value(code) == nil {
				t.Errorf("%s %s missing response %s", contract.method, contract.path, code)
			}
		}
	}

	decisionSchema := document.Components.Schemas["EvidenceCarryoverDecisionRequest"].Value
	valid := map[string]any{
		"operation_id": "cc000000-0000-4000-8000-000000000001",
		"decision": "approve", "reason": "explicit review",
	}
	if err := decisionSchema.VisitJSON(valid, openapi3.EnableJSONSchema2020()); err != nil {
		t.Fatalf("valid decision request: %v", err)
	}
	for _, field := range []string{"actor_device_id", "basis_fingerprint", "candidates", "redacted", "replayed"} {
		invalid := map[string]any{
			"operation_id": valid["operation_id"], "decision": "approve", "reason": "explicit review", field: "injected",
		}
		if err := decisionSchema.VisitJSON(invalid, openapi3.EnableJSONSchema2020()); err == nil {
			t.Fatalf("decision request schema accepted %s", field)
		}
	}
	if err := decisionSchema.VisitJSON(map[string]any{"operation_id": valid["operation_id"], "reason": "missing decision"}, openapi3.EnableJSONSchema2020()); err == nil {
		t.Fatal("decision request schema accepted missing decision")
	}
}

func TestEvidenceCarryoverOpenAPIValidatesDomainPayloads(t *testing.T) {
	document, err := openapi3.NewLoader().LoadFromFile("openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	proposal := learning.EvidenceCarryoverProposal{
		ID: "cc000000-0000-4000-8000-000000000001", KnowledgeProposalID: "cc000000-0000-4000-8000-000000000002",
		Status: learning.EvidenceCarryoverApproved,
		SourceEvidenceID: "cc000000-0000-4000-8000-000000000003",
		SourceKnowledgeRevisionID: "cc000000-0000-4000-8000-000000000004",
		SourceNodeRevisionID: "cc000000-0000-4000-8000-000000000005",
		TargetKnowledgeRevisionID: "cc000000-0000-4000-8000-000000000006",
		Candidates: []learning.EvidenceCarryoverCandidate{{
			KnowledgeRevisionID: "cc000000-0000-4000-8000-000000000006",
			NodeID: "cc000000-0000-4000-8000-000000000007",
			NodeRevisionID: "cc000000-0000-4000-8000-000000000008",
			DocumentRevisionID: "cc000000-0000-4000-8000-000000000009",
		}},
		KnowledgeBasisHash: strings.Repeat("a", 64), EvidenceFingerprint: strings.Repeat("b", 64),
		CandidateFingerprint: strings.Repeat("c", 64), BasisFingerprint: strings.Repeat("d", 64),
		KnowledgeGeneration: 1, LearningGeneration: 1, PolicyVersion: learning.EvidenceCarryoverPolicyVersion,
		Decision: &learning.EvidenceCarryoverDecision{
			ID: "cc000000-0000-4000-8000-00000000000a", OperationID: "cc000000-0000-4000-8000-00000000000b",
			RequestedDecision: "approve", Outcome: "approved", Reason: "reviewed",
			ActorDeviceID: "cc000000-0000-4000-8000-00000000000c",
			EventID: "cc000000-0000-4000-8000-00000000000d", EventSequence: 1, CreatedAt: now,
		},
		Links: []learning.EvidenceCarryoverLink{{
			ID: "cc000000-0000-4000-8000-00000000000e", ProposalID: "cc000000-0000-4000-8000-000000000001",
			SourceEvidenceID: "cc000000-0000-4000-8000-000000000003",
			TargetKnowledgeRevisionID: "cc000000-0000-4000-8000-000000000006",
			TargetNodeID: "cc000000-0000-4000-8000-000000000007",
			TargetNodeRevisionID: "cc000000-0000-4000-8000-000000000008",
			TargetDocumentRevisionID: "cc000000-0000-4000-8000-000000000009",
			DecisionID: "cc000000-0000-4000-8000-00000000000a",
			EventID: "cc000000-0000-4000-8000-00000000000d", EventSequence: 1, CreatedAt: now,
		}},
		CreatedAt: now, UpdatedAt: now, Replayed: true,
	}
	validateCarryoverPayload(t, document, proposal)

	redacted := learning.EvidenceCarryoverProposal{
		ID: proposal.ID, KnowledgeProposalID: proposal.KnowledgeProposalID,
		Status: learning.EvidenceCarryoverRedacted, KnowledgeGeneration: 2, LearningGeneration: 2,
		PolicyVersion: learning.EvidenceCarryoverPolicyVersion,
		CreatedAt: now, UpdatedAt: now, Redacted: true,
	}
	validateCarryoverPayload(t, document, redacted)
}

func validateCarryoverPayload(t *testing.T, document *openapi3.T, proposal learning.EvidenceCarryoverProposal) {
	t.Helper()
	encoded, err := json.Marshal(proposal)
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]any
	if err := json.Unmarshal(encoded, &wire); err != nil {
		t.Fatal(err)
	}
	if err := document.Components.Schemas["EvidenceCarryoverProposal"].Value.VisitJSON(wire, openapi3.EnableJSONSchema2020()); err != nil {
		t.Fatalf("domain carryover failed schema: %v\n%s", err, encoded)
	}
}
