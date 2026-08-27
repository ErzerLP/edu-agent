package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/identity"
	"github.com/edu-agent/edu-agent/server/internal/learning"
)

type fakeOfflineLearning struct {
	actor           string
	method          string
	operationID     string
	assessmentID    string
	prepare         learning.OfflinePrepareRequest
	sync            learning.OfflineSyncRequest
	assessmentQuery learning.OfflineAssessmentQuery
	decision        learning.OfflineAssessmentDecisionCommand
	prepareOut      learning.OfflinePrepareResponse
	syncOut         learning.OfflineSyncResponse
	statusOut       learning.OfflineOperationStatus
	assessmentsOut  learning.OfflineAssessmentPage
	assessmentOut   learning.OfflineAssessmentView
	decisionOut     learning.OfflineAssessmentDecisionReceipt
	err             error
	calls           int
}

func (f *fakeOfflineLearning) Prepare(_ context.Context, actor string, request learning.OfflinePrepareRequest) (learning.OfflinePrepareResponse, error) {
	f.actor, f.method, f.prepare, f.calls = actor, "prepare", request, f.calls+1
	return f.prepareOut, f.err
}

func (f *fakeOfflineLearning) Sync(_ context.Context, actor string, request learning.OfflineSyncRequest) (learning.OfflineSyncResponse, error) {
	f.actor, f.method, f.sync, f.calls = actor, "sync", request, f.calls+1
	return f.syncOut, f.err
}

func (f *fakeOfflineLearning) Status(_ context.Context, actor, operationID string) (learning.OfflineOperationStatus, error) {
	f.actor, f.method, f.operationID, f.calls = actor, "status", operationID, f.calls+1
	return f.statusOut, f.err
}

func (f *fakeOfflineLearning) ListOfflineAssessments(_ context.Context, actor string, query learning.OfflineAssessmentQuery) (learning.OfflineAssessmentPage, error) {
	f.actor, f.method, f.assessmentQuery, f.calls = actor, "assessments", query, f.calls+1
	return f.assessmentsOut, f.err
}

func (f *fakeOfflineLearning) OfflineAssessment(_ context.Context, actor, assessmentID string) (learning.OfflineAssessmentView, error) {
	f.actor, f.method, f.assessmentID, f.calls = actor, "assessment", assessmentID, f.calls+1
	return f.assessmentOut, f.err
}

func (f *fakeOfflineLearning) DecideOfflineAssessment(_ context.Context, actor, assessmentID string, command learning.OfflineAssessmentDecisionCommand) (learning.OfflineAssessmentDecisionReceipt, error) {
	f.actor, f.method, f.assessmentID, f.decision, f.calls = actor, "assessment_decision", assessmentID, command, f.calls+1
	return f.decisionOut, f.err
}

func TestOfflineHTTPEnforcesScopesActorsReplayAndClosedBodies(t *testing.T) {
	const (
		deviceID     = "90000000-0000-4000-8000-000000000001"
		operationID  = "10000000-0000-4000-8000-000000000001"
		submissionID = "20000000-0000-4000-8000-000000000001"
		receiptID    = "30000000-0000-4000-8000-000000000001"
		ticketID     = "40000000-0000-4000-8000-000000000001"
	)
	prepareBody := `{"operation_id":"` + operationID + `","payload_schema_version":1,"expected_session_version":"7","trusted_manifest_revision":"0","trusted_manifest_digest":"` + learning.OfflineZeroDigest + `"}`
	service := &fakeOfflineLearning{prepareOut: learning.OfflinePrepareResponse{
		OperationID:       operationID,
		Pack:              learning.OfflineSignedEnvelope{Payload: json.RawMessage(`{}`), SignerKeyID: "key-1", Signature: strings.Repeat("A", 86)},
		ManifestChain:     []learning.OfflineSignedEnvelope{},
		ResponseSignature: learning.OfflineSignedEnvelope{Payload: json.RawMessage(`{}`), SignerKeyID: "key-1", Signature: strings.Repeat("B", 86)},
	}}
	handler := newOfflineHTTPTestAPI(t, []string{"learning:write", "learning:read"}, service, 8<<20)
	request := httptest.NewRequest(http.MethodPost, "/v1/learning/offline/packs", strings.NewReader(prepareBody))
	request.Header.Set("Authorization", "Bearer token")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusCreated || service.method != "prepare" || service.actor != deviceID || service.prepare.ExpectedSessionVersion != "7" {
		t.Fatalf("prepare status=%d body=%s service=%+v", response.Code, response.Body.String(), service)
	}
	service.prepareOut.Replayed = true
	request = httptest.NewRequest(http.MethodPost, "/v1/learning/offline/packs", strings.NewReader(prepareBody))
	request.Header.Set("Authorization", "Bearer token")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"replayed":true`) {
		t.Fatalf("prepare replay status=%d body=%s", response.Code, response.Body.String())
	}

	service.statusOut = learning.OfflineOperationStatus{
		OperationID:      operationID,
		SubmissionID:     submissionID,
		ArchiveStatus:    learning.OfflineArchivedSucceeded,
		AssessmentStatus: learning.OfflineAssessmentCompleted,
		EvidenceStatus:   learning.OfflineEvidenceAccepted,
		ReasonCodes:      []string{},
		Receipt:          learning.OfflineIngestReceipt{ReceiptID: receiptID, ArchivedAt: time.Now().UTC(), AggregateVersion: "4", FirstEventSequence: "10", LastEventSequence: "13", ProjectionAsOf: "13", ArchiveStatus: learning.OfflineArchivedSucceeded},
		StatusTicket:     learning.OfflineStatusTicket{TicketID: ticketID, OperationID: operationID, Revision: "1", UpdatedAt: time.Now().UTC()},
	}
	request = httptest.NewRequest(http.MethodGet, "/v1/learning/offline/operations/"+operationID, nil)
	request.Header.Set("Authorization", "Bearer token")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || service.method != "status" || service.operationID != operationID || !strings.Contains(response.Body.String(), `"aggregate_version":"4"`) {
		t.Fatalf("status=%d body=%s service=%+v", response.Code, response.Body.String(), service)
	}

	calls := service.calls
	request = httptest.NewRequest(http.MethodPost, "/v1/learning/offline/packs", strings.NewReader(strings.TrimSuffix(prepareBody, "}")+`,"unknown":true}`))
	request.Header.Set("Authorization", "Bearer token")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest || service.calls != calls {
		t.Fatalf("unknown prepare field status=%d calls=%d body=%s", response.Code, service.calls, response.Body.String())
	}

	writeOnlyHandler := newOfflineHTTPTestAPI(t, []string{"learning:read"}, service, 8<<20)
	request = httptest.NewRequest(http.MethodPost, "/v1/learning/offline/packs", strings.NewReader(prepareBody))
	request.Header.Set("Authorization", "Bearer token")
	response = httptest.NewRecorder()
	writeOnlyHandler.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("missing write scope status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestOfflineAssessmentHTTPContractsAreIndependentClosedAndReplayable(t *testing.T) {
	const (
		deviceID     = "90000000-0000-4000-8000-000000000001"
		assessmentID = "81000000-0000-4000-8000-000000000001"
		attemptID    = "82000000-0000-4000-8000-000000000001"
		submissionID = "83000000-0000-4000-8000-000000000001"
		decisionID   = "84000000-0000-4000-8000-000000000001"
	)
	service := &fakeOfflineLearning{
		assessmentsOut: learning.OfflineAssessmentPage{Items: []learning.OfflineAssessmentSummary{{
			AssessmentID: assessmentID, AttemptID: attemptID, ActivityID: "85000000-0000-4000-8000-000000000001",
			ActivityRevision: "1", SubmissionID: submissionID, AggregateVersion: "2", DispositionVersion: "1",
			Disposition: learning.DispositionProvisional, AllowedDecisions: []string{"override", "void"},
		}}},
		assessmentOut: learning.OfflineAssessmentView{
			SubmissionID: submissionID, AggregateVersion: "2", AllowedDecisions: []string{"override", "void"},
			Assessment: learning.AssessmentArtifact{ID: assessmentID}, Attempt: learning.Attempt{ID: attemptID},
		},
		decisionOut: learning.OfflineAssessmentDecisionReceipt{
			OperationID: decisionID, AssessmentID: assessmentID, AttemptID: attemptID, SubmissionID: submissionID,
			AggregateVersion: "3", FirstEventSequence: "20", LastEventSequence: "20", ProjectionAsOfEventSequence: "20",
			Decision: learning.AssessmentDecision{ID: decisionID, AssessmentID: assessmentID, Version: 2, Disposition: learning.DispositionVoided, Items: []learning.AssessmentItem{}, ActorDeviceID: deviceID, CreatedAt: time.Now().UTC()},
		},
	}
	handler := newOfflineHTTPTestAPI(t, []string{"learning:read", "learning:write"}, service, 1<<20)

	request := httptest.NewRequest(http.MethodGet, "/v1/learning/offline/assessments?status=provisional&limit=1", nil)
	request.Header.Set("Authorization", "Bearer token")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || service.method != "assessments" || service.actor != deviceID || service.assessmentQuery.Status != "provisional" || service.assessmentQuery.Page.Limit != 1 {
		t.Fatalf("assessment list status=%d body=%s service=%+v", response.Code, response.Body.String(), service)
	}
	if strings.Contains(response.Body.String(), "answer") || strings.Contains(response.Body.String(), "signature") {
		t.Fatalf("assessment list leaked body fields: %s", response.Body.String())
	}

	request = httptest.NewRequest(http.MethodGet, "/v1/learning/offline/assessments/"+assessmentID, nil)
	request.Header.Set("Authorization", "Bearer token")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || service.method != "assessment" || service.assessmentID != assessmentID {
		t.Fatalf("assessment show status=%d body=%s service=%+v", response.Code, response.Body.String(), service)
	}

	body := `{"operation_id":"` + decisionID + `","payload_schema_version":1,"attempt_id":"` + attemptID + `","expected_version":"2","kind":"void","expected_disposition_version":"1","reason":"invalid assessment"}`
	request = httptest.NewRequest(http.MethodPost, "/v1/learning/offline/assessments/"+assessmentID+"/decisions", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer token")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusCreated || service.method != "assessment_decision" || service.decision.OperationID != decisionID || service.decision.AttemptID != attemptID || service.decision.ExpectedVersion != 2 || service.decision.ExpectedDispositionVersion != 1 || service.decision.Kind != "void" {
		t.Fatalf("assessment decision status=%d body=%s service=%+v", response.Code, response.Body.String(), service)
	}
	service.decisionOut.Replayed = true
	request = httptest.NewRequest(http.MethodPost, "/v1/learning/offline/assessments/"+assessmentID+"/decisions", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer token")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"replayed":true`) {
		t.Fatalf("assessment decision replay status=%d body=%s", response.Code, response.Body.String())
	}

	service.err = &learning.Error{
		Code: learning.CodeVersionConflict, AggregateType: "offline_attempt", AggregateID: submissionID,
		ExpectedVersion: 2, CurrentVersion: 3, AsOfEventSequence: 20,
	}
	request = httptest.NewRequest(http.MethodPost, "/v1/learning/offline/assessments/"+assessmentID+"/decisions", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer token")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), `"aggregate_type":"offline_attempt"`) || !strings.Contains(response.Body.String(), `"current_version":3`) {
		t.Fatalf("assessment attempt version conflict status=%d body=%s", response.Code, response.Body.String())
	}
	service.err = &learning.Error{Code: learning.CodeAssessmentDispositionConflict, CurrentDisposition: string(learning.DispositionProvisional)}
	request = httptest.NewRequest(http.MethodPost, "/v1/learning/offline/assessments/"+assessmentID+"/decisions", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer token")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), `"current_disposition":"provisional"`) {
		t.Fatalf("assessment disposition conflict status=%d body=%s", response.Code, response.Body.String())
	}
	service.err = nil

	calls := service.calls
	request = httptest.NewRequest(http.MethodPost, "/v1/learning/offline/assessments/"+assessmentID+"/decisions", strings.NewReader(strings.TrimSuffix(body, "}")+`,"answer":"secret"}`))
	request.Header.Set("Authorization", "Bearer token")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest || service.calls != calls {
		t.Fatalf("unknown decision field status=%d calls=%d body=%s", response.Code, service.calls, response.Body.String())
	}
	request = httptest.NewRequest(http.MethodGet, "/v1/learning/offline/assessments?unexpected=true", nil)
	request.Header.Set("Authorization", "Bearer token")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest || service.calls != calls {
		t.Fatalf("unknown assessment query status=%d calls=%d body=%s", response.Code, service.calls, response.Body.String())
	}

	readOnly := newOfflineHTTPTestAPI(t, []string{"learning:read"}, service, 1<<20)
	request = httptest.NewRequest(http.MethodPost, "/v1/learning/offline/assessments/"+assessmentID+"/decisions", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer token")
	response = httptest.NewRecorder()
	readOnly.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("offline assessment decision write scope status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestOfflineAssessmentHTTPEnforcesUnicodeStringLimitsBeforeService(t *testing.T) {
	const (
		assessmentID = "81000000-0000-4000-8000-000000000001"
		attemptID    = "82000000-0000-4000-8000-000000000001"
		decisionID   = "84000000-0000-4000-8000-000000000001"
	)
	service := &fakeOfflineLearning{}
	handler := newOfflineHTTPTestAPI(t, []string{"learning:write"}, service, 1<<20)
	post := func(body string) int {
		t.Helper()
		request := httptest.NewRequest(http.MethodPost, "/v1/learning/offline/assessments/"+assessmentID+"/decisions", strings.NewReader(body))
		request.Header.Set("Authorization", "Bearer token")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response.Code
	}
	voidBody := func(reason string) string {
		return `{"operation_id":"` + decisionID + `","payload_schema_version":1,"attempt_id":"` + attemptID + `","expected_version":"2","kind":"void","expected_disposition_version":"1","reason":"` + reason + `"}`
	}
	overrideBody := func(rubricID, candidate string) string {
		return `{"operation_id":"` + decisionID + `","payload_schema_version":1,"attempt_id":"` + attemptID + `","expected_version":"2","kind":"override","expected_disposition_version":"1","reason":"valid","items":[{"rubric_item_id":"` + rubricID + `","conclusion":"partial","misconception_candidate":"` + candidate + `"}]}`
	}

	if status := post(voidBody(strings.Repeat("界", learning.MaxOfflineAssessmentDecisionReasonRunes))); status != http.StatusCreated || service.calls != 1 {
		t.Fatalf("boundary reason status=%d calls=%d", status, service.calls)
	}
	if status := post(voidBody(strings.Repeat("界", learning.MaxOfflineAssessmentDecisionReasonRunes+1))); status != http.StatusBadRequest || service.calls != 1 {
		t.Fatalf("overlong reason status=%d calls=%d", status, service.calls)
	}
	if status := post(overrideBody(
		strings.Repeat("项", learning.MaxOfflineAssessmentRubricItemIDRunes),
		strings.Repeat("误", learning.MaxOfflineAssessmentMisconceptionRunes),
	)); status != http.StatusCreated || service.calls != 2 {
		t.Fatalf("boundary override status=%d calls=%d", status, service.calls)
	}
	if status := post(overrideBody(strings.Repeat("项", learning.MaxOfflineAssessmentRubricItemIDRunes+1), "")); status != http.StatusBadRequest || service.calls != 2 {
		t.Fatalf("overlong rubric ID status=%d calls=%d", status, service.calls)
	}
	if status := post(overrideBody("item-1", strings.Repeat("误", learning.MaxOfflineAssessmentMisconceptionRunes+1))); status != http.StatusBadRequest || service.calls != 2 {
		t.Fatalf("overlong misconception status=%d calls=%d", status, service.calls)
	}
}

func TestOfflineHTTPMapsSignerFailureAndBodyLimit(t *testing.T) {
	service := &fakeOfflineLearning{err: &learning.Error{Code: learning.CodeOfflineSignerUnavailable}}
	handler := newOfflineHTTPTestAPI(t, []string{"learning:write"}, service, 256)
	body := `{"operation_id":"10000000-0000-4000-8000-000000000001","payload_schema_version":1,"expected_session_version":"7","trusted_manifest_revision":"0","trusted_manifest_digest":"` + learning.OfflineZeroDigest + `"}`
	request := httptest.NewRequest(http.MethodPost, "/v1/learning/offline/packs", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer token")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), learning.CodeOfflineSignerUnavailable) {
		t.Fatalf("signer failure status=%d body=%s", response.Code, response.Body.String())
	}

	oversized := `{"sync_request_id":"70000000-0000-4000-8000-000000000001","payload_schema_version":1,"operations":["` + strings.Repeat("x", 300) + `"]}`
	request = httptest.NewRequest(http.MethodPost, "/v1/learning/offline/sync", strings.NewReader(oversized))
	request.Header.Set("Authorization", "Bearer token")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("offline body limit status=%d body=%s", response.Code, response.Body.String())
	}
}

func newOfflineHTTPTestAPI(t *testing.T, scopes []string, service *fakeOfflineLearning, maxBody int64) http.Handler {
	t.Helper()
	var logs bytes.Buffer
	id := &fakeIdentity{auth: identity.Credential{Device: identity.Device{ID: "90000000-0000-4000-8000-000000000001"}, Scopes: scopes}}
	handler, err := New(Options{
		Identity:              id,
		Offline:               service,
		Readiness:             fakeReadiness{},
		Logger:                slog.New(slog.NewJSONHandler(&logs, nil)),
		PairLimiter:           NewFixedWindowLimiter(100, time.Minute),
		AuthLimiter:           NewFixedWindowLimiter(100, time.Minute),
		DeviceLimiter:         NewFixedWindowLimiter(100, time.Minute),
		MaxOfflineRequestBody: maxBody,
	})
	if err != nil {
		t.Fatal(err)
	}
	return handler
}
