package httpapi

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/learning"
	"github.com/edu-agent/edu-agent/server/internal/privacy"
)

const (
	carryoverProposalID  = "aa000000-0000-4000-8000-000000000001"
	carryoverOperationID = "aa000000-0000-4000-8000-000000000002"
	carryoverDeviceID    = "90000000-0000-4000-8000-000000000001"
)

func httpCarryoverProposal(reason string) learning.EvidenceCarryoverProposal {
	now := time.Date(2026, 8, 28, 10, 0, 0, 0, time.UTC)
	proposal := learning.EvidenceCarryoverProposal{
		ID: carryoverProposalID, KnowledgeProposalID: "aa000000-0000-4000-8000-000000000003",
		Status:                    learning.EvidenceCarryoverOpen,
		SourceEvidenceID:          "aa000000-0000-4000-8000-000000000004",
		SourceKnowledgeRevisionID: "aa000000-0000-4000-8000-000000000005",
		SourceNodeRevisionID:      "aa000000-0000-4000-8000-000000000006",
		TargetKnowledgeRevisionID: "aa000000-0000-4000-8000-000000000007",
		Candidates: []learning.EvidenceCarryoverCandidate{{
			KnowledgeRevisionID: "aa000000-0000-4000-8000-000000000007",
			NodeID:              "aa000000-0000-4000-8000-000000000008",
			NodeRevisionID:      "aa000000-0000-4000-8000-000000000009",
			DocumentRevisionID:  "aa000000-0000-4000-8000-00000000000a",
		}},
		KnowledgeBasisHash: strings.Repeat("a", 64), EvidenceFingerprint: strings.Repeat("b", 64),
		CandidateFingerprint: strings.Repeat("c", 64), BasisFingerprint: strings.Repeat("d", 64),
		KnowledgeGeneration: 1, LearningGeneration: 1, PolicyVersion: learning.EvidenceCarryoverPolicyVersion,
		CreatedAt: now, UpdatedAt: now,
	}
	if reason != "" {
		proposal.Status = learning.EvidenceCarryoverApproved
		proposal.Decision = &learning.EvidenceCarryoverDecision{
			ID: "aa000000-0000-4000-8000-00000000000b", OperationID: carryoverOperationID,
			RequestedDecision: "approve", Outcome: "approved", Reason: reason, ActorDeviceID: carryoverDeviceID,
			EventID: "aa000000-0000-4000-8000-00000000000c", EventSequence: 1, CreatedAt: now,
		}
	}
	return proposal
}

func TestEvidenceCarryoverHTTPScopesStrictInputAndCredentialActor(t *testing.T) {
	var logs bytes.Buffer
	proposal := httpCarryoverProposal("")
	service := &fakeLearning{carryover: proposal, carryoverPage: learning.EvidenceCarryoverPage{}}
	handler := newLearningTestAPI(t, []string{"learning:read"}, service, &logs)

	response := learningRequest(t, handler, http.MethodGet, "/v1/learning/evidence-carryovers?status=all&limit=10", "")
	if response.Code != http.StatusOK || service.carryoverList.Status != "" || service.carryoverList.Limit != 10 || !strings.Contains(response.Body.String(), `"items":[]`) {
		t.Fatalf("list status=%d command=%+v body=%s", response.Code, service.carryoverList, response.Body.String())
	}
	response = learningRequest(t, handler, http.MethodGet, "/v1/learning/evidence-carryovers/"+carryoverProposalID, "")
	if response.Code != http.StatusOK || service.carryoverGetID != carryoverProposalID || !strings.Contains(response.Body.String(), `"candidates"`) {
		t.Fatalf("get status=%d id=%q body=%s", response.Code, service.carryoverGetID, response.Body.String())
	}

	service = &fakeLearning{carryover: proposal}
	handler = newLearningTestAPI(t, []string{"learning:read"}, service, &logs)
	decisionBody := `{"operation_id":"` + carryoverOperationID + `","decision":"approve","reason":"reviewed"}`
	response = learningRequest(t, handler, http.MethodPost, "/v1/learning/evidence-carryovers/"+carryoverProposalID+"/approve", decisionBody)
	if response.Code != http.StatusForbidden || service.calls != 0 {
		t.Fatalf("unauthorized decision status=%d calls=%d body=%s", response.Code, service.calls, response.Body.String())
	}

	handler = newLearningTestAPI(t, []string{"learning:approve"}, service, &logs)
	response = learningRequest(t, handler, http.MethodPost, "/v1/learning/evidence-carryovers/"+carryoverProposalID+"/approve", decisionBody)
	if response.Code != http.StatusOK || service.carryoverActor != carryoverDeviceID || service.carryoverDecision.ProposalID != carryoverProposalID || service.carryoverDecision.Decision != "approve" {
		t.Fatalf("authorized decision status=%d actor=%q command=%+v body=%s", response.Code, service.carryoverActor, service.carryoverDecision, response.Body.String())
	}
	before := service.calls
	for name, body := range map[string]string{
		"actor":    `{"operation_id":"` + carryoverOperationID + `","decision":"approve","reason":"reviewed","actor_device_id":"aa000000-0000-4000-8000-000000000099"}`,
		"computed": `{"operation_id":"` + carryoverOperationID + `","decision":"approve","reason":"reviewed","basis_fingerprint":"` + strings.Repeat("e", 64) + `"}`,
		"mismatch": `{"operation_id":"` + carryoverOperationID + `","decision":"reject","reason":"reviewed"}`,
	} {
		response = learningRequest(t, handler, http.MethodPost, "/v1/learning/evidence-carryovers/"+carryoverProposalID+"/approve", body)
		if response.Code != http.StatusBadRequest || service.calls != before {
			t.Fatalf("%s status=%d calls=%d body=%s", name, response.Code, service.calls, response.Body.String())
		}
	}
}

func TestEvidenceCarryoverHTTPValidatesListGetAndMapsDomainErrors(t *testing.T) {
	var logs bytes.Buffer
	service := &fakeLearning{carryover: httpCarryoverProposal("")}
	handler := newLearningTestAPI(t, []string{"learning:read"}, service, &logs)
	for _, path := range []string{
		"/v1/learning/evidence-carryovers?status=invalid",
		"/v1/learning/evidence-carryovers?limit=101",
		"/v1/learning/evidence-carryovers?limit=10&limit=20",
		"/v1/learning/evidence-carryovers/not-a-uuid",
	} {
		response := learningRequest(t, handler, http.MethodGet, path, "")
		if response.Code != http.StatusBadRequest {
			t.Fatalf("invalid %s status=%d body=%s", path, response.Code, response.Body.String())
		}
	}
	if service.calls != 0 {
		t.Fatalf("invalid reads reached service: calls=%d", service.calls)
	}

	for _, test := range []struct {
		code string
		want int
	}{
		{learning.CodeNotFound, http.StatusNotFound},
		{learning.CodeStaleCursor, http.StatusConflict},
		{learning.CodeOperationConflict, http.StatusConflict},
		{learning.CodeEvidenceCarryoverNoCandidates, http.StatusUnprocessableEntity},
	} {
		service = &fakeLearning{err: &learning.Error{Code: test.code, Cause: errors.New("database-secret")}}
		handler = newLearningTestAPI(t, []string{"learning:read"}, service, &logs)
		response := learningRequest(t, handler, http.MethodGet, "/v1/learning/evidence-carryovers/"+carryoverProposalID, "")
		if response.Code != test.want || !strings.Contains(response.Body.String(), `"code":"`+test.code+`"`) || strings.Contains(response.Body.String(), "database-secret") {
			t.Fatalf("code=%s status=%d body=%s", test.code, response.Code, response.Body.String())
		}
	}
}

func TestEvidenceCarryoverHTTPPrivacyBarrierPrecedesResponseWrite(t *testing.T) {
	const secret = "private carryover review reason"
	manager := privacy.NewReadPermitManager()
	started := make(chan struct{})
	service := &fakeLearning{carryoverGetFn: func(ctx context.Context, _ string) (learning.EvidenceCarryoverProposal, error) {
		close(started)
		<-ctx.Done()
		return httpCarryoverProposal(secret), nil
	}}
	var logs bytes.Buffer
	handler := newLearningTestAPIWithPermits(t, []string{"learning:read"}, service, manager, &logs)
	responseCh := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		responseCh <- learningRequest(t, handler, http.MethodGet, "/v1/learning/evidence-carryovers/"+carryoverProposalID, "")
	}()
	<-started
	drainCh := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		drainCh <- manager.CloseAndDrain(ctx, 2, privacy.OwnerKnowledge, privacy.OwnerLearning)
	}()
	response := <-responseCh
	if err := <-drainCh; err != nil {
		t.Fatal(err)
	}
	if response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), learning.CodeContentRedacted) || strings.Contains(response.Body.String(), secret) || strings.Contains(logs.String(), secret) {
		t.Fatalf("barrier status=%d body=%s logs=%s", response.Code, response.Body.String(), logs.String())
	}
}
