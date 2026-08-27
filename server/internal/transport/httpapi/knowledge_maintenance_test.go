package httpapi

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/identity"
	"github.com/edu-agent/edu-agent/server/internal/knowledge"
	"github.com/edu-agent/edu-agent/server/internal/platform/health"
	"github.com/edu-agent/edu-agent/server/internal/privacy"
)

const (
	maintenanceDeviceID   = "a0000000-0000-4000-8000-000000000001"
	maintenanceProposalID = "a0000000-0000-4000-8000-000000000002"
	maintenanceBaseID     = "a0000000-0000-4000-8000-000000000003"
)

func newKnowledgeMaintenanceHTTP(t *testing.T, id *fakeIdentity, service *fakeKnowledge, maxBody int64) http.Handler {
	t.Helper()
	handler, err := New(Options{
		Identity: id, Knowledge: service, ReadPermits: privacy.NewReadPermitManager(),
		Readiness: fakeReadiness{report: health.Report{Status: health.StatusHealthy, Components: map[string]health.Component{}}},
		Logger:    slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil)), PairLimiter: NewFixedWindowLimiter(100, time.Minute),
		AuthLimiter: NewFixedWindowLimiter(100, time.Minute), DeviceLimiter: NewFixedWindowLimiter(100, time.Minute),
		MaxKnowledgeRequestBody: maxBody,
	})
	if err != nil {
		t.Fatal(err)
	}
	return handler
}

func maintenanceRequest(handler http.Handler, method, path, body string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer valid")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func TestKnowledgeMaintenanceHTTPInjectsCredentialActorAndRejectsActorInput(t *testing.T) {
	service := &fakeKnowledge{proposal: knowledge.Proposal{ID: maintenanceProposalID, Status: knowledge.ProposalOpen}}
	id := &fakeIdentity{auth: identity.Credential{Device: identity.Device{ID: maintenanceDeviceID}, Scopes: []string{"knowledge:write", "knowledge:read"}}}
	handler := newKnowledgeMaintenanceHTTP(t, id, service, 4096)
	valid := `{"request_id":"a0000000-0000-4000-8000-000000000004","base_revision_id":"` + maintenanceBaseID + `","sources":[{"kind":"url","locator":"https://example.test/source","sha256":"` + strings.Repeat("a", 64) + `"}],"candidate_snapshot":[]}`
	response := maintenanceRequest(handler, http.MethodPost, "/v1/knowledge/maintenance/proposals", valid)
	if response.Code != http.StatusCreated || service.createCommand.ActorDeviceID != maintenanceDeviceID {
		t.Fatalf("create status=%d actor=%q body=%s", response.Code, service.createCommand.ActorDeviceID, response.Body.String())
	}
	before := service.createCommand.RequestID
	injected := strings.TrimSuffix(valid, "}") + `,"actor_device_id":"a0000000-0000-4000-8000-000000000099"}`
	response = maintenanceRequest(handler, http.MethodPost, "/v1/knowledge/maintenance/proposals", injected)
	if response.Code != http.StatusBadRequest || service.createCommand.RequestID != before || errorCode(t, response) != knowledge.CodeInvalidRequest {
		t.Fatalf("actor injection status=%d command=%+v body=%s", response.Code, service.createCommand, response.Body.String())
	}
}

func TestKnowledgeMaintenanceHTTPScopesAndDecisions(t *testing.T) {
	service := &fakeKnowledge{proposal: knowledge.Proposal{ID: maintenanceProposalID, Status: knowledge.ProposalOpen}, proposalPage: knowledge.ProposalPage{Items: []knowledge.Proposal{}}}
	id := &fakeIdentity{auth: identity.Credential{Device: identity.Device{ID: maintenanceDeviceID}, Scopes: []string{"knowledge:write", "knowledge:read"}}}
	handler := newKnowledgeMaintenanceHTTP(t, id, service, 4096)
	proposalBody := `{"request_id":"a0000000-0000-4000-8000-000000000004","base_revision_id":"` + maintenanceBaseID + `","sources":[{"kind":"url","locator":"https://example.test/source","sha256":"` + strings.Repeat("a", 64) + `"}],"candidate_snapshot":[]}`
	if response := maintenanceRequest(handler, http.MethodPost, "/v1/knowledge/maintenance/proposals", proposalBody); response.Code != http.StatusCreated {
		t.Fatalf("agent propose=%d body=%s", response.Code, response.Body.String())
	}
	if response := maintenanceRequest(handler, http.MethodGet, "/v1/knowledge/maintenance/proposals?status=open&limit=10", ""); response.Code != http.StatusOK {
		t.Fatalf("agent list=%d body=%s", response.Code, response.Body.String())
	}
	rollbackBody := `{"request_id":"a0000000-0000-4000-8000-000000000007","base_revision_id":"` + maintenanceBaseID + `","target_revision_id":"a0000000-0000-4000-8000-000000000008","sources":[{"kind":"note","locator":"review","sha256":"` + strings.Repeat("b", 64) + `"}]}`
	if response := maintenanceRequest(handler, http.MethodPost, "/v1/knowledge/maintenance/rollbacks", rollbackBody); response.Code != http.StatusForbidden || service.rollbackCommand.RequestID != "" {
		t.Fatalf("agent rollback=%d command=%+v body=%s", response.Code, service.rollbackCommand, response.Body.String())
	}
	decisionBody := `{"operation_id":"a0000000-0000-4000-8000-000000000005","reason":"reviewed"}`
	if response := maintenanceRequest(handler, http.MethodPost, "/v1/knowledge/maintenance/proposals/"+maintenanceProposalID+"/approve", decisionBody); response.Code != http.StatusForbidden || service.decisionCommand.OperationID != "" {
		t.Fatalf("agent approve=%d command=%+v body=%s", response.Code, service.decisionCommand, response.Body.String())
	}
	id.auth.Scopes = append(id.auth.Scopes, "learning:approve")
	if response := maintenanceRequest(handler, http.MethodPost, "/v1/knowledge/maintenance/rollbacks", rollbackBody); response.Code != http.StatusCreated || service.rollbackCommand.ActorDeviceID != maintenanceDeviceID {
		t.Fatalf("user rollback=%d command=%+v body=%s", response.Code, service.rollbackCommand, response.Body.String())
	}
	if response := maintenanceRequest(handler, http.MethodPost, "/v1/knowledge/maintenance/proposals/"+maintenanceProposalID+"/approve", decisionBody); response.Code != http.StatusOK || service.decisionCommand.Decision != "approve" || service.decisionCommand.ActorDeviceID != maintenanceDeviceID {
		t.Fatalf("user approve=%d command=%+v body=%s", response.Code, service.decisionCommand, response.Body.String())
	}
	decisionBody = `{"operation_id":"a0000000-0000-4000-8000-000000000006","reason":"not acceptable"}`
	if response := maintenanceRequest(handler, http.MethodPost, "/v1/knowledge/maintenance/proposals/"+maintenanceProposalID+"/reject", decisionBody); response.Code != http.StatusOK || service.decisionCommand.Decision != "reject" {
		t.Fatalf("user reject=%d command=%+v body=%s", response.Code, service.decisionCommand, response.Body.String())
	}
}

func TestKnowledgeMaintenanceHTTPStrictBodyCapQueryAndErrors(t *testing.T) {
	service := &fakeKnowledge{proposal: knowledge.Proposal{ID: maintenanceProposalID}}
	id := &fakeIdentity{auth: identity.Credential{Device: identity.Device{ID: maintenanceDeviceID}, Scopes: []string{"knowledge:write", "knowledge:read"}}}
	handler := newKnowledgeMaintenanceHTTP(t, id, service, 128)
	response := maintenanceRequest(handler, http.MethodPost, "/v1/knowledge/maintenance/proposals", `{"unknown":true}`)
	if response.Code != http.StatusBadRequest || errorCode(t, response) != knowledge.CodeInvalidRequest {
		t.Fatalf("strict status=%d body=%s", response.Code, response.Body.String())
	}
	response = maintenanceRequest(handler, http.MethodPost, "/v1/knowledge/maintenance/proposals", `{"padding":"`+strings.Repeat("x", 200)+`"}`)
	if response.Code != http.StatusRequestEntityTooLarge || errorCode(t, response) != knowledge.CodePayloadTooLarge {
		t.Fatalf("cap status=%d body=%s", response.Code, response.Body.String())
	}
	response = maintenanceRequest(handler, http.MethodGet, "/v1/knowledge/maintenance/proposals?unexpected=1", "")
	if response.Code != http.StatusBadRequest || errorCode(t, response) != knowledge.CodeInvalidRequest {
		t.Fatalf("query status=%d body=%s", response.Code, response.Body.String())
	}

	service.maintenanceErr = &knowledge.Error{Code: knowledge.CodeProposalStale}
	response = maintenanceRequest(handler, http.MethodGet, "/v1/knowledge/maintenance/proposals/"+maintenanceProposalID, "")
	if response.Code != http.StatusConflict || errorCode(t, response) != "operation_conflict" {
		t.Fatalf("conflict status=%d body=%s", response.Code, response.Body.String())
	}
	service.maintenanceErr = &knowledge.Error{Code: knowledge.CodeNotFound}
	response = maintenanceRequest(handler, http.MethodGet, "/v1/knowledge/maintenance/proposals/"+maintenanceProposalID, "")
	if response.Code != http.StatusNotFound || errorCode(t, response) != knowledge.CodeNotFound {
		t.Fatalf("not found status=%d body=%s", response.Code, response.Body.String())
	}
	service.maintenanceErr = errorsNewSensitive("database secret")
	response = maintenanceRequest(handler, http.MethodGet, "/v1/knowledge/maintenance/proposals/"+maintenanceProposalID, "")
	if response.Code != http.StatusInternalServerError || errorCode(t, response) != "internal_error" || strings.Contains(response.Body.String(), "database secret") {
		t.Fatalf("internal status=%d body=%s", response.Code, response.Body.String())
	}
}

type sensitiveMaintenanceError string

func (e sensitiveMaintenanceError) Error() string { return string(e) }
func errorsNewSensitive(value string) error       { return sensitiveMaintenanceError(value) }

func errorCode(t *testing.T, response *httptest.ResponseRecorder) string {
	t.Helper()
	var envelope struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	return envelope.Error.Code
}
