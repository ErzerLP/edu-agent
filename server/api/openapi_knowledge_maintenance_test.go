package api_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/knowledge"
	"github.com/getkin/kin-openapi/openapi3"
)

func TestKnowledgeMaintenanceOpenAPIContracts(t *testing.T) {
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
		{"/v1/knowledge/maintenance/proposals", "post", "knowledge:write", []string{"200", "201", "400", "401", "403", "409", "413", "429", "500", "503"}},
		{"/v1/knowledge/maintenance/proposals", "get", "knowledge:read", []string{"200", "400", "401", "403", "409", "429", "500", "503"}},
		{"/v1/knowledge/maintenance/rollbacks", "post", "learning:approve", []string{"200", "201", "400", "401", "403", "409", "413", "429", "500", "503"}},
		{"/v1/knowledge/maintenance/proposals/{proposalID}", "get", "knowledge:read", []string{"200", "400", "401", "403", "404", "429", "500", "503"}},
		{"/v1/knowledge/maintenance/proposals/{proposalID}/approve", "post", "learning:approve", []string{"200", "400", "401", "403", "404", "409", "413", "429", "500", "503"}},
		{"/v1/knowledge/maintenance/proposals/{proposalID}/reject", "post", "learning:approve", []string{"200", "400", "401", "403", "404", "409", "413", "429", "500", "503"}},
	}
	for _, contract := range contracts {
		operation := document.Paths.Find(contract.path).Get
		if contract.method == "post" {
			operation = document.Paths.Find(contract.path).Post
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

	proposalRequest := map[string]any{
		"request_id":         "c0000000-0000-4000-8000-000000000001",
		"base_revision_id":   "c0000000-0000-4000-8000-000000000002",
		"sources":            []any{map[string]any{"kind": "url", "locator": "https://example.test", "sha256": strings.Repeat("a", 64)}},
		"candidate_snapshot": []any{},
	}
	requestSchema := document.Components.Schemas["KnowledgeMaintenanceProposalRequest"].Value
	if err := requestSchema.VisitJSON(proposalRequest, openapi3.EnableJSONSchema2020()); err != nil {
		t.Fatalf("valid proposal request: %v", err)
	}
	proposalRequest["actor_device_id"] = "c0000000-0000-4000-8000-000000000099"
	if err := requestSchema.VisitJSON(proposalRequest, openapi3.EnableJSONSchema2020()); err == nil {
		t.Fatal("proposal request schema accepted actor_device_id")
	}
	rollbackRequest := map[string]any{
		"request_id":         "c0000000-0000-4000-8000-000000000003",
		"base_revision_id":   "c0000000-0000-4000-8000-000000000002",
		"target_revision_id": "c0000000-0000-4000-8000-000000000004",
		"sources":            []any{map[string]any{"kind": "note", "locator": "review", "sha256": strings.Repeat("b", 64)}},
	}
	rollbackSchema := document.Components.Schemas["KnowledgeMaintenanceRollbackRequest"].Value
	rollbackRequest["actor_device_id"] = "c0000000-0000-4000-8000-000000000099"
	if err := rollbackSchema.VisitJSON(rollbackRequest, openapi3.EnableJSONSchema2020()); err == nil {
		t.Fatal("rollback request schema accepted actor_device_id")
	}
	decisionSchema := document.Components.Schemas["KnowledgeMaintenanceDecisionRequest"].Value
	if err := decisionSchema.VisitJSON(map[string]any{"operation_id": "c0000000-0000-4000-8000-000000000005", "reason": "reviewed", "actor_device_id": "c0000000-0000-4000-8000-000000000099"}, openapi3.EnableJSONSchema2020()); err == nil {
		t.Fatal("decision request schema accepted actor_device_id")
	}
}

func TestKnowledgeMaintenanceOpenAPIValidatesDomainProposal(t *testing.T) {
	document, err := openapi3.NewLoader().LoadFromFile("openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	proposal := knowledge.Proposal{
		ID: "d0000000-0000-4000-8000-000000000001", RequestID: "d0000000-0000-4000-8000-000000000002",
		Kind: knowledge.ProposalRollback, Status: knowledge.ProposalOpen,
		BaseRevisionID:           "d0000000-0000-4000-8000-000000000003",
		RollbackTargetRevisionID: "d0000000-0000-4000-8000-000000000004",
		Sources:                  []knowledge.ProposalSource{{Kind: "note", Locator: "review", SHA256: strings.Repeat("a", 64)}},
		CandidateSnapshot:        []knowledge.ImportDocument{}, Diff: []knowledge.DocumentDiff{},
		IdentityImpact: knowledge.IdentityImpact{PreservedDocumentIDs: []string{}, AddedDocumentIDs: []string{}, RemovedDocumentIDs: []string{}, MovedDocumentIDs: []string{}, PreservedNodeIDs: []string{}, AddedNodeIDs: []string{}, RemovedNodeIDs: []string{}},
		LineageImpact:  knowledge.LineageImpact{Lineages: []knowledge.Lineage{}, Rollback: true},
		EvidenceImpact: knowledge.AcceptedEvidenceImpact{References: []knowledge.AcceptedEvidenceReference{}, Fingerprint: strings.Repeat("b", 64), Generation: 1},
		Risk:           knowledge.ProposalRisk{Level: "high", Reasons: []string{"rollback"}, PolicyVersion: knowledge.MaintenanceAutoPolicy},
		BasisHash:      strings.Repeat("c", 64), KnowledgeGeneration: 1,
		CanonicalizerVersion: knowledge.CanonicalizerVersion, IdentityPolicyVersion: knowledge.IdentityPolicyVersion,
		DiffVersion: knowledge.MaintenanceDiffVersion, RiskVersion: knowledge.MaintenanceRiskVersion,
		AutoPolicyVersion: knowledge.MaintenanceAutoPolicy, CreatedByDeviceID: "d0000000-0000-4000-8000-000000000005",
		CreatedAt: now, UpdatedAt: now,
	}
	encoded, err := json.Marshal(proposal)
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]any
	if err := json.Unmarshal(encoded, &wire); err != nil {
		t.Fatal(err)
	}
	if err := document.Components.Schemas["KnowledgeMaintenanceProposal"].Value.VisitJSON(wire, openapi3.EnableJSONSchema2020()); err != nil {
		t.Fatalf("domain proposal failed schema: %v\n%s", err, encoded)
	}

	redacted := knowledge.Proposal{
		ID: proposal.ID, RequestID: proposal.RequestID, Kind: proposal.Kind, Status: knowledge.ProposalApplied,
		BaseRevisionID: proposal.BaseRevisionID, KnowledgeGeneration: 1, EvidenceImpact: knowledge.AcceptedEvidenceImpact{Generation: 1},
		CanonicalizerVersion: knowledge.CanonicalizerVersion, IdentityPolicyVersion: knowledge.IdentityPolicyVersion,
		DiffVersion: knowledge.MaintenanceDiffVersion, RiskVersion: knowledge.MaintenanceRiskVersion,
		AutoPolicyVersion: knowledge.MaintenanceAutoPolicy, CreatedByDeviceID: proposal.CreatedByDeviceID,
		CreatedAt: now, UpdatedAt: now, Redacted: true,
		Origin: &knowledge.RevisionOrigin{Version: knowledge.MaintenanceOriginVersion, Kind: string(proposal.Kind), ProposalID: proposal.ID, BaseRevisionID: proposal.BaseRevisionID},
	}
	encoded, err = json.Marshal(redacted)
	if err != nil || json.Unmarshal(encoded, &wire) != nil {
		t.Fatal(err)
	}
	if err := document.Components.Schemas["KnowledgeMaintenanceProposal"].Value.VisitJSON(wire, openapi3.EnableJSONSchema2020()); err != nil {
		t.Fatalf("redacted proposal failed schema: %v\n%s", err, encoded)
	}
}
