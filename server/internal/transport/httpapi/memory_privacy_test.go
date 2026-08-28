package httpapi

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/identity"
	"github.com/edu-agent/edu-agent/server/internal/memory"
	"github.com/edu-agent/edu-agent/server/internal/platform/health"
	"github.com/edu-agent/edu-agent/server/internal/privacy"
	"github.com/getkin/kin-openapi/openapi3"
)

const (
	testDeviceID    = "90000000-0000-4000-8000-000000000001"
	testCandidateID = "20000000-0000-4000-8000-000000000001"
	testMemoryID    = "30000000-0000-4000-8000-000000000001"
	testDeliveryID  = "40000000-0000-4000-8000-000000000001"
	testRecordID    = "50000000-0000-4000-8000-000000000001"
	testReceiptID   = "60000000-0000-4000-8000-000000000001"
	testErasureID   = "70000000-0000-4000-8000-000000000001"
)

type fakeMemoryHTTP struct {
	mu                sync.Mutex
	method            string
	principal         memory.DevicePrincipal
	candidate         memory.CandidateView
	candidatePg       memory.CandidatePage
	record            memory.RecordView
	recordPg          memory.RecordPage
	operation         memory.OperationResult
	createCommand     memory.CreateCandidateCommand
	correctionCommand memory.CreateCorrectionCandidateCommand
	err               error
	calls             int
	candidateFn       func(context.Context, string) (memory.CandidateView, error)
}

func (f *fakeMemoryHTTP) called(method string, principal memory.DevicePrincipal) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.method, f.principal, f.calls = method, principal, f.calls+1
}
func (f *fakeMemoryHTTP) CreateCandidate(_ context.Context, principal memory.DevicePrincipal, command memory.CreateCandidateCommand) (memory.OperationResult, error) {
	f.called("create_candidate", principal)
	f.createCommand = command
	return f.operation, f.err
}
func (f *fakeMemoryHTTP) CreateCorrectionCandidate(_ context.Context, principal memory.DevicePrincipal, command memory.CreateCorrectionCandidateCommand) (memory.OperationResult, error) {
	f.called("create_correction", principal)
	f.correctionCommand = command
	return f.operation, f.err
}
func (f *fakeMemoryHTTP) DecideCandidate(_ context.Context, principal memory.DevicePrincipal, _ memory.DecideCandidateCommand) (memory.OperationResult, error) {
	f.called("decide_candidate", principal)
	return f.operation, f.err
}
func (f *fakeMemoryHTTP) DeleteRecord(_ context.Context, principal memory.DevicePrincipal, _ memory.DeleteRecordCommand) (memory.OperationResult, error) {
	f.called("delete_record", principal)
	return f.operation, f.err
}
func (f *fakeMemoryHTTP) ReplayDelivery(_ context.Context, principal memory.DevicePrincipal, _ memory.ReplayDeliveryCommand) (memory.OperationResult, error) {
	f.called("replay_delivery", principal)
	return f.operation, f.err
}
func (f *fakeMemoryHTTP) Candidate(ctx context.Context, id string) (memory.CandidateView, error) {
	f.called("candidate", memory.DevicePrincipal{})
	if f.candidateFn != nil {
		return f.candidateFn(ctx, id)
	}
	return f.candidate, f.err
}
func (f *fakeMemoryHTTP) ListCandidates(context.Context, memory.PageRequest) (memory.CandidatePage, error) {
	f.called("list_candidates", memory.DevicePrincipal{})
	return f.candidatePg, f.err
}
func (f *fakeMemoryHTTP) ListRecords(context.Context, memory.PageRequest) (memory.RecordPage, error) {
	f.called("list_records", memory.DevicePrincipal{})
	return f.recordPg, f.err
}
func (f *fakeMemoryHTTP) Record(context.Context, string) (memory.RecordView, error) {
	f.called("record", memory.DevicePrincipal{})
	return f.record, f.err
}

type fakeMemoryExporter struct {
	page        memory.ExportPage
	detail      memory.RecordDetail
	err         error
	calls       int
	detailCalls int
	detailFn    func(context.Context, string) (memory.RecordDetail, error)
}

func (f *fakeMemoryExporter) Detail(ctx context.Context, id string) (memory.RecordDetail, error) {
	f.detailCalls++
	if f.detailFn != nil {
		return f.detailFn(ctx, id)
	}
	return f.detail, f.err
}

func (f *fakeMemoryExporter) Export(context.Context, memory.PageRequest) (memory.ExportPage, error) {
	f.calls++
	return f.page, f.err
}

type fakePrivacyHTTP struct {
	commitRequest  privacy.ErasureRequest
	receipt        privacy.ErasureReceipt
	purgeChallenge privacy.OfflinePurgeChallenge
	purgeFound     bool
	childReceipt   privacy.OfflineDeviceChildReceipt
	validToken     string
	grantUsed      bool
	operationID    string
	operationHash  string
	httpNow        time.Time
	commitErr      error
	receiptErr     error
	localErr       error
	remoteErr      error
	ackErr         error
	ackErasureID   string
	ackDeviceID    string
	acknowledged   privacy.OfflineDevicePurgeAcknowledgment
	calls          []string
}

func (f *fakePrivacyHTTP) AuthorizeAndCommitBarrier(_ context.Context, request privacy.ErasureRequest, authorization privacy.ErasureGrantAuthorization) (privacy.ErasureReceipt, error) {
	f.calls = append(f.calls, "commit")
	f.commitRequest = request
	hash, err := request.OperationHash()
	if err != nil {
		return privacy.ErasureReceipt{}, err
	}
	if f.operationID == request.OperationID {
		if f.operationHash != hash {
			return privacy.ErasureReceipt{}, &privacy.Error{Code: privacy.CodeIdempotencyConflict, Reason: "operation_hash_mismatch"}
		}
		return f.receipt, nil
	}
	expected := privacy.NewErasureGrantAuthorization(request.DeviceID, f.validToken)
	if !authorization.Canonical || authorization.DeviceID != request.DeviceID || authorization.CandidateHash != expected.CandidateHash || f.grantUsed {
		return privacy.ErasureReceipt{}, privacy.ErrErasureGrantInvalid
	}
	if f.commitErr != nil {
		return privacy.ErasureReceipt{}, f.commitErr
	}
	f.grantUsed = true
	f.operationID, f.operationHash = request.OperationID, hash
	return f.receipt, nil
}
func (f *fakePrivacyHTTP) Receipt(context.Context, string) (privacy.ErasureReceipt, error) {
	f.calls = append(f.calls, "receipt")
	return f.receipt, f.receiptErr
}
func (f *fakePrivacyHTTP) RunLocal(context.Context, string) (privacy.ErasureReceipt, error) {
	f.calls = append(f.calls, "local")
	return f.receipt, f.localErr
}
func (f *fakePrivacyHTTP) RunNocturne(context.Context, string) (privacy.ErasureReceipt, error) {
	f.calls = append(f.calls, "nocturne")
	return f.receipt, f.remoteErr
}
func (f *fakePrivacyHTTP) CurrentOfflineDevicePurge(_ context.Context, deviceID string) (privacy.OfflinePurgeChallenge, bool, error) {
	f.calls = append(f.calls, "offline_get")
	if f.purgeChallenge.DeviceID != "" && f.purgeChallenge.DeviceID != deviceID {
		return privacy.OfflinePurgeChallenge{}, false, nil
	}
	return f.purgeChallenge, f.purgeFound, f.ackErr
}
func (f *fakePrivacyHTTP) AcknowledgeOfflineDevicePurge(_ context.Context, erasureID, deviceID string, acknowledgment privacy.OfflineDevicePurgeAcknowledgment) (privacy.OfflineDeviceChildReceipt, error) {
	f.calls = append(f.calls, "offline_ack")
	f.ackErasureID, f.ackDeviceID, f.acknowledged = erasureID, deviceID, acknowledgment
	return f.childReceipt, f.ackErr
}

type singleUseGrant struct {
	validToken string
}

func canonicalGrantToken(fill byte) string {
	return base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{fill}, 32))
}

func memoryHTTPFixtures() (memory.CandidateView, memory.RecordView, memory.OperationResult) {
	now := time.Date(2026, time.August, 20, 14, 0, 0, 0, time.UTC)
	candidate := memory.CandidateView{
		Candidate: memory.Candidate{
			ID: testCandidateID, URI: memory.CandidateURI(testCandidateID), PayloadID: testReceiptID,
			ContentHash: strings.Repeat("a", 64), Source: memory.SourceUserStatement,
			ProposerID: testDeviceID, Reason: "remember", Category: memory.CategoryInteractionPreference,
			Sensitivity: memory.SensitivityNonSensitive, Stability: memory.StabilityStable,
			ValidUntil: now.Add(time.Hour), PolicyVersion: memory.AdmissionPolicyVersion,
			Status: memory.CandidatePending, Revision: 1, CreatedAt: now,
		},
		ContentStatus: "available", ProposedContent: "private candidate content",
		ReadGeneration: memory.GenerationStamp{LearnerGeneration: 1, MemoryGeneration: 1},
	}
	record := memory.Record{
		LogicalMemoryID: testMemoryID, ID: testRecordID, Revision: 1, RecordGeneration: 1,
		LearnerGeneration: 1, CandidateID: testCandidateID,
		ExternalURI: memory.DeterministicExternalURI(testMemoryID), ExternalURIDigest: memory.SHA256String(memory.DeterministicExternalURI(testMemoryID)),
		ContentHash: strings.Repeat("b", 64), Status: memory.RecordApplied,
		DeliveryID: testDeliveryID, ReceiptID: testReceiptID, CreatedAt: now,
	}
	delivery := memory.Delivery{
		ID: testDeliveryID, Kind: memory.DeliveryAdmit, LogicalMemoryID: testMemoryID,
		RecordRevisionID: testRecordID, RecordRevision: 1, LearnerGeneration: 1,
		RecordGeneration: 1, PayloadID: testReceiptID, PayloadHash: strings.Repeat("b", 64),
		ExternalURI: memory.DeterministicExternalURI(testMemoryID), AttemptState: memory.AttemptPrepared,
		Status: memory.DeliveryStatusQueued, PublicStatus: memory.DeliveryQueued,
		ValidUntil: now.Add(time.Hour), ReceiptID: testReceiptID, CreatedAt: now, UpdatedAt: now,
	}
	receipt := memory.Receipt{ID: testReceiptID, DeliveryID: testDeliveryID, Version: 1, Status: memory.ReceiptPending, Reason: "queued", VerificationMethod: "pending", CreatedAt: now}
	view := memory.RecordView{Record: record, Delivery: delivery, Receipt: receipt, ReadGeneration: memory.GenerationStamp{LearnerGeneration: 1, MemoryGeneration: 1}}
	return candidate, view, memory.OperationResult{Candidate: candidate, Record: &record, Delivery: &delivery}
}

func newMemoryPrivacyAPI(t *testing.T, scopes []string, memoryService *fakeMemoryHTTP, exporter *fakeMemoryExporter, privacyService *fakePrivacyHTTP, grant *singleUseGrant, permits *privacy.ReadPermitManager, logs *bytes.Buffer) http.Handler {
	t.Helper()
	id := &fakeIdentity{auth: identity.Credential{Device: identity.Device{ID: testDeviceID}, Scopes: scopes}}
	options := Options{
		Identity: id, Readiness: fakeReadiness{report: health.Report{Status: health.StatusHealthy}},
		Logger: slog.New(slog.NewJSONHandler(logs, nil)), PairLimiter: NewFixedWindowLimiter(100, time.Minute),
		AuthLimiter: NewFixedWindowLimiter(100, time.Minute), DeviceLimiter: NewFixedWindowLimiter(100, time.Minute),
		PrivacyLimiter: NewFixedWindowLimiter(100, time.Minute), ReadPermits: permits,
		Now: func() time.Time {
			if privacyService != nil && !privacyService.httpNow.IsZero() {
				return privacyService.httpNow
			}
			return time.Date(2026, time.August, 20, 14, 0, 0, 0, time.UTC)
		},
		PrivacyBackupDeadline: 7 * 24 * time.Hour,
	}
	if memoryService != nil {
		options.Memory, options.MemoryExporter = memoryService, exporter
	}
	if privacyService != nil {
		options.Privacy = privacyService
		if grant != nil {
			privacyService.validToken = grant.validToken
		}
	}
	handler, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	return handler
}

func authenticatedRequest(handler http.Handler, method, path, body string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer valid-device-token")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func TestMemoryRoutesMethodsScopesAndActors(t *testing.T) {
	candidate, record, operation := memoryHTTPFixtures()
	service := &fakeMemoryHTTP{
		candidate: candidate, candidatePg: memory.CandidatePage{Items: []memory.CandidateView{candidate}, ReadGeneration: candidate.ReadGeneration},
		record: record, recordPg: memory.RecordPage{Items: []memory.Record{record.Record}, ReadGeneration: record.ReadGeneration}, operation: operation,
	}
	exporter := &fakeMemoryExporter{
		page: memory.ExportPage{Items: []memory.ExportItem{}, ReadGeneration: candidate.ReadGeneration, ReasonCodes: []string{}},
		detail: memory.RecordDetail{Record: record.Record, Delivery: record.Delivery, Receipt: record.Receipt,
			ReadGeneration: record.ReadGeneration, ContentStatus: memory.ExportContentUnavailable},
	}
	var logs bytes.Buffer
	handler := newMemoryPrivacyAPI(t, []string{"memory:read", "memory:write"}, service, exporter, nil, nil, privacy.NewReadPermitManager(), &logs)
	candidateBody := `{"operation_id":"` + testOperationID + `","payload_schema_version":1,"content":"remember concise answers","reason":"explicit preference","category":"interaction_preference","sensitivity":"non_sensitive","stability":"stable","valid_until":"2030-01-01T00:00:00Z"}`
	decisionBody := `{"operation_id":"` + testOperationID + `","payload_schema_version":1,"expected_revision":1,"decision":"reject","reason":"do not retain"}`
	correctionBody := `{"operation_id":"` + testOperationID + `","payload_schema_version":1,"content":"remember detailed answers","reason":"updated preference","category":"interaction_preference","sensitivity":"non_sensitive","stability":"stable","valid_until":"2030-01-01T00:00:00Z","expected_record_revision":1,"expected_record_generation":1}`
	deleteBody := `{"operation_id":"` + testOperationID + `","payload_schema_version":1,"expected_revision":1,"expected_record_generation":1}`
	replayBody := `{"operation_id":"` + testOperationID + `","payload_schema_version":1}`
	cases := []struct {
		method, path, body, called string
		status                     int
	}{
		{http.MethodPost, "/v1/memory/candidates", candidateBody, "create_candidate", http.StatusCreated},
		{http.MethodGet, "/v1/memory/candidates", "", "list_candidates", http.StatusOK},
		{http.MethodGet, "/v1/memory/candidates/" + testCandidateID, "", "candidate", http.StatusOK},
		{http.MethodPost, "/v1/memory/candidates/" + testCandidateID + "/decisions", decisionBody, "decide_candidate", http.StatusOK},
		{http.MethodGet, "/v1/memory/records", "", "list_records", http.StatusOK},
		{http.MethodGet, "/v1/memory/records/" + testMemoryID, "", "record_detail", http.StatusOK},
		{http.MethodPost, "/v1/memory/records/" + testMemoryID + "/candidates", correctionBody, "create_correction", http.StatusCreated},
		{http.MethodDelete, "/v1/memory/records/" + testMemoryID, deleteBody, "delete_record", http.StatusAccepted},
		{http.MethodGet, "/v1/memory/export", "", "export", http.StatusOK},
		{http.MethodPost, "/v1/memory/deliveries/" + testDeliveryID + "/replays", replayBody, "replay_delivery", http.StatusAccepted},
	}
	for _, test := range cases {
		service.method = ""
		beforeExport := exporter.calls
		response := authenticatedRequest(handler, test.method, test.path, test.body)
		if response.Code != test.status {
			t.Fatalf("%s %s = %d, want %d: %s", test.method, test.path, response.Code, test.status, response.Body.String())
		}
		if test.called == "export" {
			if exporter.calls != beforeExport+1 {
				t.Fatalf("export was not called")
			}
		} else if test.called == "record_detail" {
			if exporter.detailCalls == 0 || service.method != "" {
				t.Fatalf("record detail did not use live exporter: detail_calls=%d service=%q", exporter.detailCalls, service.method)
			}
		} else if service.method != test.called {
			t.Fatalf("%s %s called %q, want %q", test.method, test.path, service.method, test.called)
		}
		if strings.Contains(test.called, "create") || test.called == "decide_candidate" || test.called == "delete_record" || test.called == "replay_delivery" {
			if service.principal.DeviceID != testDeviceID {
				t.Fatalf("%s actor = %q", test.called, service.principal.DeviceID)
			}
		}
	}

	readOnly := newMemoryPrivacyAPI(t, []string{"knowledge:read", "knowledge:write", "learning:read", "learning:write", "memory:read"}, service, exporter, nil, nil, privacy.NewReadPermitManager(), &logs)
	for _, request := range []struct {
		method string
		path   string
		body   string
	}{
		{http.MethodPost, "/v1/memory/candidates", candidateBody},
		{http.MethodPost, "/v1/memory/candidates/" + testCandidateID + "/decisions", decisionBody},
		{http.MethodDelete, "/v1/memory/records/" + testMemoryID, deleteBody},
	} {
		service.method = ""
		response := authenticatedRequest(readOnly, request.method, request.path, request.body)
		if response.Code != http.StatusForbidden || service.method != "" {
			t.Fatalf("restricted Agent memory path %s %s = %d method=%q", request.method, request.path, response.Code, service.method)
		}
	}
	response := authenticatedRequest(handler, http.MethodPatch, "/v1/memory/candidates/"+testCandidateID, "")
	if response.Code != http.StatusMethodNotAllowed {
		t.Fatalf("wrong method = %d", response.Code)
	}
}

func TestMemoryStrictJSONUUIDQueryBodyAndErrorMapping(t *testing.T) {
	candidate, _, operation := memoryHTTPFixtures()
	service := &fakeMemoryHTTP{candidate: candidate, operation: operation}
	exporter := &fakeMemoryExporter{}
	var logs bytes.Buffer
	handler := newMemoryPrivacyAPI(t, []string{"memory:read", "memory:write"}, service, exporter, nil, nil, privacy.NewReadPermitManager(), &logs)
	valid := `{"operation_id":"` + testOperationID + `","payload_schema_version":1,"content":"secret memory text","reason":"explicit","category":"interaction_preference","sensitivity":"non_sensitive","stability":"stable","valid_until":"2030-01-01T00:00:00Z"}`
	invalidBodies := []string{
		strings.Replace(valid, `}`, `,"source_kind":"user_statement"}`, 1),
		strings.Replace(valid, `}`, `,"source_event_id":"`+testCandidateID+`"}`, 1),
		strings.Replace(valid, `}`, `,"source_operation_id":"`+testOperationID+`"}`, 1),
		strings.Replace(valid, `"reason":"explicit"`, `"reason":null`, 1),
		strings.Replace(valid, `"reason":"explicit"`, `"reason":"explicit","reason":"duplicate"`, 1),
		strings.Replace(valid, testOperationID, "10000000000040008000000000000001", 1),
	}
	for _, body := range invalidBodies {
		before := service.calls
		response := authenticatedRequest(handler, http.MethodPost, "/v1/memory/candidates", body)
		if response.Code != http.StatusBadRequest || service.calls != before {
			t.Fatalf("strict body = %d calls=%d body=%s", response.Code, service.calls-before, response.Body.String())
		}
	}
	for _, path := range []string{
		"/v1/memory/candidates?limit=0", "/v1/memory/candidates?limit=201",
		"/v1/memory/candidates?limit=01", "/v1/memory/candidates?limit=5&limit=6",
		"/v1/memory/candidates?unknown=x", "/v1/memory/candidates?cursor=",
		"/v1/memory/candidates/20000000000040008000000000000001",
	} {
		before := service.calls
		response := authenticatedRequest(handler, http.MethodGet, path, "")
		if response.Code != http.StatusBadRequest || service.calls != before {
			t.Fatalf("strict query/UUID %s = %d calls=%d", path, response.Code, service.calls-before)
		}
	}
	large := strings.Replace(valid, "secret memory text", strings.Repeat("x", 1<<20), 1)
	response := authenticatedRequest(handler, http.MethodPost, "/v1/memory/candidates", large)
	if response.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("body limit = %d", response.Code)
	}

	offset := strings.Replace(valid, "2030-01-01T00:00:00Z", "2030-01-01T00:00:00+00:00", 1)
	response = authenticatedRequest(handler, http.MethodPost, "/v1/memory/candidates", offset)
	if response.Code != http.StatusCreated || service.createCommand.ValidUntil.Location() != time.UTC ||
		service.createCommand.SourceEventID != "" || service.createCommand.SourceOperationID != "" {
		t.Fatalf("RFC3339 offset/provenance = %d command=%+v body=%s", response.Code, service.createCommand, response.Body.String())
	}

	for code, status := range map[string]int{
		memory.CodeNotFound:             http.StatusNotFound,
		memory.CodeCandidateConflict:    http.StatusConflict,
		memory.CodeMemoryPolicyRejected: http.StatusUnprocessableEntity,
		memory.CodeMemoryUnavailable:    http.StatusServiceUnavailable,
	} {
		service.err = &memory.Error{Code: code}
		response = authenticatedRequest(handler, http.MethodGet, "/v1/memory/candidates/"+testCandidateID, "")
		if response.Code != status || !strings.Contains(response.Body.String(), `"code":"`+code+`"`) {
			t.Fatalf("error %s = %d %s", code, response.Code, response.Body.String())
		}
	}
	service.err = errors.New("private upstream body secret")
	response = authenticatedRequest(handler, http.MethodGet, "/v1/memory/candidates/"+testCandidateID, "")
	if response.Code != http.StatusInternalServerError || strings.Contains(response.Body.String(), "upstream body") || strings.Contains(logs.String(), "upstream body") || strings.Contains(logs.String(), "secret memory text") {
		t.Fatalf("internal redaction response=%s logs=%s", response.Body.String(), logs.String())
	}
}

func TestPrivacyErasureGrantOrderSingleUseAndServerFields(t *testing.T) {
	now := time.Date(2026, time.August, 20, 14, 0, 0, 0, time.UTC)
	service := &fakePrivacyHTTP{httpNow: now, receipt: privacy.ErasureReceipt{
		ErasureID: testErasureID, Status: privacy.StatusLocalScrubbed, SummaryVersion: 2,
		LearnerGeneration: 2, PolicyVersion: privacy.PolicyVersion, ReasonCode: string(privacy.ReasonLearnerRequest),
		RequestedAt: now, UpdatedAt: now, Steps: []privacy.StepReceipt{},
	}}
	grant := &singleUseGrant{validToken: canonicalGrantToken(0x11)}
	var logs bytes.Buffer
	handler := newMemoryPrivacyAPI(t, []string{"privacy:read"}, nil, nil, service, grant, privacy.NewReadPermitManager(), &logs)
	body := `{"operation_id":"` + testOperationID + `","payload_schema_version":1,"expected_current_learner_generation":1,"reason_code":"learner_request","explicit_confirmation":true}`

	request := httptest.NewRequest(http.MethodPost, "/v1/privacy/erasures", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer valid-device-token")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden || strings.Join(service.calls, ",") != "commit" || service.grantUsed {
		t.Fatalf("missing grant = %d calls=%v used=%v", response.Code, service.calls, service.grantUsed)
	}

	request = httptest.NewRequest(http.MethodPost, "/v1/privacy/erasures", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer valid-device-token")
	request.Header.Set(privacyErasureGrantHeader, "wrong-grant")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden || strings.Join(service.calls, ",") != "commit,commit" || service.grantUsed {
		t.Fatalf("wrong grant = %d calls=%v used=%v", response.Code, service.calls, service.grantUsed)
	}

	request = httptest.NewRequest(http.MethodPost, "/v1/privacy/erasures", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer valid-device-token")
	request.Header.Set(privacyErasureGrantHeader, grant.validToken)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusAccepted || strings.Join(service.calls, ",") != "commit,commit,commit,local,nocturne" || !service.grantUsed {
		t.Fatalf("valid grant = %d calls=%v used=%v body=%s", response.Code, service.calls, service.grantUsed, response.Body.String())
	}
	if service.commitRequest.DeviceID != testDeviceID || service.commitRequest.ActorDeviceID != testDeviceID || !service.commitRequest.RequestedAt.Equal(now) || !service.commitRequest.ManagedBackupUnrecoverableAfter.Equal(now.Add(7*24*time.Hour)) {
		t.Fatalf("server-owned erasure fields: request=%+v", service.commitRequest)
	}

	service.httpNow = now.Add(3 * time.Hour)
	response = authenticatedRequest(handler, http.MethodPost, "/v1/privacy/erasures", body)
	if response.Code != http.StatusAccepted || !service.commitRequest.RequestedAt.Equal(service.httpNow) || strings.Join(service.calls[len(service.calls)-3:], ",") != "commit,local,nocturne" {
		t.Fatalf("lost-response retry = %d calls=%v request=%+v body=%s", response.Code, service.calls, service.commitRequest, response.Body.String())
	}
	newOperation := strings.Replace(body, testOperationID, "10000000-0000-4000-8000-000000000099", 1)
	response = authenticatedRequest(handler, http.MethodPost, "/v1/privacy/erasures", newOperation)
	if response.Code != http.StatusForbidden {
		t.Fatalf("consumed grant authorized a new operation: %d %s", response.Code, response.Body.String())
	}
	if strings.Contains(logs.String(), grant.validToken) || strings.Contains(logs.String(), "single-use-grant") {
		t.Fatalf("grant leaked to logs: %s", logs.String())
	}
}

func TestPrivacyVerifiedReplayReturnsOriginalReceiptWithoutRerunningErasure(t *testing.T) {
	requestedAt := time.Date(2026, time.August, 20, 14, 0, 0, 0, time.UTC)
	service := &fakePrivacyHTTP{httpNow: requestedAt, receipt: privacy.ErasureReceipt{
		ErasureID: testErasureID, Status: privacy.StatusBarrierCommitted, SummaryVersion: 1,
		LearnerGeneration: 2, PolicyVersion: privacy.PolicyVersion, ReasonCode: string(privacy.ReasonLearnerRequest),
		RequestedAt: requestedAt, UpdatedAt: requestedAt, Steps: []privacy.StepReceipt{},
	}}
	grant := &singleUseGrant{validToken: canonicalGrantToken(0x71)}
	var logs bytes.Buffer
	handler := newMemoryPrivacyAPI(t, []string{"privacy:read"}, nil, nil, service, grant, privacy.NewReadPermitManager(), &logs)
	body := `{"operation_id":"` + testOperationID + `","payload_schema_version":1,"expected_current_learner_generation":1,"reason_code":"learner_request","explicit_confirmation":true}`
	request := httptest.NewRequest(http.MethodPost, "/v1/privacy/erasures", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer valid-device-token")
	request.Header.Set(privacyErasureGrantHeader, grant.validToken)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusAccepted || strings.Join(service.calls, ",") != "commit,local,nocturne" {
		t.Fatalf("initial erasure = %d calls=%v body=%s", response.Code, service.calls, response.Body.String())
	}

	verifiedAt := requestedAt.Add(48 * time.Hour)
	service.receipt.Status = privacy.StatusVerified
	service.receipt.SummaryVersion = 7
	service.receipt.UpdatedAt = verifiedAt
	service.httpNow = requestedAt.Add(30 * 24 * time.Hour)
	service.calls = nil
	response = authenticatedRequest(handler, http.MethodPost, "/v1/privacy/erasures", body)
	if response.Code != http.StatusAccepted || strings.Join(service.calls, ",") != "commit" {
		t.Fatalf("verified replay = %d calls=%v body=%s", response.Code, service.calls, response.Body.String())
	}
	var replayed privacy.ErasureReceipt
	if err := json.Unmarshal(response.Body.Bytes(), &replayed); err != nil {
		t.Fatal(err)
	}
	if replayed.Status != privacy.StatusVerified || replayed.SummaryVersion != 7 || !replayed.RequestedAt.Equal(requestedAt) || !replayed.UpdatedAt.Equal(verifiedAt) {
		t.Fatalf("verified replay receipt=%+v", replayed)
	}
}

func TestPrivacyOfflineDevicePurgeBindsAuthenticatedDeviceAndClosedRequest(t *testing.T) {
	now := time.Date(2026, time.August, 25, 16, 0, 0, 0, time.UTC)
	challenge := canonicalGrantToken(0x63)
	service := &fakePrivacyHTTP{
		purgeFound: true,
		purgeChallenge: privacy.OfflinePurgeChallenge{
			ErasureID: testErasureID, DeviceID: testDeviceID, OldGeneration: 1, CurrentGeneration: 2,
			ChallengeRevision: 1, Challenge: challenge, IssuedAt: now, Status: privacy.OfflineDeviceChildPending,
		},
		childReceipt: privacy.OfflineDeviceChildReceipt{
			ErasureID: testErasureID, DeviceID: testDeviceID, SourceGeneration: 1, CurrentGeneration: 2,
			ChallengeRevision: 1, Status: privacy.OfflineDeviceChildSucceeded, UpdatedAt: now, StableReason: "device_acknowledged",
		},
	}
	var logs bytes.Buffer
	handler := newMemoryPrivacyAPI(t, []string{"privacy:device"}, nil, nil, service, nil, privacy.NewReadPermitManager(), &logs)
	response := authenticatedRequest(handler, http.MethodGet, "/v1/privacy/erasures/"+testErasureID+"/offline-device-purge", "")
	if response.Code != http.StatusOK || strings.Join(service.calls, ",") != "offline_get" || !strings.Contains(response.Body.String(), `"device_id":"`+testDeviceID+`"`) {
		t.Fatalf("purge challenge = %d calls=%v body=%s", response.Code, service.calls, response.Body.String())
	}

	managedAbsent := true
	body := fmt.Sprintf(`{"challenge_revision":1,"challenge":%q,"outcome":"succeeded","managed_objects_absent":%t}`, challenge, managedAbsent)
	response = authenticatedRequest(handler, http.MethodPost, "/v1/privacy/erasures/"+testErasureID+"/offline-device-purge/ack", body)
	if response.Code != http.StatusOK || strings.Join(service.calls, ",") != "offline_get,offline_ack" {
		t.Fatalf("ack = %d calls=%v body=%s", response.Code, service.calls, response.Body.String())
	}
	if service.ackErasureID != testErasureID || service.ackDeviceID != testDeviceID || service.acknowledged.ChallengeRevision != 1 || service.acknowledged.Challenge != challenge || service.acknowledged.Outcome != privacy.OfflinePurgeOutcomeSucceeded || service.acknowledged.ManagedObjectsAbsent == nil || !*service.acknowledged.ManagedObjectsAbsent {
		t.Fatalf("ack binding=%+v erasure=%s device=%s", service.acknowledged, service.ackErasureID, service.ackDeviceID)
	}

	service.calls = nil
	response = authenticatedRequest(handler, http.MethodPost, "/v1/privacy/erasures/"+testErasureID+"/offline-device-purge/ack", strings.TrimSuffix(body, "}")+`,"unexpected":true}`)
	if response.Code != http.StatusBadRequest || len(service.calls) != 0 {
		t.Fatalf("unknown field = %d calls=%v body=%s", response.Code, service.calls, response.Body.String())
	}

	withoutScope := newMemoryPrivacyAPI(t, []string{"privacy:read"}, nil, nil, service, nil, privacy.NewReadPermitManager(), &logs)
	response = authenticatedRequest(withoutScope, http.MethodGet, "/v1/privacy/erasures/"+testErasureID+"/offline-device-purge", "")
	if response.Code != http.StatusForbidden {
		t.Fatalf("missing privacy:device = %d body=%s", response.Code, response.Body.String())
	}

	service.purgeFound = false
	response = authenticatedRequest(handler, http.MethodGet, "/v1/privacy/erasures/"+testErasureID+"/offline-device-purge", "")
	if response.Code != http.StatusNoContent {
		t.Fatalf("no purge task = %d body=%s", response.Code, response.Body.String())
	}
}

func TestPrivacyInvalidRequestsDoNotConsumeGrant(t *testing.T) {
	now := time.Date(2026, time.August, 20, 14, 0, 0, 0, time.UTC)
	service := &fakePrivacyHTTP{httpNow: now, receipt: privacy.ErasureReceipt{
		ErasureID: testErasureID, Status: privacy.StatusBarrierCommitted, SummaryVersion: 1,
		LearnerGeneration: 2, PolicyVersion: privacy.PolicyVersion, ReasonCode: string(privacy.ReasonLearnerRequest),
		RequestedAt: now, UpdatedAt: now, Steps: []privacy.StepReceipt{},
	}}
	grant := &singleUseGrant{validToken: canonicalGrantToken(0x55)}
	var logs bytes.Buffer
	handler := newMemoryPrivacyAPI(t, []string{"privacy:read"}, nil, nil, service, grant, privacy.NewReadPermitManager(), &logs)
	valid := `{"operation_id":"` + testOperationID + `","payload_schema_version":1,"expected_current_learner_generation":1,"reason_code":"learner_request","explicit_confirmation":true}`
	tests := []struct {
		name, path, body string
		status           int
	}{
		{name: "query", path: "/v1/privacy/erasures?unexpected=1", body: valid, status: http.StatusBadRequest},
		{name: "json", path: "/v1/privacy/erasures", body: `{`, status: http.StatusBadRequest},
		{name: "null", path: "/v1/privacy/erasures", body: strings.Replace(valid, `"reason_code":"learner_request"`, `"reason_code":null`, 1), status: http.StatusBadRequest},
		{name: "duplicate", path: "/v1/privacy/erasures", body: strings.Replace(valid, `"reason_code":"learner_request"`, `"reason_code":"learner_request","reason_code":"account_closure"`, 1), status: http.StatusBadRequest},
		{name: "too_large", path: "/v1/privacy/erasures", body: `{"padding":"` + strings.Repeat("x", 1<<20) + `"}`, status: http.StatusRequestEntityTooLarge},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			before := len(service.calls)
			request := httptest.NewRequest(http.MethodPost, test.path, strings.NewReader(test.body))
			request.Header.Set("Authorization", "Bearer valid-device-token")
			request.Header.Set(privacyErasureGrantHeader, grant.validToken)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != test.status || len(service.calls) != before || service.grantUsed {
				t.Fatalf("%s = %d calls=%v used=%v body=%s", test.name, response.Code, service.calls, service.grantUsed, response.Body.String())
			}
		})
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/privacy/erasures", strings.NewReader(valid))
	request.Header.Set("Authorization", "Bearer valid-device-token")
	request.Header.Add(privacyErasureGrantHeader, grant.validToken)
	request.Header.Add(privacyErasureGrantHeader, grant.validToken)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden || len(service.calls) != 0 || service.grantUsed {
		t.Fatalf("duplicate header = %d calls=%v used=%v", response.Code, service.calls, service.grantUsed)
	}
	request = httptest.NewRequest(http.MethodPost, "/v1/privacy/erasures", strings.NewReader(valid))
	request.Header.Set("Authorization", "Bearer valid-device-token")
	request.Header.Set(privacyErasureGrantHeader, grant.validToken)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusAccepted || !service.grantUsed {
		t.Fatalf("grant was not reusable after invalid requests: %d %s", response.Code, response.Body.String())
	}
}

func TestPrivacyBarrierFailureDoesNotConsumeGrantAndReceiptNotFoundIs404(t *testing.T) {
	now := time.Date(2026, time.August, 20, 14, 0, 0, 0, time.UTC)
	service := &fakePrivacyHTTP{
		httpNow: now,
		receipt: privacy.ErasureReceipt{
			ErasureID: testErasureID, Status: privacy.StatusBarrierCommitted, SummaryVersion: 1,
			LearnerGeneration: 2, PolicyVersion: privacy.PolicyVersion, ReasonCode: string(privacy.ReasonLearnerRequest),
			RequestedAt: now, UpdatedAt: now, Steps: []privacy.StepReceipt{},
		},
		commitErr: &privacy.Error{Code: privacy.CodeErasureInProgress, Reason: "injected_barrier_conflict"},
	}
	grant := &singleUseGrant{validToken: canonicalGrantToken(0x66)}
	var logs bytes.Buffer
	handler := newMemoryPrivacyAPI(t, []string{"privacy:read"}, nil, nil, service, grant, privacy.NewReadPermitManager(), &logs)
	body := `{"operation_id":"` + testOperationID + `","payload_schema_version":1,"expected_current_learner_generation":1,"reason_code":"learner_request","explicit_confirmation":true}`
	request := httptest.NewRequest(http.MethodPost, "/v1/privacy/erasures", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer valid-device-token")
	request.Header.Set(privacyErasureGrantHeader, grant.validToken)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusConflict || service.grantUsed {
		t.Fatalf("barrier conflict = %d used=%v body=%s", response.Code, service.grantUsed, response.Body.String())
	}
	service.commitErr = nil
	request = httptest.NewRequest(http.MethodPost, "/v1/privacy/erasures", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer valid-device-token")
	request.Header.Set(privacyErasureGrantHeader, grant.validToken)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusAccepted || !service.grantUsed {
		t.Fatalf("grant was burned by barrier conflict: %d %s", response.Code, response.Body.String())
	}

	service.receiptErr = &privacy.Error{Code: privacy.CodeNotFound, Reason: "unknown_receipt"}
	response = authenticatedRequest(handler, http.MethodGet, "/v1/privacy/erasures/"+"70000000-0000-4000-8000-000000000099", "")
	if response.Code != http.StatusNotFound || !strings.Contains(response.Body.String(), `"code":"not_found"`) {
		t.Fatalf("unknown receipt = %d %s", response.Code, response.Body.String())
	}
}

func TestPrivacyErasureDoesNotTrustOrdinaryScopeAndQueuesNocturneFailure(t *testing.T) {
	now := time.Date(2026, time.August, 20, 14, 0, 0, 0, time.UTC)
	service := &fakePrivacyHTTP{
		receipt:   privacy.ErasureReceipt{ErasureID: testErasureID, Status: privacy.StatusLocalScrubbed, LearnerGeneration: 2, PolicyVersion: privacy.PolicyVersion, ReasonCode: string(privacy.ReasonLearnerRequest), RequestedAt: now, UpdatedAt: now, Steps: []privacy.StepReceipt{}},
		remoteErr: errors.New("nocturne bearer raw response secret"),
	}
	grant := &singleUseGrant{validToken: canonicalGrantToken(0x22)}
	var logs bytes.Buffer
	handler := newMemoryPrivacyAPI(t, []string{"memory:read", "privacy:read"}, nil, nil, service, grant, privacy.NewReadPermitManager(), &logs)
	body := `{"operation_id":"` + testOperationID + `","payload_schema_version":1,"expected_current_learner_generation":1,"reason_code":"learner_request","explicit_confirmation":true}`
	request := httptest.NewRequest(http.MethodPost, "/v1/privacy/erasures", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer valid-device-token")
	request.Header.Set(privacyErasureGrantHeader, grant.validToken)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusAccepted || !strings.Contains(response.Body.String(), testErasureID) {
		t.Fatalf("queued Nocturne = %d %s", response.Code, response.Body.String())
	}
	if strings.Contains(logs.String(), "bearer raw") || strings.Contains(logs.String(), "secret") {
		t.Fatalf("remote error leaked: %s", logs.String())
	}

	grant = &singleUseGrant{validToken: canonicalGrantToken(0x33)}
	service = &fakePrivacyHTTP{httpNow: now, receipt: service.receipt}
	handler = newMemoryPrivacyAPI(t, []string{"privacy:erase"}, nil, nil, service, grant, privacy.NewReadPermitManager(), &logs)
	response = authenticatedRequest(handler, http.MethodPost, "/v1/privacy/erasures", body)
	if response.Code != http.StatusForbidden {
		t.Fatalf("ordinary privacy:erase scope bypassed grant: %d", response.Code)
	}
}

func TestResponseReadPermitDropsCandidateBodyWhenBarrierCancels(t *testing.T) {
	manager := privacy.NewReadPermitManager()
	started := make(chan struct{})
	candidate, _, _ := memoryHTTPFixtures()
	service := &fakeMemoryHTTP{candidateFn: func(ctx context.Context, _ string) (memory.CandidateView, error) {
		close(started)
		<-ctx.Done()
		return candidate, nil
	}}
	var logs bytes.Buffer
	handler := newMemoryPrivacyAPI(t, []string{"memory:read"}, service, &fakeMemoryExporter{}, nil, nil, manager, &logs)
	responseCh := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		responseCh <- authenticatedRequest(handler, http.MethodGet, "/v1/memory/candidates/"+testCandidateID, "")
	}()
	<-started
	drainCh := make(chan error, 1)
	go func() {
		drainCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		drainCh <- manager.CloseAndDrain(drainCtx, 2, privacy.OwnerMemory)
	}()
	response := <-responseCh
	if err := <-drainCh; err != nil {
		t.Fatal(err)
	}
	if response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), memory.CodeContentRedacted) || strings.Contains(response.Body.String(), candidate.ProposedContent) {
		t.Fatalf("canceled permit = %d %s", response.Code, response.Body.String())
	}
	if strings.Contains(logs.String(), candidate.ProposedContent) {
		t.Fatalf("canceled response content leaked to logs: %s", logs.String())
	}
}

func TestResponseReadPermitDropsRecordDetailWhenBarrierCancels(t *testing.T) {
	manager := privacy.NewReadPermitManager()
	started := make(chan struct{})
	_, record, _ := memoryHTTPFixtures()
	privateContent := "live private memory body"
	exporter := &fakeMemoryExporter{detailFn: func(ctx context.Context, _ string) (memory.RecordDetail, error) {
		close(started)
		<-ctx.Done()
		return memory.RecordDetail{
			Record: record.Record, Delivery: record.Delivery, Receipt: record.Receipt,
			ReadGeneration: record.ReadGeneration, ContentStatus: memory.ExportContentAvailable, Content: privateContent,
		}, nil
	}}
	var logs bytes.Buffer
	handler := newMemoryPrivacyAPI(t, []string{"memory:read"}, &fakeMemoryHTTP{}, exporter, nil, nil, manager, &logs)
	responseCh := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		responseCh <- authenticatedRequest(handler, http.MethodGet, "/v1/memory/records/"+testMemoryID, "")
	}()
	<-started
	drainCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := manager.CloseAndDrain(drainCtx, 2, privacy.OwnerMemory); err != nil {
		t.Fatal(err)
	}
	response := <-responseCh
	if response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), memory.CodeContentRedacted) ||
		strings.Contains(response.Body.String(), privateContent) || strings.Contains(logs.String(), privateContent) {
		t.Fatalf("canceled detail response=%d body=%s logs=%s", response.Code, response.Body.String(), logs.String())
	}
}

func TestMemoryDeliveryPublicStatusesPreserveQueuedRejectedApplied(t *testing.T) {
	_, _, operation := memoryHTTPFixtures()
	service := &fakeMemoryHTTP{operation: operation}
	var logs bytes.Buffer
	handler := newMemoryPrivacyAPI(t, []string{"memory:write"}, service, &fakeMemoryExporter{}, nil, nil, privacy.NewReadPermitManager(), &logs)
	body := `{"operation_id":"` + testOperationID + `","payload_schema_version":1}`
	for status, httpStatus := range map[memory.DeliveryPublicStatus]int{
		memory.DeliveryQueued: http.StatusAccepted, memory.DeliveryApplied: http.StatusOK, memory.DeliveryRejected: http.StatusOK,
	} {
		service.operation.Delivery.PublicStatus = status
		response := authenticatedRequest(handler, http.MethodPost, "/v1/memory/deliveries/"+testDeliveryID+"/replays", body)
		if response.Code != httpStatus || !strings.Contains(response.Body.String(), `"public_status":"`+string(status)+`"`) {
			t.Fatalf("delivery %s = %d %s", status, response.Code, response.Body.String())
		}
	}
}

func TestMemoryPrivacyHTTPActualResponsesValidateOpenAPI(t *testing.T) {
	document, err := openapi3.NewLoader().LoadFromFile("../../../api/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if err := document.Validate(context.Background()); err != nil {
		t.Fatal(err)
	}
	validate := func(path, method, code string, response *httptest.ResponseRecorder) {
		t.Helper()
		var payload any
		if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
			t.Fatalf("decode %s %s response: %v; body=%s", method, path, err, response.Body.String())
		}
		item := document.Paths.Find(path)
		var schema *openapi3.Schema
		switch method {
		case http.MethodGet:
			schema = item.Get.Responses.Value(code).Value.Content.Get("application/json").Schema.Value
		case http.MethodPost:
			schema = item.Post.Responses.Value(code).Value.Content.Get("application/json").Schema.Value
		case http.MethodDelete:
			schema = item.Delete.Responses.Value(code).Value.Content.Get("application/json").Schema.Value
		}
		if err := schema.VisitJSON(payload, openapi3.EnableJSONSchema2020()); err != nil {
			t.Fatalf("actual %s %s response %s failed schema: %v; body=%s", method, path, code, err, response.Body.String())
		}
	}

	candidate, record, operation := memoryHTTPFixtures()
	memoryService := &fakeMemoryHTTP{
		candidate: candidate, candidatePg: memory.CandidatePage{Items: []memory.CandidateView{candidate}, ReadGeneration: candidate.ReadGeneration},
		record: record, recordPg: memory.RecordPage{Items: []memory.Record{record.Record}, ReadGeneration: record.ReadGeneration}, operation: operation,
	}
	var logs bytes.Buffer
	exporter := &fakeMemoryExporter{
		page: memory.ExportPage{Items: []memory.ExportItem{}, ReadGeneration: candidate.ReadGeneration, ReasonCodes: []string{}},
		detail: memory.RecordDetail{Record: record.Record, Delivery: record.Delivery, Receipt: record.Receipt,
			ReadGeneration: record.ReadGeneration, ContentStatus: memory.ExportContentUnavailable},
	}
	handler := newMemoryPrivacyAPI(t, []string{"memory:read", "memory:write"}, memoryService, exporter, nil, nil, privacy.NewReadPermitManager(), &logs)
	response := authenticatedRequest(handler, http.MethodGet, "/v1/memory/candidates/"+testCandidateID, "")
	validate("/v1/memory/candidates/{candidateID}", http.MethodGet, "200", response)
	response = authenticatedRequest(handler, http.MethodGet, "/v1/memory/records/"+testMemoryID, "")
	validate("/v1/memory/records/{memoryID}", http.MethodGet, "200", response)
	candidateBody := `{"operation_id":"` + testOperationID + `","payload_schema_version":1,"content":"concise answers","reason":"explicit preference","category":"interaction_preference","sensitivity":"non_sensitive","stability":"stable","valid_until":"2030-01-01T00:00:00Z"}`
	response = authenticatedRequest(handler, http.MethodPost, "/v1/memory/candidates", candidateBody)
	validate("/v1/memory/candidates", http.MethodPost, "201", response)
	response = authenticatedRequest(handler, http.MethodGet, "/v1/memory/candidates?limit=0", "")
	validate("/v1/memory/candidates", http.MethodGet, "400", response)

	now := time.Date(2026, time.August, 20, 14, 0, 0, 0, time.UTC)
	privacyService := &fakePrivacyHTTP{receipt: privacy.ErasureReceipt{
		ErasureID: testErasureID, Status: privacy.StatusPartial, SummaryVersion: 2, LearnerGeneration: 2,
		PolicyVersion: privacy.PolicyVersion, ReasonCode: string(privacy.ReasonLearnerRequest),
		RequestedAt: now, UpdatedAt: now, Steps: []privacy.StepReceipt{},
	}}
	grant := &singleUseGrant{validToken: canonicalGrantToken(0x44)}
	handler = newMemoryPrivacyAPI(t, []string{"privacy:read"}, nil, nil, privacyService, grant, privacy.NewReadPermitManager(), &logs)
	erasureBody := `{"operation_id":"` + testOperationID + `","payload_schema_version":1,"expected_current_learner_generation":1,"reason_code":"learner_request","explicit_confirmation":true}`
	request := httptest.NewRequest(http.MethodPost, "/v1/privacy/erasures", strings.NewReader(erasureBody))
	request.Header.Set("Authorization", "Bearer valid-device-token")
	request.Header.Set(privacyErasureGrantHeader, grant.validToken)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	validate("/v1/privacy/erasures", http.MethodPost, "202", response)
	response = authenticatedRequest(handler, http.MethodGet, "/v1/privacy/erasures/"+testErasureID, "")
	validate("/v1/privacy/erasures/{erasureID}", http.MethodGet, "200", response)
}
