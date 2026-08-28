package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const (
	maintenanceProposalID        = "61000000-0000-4000-8000-000000000001"
	maintenanceRequestID         = "62000000-0000-4000-8000-000000000001"
	maintenanceBaseID            = "63000000-0000-4000-8000-000000000001"
	maintenanceDeviceID          = "64000000-0000-4000-8000-000000000001"
	maintenanceOperationID       = "65000000-0000-4000-8000-000000000001"
	maintenanceEmptyEvidenceHash = "d5a21eefa61c3446e252a2b4f85987bd2c4fd0a0624008d459203ab8a9af73ef"
)

func TestKnowledgeMaintenanceClientEndpointsAndBindings(t *testing.T) {
	t.Parallel()
	proposalRequest := maintenanceProposalRequest()
	rollbackRequest := maintenanceRollbackRequest()
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Header.Get("Authorization") != "Bearer token" {
			t.Fatalf("authorization = %q", r.Header.Get("Authorization"))
		}
		switch calls {
		case 1:
			if r.Method != http.MethodPost || r.URL.Path != "/v1/knowledge/maintenance/proposals" {
				t.Fatalf("request = %s %s", r.Method, r.URL.String())
			}
			var got KnowledgeMaintenanceProposalRequest
			decodeMaintenanceRequest(t, r, &got)
			if got.RequestID != proposalRequest.RequestID || got.Sources[0].Excerpt != "source" {
				t.Fatalf("proposal request = %+v", got)
			}
			writeMaintenanceJSON(t, w, http.StatusCreated, maintenanceOpenProposal(KnowledgeMaintenanceKindCandidate))
		case 2:
			if r.Method != http.MethodPost || r.URL.Path != "/v1/knowledge/maintenance/rollbacks" {
				t.Fatalf("request = %s %s", r.Method, r.URL.String())
			}
			writeMaintenanceJSON(t, w, http.StatusCreated, maintenanceOpenProposal(KnowledgeMaintenanceKindRollback))
		case 3:
			if r.Method != http.MethodGet || r.URL.Path != "/v1/knowledge/maintenance/proposals" || r.URL.Query().Get("status") != "open" || r.URL.Query().Get("cursor") != "opaque-cursor" || r.URL.Query().Get("limit") != "1" {
				t.Fatalf("list request = %s %s", r.Method, r.URL.String())
			}
			writeMaintenanceJSON(t, w, http.StatusOK, KnowledgeMaintenanceProposalPage{Items: []KnowledgeMaintenanceProposal{maintenanceOpenProposal(KnowledgeMaintenanceKindCandidate)}, NextCursor: "next-opaque"})
		case 4:
			if r.Method != http.MethodGet || r.URL.Path != "/v1/knowledge/maintenance/proposals/"+maintenanceProposalID {
				t.Fatalf("get request = %s %s", r.Method, r.URL.String())
			}
			writeMaintenanceJSON(t, w, http.StatusOK, maintenanceOpenProposal(KnowledgeMaintenanceKindCandidate))
		case 5, 6:
			decision := "approve"
			if calls == 6 {
				decision = "reject"
			}
			if r.Method != http.MethodPost || r.URL.Path != "/v1/knowledge/maintenance/proposals/"+maintenanceProposalID+"/"+decision {
				t.Fatalf("decision request = %s %s", r.Method, r.URL.String())
			}
			var got KnowledgeMaintenanceDecisionRequest
			decodeMaintenanceRequest(t, r, &got)
			if got.OperationID != maintenanceOperationID || got.Reason != decision+" reason" {
				t.Fatalf("decision body = %+v", got)
			}
			writeMaintenanceJSON(t, w, http.StatusOK, maintenanceDecidedProposal(decision))
		default:
			t.Fatalf("unexpected call %d", calls)
		}
	}))
	defer server.Close()
	client := NewClient(server.URL, "token", time.Second, nil)
	if err := ValidateKnowledgeMaintenanceProposal(maintenanceOpenProposal(KnowledgeMaintenanceKindCandidate)); err != nil {
		t.Fatalf("proposal fixture: %v", err)
	}
	if _, err := client.CreateKnowledgeMaintenanceProposal(t.Context(), proposalRequest); err != nil {
		t.Fatal(err)
	}
	if _, err := client.CreateKnowledgeMaintenanceRollback(t.Context(), rollbackRequest); err != nil {
		t.Fatal(err)
	}
	if _, err := client.KnowledgeMaintenanceProposals(t.Context(), "open", "opaque-cursor", 1); err != nil {
		t.Fatal(err)
	}
	if _, err := client.KnowledgeMaintenanceProposal(t.Context(), maintenanceProposalID); err != nil {
		t.Fatal(err)
	}
	if _, err := client.DecideKnowledgeMaintenanceProposal(t.Context(), maintenanceProposalID, "approve", KnowledgeMaintenanceDecisionRequest{OperationID: maintenanceOperationID, Reason: "approve reason"}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.DecideKnowledgeMaintenanceProposal(t.Context(), maintenanceProposalID, "reject", KnowledgeMaintenanceDecisionRequest{OperationID: maintenanceOperationID, Reason: "reject reason"}); err != nil {
		t.Fatal(err)
	}
}

func TestKnowledgeMaintenanceStrictRequestAndMalformedResponse(t *testing.T) {
	valid := `{"request_id":"` + maintenanceRequestID + `","base_revision_id":"` + maintenanceBaseID + `","sources":[{"kind":"url","locator":"https://example.test","excerpt":"source","sha256":"` + maintenanceSourceHash() + `"}],"candidate_snapshot":[]}`
	if _, err := DecodeKnowledgeMaintenanceProposalRequest([]byte(valid)); err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{
		strings.Replace(valid, `"candidate_snapshot":[]`, `"candidate_snapshot":[],"actor_device_id":"`+maintenanceDeviceID+`"`, 1),
		strings.Replace(valid, `"candidate_snapshot":[]`, `"candidate_snapshot":[],"risk":{"level":"low"}`, 1),
		strings.Replace(valid, `"candidate_snapshot":[]`, `"candidate_snapshot":[],"request_id":"`+maintenanceRequestID+`"`, 1),
	} {
		if _, err := DecodeKnowledgeMaintenanceProposalRequest([]byte(body)); err == nil {
			t.Fatalf("accepted closed-schema violation: %s", body)
		}
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		value := maintenanceOpenProposal(KnowledgeMaintenanceKindCandidate)
		data, _ := json.Marshal(value)
		_, _ = w.Write(append(data[:len(data)-1], []byte(`,"actor_device_id":"secret"}`)...))
	}))
	defer server.Close()
	_, err := NewClient(server.URL, "token", time.Second, nil).KnowledgeMaintenanceProposal(t.Context(), maintenanceProposalID)
	var protocol *ProtocolError
	if !errors.As(err, &protocol) {
		t.Fatalf("malformed response error = %v", err)
	}
}

func TestKnowledgeMaintenanceRedactedAuditAndLineageCardinality(t *testing.T) {
	redacted := maintenanceOpenProposal(KnowledgeMaintenanceKindCandidate)
	redacted.Status, redacted.Redacted = "applied", true
	redacted.Sources, redacted.CandidateSnapshot, redacted.Diff, redacted.BasisHash = nil, nil, nil, ""
	redacted.IdentityImpact = KnowledgeMaintenanceIdentityImpact{}
	redacted.LineageImpact = KnowledgeMaintenanceLineageImpact{}
	redacted.AcceptedLearningEvidenceImpact = KnowledgeMaintenanceEvidenceImpact{Generation: 1}
	redacted.Risk = KnowledgeMaintenanceRisk{}
	redacted.Origin = &KnowledgeMaintenanceRevisionOrigin{Version: "knowledge-revision-origin-v1", Kind: redacted.Kind, ProposalID: redacted.ProposalID, BaseRevisionID: redacted.BaseRevisionID}
	if err := ValidateKnowledgeMaintenanceProposal(redacted); err != nil {
		t.Fatalf("redacted applied audit rejected: %v", err)
	}
	encoded, err := json.Marshal(redacted)
	if err != nil {
		t.Fatal(err)
	}
	var decoded KnowledgeMaintenanceProposal
	if err := decodeStrict(encoded, &decoded); err != nil {
		t.Fatalf("redacted wire response rejected: %v", err)
	}

	invalid := maintenanceOpenProposal(KnowledgeMaintenanceKindCandidate)
	invalid.LineageImpact.Lineages = []NodeLineage{{
		LineageID: "69000000-0000-4000-8000-000000000001", KnowledgeRevisionID: maintenanceBaseID,
		Action: "split", ActorDeviceID: maintenanceDeviceID, PolicyVersion: "identity-policy-v1", CreatedAt: invalid.CreatedAt,
		Members: []LineageMember{{Role: "source", NodeRevisionID: "6a000000-0000-4000-8000-000000000001"}, {Role: "target", NodeRevisionID: "6b000000-0000-4000-8000-000000000001"}},
	}}
	if err := ValidateKnowledgeMaintenanceProposal(invalid); err == nil {
		t.Fatal("split lineage with one target was accepted")
	}
}

func maintenanceProposalRequest() KnowledgeMaintenanceProposalRequest {
	return KnowledgeMaintenanceProposalRequest{RequestID: maintenanceRequestID, BaseRevisionID: maintenanceBaseID, Sources: maintenanceSources(), CandidateSnapshot: []ImportDocument{}}
}

func maintenanceRollbackRequest() KnowledgeMaintenanceRollbackRequest {
	return KnowledgeMaintenanceRollbackRequest{RequestID: maintenanceRequestID, BaseRevisionID: maintenanceBaseID, TargetRevisionID: "66000000-0000-4000-8000-000000000001", Sources: maintenanceSources()}
}

func maintenanceSources() []KnowledgeMaintenanceSource {
	return []KnowledgeMaintenanceSource{{Kind: "url", Locator: "https://example.test", Excerpt: "source", SHA256: maintenanceSourceHash()}}
}

func maintenanceSourceHash() string {
	return "41cf6794ba4200b839c53531555f0f3998df4cbb01a4d5cb0b94e3ca5e23947d"
}

func maintenanceOpenProposal(kind string) KnowledgeMaintenanceProposal {
	now := time.Date(2026, 9, 1, 2, 3, 4, 0, time.UTC)
	value := KnowledgeMaintenanceProposal{
		ProposalID: maintenanceProposalID, RequestID: maintenanceRequestID, Kind: kind, Status: "open", BaseRevisionID: maintenanceBaseID,
		Sources: maintenanceSources(), CandidateSnapshot: []ImportDocument{}, Diff: []KnowledgeMaintenanceDocumentDiff{},
		IdentityImpact: KnowledgeMaintenanceIdentityImpact{PreservedDocumentIDs: []string{}, AddedDocumentIDs: []string{}, RemovedDocumentIDs: []string{}, MovedDocumentIDs: []string{}, PreservedNodeIDs: []string{}, AddedNodeIDs: []string{}, RemovedNodeIDs: []string{}},
		LineageImpact:  KnowledgeMaintenanceLineageImpact{Lineages: []NodeLineage{}}, AcceptedLearningEvidenceImpact: KnowledgeMaintenanceEvidenceImpact{References: []KnowledgeMaintenanceEvidenceReference{}, Fingerprint: maintenanceEmptyEvidenceHash, Generation: 1},
		Risk: KnowledgeMaintenanceRisk{Level: "medium", Reasons: []string{"review_required"}, PolicyVersion: "knowledge-auto-apply-v1"}, BasisHash: strings.Repeat("a", 64), KnowledgeGeneration: 1,
		CanonicalizerVersion: "edu-markdown-v1", IdentityPolicyVersion: "identity-policy-v1", DiffVersion: "knowledge-diff-v1", RiskVersion: "knowledge-risk-v1", AutoApplyPolicyVersion: "knowledge-auto-apply-v1",
		CreatedByDeviceID: maintenanceDeviceID, CreatedAt: now, UpdatedAt: now,
	}
	if kind == KnowledgeMaintenanceKindRollback {
		value.RollbackTargetRevisionID = "66000000-0000-4000-8000-000000000001"
		value.Risk.Level = "high"
	}
	return value
}

func maintenanceDecidedProposal(decision string) KnowledgeMaintenanceProposal {
	value := maintenanceOpenProposal(KnowledgeMaintenanceKindCandidate)
	value.Decision = &KnowledgeMaintenanceDecision{DecisionID: "67000000-0000-4000-8000-000000000001", OperationID: maintenanceOperationID, RequestedDecision: decision, Reason: decision + " reason", ActorDeviceID: maintenanceDeviceID, CreatedAt: value.CreatedAt}
	if decision == "approve" {
		value.Status, value.Decision.Outcome, value.AppliedRevisionID = "applied", "applied", "68000000-0000-4000-8000-000000000001"
		value.CurrentRevisionID = value.AppliedRevisionID
		value.Origin = &KnowledgeMaintenanceRevisionOrigin{Version: "knowledge-revision-origin-v1", Kind: value.Kind, ProposalID: value.ProposalID, BaseRevisionID: value.BaseRevisionID, BasisHash: value.BasisHash}
	} else {
		value.Status, value.Decision.Outcome = "rejected", "rejected"
	}
	return value
}

func writeMaintenanceJSON(t *testing.T, w http.ResponseWriter, status int, value any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Fatal(err)
	}
}

func decodeMaintenanceRequest(t *testing.T, r *http.Request, target any) {
	t.Helper()
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		t.Fatal(err)
	}
	if _, err := decoder.Token(); err != io.EOF {
		t.Fatalf("trailing JSON: %v", err)
	}
}
