package command

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/api"
)

const (
	commandMaintenanceProposalID   = "71000000-0000-4000-8000-000000000001"
	commandMaintenanceRequestID    = "72000000-0000-4000-8000-000000000001"
	commandMaintenanceBaseID       = "73000000-0000-4000-8000-000000000001"
	commandMaintenanceDeviceID     = "74000000-0000-4000-8000-000000000001"
	commandMaintenanceOperationID  = "75000000-0000-4000-8000-000000000001"
	commandMaintenanceEvidenceHash = "d5a21eefa61c3446e252a2b4f85987bd2c4fd0a0624008d459203ab8a9af73ef"
)

func TestKnowledgeMaintenanceSixCommands(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer maintenance-token" {
			t.Fatalf("authorization = %q", r.Header.Get("Authorization"))
		}
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/knowledge/maintenance/proposals":
			var request map[string]any
			decodeCommandJSON(t, r, &request)
			if _, found := request["actor_device_id"]; found || len(request) != 4 {
				t.Fatalf("proposal body = %+v", request)
			}
			writeJSONTest(w, http.StatusCreated, commandMaintenanceProposal("open", ""))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/knowledge/maintenance/rollbacks":
			var request map[string]any
			decodeCommandJSON(t, r, &request)
			if _, found := request["actor_device_id"]; found || len(request) != 4 {
				t.Fatalf("rollback body = %+v", request)
			}
			value := commandMaintenanceProposal("open", "")
			value.Kind, value.RollbackTargetRevisionID, value.Risk.Level = "rollback", "76000000-0000-4000-8000-000000000001", "high"
			writeJSONTest(w, http.StatusCreated, value)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/knowledge/maintenance/proposals":
			if r.URL.Query().Get("status") != "open" || r.URL.Query().Get("cursor") != "cursor-one" || r.URL.Query().Get("limit") != "1" {
				t.Fatalf("query = %s", r.URL.RawQuery)
			}
			writeJSONTest(w, http.StatusOK, api.KnowledgeMaintenanceProposalPage{Items: []api.KnowledgeMaintenanceProposal{commandMaintenanceProposal("open", "")}, NextCursor: "cursor-two"})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/knowledge/maintenance/proposals/"+commandMaintenanceProposalID:
			writeJSONTest(w, http.StatusOK, commandMaintenanceProposal("open", ""))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/approve"):
			var request api.KnowledgeMaintenanceDecisionRequest
			decodeCommandJSON(t, r, &request)
			if request.OperationID != commandMaintenanceOperationID || request.Reason != "approved by edu-agent Go CLI" {
				t.Fatalf("approve body = %+v", request)
			}
			writeJSONTest(w, http.StatusOK, commandMaintenanceProposal("applied", "approve"))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/reject"):
			var request api.KnowledgeMaintenanceDecisionRequest
			decodeCommandJSON(t, r, &request)
			if request.OperationID != commandMaintenanceOperationID || request.Reason != "manual rejection" {
				t.Fatalf("reject body = %+v", request)
			}
			writeJSONTest(w, http.StatusOK, commandMaintenanceProposal("rejected", "reject"))
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.String())
		}
	}))
	defer server.Close()
	dir := t.TempDir()
	proposalFile := filepath.Join(dir, "proposal.json")
	rollbackFile := filepath.Join(dir, "rollback.json")
	proposalJSON := `{"request_id":"` + commandMaintenanceRequestID + `","base_revision_id":"` + commandMaintenanceBaseID + `","sources":[{"kind":"url","locator":"https://example.test","excerpt":"source excerpt secret","sha256":"90b5c6065da93fe02e507cd4f640336eb86b1446743c1def3c5ff5af8e335b9d"}],"candidate_snapshot":[]}`
	rollbackJSON := `{"request_id":"` + commandMaintenanceRequestID + `","base_revision_id":"` + commandMaintenanceBaseID + `","target_revision_id":"76000000-0000-4000-8000-000000000001","sources":[{"kind":"url","locator":"https://example.test","sha256":"e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"}]}`
	if err := os.WriteFile(proposalFile, []byte(proposalJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rollbackFile, []byte(rollbackJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	commands := [][]string{
		{"knowledge", "maintenance", "propose", "--request-file", proposalFile},
		{"knowledge", "maintenance", "rollback", "--request-file", rollbackFile},
		{"knowledge", "maintenance", "proposals", "--status", "open", "--cursor", "cursor-one", "--limit", "1"},
		{"knowledge", "maintenance", "proposal", commandMaintenanceProposalID},
		{"knowledge", "maintenance", "approve", commandMaintenanceProposalID, "--operation-id", commandMaintenanceOperationID},
		{"knowledge", "maintenance", "reject", commandMaintenanceProposalID, "--operation-id", commandMaintenanceOperationID, "--reason", "manual rejection"},
	}
	for _, args := range commands {
		configStore, credentialStore := pairedStores(server.URL, "maintenance-token")
		app, out, errOut := newTestApp(configStore, credentialStore, &fakeTerminal{})
		if exit := app.Run(t.Context(), args); exit != ExitOK {
			t.Fatalf("args=%v exit=%d out=%q err=%q", args, exit, out.String(), errOut.String())
		}
		if strings.Contains(out.String(), "candidate body secret") || strings.Contains(out.String(), "source excerpt secret") {
			t.Fatalf("sensitive candidate/source content printed: %q", out.String())
		}
	}
}

func TestKnowledgeMaintenanceRequestFilesFailClosedBeforeNetwork(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	valid := `{"request_id":"` + commandMaintenanceRequestID + `","base_revision_id":"` + commandMaintenanceBaseID + `","sources":[{"kind":"url","locator":"https://example.test","sha256":"e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"}],"candidate_snapshot":[]}`
	files := map[string][]byte{
		"unknown.json":      []byte(strings.Replace(valid, `"candidate_snapshot":[]`, `"candidate_snapshot":[],"risk":{"level":"low"}`, 1)),
		"actor.json":        []byte(strings.Replace(valid, `"candidate_snapshot":[]`, `"candidate_snapshot":[],"actor_device_id":"`+commandMaintenanceDeviceID+`"`, 1)),
		"duplicate.json":    []byte(strings.Replace(valid, `"candidate_snapshot":[]`, `"candidate_snapshot":[],"request_id":"`+commandMaintenanceRequestID+`"`, 1)),
		"invalid-utf8.json": {0xff},
	}
	for name, data := range files {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		assertMaintenanceFileRejected(t, path)
	}
	oversized := filepath.Join(dir, "oversized.json")
	file, err := os.Create(oversized)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(maxKnowledgeMaintenanceRequestBytes + 1); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	assertMaintenanceFileRejected(t, oversized)
	target := filepath.Join(dir, "target.json")
	if err := os.WriteFile(target, []byte(valid), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.json")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	assertMaintenanceFileRejected(t, link)
}

func assertMaintenanceFileRejected(t *testing.T, path string) {
	t.Helper()
	configStore, credentialStore := pairedStores("http://127.0.0.1:1", "token")
	app, out, errOut := newTestApp(configStore, credentialStore, &fakeTerminal{})
	app.NewClient = func(string, string, time.Duration) APIClient { panic("invalid file must not create client") }
	if exit := app.Run(t.Context(), []string{"knowledge", "maintenance", "propose", "--request-file", path}); exit != ExitInput || out.Len() != 0 || errOut.Len() == 0 {
		t.Fatalf("path=%s exit=%d out=%q err=%q", path, exit, out.String(), errOut.String())
	}
}

func commandMaintenanceProposal(status, decision string) api.KnowledgeMaintenanceProposal {
	now := time.Date(2026, 9, 2, 3, 4, 5, 0, time.UTC)
	value := api.KnowledgeMaintenanceProposal{
		ProposalID: commandMaintenanceProposalID, RequestID: commandMaintenanceRequestID, Kind: "candidate", Status: status, BaseRevisionID: commandMaintenanceBaseID,
		Sources:           []api.KnowledgeMaintenanceSource{{Kind: "url", Locator: "https://example.test", Title: "Source", Excerpt: "source excerpt secret", SHA256: "90b5c6065da93fe02e507cd4f640336eb86b1446743c1def3c5ff5af8e335b9d"}},
		CandidateSnapshot: []api.ImportDocument{{Path: "note.md", Markdown: "# candidate body secret"}}, Diff: []api.KnowledgeMaintenanceDocumentDiff{},
		IdentityImpact: api.KnowledgeMaintenanceIdentityImpact{PreservedDocumentIDs: []string{}, AddedDocumentIDs: []string{}, RemovedDocumentIDs: []string{}, MovedDocumentIDs: []string{}, PreservedNodeIDs: []string{}, AddedNodeIDs: []string{}, RemovedNodeIDs: []string{}},
		LineageImpact:  api.KnowledgeMaintenanceLineageImpact{Lineages: []api.NodeLineage{}}, AcceptedLearningEvidenceImpact: api.KnowledgeMaintenanceEvidenceImpact{References: []api.KnowledgeMaintenanceEvidenceReference{}, Fingerprint: commandMaintenanceEvidenceHash, Generation: 1},
		Risk: api.KnowledgeMaintenanceRisk{Level: "medium", Reasons: []string{"review_required"}, PolicyVersion: "knowledge-auto-apply-v1"}, BasisHash: strings.Repeat("c", 64), KnowledgeGeneration: 2,
		CanonicalizerVersion: "edu-markdown-v1", IdentityPolicyVersion: "identity-policy-v1", DiffVersion: "knowledge-diff-v1", RiskVersion: "knowledge-risk-v1", AutoApplyPolicyVersion: "knowledge-auto-apply-v1",
		CreatedByDeviceID: commandMaintenanceDeviceID, CreatedAt: now, UpdatedAt: now,
	}
	if decision != "" {
		outcome := status
		if status == "applied" {
			outcome = "applied"
			value.AppliedRevisionID = "77000000-0000-4000-8000-000000000001"
			value.Origin = &api.KnowledgeMaintenanceRevisionOrigin{Version: "knowledge-revision-origin-v1", Kind: value.Kind, ProposalID: value.ProposalID, BaseRevisionID: value.BaseRevisionID, BasisHash: value.BasisHash}
		}
		value.Decision = &api.KnowledgeMaintenanceDecision{DecisionID: "78000000-0000-4000-8000-000000000001", OperationID: commandMaintenanceOperationID, RequestedDecision: decision, Outcome: outcome, Reason: decision + " reason", ActorDeviceID: commandMaintenanceDeviceID, CreatedAt: now}
	}
	return value
}
