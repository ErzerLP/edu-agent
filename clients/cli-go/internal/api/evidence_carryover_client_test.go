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
	carryoverProposalID          = "81000000-0000-4000-8000-000000000001"
	carryoverKnowledgeProposalID = "82000000-0000-4000-8000-000000000001"
	carryoverSourceEvidenceID    = "83000000-0000-4000-8000-000000000001"
	carryoverSourceKnowledgeID   = "84000000-0000-4000-8000-000000000001"
	carryoverSourceNodeID        = "85000000-0000-4000-8000-000000000001"
	carryoverTargetKnowledgeID   = "86000000-0000-4000-8000-000000000001"
	carryoverTargetNodeID        = "87000000-0000-4000-8000-000000000001"
	carryoverTargetNodeRevision  = "88000000-0000-4000-8000-000000000001"
	carryoverTargetDocumentID    = "89000000-0000-4000-8000-000000000001"
	carryoverOperationID         = "8a000000-0000-4000-8000-000000000001"
	carryoverDecisionID          = "8b000000-0000-4000-8000-000000000001"
	carryoverDeviceID            = "8c000000-0000-4000-8000-000000000001"
	carryoverEventID             = "8d000000-0000-4000-8000-000000000001"
	carryoverLinkID              = "8e000000-0000-4000-8000-000000000001"
)

func TestEvidenceCarryoverClientExactRequests(t *testing.T) {
	t.Parallel()
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Header.Get("Authorization") != "Bearer token" || r.Header.Get("Accept") != "application/json" {
			t.Fatalf("headers = %v", r.Header)
		}
		switch calls {
		case 1:
			if r.Method != http.MethodGet || r.URL.EscapedPath() != "/v1/learning/evidence-carryovers" || r.URL.RawQuery != "cursor=cursor%2F%2B%3D%3F&limit=1&status=all" {
				t.Fatalf("list request = %s %s", r.Method, r.URL.String())
			}
			writeCarryoverJSON(t, w, http.StatusOK, EvidenceCarryoverPage{Items: []EvidenceCarryoverProposal{openCarryover()}, NextCursor: "next"})
		case 2:
			if r.Method != http.MethodGet || r.URL.EscapedPath() != "/v1/learning/evidence-carryovers/"+carryoverProposalID || r.URL.RawQuery != "" {
				t.Fatalf("get request = %s %s", r.Method, r.URL.String())
			}
			writeCarryoverJSON(t, w, http.StatusOK, openCarryover())
		case 3, 4:
			decision := "approve"
			if calls == 4 {
				decision = "reject"
			}
			if r.Method != http.MethodPost || r.URL.EscapedPath() != "/v1/learning/evidence-carryovers/"+carryoverProposalID+"/"+decision || r.URL.RawQuery != "" {
				t.Fatalf("decision request = %s %s", r.Method, r.URL.String())
			}
			data, err := io.ReadAll(r.Body)
			if err != nil {
				t.Fatal(err)
			}
			want := `{"operation_id":"` + carryoverOperationID + `","decision":"` + decision + `","reason":"` + decision + ` reason"}`
			if string(data) != want {
				t.Fatalf("decision body = %s want %s", data, want)
			}
			writeCarryoverJSON(t, w, http.StatusOK, decidedCarryover(decision, false))
		default:
			t.Fatalf("unexpected request %d", calls)
		}
	}))
	defer server.Close()
	client := NewClient(server.URL, "token", time.Second, nil)
	if _, err := client.EvidenceCarryovers(t.Context(), "all", "cursor/+=?", 1); err != nil {
		t.Fatal(err)
	}
	if _, err := client.EvidenceCarryover(t.Context(), carryoverProposalID); err != nil {
		t.Fatal(err)
	}
	for _, decision := range []string{"approve", "reject"} {
		request := EvidenceCarryoverDecisionRequest{OperationID: carryoverOperationID, Decision: decision, Reason: decision + " reason"}
		if _, err := client.DecideEvidenceCarryover(t.Context(), carryoverProposalID, decision, request); err != nil {
			t.Fatal(err)
		}
	}
}

func TestEvidenceCarryoverClientRejectsMalformedAndContradictorySuccess(t *testing.T) {
	t.Parallel()
	valid, err := json.Marshal(openCarryover())
	if err != nil {
		t.Fatal(err)
	}
	approved, err := json.Marshal(decidedCarryover("approve", false))
	if err != nil {
		t.Fatal(err)
	}
	redacted, err := json.Marshal(decidedCarryover("approve", true))
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string][]byte{
		"unknown field":   append(append([]byte(nil), valid[:len(valid)-1]...), []byte(`,"actor_device_id":"secret"}`)...),
		"bad timestamp":   []byte(strings.Replace(string(valid), `"created_at":"2026-09-03T04:05:06Z"`, `"created_at":"not-rfc3339"`, 1)),
		"bad hash":        []byte(strings.Replace(string(valid), strings.Repeat("a", 64), "ABC", 1)),
		"open decision":   []byte(strings.Replace(string(approved), `"status":"approved"`, `"status":"open"`, 1)),
		"wrong outcome":   []byte(strings.Replace(string(approved), `"outcome":"approved"`, `"outcome":"rejected"`, 1)),
		"bad link":        []byte(strings.Replace(string(approved), `"proposal_id":"`+carryoverProposalID+`"`, `"proposal_id":"90000000-0000-4000-8000-000000000001"`, 2)),
		"redacted status": []byte(strings.Replace(string(redacted), `"status":"redacted"`, `"status":"approved"`, 1)),
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write(body)
			}))
			defer server.Close()
			_, err := NewClient(server.URL, "token", time.Second, nil).EvidenceCarryover(t.Context(), carryoverProposalID)
			var protocol *ProtocolError
			if !errors.As(err, &protocol) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestEvidenceCarryoverRedactedResponseAndScopeError(t *testing.T) {
	t.Parallel()
	redacted := decidedCarryover("approve", true)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			writeCarryoverJSON(t, w, http.StatusOK, redacted)
			return
		}
		writeCarryoverJSON(t, w, http.StatusForbidden, ErrorResponse{Error: ErrorBody{Code: "forbidden", Message: "scope required", RequestID: "carryover-scope-request"}})
	}))
	defer server.Close()
	client := NewClient(server.URL, "token", time.Second, nil)
	value, err := client.EvidenceCarryover(t.Context(), carryoverProposalID)
	if err != nil || !value.Redacted || value.Status != "redacted" || value.Decision == nil || value.Decision.Reason != "" || len(value.Links) != 1 {
		t.Fatalf("redacted value=%+v err=%v", value, err)
	}
	request := EvidenceCarryoverDecisionRequest{OperationID: carryoverOperationID, Decision: "approve", Reason: "reviewed"}
	_, err = client.DecideEvidenceCarryover(t.Context(), carryoverProposalID, "approve", request)
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Code != "forbidden" || apiErr.Status != http.StatusForbidden || apiErr.RequestID != "carryover-scope-request" {
		t.Fatalf("scope error = %#v (%v)", apiErr, err)
	}
}

func TestEvidenceCarryoverClientRejectsInvalidInputBeforeHTTP(t *testing.T) {
	t.Parallel()
	client := NewClient("http://127.0.0.1:1", "token", time.Second, nil)
	if _, err := client.EvidenceCarryovers(t.Context(), "ALL", "", 50); err == nil {
		t.Fatal("accepted non-exact status")
	}
	if _, err := client.EvidenceCarryover(t.Context(), strings.ToUpper(carryoverProposalID)); err == nil {
		t.Fatal("accepted non-canonical UUID")
	}
	request := EvidenceCarryoverDecisionRequest{OperationID: carryoverOperationID, Decision: "reject", Reason: "reviewed"}
	if _, err := client.DecideEvidenceCarryover(t.Context(), carryoverProposalID, "approve", request); err == nil {
		t.Fatal("accepted endpoint/body decision mismatch")
	}
}

func openCarryover() EvidenceCarryoverProposal {
	now := time.Date(2026, 9, 3, 4, 5, 6, 0, time.UTC)
	return EvidenceCarryoverProposal{
		ProposalID: carryoverProposalID, KnowledgeProposalID: carryoverKnowledgeProposalID, Status: "open",
		SourceEvidenceID: carryoverSourceEvidenceID, SourceKnowledgeRevisionID: carryoverSourceKnowledgeID,
		SourceNodeRevisionID: carryoverSourceNodeID, TargetKnowledgeRevisionID: carryoverTargetKnowledgeID,
		Candidates:         []EvidenceCarryoverCandidate{{KnowledgeRevisionID: carryoverTargetKnowledgeID, NodeID: carryoverTargetNodeID, NodeRevisionID: carryoverTargetNodeRevision, DocumentRevisionID: carryoverTargetDocumentID}},
		KnowledgeBasisHash: strings.Repeat("a", 64), AcceptedEvidenceFingerprint: strings.Repeat("b", 64), CandidateFingerprint: strings.Repeat("c", 64), BasisFingerprint: strings.Repeat("d", 64),
		KnowledgeGeneration: 2, LearningGeneration: 3, PolicyVersion: EvidenceCarryoverPolicyVersion, CreatedAt: now, UpdatedAt: now,
	}
}

func decidedCarryover(decision string, redacted bool) EvidenceCarryoverProposal {
	value := openCarryover()
	outcome, status := "rejected", "rejected"
	if decision == "approve" {
		outcome, status = "approved", "approved"
	}
	value.Status = status
	value.Decision = &EvidenceCarryoverDecision{DecisionID: carryoverDecisionID, OperationID: carryoverOperationID, RequestedDecision: decision, Outcome: outcome, Reason: decision + " reason", ActorDeviceID: carryoverDeviceID, EventID: carryoverEventID, EventSequence: 17, CreatedAt: value.CreatedAt}
	if decision == "approve" {
		value.Links = []EvidenceCarryoverLink{{LinkID: carryoverLinkID, ProposalID: value.ProposalID, SourceEvidenceID: value.SourceEvidenceID, TargetKnowledgeRevisionID: value.TargetKnowledgeRevisionID, TargetNodeID: carryoverTargetNodeID, TargetNodeRevisionID: carryoverTargetNodeRevision, TargetDocumentRevisionID: carryoverTargetDocumentID, DecisionID: carryoverDecisionID, EventID: carryoverEventID, EventSequence: 17, CreatedAt: value.CreatedAt}}
	}
	if redacted {
		value.Status = "redacted"
		value.Redacted = true
		value.SourceEvidenceID, value.SourceKnowledgeRevisionID, value.SourceNodeRevisionID, value.TargetKnowledgeRevisionID = "", "", "", ""
		value.Candidates = nil
		value.KnowledgeBasisHash, value.AcceptedEvidenceFingerprint, value.CandidateFingerprint, value.BasisFingerprint = "", "", "", ""
		value.Decision.Reason = ""
		for index := range value.Links {
			value.Links[index].SourceEvidenceID = ""
			value.Links[index].TargetKnowledgeRevisionID = ""
			value.Links[index].TargetNodeID = ""
			value.Links[index].TargetNodeRevisionID = ""
			value.Links[index].TargetDocumentRevisionID = ""
		}
	}
	return value
}

func writeCarryoverJSON(t *testing.T, w http.ResponseWriter, status int, value any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Fatal(err)
	}
}
