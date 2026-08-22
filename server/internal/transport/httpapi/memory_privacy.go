package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/edu-agent/edu-agent/server/internal/memory"
	"github.com/edu-agent/edu-agent/server/internal/privacy"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

const (
	privacyErasureGrantHeader = "X-Privacy-Erasure-Grant"
	memoryRequestBodyLimit    = int64(1 << 20)
	memoryPayloadSchemaV1     = 1
)

type memoryCandidateInput struct {
	OperationID          *string             `json:"operation_id"`
	PayloadSchemaVersion *int                `json:"payload_schema_version"`
	Content              *string             `json:"content"`
	Reason               *string             `json:"reason"`
	Category             *memory.Category    `json:"category"`
	Sensitivity          *memory.Sensitivity `json:"sensitivity"`
	Stability            *memory.Stability   `json:"stability"`
	ValidUntil           *time.Time          `json:"valid_until"`
}

type memoryCorrectionCandidateInput struct {
	memoryCandidateInput
	ExpectedRecordRevision   *int64 `json:"expected_record_revision"`
	ExpectedRecordGeneration *int64 `json:"expected_record_generation"`
}

type memoryDecisionInput struct {
	OperationID              *string          `json:"operation_id"`
	PayloadSchemaVersion     *int             `json:"payload_schema_version"`
	ExpectedRevision         *int64           `json:"expected_revision"`
	ExpectedRecordRevision   *int64           `json:"expected_record_revision,omitempty"`
	ExpectedRecordGeneration *int64           `json:"expected_record_generation,omitempty"`
	Decision                 *memory.Decision `json:"decision"`
	Reason                   *string          `json:"reason"`
}

type memoryDeleteInput struct {
	OperationID              *string `json:"operation_id"`
	PayloadSchemaVersion     *int    `json:"payload_schema_version"`
	ExpectedRevision         *int64  `json:"expected_revision"`
	ExpectedRecordGeneration *int64  `json:"expected_record_generation"`
}

type memoryReplayInput struct {
	OperationID          *string `json:"operation_id"`
	PayloadSchemaVersion *int    `json:"payload_schema_version"`
}

type privacyErasureInput struct {
	OperationID                      *string `json:"operation_id"`
	PayloadSchemaVersion             *int    `json:"payload_schema_version"`
	ExpectedCurrentLearnerGeneration *int64  `json:"expected_current_learner_generation"`
	ReasonCode                       *string `json:"reason_code"`
	ExplicitConfirmation             *bool   `json:"explicit_confirmation"`
}

type memoryCandidateResponse struct {
	Candidate       memory.Candidate        `json:"candidate"`
	ContentStatus   string                  `json:"content_status"`
	ProposedContent string                  `json:"proposed_content,omitempty"`
	ReadGeneration  *memory.GenerationStamp `json:"read_generation,omitempty"`
}

type memoryCandidatePageResponse struct {
	Items          []memoryCandidateResponse `json:"items"`
	NextCursor     string                    `json:"next_cursor,omitempty"`
	ReadGeneration memory.GenerationStamp    `json:"read_generation"`
}

type memoryOperationResponse struct {
	Candidate *memoryCandidateResponse `json:"candidate,omitempty"`
	Record    *memory.Record           `json:"record,omitempty"`
	Delivery  *memory.Delivery         `json:"delivery,omitempty"`
	Replayed  bool                     `json:"replayed"`
}

type bufferedHTTPResponse struct {
	header http.Header
	body   bytes.Buffer
	status int
}

func newBufferedHTTPResponse() *bufferedHTTPResponse {
	return &bufferedHTTPResponse{header: make(http.Header)}
}

func (w *bufferedHTTPResponse) Header() http.Header { return w.header }
func (w *bufferedHTTPResponse) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
}
func (w *bufferedHTTPResponse) Write(value []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.body.Write(value)
}
func (w *bufferedHTTPResponse) flush(target http.ResponseWriter) {
	for key, values := range w.header {
		for _, value := range values {
			target.Header().Add(key, value)
		}
	}
	status := w.status
	if status == 0 {
		status = http.StatusOK
	}
	target.WriteHeader(status)
	_, _ = target.Write(w.body.Bytes())
}

// responseReadPermit keeps the permit alive until the buffered response has
// been written. A privacy barrier can therefore cancel and drain the handler
// without allowing already-read content to escape after barrier commit.
func (a *API) handleResponseReadPermit(closedCode string, owners ...privacy.OwnerKind) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			permit, err := a.readPermits.Acquire(r.Context(), owners...)
			if err != nil {
				if privacy.ErrorCode(err) == privacy.CodeContentRedacted {
					writeError(w, r, http.StatusServiceUnavailable, closedCode, "Content is unavailable while privacy clearing is in progress")
					return
				}
				a.logger.ErrorContext(r.Context(), "response read permit failed", "request_id", middleware.GetReqID(r.Context()), "error_category", "internal")
				writeError(w, r, http.StatusInternalServerError, "internal_error", "Request could not be completed")
				return
			}
			defer permit.Release()

			buffered := newBufferedHTTPResponse()
			request := r.WithContext(permit.Context())
			next.ServeHTTP(buffered, request)
			if cause := context.Cause(permit.Context()); cause != nil {
				if privacy.ErrorCode(cause) == privacy.CodeContentRedacted {
					writeError(w, r, http.StatusServiceUnavailable, memory.CodeContentRedacted, "Content was redacted before the response completed")
				}
				return
			}
			buffered.flush(w)
		})
	}
}

func (a *API) handlePrivacyErasureRateLimit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		credential, ok := credentialFromContext(r.Context())
		if !ok {
			writeError(w, r, http.StatusUnauthorized, "authentication_failed", "Device credentials are invalid")
			return
		}
		if !a.privacyLimiter.Allow("privacy-erasure-ip:"+clientIP(r)) || !a.privacyLimiter.Allow("privacy-erasure-device:"+credential.Device.ID) {
			writeError(w, r, http.StatusTooManyRequests, "rate_limited", "Too many privacy erasure attempts")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (a *API) handleMemoryCreateCandidate(w http.ResponseWriter, r *http.Request) {
	if _, ok := strictMemoryQuery(w, r); !ok {
		return
	}
	var input memoryCandidateInput
	if !decodeMemoryJSON(w, r, &input) || !validMemoryCandidateInput(input) {
		writeMemoryInvalid(w, r)
		return
	}
	credential, _ := credentialFromContext(r.Context())
	result, err := a.memory.CreateCandidate(r.Context(), memory.DevicePrincipal{DeviceID: credential.Device.ID}, memory.CreateCandidateCommand{
		OperationID: *input.OperationID, Content: *input.Content,
		Reason: *input.Reason, Category: *input.Category, Sensitivity: *input.Sensitivity,
		Stability: *input.Stability, ValidUntil: input.ValidUntil.UTC(),
	})
	if err != nil {
		a.writeMemoryFailure(w, r, "create_candidate", err)
		return
	}
	status := http.StatusCreated
	if result.Replayed {
		status = http.StatusOK
	}
	writeJSON(w, status, publicMemoryOperation(result))
}

func (a *API) handleMemoryListCandidates(w http.ResponseWriter, r *http.Request) {
	query, ok := strictMemoryQuery(w, r, "cursor", "limit")
	if !ok {
		return
	}
	page, ok := memoryPage(w, r, query)
	if !ok {
		return
	}
	result, err := a.memory.ListCandidates(r.Context(), page)
	if err != nil {
		a.writeMemoryFailure(w, r, "list_candidates", err)
		return
	}
	items := make([]memoryCandidateResponse, 0, len(result.Items))
	for _, item := range result.Items {
		items = append(items, publicMemoryCandidate(item))
	}
	writeJSON(w, http.StatusOK, memoryCandidatePageResponse{Items: items, NextCursor: result.NextCursor, ReadGeneration: result.ReadGeneration})
}

func (a *API) handleMemoryCandidate(w http.ResponseWriter, r *http.Request) {
	if _, ok := strictMemoryQuery(w, r); !ok {
		return
	}
	id := chi.URLParam(r, "candidateID")
	if !privacy.CanonicalUUID(id) {
		writeMemoryInvalid(w, r)
		return
	}
	result, err := a.memory.Candidate(r.Context(), id)
	if err != nil {
		a.writeMemoryFailure(w, r, "candidate", err)
		return
	}
	writeJSON(w, http.StatusOK, publicMemoryCandidate(result))
}

func (a *API) handleMemoryCandidateDecision(w http.ResponseWriter, r *http.Request) {
	if _, ok := strictMemoryQuery(w, r); !ok {
		return
	}
	candidateID := chi.URLParam(r, "candidateID")
	var input memoryDecisionInput
	if !privacy.CanonicalUUID(candidateID) || !decodeMemoryJSON(w, r, &input) || !validMemoryDecisionInput(input) {
		writeMemoryInvalid(w, r)
		return
	}
	credential, _ := credentialFromContext(r.Context())
	result, err := a.memory.DecideCandidate(r.Context(), memory.DevicePrincipal{DeviceID: credential.Device.ID}, memory.DecideCandidateCommand{
		OperationID: *input.OperationID, CandidateID: candidateID, ExpectedRevision: *input.ExpectedRevision,
		ExpectedRecordRevision:   int64PointerValue(input.ExpectedRecordRevision),
		ExpectedRecordGeneration: int64PointerValue(input.ExpectedRecordGeneration),
		Decision:                 *input.Decision, Reason: *input.Reason,
	})
	if err != nil {
		a.writeMemoryFailure(w, r, "candidate_decision", err)
		return
	}
	writeJSON(w, http.StatusOK, publicMemoryOperation(result))
}

func (a *API) handleMemoryListRecords(w http.ResponseWriter, r *http.Request) {
	query, ok := strictMemoryQuery(w, r, "cursor", "limit")
	if !ok {
		return
	}
	page, ok := memoryPage(w, r, query)
	if !ok {
		return
	}
	result, err := a.memory.ListRecords(r.Context(), page)
	if err != nil {
		a.writeMemoryFailure(w, r, "list_records", err)
		return
	}
	if result.Items == nil {
		result.Items = []memory.Record{}
	}
	writeJSON(w, http.StatusOK, result)
}

func (a *API) handleMemoryRecord(w http.ResponseWriter, r *http.Request) {
	if _, ok := strictMemoryQuery(w, r); !ok {
		return
	}
	id := chi.URLParam(r, "memoryID")
	if !privacy.CanonicalUUID(id) {
		writeMemoryInvalid(w, r)
		return
	}
	result, err := a.memoryExporter.Detail(r.Context(), id)
	if err != nil {
		a.writeMemoryFailure(w, r, "record_detail", err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (a *API) handleMemoryCreateCorrectionCandidate(w http.ResponseWriter, r *http.Request) {
	if _, ok := strictMemoryQuery(w, r); !ok {
		return
	}
	memoryID := chi.URLParam(r, "memoryID")
	var input memoryCorrectionCandidateInput
	if !privacy.CanonicalUUID(memoryID) || !decodeMemoryJSON(w, r, &input) || !validMemoryCandidateInput(input.memoryCandidateInput) || input.ExpectedRecordRevision == nil || input.ExpectedRecordGeneration == nil || *input.ExpectedRecordRevision < 1 || *input.ExpectedRecordGeneration < 1 {
		writeMemoryInvalid(w, r)
		return
	}
	credential, _ := credentialFromContext(r.Context())
	result, err := a.memory.CreateCorrectionCandidate(r.Context(), memory.DevicePrincipal{DeviceID: credential.Device.ID}, memory.CreateCorrectionCandidateCommand{
		OperationID: *input.OperationID, LogicalMemoryID: memoryID,
		ExpectedRevision: *input.ExpectedRecordRevision, ExpectedRecordGeneration: *input.ExpectedRecordGeneration,
		Content: *input.Content, Reason: *input.Reason,
		Category: *input.Category, Sensitivity: *input.Sensitivity, Stability: *input.Stability,
		ValidUntil: input.ValidUntil.UTC(),
	})
	if err != nil {
		a.writeMemoryFailure(w, r, "create_correction_candidate", err)
		return
	}
	status := http.StatusCreated
	if result.Replayed {
		status = http.StatusOK
	}
	writeJSON(w, status, publicMemoryOperation(result))
}

func (a *API) handleMemoryDeleteRecord(w http.ResponseWriter, r *http.Request) {
	if _, ok := strictMemoryQuery(w, r); !ok {
		return
	}
	memoryID := chi.URLParam(r, "memoryID")
	var input memoryDeleteInput
	if !privacy.CanonicalUUID(memoryID) || !decodeMemoryJSON(w, r, &input) || input.OperationID == nil || !privacy.CanonicalUUID(*input.OperationID) || input.PayloadSchemaVersion == nil || *input.PayloadSchemaVersion != memoryPayloadSchemaV1 || input.ExpectedRevision == nil || *input.ExpectedRevision < 1 || input.ExpectedRecordGeneration == nil || *input.ExpectedRecordGeneration < 1 {
		writeMemoryInvalid(w, r)
		return
	}
	credential, _ := credentialFromContext(r.Context())
	result, err := a.memory.DeleteRecord(r.Context(), memory.DevicePrincipal{DeviceID: credential.Device.ID}, memory.DeleteRecordCommand{
		OperationID: *input.OperationID, LogicalMemoryID: memoryID,
		ExpectedRevision: *input.ExpectedRevision, ExpectedRecordGeneration: *input.ExpectedRecordGeneration,
	})
	if err != nil {
		a.writeMemoryFailure(w, r, "delete_record", err)
		return
	}
	writeJSON(w, memoryDeliveryHTTPStatus(result), publicMemoryOperation(result))
}

func (a *API) handleMemoryExport(w http.ResponseWriter, r *http.Request) {
	query, ok := strictMemoryQuery(w, r, "cursor", "limit")
	if !ok {
		return
	}
	page, ok := memoryPage(w, r, query)
	if !ok {
		return
	}
	result, err := a.memoryExporter.Export(r.Context(), page)
	if err != nil {
		a.writeMemoryFailure(w, r, "export", err)
		return
	}
	if result.Items == nil {
		result.Items = []memory.ExportItem{}
	}
	if result.ReasonCodes == nil {
		result.ReasonCodes = []string{}
	}
	writeJSON(w, http.StatusOK, result)
}

func (a *API) handleMemoryReplayDelivery(w http.ResponseWriter, r *http.Request) {
	if _, ok := strictMemoryQuery(w, r); !ok {
		return
	}
	deliveryID := chi.URLParam(r, "deliveryID")
	var input memoryReplayInput
	if !privacy.CanonicalUUID(deliveryID) || !decodeMemoryJSON(w, r, &input) || input.OperationID == nil || !privacy.CanonicalUUID(*input.OperationID) || input.PayloadSchemaVersion == nil || *input.PayloadSchemaVersion != memoryPayloadSchemaV1 {
		writeMemoryInvalid(w, r)
		return
	}
	credential, _ := credentialFromContext(r.Context())
	result, err := a.memory.ReplayDelivery(r.Context(), memory.DevicePrincipal{DeviceID: credential.Device.ID}, memory.ReplayDeliveryCommand{
		OperationID: *input.OperationID, DeliveryID: deliveryID,
	})
	if err != nil {
		a.writeMemoryFailure(w, r, "replay_delivery", err)
		return
	}
	writeJSON(w, memoryDeliveryHTTPStatus(result), publicMemoryOperation(result))
}

func (a *API) handlePrivacyCreateErasure(w http.ResponseWriter, r *http.Request) {
	if _, ok := strictMemoryQuery(w, r); !ok {
		return
	}
	var input privacyErasureInput
	if !decodeMemoryJSON(w, r, &input) || input.OperationID == nil || !privacy.CanonicalUUID(*input.OperationID) || input.PayloadSchemaVersion == nil || *input.PayloadSchemaVersion != memoryPayloadSchemaV1 || input.ExpectedCurrentLearnerGeneration == nil || *input.ExpectedCurrentLearnerGeneration < 1 || input.ReasonCode == nil || !privacy.ValidReasonCode(*input.ReasonCode) || input.ExplicitConfirmation == nil || !*input.ExplicitConfirmation {
		writeMemoryInvalid(w, r)
		return
	}
	credential, _ := credentialFromContext(r.Context())
	requestedAt := a.now().UTC()
	request := privacy.ErasureRequest{
		DeviceID: credential.Device.ID, OperationID: *input.OperationID, ReasonCode: *input.ReasonCode,
		ActorDeviceID: credential.Device.ID, RequestedAt: requestedAt,
		ManagedBackupUnrecoverableAfter:  requestedAt.Add(a.privacyBackupDeadline),
		ExpectedCurrentLearnerGeneration: *input.ExpectedCurrentLearnerGeneration,
	}
	values := r.Header.Values(privacyErasureGrantHeader)
	if len(values) > 1 {
		writeError(w, r, http.StatusForbidden, "forbidden", "Privacy erasure authorization is invalid or unavailable")
		return
	}
	token := ""
	if len(values) == 1 {
		token = values[0]
	}
	authorization := privacy.NewErasureGrantAuthorization(credential.Device.ID, token)
	receipt, err := a.privacy.AuthorizeAndCommitBarrier(r.Context(), request, authorization)
	if err != nil {
		if errors.Is(err, privacy.ErrErasureGrantInvalid) {
			writeError(w, r, http.StatusForbidden, "forbidden", "Privacy erasure authorization is invalid or unavailable")
			return
		}
		a.writePrivacyFailure(w, r, "commit_barrier", err)
		return
	}
	if receipt.Status == privacy.StatusVerified {
		writeJSON(w, http.StatusAccepted, receipt)
		return
	}
	receipt, err = a.privacy.RunLocal(r.Context(), receipt.ErasureID)
	if err != nil {
		a.writePrivacyFailure(w, r, "run_local", err)
		return
	}
	remote, err := a.privacy.RunNocturne(r.Context(), receipt.ErasureID)
	if err != nil {
		if privacy.ErrorCode(err) == "" {
			a.logger.InfoContext(r.Context(), "privacy remote erase queued", "request_id", middleware.GetReqID(r.Context()), "erasure_id", receipt.ErasureID, "error_category", "nocturne_unavailable")
			writeJSON(w, http.StatusAccepted, receipt)
			return
		}
		a.writePrivacyFailure(w, r, "run_nocturne", err)
		return
	}
	writeJSON(w, http.StatusAccepted, remote)
}

func (a *API) handlePrivacyErasureReceipt(w http.ResponseWriter, r *http.Request) {
	if _, ok := strictMemoryQuery(w, r); !ok {
		return
	}
	id := chi.URLParam(r, "erasureID")
	if !privacy.CanonicalUUID(id) {
		writeMemoryInvalid(w, r)
		return
	}
	receipt, err := a.privacy.Receipt(r.Context(), id)
	if err != nil {
		a.writePrivacyFailure(w, r, "receipt", err)
		return
	}
	if receipt.Steps == nil {
		receipt.Steps = []privacy.StepReceipt{}
	}
	writeJSON(w, http.StatusOK, receipt)
}

func publicMemoryCandidate(value memory.CandidateView) memoryCandidateResponse {
	response := memoryCandidateResponse{
		Candidate: value.Candidate, ContentStatus: value.ContentStatus, ProposedContent: value.ProposedContent,
	}
	if value.ReadGeneration.LearnerGeneration > 0 && value.ReadGeneration.MemoryGeneration > 0 {
		generation := value.ReadGeneration
		response.ReadGeneration = &generation
	}
	return response
}

func publicMemoryOperation(value memory.OperationResult) memoryOperationResponse {
	response := memoryOperationResponse{Record: value.Record, Delivery: value.Delivery, Replayed: value.Replayed}
	if value.Candidate.Candidate.ID != "" {
		candidate := publicMemoryCandidate(value.Candidate)
		response.Candidate = &candidate
	}
	return response
}

func memoryDeliveryHTTPStatus(result memory.OperationResult) int {
	if result.Replayed || result.Delivery == nil || result.Delivery.PublicStatus != memory.DeliveryQueued {
		return http.StatusOK
	}
	return http.StatusAccepted
}

func decodeMemoryJSON(w http.ResponseWriter, r *http.Request, target any) bool {
	data, err := readJSONBody(w, r, memoryRequestBodyLimit)
	if err != nil {
		var maxBytes *http.MaxBytesError
		if errors.As(err, &maxBytes) {
			writeError(w, r, http.StatusRequestEntityTooLarge, "payload_too_large", "Request body exceeds the memory and privacy limit")
		} else {
			writeMemoryInvalid(w, r)
		}
		return false
	}
	if rejectJSONNulls(data) != nil || decodeJSONData(data, target) != nil {
		writeMemoryInvalid(w, r)
		return false
	}
	return true
}

func rejectJSONNulls(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return err
	}
	var walk func(any) error
	walk = func(candidate any) error {
		switch typed := candidate.(type) {
		case nil:
			return errors.New("explicit null is not allowed")
		case []any:
			for _, item := range typed {
				if err := walk(item); err != nil {
					return err
				}
			}
		case map[string]any:
			for _, item := range typed {
				if err := walk(item); err != nil {
					return err
				}
			}
		}
		return nil
	}
	return walk(value)
}

func validMemoryCandidateInput(input memoryCandidateInput) bool {
	if input.OperationID == nil || !privacy.CanonicalUUID(*input.OperationID) || input.PayloadSchemaVersion == nil || *input.PayloadSchemaVersion != memoryPayloadSchemaV1 || input.Content == nil || input.Reason == nil || input.Category == nil || input.Sensitivity == nil || input.Stability == nil || input.ValidUntil == nil || input.ValidUntil.IsZero() {
		return false
	}
	if !validMemoryReason(*input.Reason) || !validMemoryCategory(*input.Category) || (*input.Sensitivity != memory.SensitivityNonSensitive && *input.Sensitivity != memory.SensitivitySensitive) || (*input.Stability != memory.StabilityTransient && *input.Stability != memory.StabilityStable) {
		return false
	}
	return utf8.ValidString(*input.Content) && strings.TrimSpace(*input.Content) != "" && len(*input.Content) <= memory.MaxContentBytes && utf8.RuneCountInString(*input.Content) <= memory.MaxContentRunes
}

func validMemoryDecisionInput(input memoryDecisionInput) bool {
	if input.OperationID == nil || !privacy.CanonicalUUID(*input.OperationID) || input.PayloadSchemaVersion == nil || *input.PayloadSchemaVersion != memoryPayloadSchemaV1 || input.ExpectedRevision == nil || *input.ExpectedRevision < 1 || input.Decision == nil || (*input.Decision != memory.DecisionAdmit && *input.Decision != memory.DecisionReject) || input.Reason == nil || !validMemoryReason(*input.Reason) {
		return false
	}
	if (input.ExpectedRecordRevision == nil) != (input.ExpectedRecordGeneration == nil) {
		return false
	}
	return input.ExpectedRecordRevision == nil || *input.ExpectedRecordRevision > 0 && *input.ExpectedRecordGeneration > 0
}

func validMemoryReason(value string) bool {
	return utf8.ValidString(value) && strings.TrimSpace(value) != "" && len(value) <= memory.MaxReferenceBytes*4 && utf8.RuneCountInString(value) <= memory.MaxReasonRunes
}

func validMemoryCategory(value memory.Category) bool {
	switch value {
	case memory.CategoryInteractionPreference, memory.CategoryTimeConstraint, memory.CategoryPersonalContext,
		memory.CategoryGeneratedSummary, memory.CategoryRawChat, memory.CategoryCompleteAttempt,
		memory.CategoryQuestionOrRubric, memory.CategoryGoal, memory.CategoryRoute, memory.CategoryMastery,
		memory.CategoryEvidence, memory.CategoryMisconception, memory.CategoryReviewQueue,
		memory.CategorySyncState, memory.CategoryDeviceToken, memory.CategoryModelSecret,
		memory.CategoryNocturneSecret:
		return true
	default:
		return false
	}
}

func strictMemoryQuery(w http.ResponseWriter, r *http.Request, allowed ...string) (url.Values, bool) {
	values, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		writeMemoryInvalid(w, r)
		return nil, false
	}
	set := make(map[string]bool, len(allowed))
	for _, key := range allowed {
		set[key] = true
	}
	for key, entries := range values {
		if !set[key] || len(entries) != 1 || entries[0] == "" {
			writeMemoryInvalid(w, r)
			return nil, false
		}
	}
	return values, true
}

func memoryPage(w http.ResponseWriter, r *http.Request, query url.Values) (memory.PageRequest, bool) {
	value := memory.PageRequest{Cursor: query.Get("cursor"), Limit: 50}
	if entries, present := query["limit"]; present {
		limit, err := strconv.Atoi(entries[0])
		if err != nil || limit < 1 || limit > 200 || strconv.Itoa(limit) != entries[0] {
			writeMemoryInvalid(w, r)
			return value, false
		}
		value.Limit = limit
	}
	return value, true
}

func writeMemoryInvalid(w http.ResponseWriter, r *http.Request) {
	writeError(w, r, http.StatusBadRequest, memory.CodeInvalidRequest, "Memory or privacy request is invalid")
}

func (a *API) writeMemoryFailure(w http.ResponseWriter, r *http.Request, operation string, err error) {
	code := memory.ErrorCode(err)
	status := http.StatusInternalServerError
	message := "Request could not be completed"
	switch code {
	case memory.CodeInvalidRequest:
		status, message = http.StatusBadRequest, "Memory request is invalid"
	case memory.CodeNotFound:
		status, message = http.StatusNotFound, "Memory resource was not found"
	case memory.CodeIdempotencyConflict, memory.CodeCandidateConflict, memory.CodeMemoryConflict,
		memory.CodeInvalidMemoryTransition, memory.CodeDeliveryConflict, memory.CodeStaleCursor:
		status, message = http.StatusConflict, "Memory request conflicts with current state"
	case memory.CodeMemoryPolicyRejected:
		status, message = http.StatusUnprocessableEntity, "Memory content is not eligible for storage"
	case memory.CodeMemoryUnavailable, memory.CodePrivacyClearInProgress, memory.CodeContentRedacted,
		"upstream_contract_mismatch":
		status, message = http.StatusServiceUnavailable, "Memory content is currently unavailable"
	case "":
		code = "internal_error"
		a.logger.ErrorContext(r.Context(), "memory request failed", "request_id", middleware.GetReqID(r.Context()), "operation", operation, "error_category", "internal")
	}
	response := map[string]any{"error": map[string]string{"code": code, "message": message, "request_id": middleware.GetReqID(r.Context())}}
	var domain *memory.Error
	if errors.As(err, &domain) && (domain.CandidateID != "" || domain.ExpectedRevision != 0 || domain.CurrentRevision != 0) {
		response["candidate_conflict"] = map[string]any{
			"candidate_id": domain.CandidateID, "expected_revision": domain.ExpectedRevision,
			"current_revision": domain.CurrentRevision,
		}
	}
	writeJSON(w, status, response)
}

func (a *API) writePrivacyFailure(w http.ResponseWriter, r *http.Request, operation string, err error) {
	code := privacy.ErrorCode(err)
	status := http.StatusInternalServerError
	message := "Request could not be completed"
	switch code {
	case privacy.CodeInvalidRequest:
		status, message = http.StatusBadRequest, "Privacy erasure request is invalid"
	case privacy.CodeIdempotencyConflict, privacy.CodeErasureInProgress, privacy.CodeReceiptNotCurrent:
		code, status, message = "erasure_conflict", http.StatusConflict, "Privacy erasure conflicts with current state"
	case privacy.CodeContentRedacted, privacy.CodePrivacyClearInProgress,
		privacy.CodeVerificationFailed, privacy.CodeUnsupportedReceiptStore:
		status, message = http.StatusServiceUnavailable, "Privacy erasure is not currently available"
	case privacy.CodeNotFound:
		status, message = http.StatusNotFound, "Privacy erasure receipt was not found"
	case "":
		code = "internal_error"
		a.logger.ErrorContext(r.Context(), "privacy request failed", "request_id", middleware.GetReqID(r.Context()), "operation", operation, "error_category", "internal")
	}
	writeError(w, r, status, code, message)
}

func int64PointerValue(value *int64) int64 {
	if value == nil {
		return 0
	}
	return *value
}
