package command

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/api"
)

const (
	commandCarryoverProposalID          = "91000000-0000-4000-8000-000000000001"
	commandCarryoverKnowledgeProposalID = "92000000-0000-4000-8000-000000000001"
	commandCarryoverSourceEvidenceID    = "93000000-0000-4000-8000-000000000001"
	commandCarryoverSourceKnowledgeID   = "94000000-0000-4000-8000-000000000001"
	commandCarryoverSourceNodeID        = "95000000-0000-4000-8000-000000000001"
	commandCarryoverTargetKnowledgeID   = "96000000-0000-4000-8000-000000000001"
	commandCarryoverTargetNodeID        = "97000000-0000-4000-8000-000000000001"
	commandCarryoverTargetNodeRevision  = "98000000-0000-4000-8000-000000000001"
	commandCarryoverTargetDocumentID    = "99000000-0000-4000-8000-000000000001"
	commandCarryoverOperationID         = "9a000000-0000-4000-8000-000000000001"
	commandCarryoverDecisionID          = "9b000000-0000-4000-8000-000000000001"
	commandCarryoverDeviceID            = "9c000000-0000-4000-8000-000000000001"
	commandCarryoverEventID             = "9d000000-0000-4000-8000-000000000001"
	commandCarryoverLinkID              = "9e000000-0000-4000-8000-000000000001"
)

func TestEvidenceCarryoverCommandsDispatchExactRequestsAndStableJSON(t *testing.T) {
	t.Parallel()
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		switch calls {
		case 1:
			if r.Method != http.MethodGet || r.URL.Path != "/v1/learning/evidence-carryovers" || r.URL.RawQuery != "cursor=cursor%2F%2B%3D%3F&limit=1&status=all" {
				t.Fatalf("list request = %s %s", r.Method, r.URL.String())
			}
			writeJSONTest(w, http.StatusOK, api.EvidenceCarryoverPage{Items: []api.EvidenceCarryoverProposal{commandOpenCarryover()}, NextCursor: "next"})
		case 2:
			if r.Method != http.MethodGet || r.URL.Path != "/v1/learning/evidence-carryovers/"+commandCarryoverProposalID || r.URL.RawQuery != "" {
				t.Fatalf("get request = %s %s", r.Method, r.URL.String())
			}
			writeJSONTest(w, http.StatusOK, commandOpenCarryover())
		case 3, 4:
			decision := "approve"
			if calls == 4 {
				decision = "reject"
			}
			if r.Method != http.MethodPost || r.URL.Path != "/v1/learning/evidence-carryovers/"+commandCarryoverProposalID+"/"+decision || r.URL.RawQuery != "" {
				t.Fatalf("decision request = %s %s", r.Method, r.URL.String())
			}
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Fatal(err)
			}
			want := `{"operation_id":"` + commandCarryoverOperationID + `","decision":"` + decision + `","reason":"explicit review"}`
			if string(body) != want {
				t.Fatalf("body = %s want %s", body, want)
			}
			writeJSONTest(w, http.StatusOK, commandDecidedCarryover(decision))
		default:
			t.Fatalf("unexpected request %d", calls)
		}
	}))
	defer server.Close()
	commands := [][]string{
		{"knowledge", "maintenance", "carryovers", "list", "--status", "all", "--cursor", "cursor/+=?", "--limit", "1"},
		{"knowledge", "maintenance", "carryovers", "get", "--proposal-id", commandCarryoverProposalID},
		{"knowledge", "maintenance", "carryovers", "approve", "--proposal-id", commandCarryoverProposalID, "--operation-id", commandCarryoverOperationID, "--reason", "explicit review"},
		{"knowledge", "maintenance", "carryovers", "reject", "--proposal-id", commandCarryoverProposalID, "--operation-id", commandCarryoverOperationID, "--reason", "explicit review"},
	}
	for _, args := range commands {
		configStore, credentialStore := pairedStores(server.URL, "carryover-token")
		app, out, errOut := newTestApp(configStore, credentialStore, &fakeTerminal{})
		if exit := app.Run(t.Context(), args); exit != ExitOK || errOut.Len() != 0 {
			t.Fatalf("args=%v exit=%d out=%q err=%q", args, exit, out.String(), errOut.String())
		}
		if !json.Valid(out.Bytes()) || strings.Count(out.String(), "\n") != 1 || !strings.HasSuffix(out.String(), "\n") {
			t.Fatalf("unstable JSON output for %v: %q", args, out.String())
		}
	}
}

func TestEvidenceCarryoverArgumentsFailBeforeNetwork(t *testing.T) {
	t.Parallel()
	cases := [][]string{
		{"knowledge", "maintenance", "carryovers"},
		{"knowledge", "maintenance", "carryovers", "unknown"},
		{"knowledge", "maintenance", "carryovers", "list", "--status", "ALL"},
		{"knowledge", "maintenance", "carryovers", "list", "--limit", "0"},
		{"knowledge", "maintenance", "carryovers", "list", "--help"},
		{"knowledge", "maintenance", "carryovers", "get", commandCarryoverProposalID},
		{"knowledge", "maintenance", "carryovers", "get", "--proposal-id", "9F000000-0000-4000-8000-000000000001"},
		{"knowledge", "maintenance", "carryovers", "approve", "--proposal-id", commandCarryoverProposalID, "--operation-id", commandCarryoverOperationID},
		{"knowledge", "maintenance", "carryovers", "approve", "--proposal-id", commandCarryoverProposalID, "--operation-id", commandCarryoverOperationID, "--reason", " "},
		{"knowledge", "maintenance", "carryovers", "reject", "--proposal-id", commandCarryoverProposalID, "--operation-id", commandCarryoverOperationID, "--reason", "reviewed", "--actor-device-id", commandCarryoverDeviceID},
		{"knowledge", "maintenance", "carryovers", "approve", "--proposal-id", commandCarryoverProposalID, "--operation-id", commandCarryoverOperationID, "--reason", "reviewed", "--candidates", "[]"},
		{"knowledge", "maintenance", "carryovers", "approve", "--proposal-id", commandCarryoverProposalID, "--operation-id", commandCarryoverOperationID, "--reason", "reviewed", "--policy-version", "forged"},
	}
	for _, args := range cases {
		configStore, credentialStore := pairedStores("http://127.0.0.1:1", "token")
		app, out, errOut := newTestApp(configStore, credentialStore, &fakeTerminal{})
		app.NewClient = func(string, string, time.Duration) APIClient { panic("invalid arguments must not create a client") }
		if exit := app.Run(t.Context(), args); exit != ExitInput || out.Len() != 0 || errOut.Len() == 0 {
			t.Fatalf("args=%v exit=%d out=%q err=%q", args, exit, out.String(), errOut.String())
		}
		if len(args) == 3 && !strings.Contains(errOut.String(), "list|get|approve|reject") {
			t.Fatalf("carryover commands are not discoverable: %q", errOut.String())
		}
	}
}

func TestEvidenceCarryoverScopeErrorPassthrough(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("method = %s", r.Method)
		}
		writeJSONTest(w, http.StatusForbidden, api.ErrorResponse{Error: api.ErrorBody{Code: "forbidden", Message: "scope required", RequestID: "scope-request"}})
	}))
	defer server.Close()
	configStore, credentialStore := pairedStores(server.URL, "restricted-agent-token")
	app, out, errOut := newTestApp(configStore, credentialStore, &fakeTerminal{})
	args := []string{"knowledge", "maintenance", "carryovers", "approve", "--proposal-id", commandCarryoverProposalID, "--operation-id", commandCarryoverOperationID, "--reason", "reviewed"}
	if exit := app.Run(t.Context(), args); exit != ExitAuth || out.Len() != 0 || !strings.Contains(errOut.String(), "error[forbidden] request_id=scope-request") {
		t.Fatalf("exit=%d out=%q err=%q", exit, out.String(), errOut.String())
	}
}

func commandOpenCarryover() api.EvidenceCarryoverProposal {
	now := time.Date(2026, 9, 4, 5, 6, 7, 0, time.UTC)
	return api.EvidenceCarryoverProposal{
		ProposalID: commandCarryoverProposalID, KnowledgeProposalID: commandCarryoverKnowledgeProposalID, Status: "open",
		SourceEvidenceID: commandCarryoverSourceEvidenceID, SourceKnowledgeRevisionID: commandCarryoverSourceKnowledgeID,
		SourceNodeRevisionID: commandCarryoverSourceNodeID, TargetKnowledgeRevisionID: commandCarryoverTargetKnowledgeID,
		Candidates:         []api.EvidenceCarryoverCandidate{{KnowledgeRevisionID: commandCarryoverTargetKnowledgeID, NodeID: commandCarryoverTargetNodeID, NodeRevisionID: commandCarryoverTargetNodeRevision, DocumentRevisionID: commandCarryoverTargetDocumentID}},
		KnowledgeBasisHash: strings.Repeat("1", 64), AcceptedEvidenceFingerprint: strings.Repeat("2", 64), CandidateFingerprint: strings.Repeat("3", 64), BasisFingerprint: strings.Repeat("4", 64),
		KnowledgeGeneration: 4, LearningGeneration: 5, PolicyVersion: api.EvidenceCarryoverPolicyVersion, CreatedAt: now, UpdatedAt: now,
	}
}

func commandDecidedCarryover(decision string) api.EvidenceCarryoverProposal {
	value := commandOpenCarryover()
	outcome, status := "rejected", "rejected"
	if decision == "approve" {
		outcome, status = "approved", "approved"
	}
	value.Status = status
	value.Decision = &api.EvidenceCarryoverDecision{DecisionID: commandCarryoverDecisionID, OperationID: commandCarryoverOperationID, RequestedDecision: decision, Outcome: outcome, Reason: "explicit review", ActorDeviceID: commandCarryoverDeviceID, EventID: commandCarryoverEventID, EventSequence: 21, CreatedAt: value.CreatedAt}
	if decision == "approve" {
		value.Links = []api.EvidenceCarryoverLink{{LinkID: commandCarryoverLinkID, ProposalID: value.ProposalID, SourceEvidenceID: value.SourceEvidenceID, TargetKnowledgeRevisionID: value.TargetKnowledgeRevisionID, TargetNodeID: commandCarryoverTargetNodeID, TargetNodeRevisionID: commandCarryoverTargetNodeRevision, TargetDocumentRevisionID: commandCarryoverTargetDocumentID, DecisionID: commandCarryoverDecisionID, EventID: commandCarryoverEventID, EventSequence: 21, CreatedAt: value.CreatedAt}}
	}
	return value
}
