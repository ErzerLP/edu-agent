package blackbox

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	crossTransportProposalRequestID  = "a7100000-0000-4000-8000-000000000001"
	crossTransportApproveOperation   = "a7100000-0000-4000-8000-000000000002"
	crossTransportRejectOperation    = "a7100000-0000-4000-8000-000000000003"
	crossTransportUserOperation      = "a7100000-0000-4000-8000-000000000004"
	crossTransportCarryoverOperation = "a7100000-0000-4000-8000-000000000005"
)

type maintenanceProposalState struct {
	ProposalID        string `json:"proposal_id"`
	Status            string `json:"status"`
	BaseRevisionID    string `json:"base_revision_id"`
	AppliedRevisionID string `json:"applied_revision_id"`
}

type evidenceCarryoverState struct {
	ProposalID       string `json:"proposal_id"`
	Status           string `json:"status"`
	SourceEvidenceID string `json:"source_evidence_id"`
	TargetRevisionID string `json:"target_knowledge_revision_id"`
	Replayed         bool   `json:"replayed"`
	Decision         *struct {
		DecisionID        string `json:"decision_id"`
		OperationID       string `json:"operation_id"`
		RequestedDecision string `json:"requested_decision"`
		Outcome           string `json:"outcome"`
		ActorDeviceID     string `json:"actor_device_id"`
		EventID           string `json:"event_id"`
		EventSequence     int64  `json:"event_seq"`
	} `json:"decision"`
	Links []struct {
		ProposalID     string `json:"proposal_id"`
		SourceEvidence string `json:"source_evidence_id"`
		DecisionID     string `json:"decision_id"`
		EventID        string `json:"event_id"`
		EventSequence  int64  `json:"event_seq"`
	} `json:"links"`
}

type evidenceCarryoverPage struct {
	Items []evidenceCarryoverState `json:"items"`
}

type bearerTransport struct {
	token string
}

func (transport bearerTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	cloned := request.Clone(request.Context())
	cloned.Header = request.Header.Clone()
	cloned.Header.Set("Authorization", "Bearer "+transport.token)
	return http.DefaultTransport.RoundTrip(cloned)
}

func TestBlackBoxKnowledgeMaintenanceSharesProposalAcrossHTTPMCPAndCLI(t *testing.T) {
	h := newHarness(t)
	h.primaryHome = h.newCLIHome("knowledge-maintenance-user")
	h.pair(h.primaryHome, h.serverURL, "blackbox-maintenance-user")
	agent := h.pairCredential("agent", "blackbox-maintenance-agent")
	h.importFixture(h.primaryHome)
	learningSessionID := h.setGoal(h.primaryHome, "Verify the stable concept before maintaining its source")
	learned := h.runCLI(h.primaryHome, standardTeachingInput().
		answer("accepted response").
		defaultHelp().
		acknowledgeFeedback().
		String(), "learn")
	requireExit(t, learned, 0, "knowledge maintenance accepted evidence setup")
	acceptedEvidenceID := h.scalarString("knowledge maintenance accepted evidence", `
		SELECT id::text FROM learning_evidence WHERE session_id=$1`, learningSessionID)
	if acceptedEvidenceID == "" {
		t.Fatal("knowledge maintenance setup failed: missing accepted evidence")
	}

	var head struct {
		Revision struct {
			RevisionID string `json:"revision_id"`
		} `json:"revision"`
	}
	h.authenticatedJSON(http.MethodGet, h.serverURL+"/v1/knowledge/revisions/head", agent.Token, nil, http.StatusOK, &head)
	if head.Revision.RevisionID == "" {
		t.Fatal("knowledge maintenance setup failed: missing base revision")
	}
	var exported struct {
		RevisionID string `json:"revision_id"`
		Documents  []struct {
			Path     string `json:"path"`
			Markdown string `json:"markdown"`
		} `json:"documents"`
	}
	h.authenticatedJSON(http.MethodGet, h.serverURL+"/v1/knowledge/revisions/"+head.Revision.RevisionID+"/export", agent.Token, nil, http.StatusOK, &exported)
	if exported.RevisionID != head.Revision.RevisionID || len(exported.Documents) != 1 {
		t.Fatalf("knowledge maintenance setup failed: export revision=%q documents=%d", exported.RevisionID, len(exported.Documents))
	}
	candidate := strings.Replace(exported.Documents[0].Markdown, "# Stable Concept Verification", "# Renamed Stable Concept Verification", 1)
	candidate = strings.Replace(candidate, "Verification checks the consequence", "Verification reliably checks the consequence", 1)
	if candidate == exported.Documents[0].Markdown || !strings.Contains(candidate, "Renamed Stable Concept Verification") || !strings.Contains(candidate, "reliably checks") {
		t.Fatal("knowledge maintenance setup failed: deterministic title/body markers absent")
	}
	sourceExcerpt := "black-box agent proposes a deterministic title change"
	sourceDigest := sha256.Sum256([]byte(sourceExcerpt))
	request := map[string]any{
		"request_id":       crossTransportProposalRequestID,
		"base_revision_id": head.Revision.RevisionID,
		"sources": []any{map[string]any{
			"kind": "note", "locator": "blackbox/knowledge-maintenance/cross-transport",
			"excerpt": sourceExcerpt, "sha256": hex.EncodeToString(sourceDigest[:]),
		}},
		"candidate_snapshot": []any{map[string]any{"path": exported.Documents[0].Path, "markdown": candidate}},
	}
	var created maintenanceProposalState
	h.authenticatedJSON(http.MethodPost, h.serverURL+"/v1/knowledge/maintenance/proposals", agent.Token, request, http.StatusCreated, &created)
	if created.ProposalID == "" || created.Status != "open" || created.BaseRevisionID != head.Revision.RevisionID || created.AppliedRevisionID != "" {
		t.Fatalf("HTTP proposal state=%+v", created)
	}

	mcpSession := connectMaintenanceMCP(t, h.serverURL+"/mcp", agent.Token)
	requireMCPTool(t, mcpSession, "knowledge.maintenance.get")
	mcpOpen := callMaintenanceMCPGet(t, mcpSession, created.ProposalID)

	listed := h.runCLI(h.primaryHome, "", "knowledge", "maintenance", "proposals", "--status", "open", "--limit", "10")
	requireExit(t, listed, 0, "knowledge maintenance CLI list")
	requireContains(t, listed.stdout, "Proposal: id="+created.ProposalID, "knowledge maintenance CLI list proposal")
	requireContains(t, listed.stdout, "status=open", "knowledge maintenance CLI list status")
	cliOpenResult := h.runCLI(h.primaryHome, "", "knowledge", "maintenance", "proposal", created.ProposalID)
	requireExit(t, cliOpenResult, 0, "knowledge maintenance CLI get open")
	cliOpen := parseMaintenanceCLIState(t, cliOpenResult.stdout)
	requireProposalStateEqual(t, "open HTTP/MCP", created, mcpOpen)
	requireProposalStateEqual(t, "open HTTP/CLI", created, cliOpen)

	decisionPath := h.serverURL + "/v1/knowledge/maintenance/proposals/" + created.ProposalID
	h.authenticatedJSON(http.MethodPost, decisionPath+"/approve", agent.Token, map[string]string{
		"operation_id": crossTransportApproveOperation, "reason": "agent must not approve",
	}, http.StatusForbidden, nil)
	h.authenticatedJSON(http.MethodPost, decisionPath+"/reject", agent.Token, map[string]string{
		"operation_id": crossTransportRejectOperation, "reason": "agent must not reject",
	}, http.StatusForbidden, nil)

	approved := h.runCLI(h.primaryHome, "", "knowledge", "maintenance", "approve", created.ProposalID,
		"--operation-id", crossTransportUserOperation, "--reason", "approved by black-box user")
	requireExit(t, approved, 0, "knowledge maintenance CLI approve")
	cliApproved := parseMaintenanceCLIState(t, approved.stdout)
	if cliApproved.Status != "applied" || cliApproved.AppliedRevisionID == "" {
		t.Fatalf("CLI approved proposal state=%+v", cliApproved)
	}

	var httpApplied maintenanceProposalState
	h.authenticatedJSON(http.MethodGet, decisionPath, agent.Token, nil, http.StatusOK, &httpApplied)
	mcpApplied := callMaintenanceMCPGet(t, mcpSession, created.ProposalID)
	cliAppliedResult := h.runCLI(h.primaryHome, "", "knowledge", "maintenance", "proposal", created.ProposalID)
	requireExit(t, cliAppliedResult, 0, "knowledge maintenance CLI get applied")
	cliApplied := parseMaintenanceCLIState(t, cliAppliedResult.stdout)
	requireProposalStateEqual(t, "applied HTTP/MCP", httpApplied, mcpApplied)
	requireProposalStateEqual(t, "applied HTTP/CLI", httpApplied, cliApplied)
	requireProposalStateEqual(t, "applied HTTP/CLI decision", httpApplied, cliApproved)
	if httpApplied.ProposalID != created.ProposalID || httpApplied.BaseRevisionID != created.BaseRevisionID || httpApplied.Status != "applied" || httpApplied.AppliedRevisionID == "" {
		t.Fatalf("final cross-transport proposal state=%+v created=%+v", httpApplied, created)
	}

	carryoverListResult := h.runCLI(h.primaryHome, "", "knowledge", "maintenance", "carryovers", "list", "--status", "open", "--limit", "10")
	requireExit(t, carryoverListResult, 0, "evidence carryover CLI list")
	var carryoverPage evidenceCarryoverPage
	if err := json.Unmarshal(carryoverListResult.stdout, &carryoverPage); err != nil || len(carryoverPage.Items) != 1 {
		t.Fatalf("evidence carryover CLI list=%q page=%+v err=%v", carryoverListResult.stdout, carryoverPage, err)
	}
	openCarryover := carryoverPage.Items[0]
	if openCarryover.ProposalID == "" || openCarryover.Status != "open" || openCarryover.SourceEvidenceID != acceptedEvidenceID || openCarryover.TargetRevisionID != httpApplied.AppliedRevisionID || openCarryover.Decision != nil || len(openCarryover.Links) != 0 {
		t.Fatalf("open evidence carryover=%+v evidence=%s revision=%s", openCarryover, acceptedEvidenceID, httpApplied.AppliedRevisionID)
	}
	carryoverPath := h.serverURL + "/v1/learning/evidence-carryovers/" + openCarryover.ProposalID
	var httpOpenCarryover evidenceCarryoverState
	h.authenticatedJSON(http.MethodGet, carryoverPath, agent.Token, nil, http.StatusOK, &httpOpenCarryover)
	requireMCPTool(t, mcpSession, "learning.evidence_carryover.get")
	requireNoCarryoverMutationTool(t, mcpSession)
	mcpOpenCarryover := callCarryoverMCPGet(t, mcpSession, openCarryover.ProposalID)
	requireCarryoverStateEqual(t, "open CLI/HTTP", openCarryover, httpOpenCarryover)
	requireCarryoverStateEqual(t, "open CLI/MCP", openCarryover, mcpOpenCarryover)

	h.authenticatedJSON(http.MethodPost, carryoverPath+"/approve", agent.Token, map[string]string{
		"operation_id": crossTransportCarryoverOperation, "decision": "approve", "reason": "agent must not approve carryover",
	}, http.StatusForbidden, nil)
	h.authenticatedJSON(http.MethodPost, carryoverPath+"/reject", agent.Token, map[string]string{
		"operation_id": crossTransportCarryoverOperation, "decision": "reject", "reason": "agent must not reject carryover",
	}, http.StatusForbidden, nil)

	carryoverApprovedResult := h.runCLI(h.primaryHome, "", "knowledge", "maintenance", "carryovers", "approve",
		"--proposal-id", openCarryover.ProposalID, "--operation-id", crossTransportCarryoverOperation,
		"--reason", "approved by black-box user")
	requireExit(t, carryoverApprovedResult, 0, "evidence carryover CLI approve")
	var cliApprovedCarryover evidenceCarryoverState
	if err := json.Unmarshal(carryoverApprovedResult.stdout, &cliApprovedCarryover); err != nil {
		t.Fatalf("evidence carryover CLI approve decode failed: %v output=%q", err, carryoverApprovedResult.stdout)
	}
	if cliApprovedCarryover.Status != "approved" || cliApprovedCarryover.Decision == nil || cliApprovedCarryover.Decision.OperationID != crossTransportCarryoverOperation || cliApprovedCarryover.Decision.RequestedDecision != "approve" || cliApprovedCarryover.Decision.Outcome != "approved" || len(cliApprovedCarryover.Links) == 0 {
		t.Fatalf("approved evidence carryover=%+v", cliApprovedCarryover)
	}

	var httpApprovedCarryover evidenceCarryoverState
	h.authenticatedJSON(http.MethodGet, carryoverPath, agent.Token, nil, http.StatusOK, &httpApprovedCarryover)
	mcpApprovedCarryover := callCarryoverMCPGet(t, mcpSession, openCarryover.ProposalID)
	cliApprovedGetResult := h.runCLI(h.primaryHome, "", "knowledge", "maintenance", "carryovers", "get", "--proposal-id", openCarryover.ProposalID)
	requireExit(t, cliApprovedGetResult, 0, "evidence carryover CLI get approved")
	var cliApprovedGet evidenceCarryoverState
	if err := json.Unmarshal(cliApprovedGetResult.stdout, &cliApprovedGet); err != nil {
		t.Fatalf("evidence carryover CLI get decode failed: %v output=%q", err, cliApprovedGetResult.stdout)
	}
	requireCarryoverStateEqual(t, "approved CLI/HTTP", cliApprovedCarryover, httpApprovedCarryover)
	requireCarryoverStateEqual(t, "approved CLI/MCP", cliApprovedCarryover, mcpApprovedCarryover)
	requireCarryoverStateEqual(t, "approved CLI/get", cliApprovedCarryover, cliApprovedGet)
	if got := h.scalarInt("accepted evidence remains immutable", `SELECT count(*) FROM learning_evidence WHERE id=$1 AND accepted_event_seq IS NOT NULL`, acceptedEvidenceID); got != 1 {
		t.Fatalf("accepted evidence was rewritten or removed: count=%d", got)
	}
	if got := h.scalarInt("one carryover decision event", `SELECT count(*) FROM learning_events WHERE event_type='EvidenceCarryoverApproved' AND aggregate_id=$1`, openCarryover.ProposalID); got != 1 {
		t.Fatalf("carryover decision event count=%d want=1", got)
	}
	if got := h.scalarInt("one carryover link", `SELECT count(*) FROM learning_evidence_carryover_links WHERE proposal_id=$1 AND source_evidence_id=$2`, openCarryover.ProposalID, acceptedEvidenceID); got != 1 {
		t.Fatalf("carryover link count=%d want=1", got)
	}
}

func connectMaintenanceMCP(t *testing.T, endpoint, token string) *sdkmcp.ClientSession {
	t.Helper()
	client := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "cli-m1-knowledge-maintenance-blackbox", Version: "1"}, &sdkmcp.ClientOptions{Capabilities: &sdkmcp.ClientCapabilities{}})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	session, err := client.Connect(ctx, &sdkmcp.StreamableClientTransport{
		Endpoint: endpoint, HTTPClient: &http.Client{Transport: bearerTransport{token: token}},
		DisableStandaloneSSE: true, MaxRetries: -1,
	}, nil)
	if err != nil {
		t.Fatalf("MCP connection failed: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session
}

func requireMCPTool(t *testing.T, session *sdkmcp.ClientSession, name string) {
	t.Helper()
	result, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("MCP discovery failed: %v", err)
	}
	for _, tool := range result.Tools {
		if tool.Name == name {
			return
		}
	}
	t.Fatalf("MCP discovery missing production descriptor %q", name)
}

func callMaintenanceMCPGet(t *testing.T, session *sdkmcp.ClientSession, proposalID string) maintenanceProposalState {
	t.Helper()
	result, err := session.CallTool(context.Background(), &sdkmcp.CallToolParams{
		Name: "knowledge.maintenance.get", Arguments: map[string]any{"proposal_id": proposalID},
	})
	if err != nil || result == nil || result.IsError {
		t.Fatalf("MCP knowledge maintenance get result=%+v err=%v", result, err)
	}
	encoded, err := json.Marshal(result.StructuredContent)
	if err != nil {
		t.Fatalf("MCP knowledge maintenance get encode failed: %v", err)
	}
	var state maintenanceProposalState
	if err := json.Unmarshal(encoded, &state); err != nil {
		t.Fatalf("MCP knowledge maintenance get decode failed: %v", err)
	}
	if state.ProposalID == "" {
		t.Fatalf("MCP knowledge maintenance get missing proposal state: %s", encoded)
	}
	return state
}

func callCarryoverMCPGet(t *testing.T, session *sdkmcp.ClientSession, proposalID string) evidenceCarryoverState {
	t.Helper()
	result, err := session.CallTool(context.Background(), &sdkmcp.CallToolParams{
		Name: "learning.evidence_carryover.get", Arguments: map[string]any{"proposal_id": proposalID},
	})
	if err != nil || result == nil || result.IsError {
		t.Fatalf("MCP evidence carryover get result=%+v err=%v", result, err)
	}
	encoded, err := json.Marshal(result.StructuredContent)
	if err != nil {
		t.Fatalf("MCP evidence carryover get encode failed: %v", err)
	}
	var state evidenceCarryoverState
	if err := json.Unmarshal(encoded, &state); err != nil {
		t.Fatalf("MCP evidence carryover get decode failed: %v", err)
	}
	if state.ProposalID == "" {
		t.Fatalf("MCP evidence carryover get missing proposal state: %s", encoded)
	}
	return state
}

func requireNoCarryoverMutationTool(t *testing.T, session *sdkmcp.ClientSession) {
	t.Helper()
	result, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("MCP discovery failed: %v", err)
	}
	for _, tool := range result.Tools {
		if !strings.HasPrefix(tool.Name, "learning.evidence_carryover.") {
			continue
		}
		if tool.Name != "learning.evidence_carryover.list" && tool.Name != "learning.evidence_carryover.get" {
			t.Fatalf("forbidden production carryover MCP descriptor %q", tool.Name)
		}
	}
}

func requireCarryoverStateEqual(t *testing.T, label string, want, got evidenceCarryoverState) {
	t.Helper()
	if !reflect.DeepEqual(want, got) {
		t.Fatalf("%s state differs: want=%+v got=%+v", label, want, got)
	}
}

func parseMaintenanceCLIState(t *testing.T, output []byte) maintenanceProposalState {
	t.Helper()
	state := maintenanceProposalState{}
	for _, line := range strings.Split(string(output), "\n") {
		key, value, ok := strings.Cut(line, ": ")
		if !ok {
			continue
		}
		switch key {
		case "Proposal ID":
			state.ProposalID = value
		case "Status":
			state.Status = value
		case "Base revision":
			state.BaseRevisionID = value
		case "Applied revision":
			state.AppliedRevisionID = value
		}
	}
	if state.ProposalID == "" || state.Status == "" || state.BaseRevisionID == "" {
		t.Fatalf("CLI knowledge maintenance output missing state fields: %q", bytes.TrimSpace(output))
	}
	return state
}

func requireProposalStateEqual(t *testing.T, label string, want, got maintenanceProposalState) {
	t.Helper()
	if got.ProposalID != want.ProposalID || got.Status != want.Status || got.BaseRevisionID != want.BaseRevisionID || got.AppliedRevisionID != want.AppliedRevisionID {
		t.Fatalf("%s state differs: want=%+v got=%+v", label, want, got)
	}
}
