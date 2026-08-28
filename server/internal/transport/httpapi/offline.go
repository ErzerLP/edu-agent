package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/edu-agent/edu-agent/server/internal/learning"
	"github.com/edu-agent/edu-agent/server/internal/privacy"
	"github.com/go-chi/chi/v5"
)

func (a *API) handleOfflinePrepare(w http.ResponseWriter, r *http.Request) {
	if _, ok := strictLearningQuery(w, r); !ok {
		return
	}
	var request learning.OfflinePrepareRequest
	if !a.decodeOffline(w, r, &request) || request.Validate() != nil {
		writeLearningInvalid(w, r)
		return
	}
	credential, _ := credentialFromContext(r.Context())
	response, err := a.offline.Prepare(r.Context(), credential.Device.ID, request)
	if err != nil {
		a.writeOfflineFailure(w, r, "offline_prepare", err)
		return
	}
	status := http.StatusCreated
	if response.Replayed {
		status = http.StatusOK
	}
	writeJSON(w, status, response)
}

func (a *API) handleOfflineSync(w http.ResponseWriter, r *http.Request) {
	if _, ok := strictLearningQuery(w, r); !ok {
		return
	}
	var request learning.OfflineSyncRequest
	if !a.decodeOffline(w, r, &request) || request.Validate() != nil {
		writeLearningInvalid(w, r)
		return
	}
	credential, _ := credentialFromContext(r.Context())
	response, err := a.offline.Sync(r.Context(), credential.Device.ID, request)
	if err != nil {
		a.writeOfflineFailure(w, r, "offline_sync", err)
		return
	}
	writeJSON(w, http.StatusOK, response)
}

func (a *API) handleOfflineStatus(w http.ResponseWriter, r *http.Request) {
	if _, ok := strictLearningQuery(w, r); !ok {
		return
	}
	operationID := chi.URLParam(r, "operationID")
	if !validLearningUUID(operationID) {
		writeLearningInvalid(w, r)
		return
	}
	credential, _ := credentialFromContext(r.Context())
	response, err := a.offline.Status(r.Context(), credential.Device.ID, operationID)
	if err != nil {
		a.writeOfflineFailure(w, r, "offline_status", err)
		return
	}
	writeJSON(w, http.StatusOK, response)
}

func (a *API) handleOfflineAssessments(w http.ResponseWriter, r *http.Request) {
	query, ok := strictLearningQuery(w, r, "status", "cursor", "limit")
	if !ok {
		return
	}
	status := strings.TrimSpace(query.Get("status"))
	if status == "" {
		status = learning.OfflineAssessmentFilterProvisional
	}
	page, ok := learningPage(w, r, query)
	if !ok || status != learning.OfflineAssessmentFilterProvisional {
		if ok {
			writeLearningInvalid(w, r)
		}
		return
	}
	credential, _ := credentialFromContext(r.Context())
	response, err := a.offline.ListOfflineAssessments(r.Context(), credential.Device.ID, learning.OfflineAssessmentQuery{Status: status, Page: page})
	if err != nil {
		a.writeOfflineFailure(w, r, "offline_assessments", err)
		return
	}
	normalizeOfflineAssessmentPage(&response)
	writeJSON(w, http.StatusOK, response)
}

func (a *API) handleOfflineAssessment(w http.ResponseWriter, r *http.Request) {
	if _, ok := strictLearningQuery(w, r); !ok {
		return
	}
	assessmentID := chi.URLParam(r, "assessmentID")
	if !validLearningUUID(assessmentID) {
		writeLearningInvalid(w, r)
		return
	}
	credential, _ := credentialFromContext(r.Context())
	response, err := a.offline.OfflineAssessment(r.Context(), credential.Device.ID, assessmentID)
	if err != nil {
		a.writeOfflineFailure(w, r, "offline_assessment", err)
		return
	}
	normalizeOfflineAssessmentView(&response)
	writeJSON(w, http.StatusOK, response)
}

func (a *API) handleOfflineAssessmentDecision(w http.ResponseWriter, r *http.Request) {
	if _, ok := strictLearningQuery(w, r); !ok {
		return
	}
	assessmentID := chi.URLParam(r, "assessmentID")
	if !validLearningUUID(assessmentID) {
		writeLearningInvalid(w, r)
		return
	}
	data, ok := a.readOfflineData(w, r)
	if !ok {
		return
	}
	var discriminator struct {
		Kind *string `json:"kind"`
	}
	if json.Unmarshal(data, &discriminator) != nil || discriminator.Kind == nil {
		writeLearningInvalid(w, r)
		return
	}
	var base offlineAssessmentDecisionBaseInput
	command := learning.OfflineAssessmentDecisionCommand{Kind: *discriminator.Kind}
	switch command.Kind {
	case "confirm":
		var input offlineAssessmentConfirmInput
		if decodeLearningData(data, &input) != nil {
			writeLearningInvalid(w, r)
			return
		}
		base = input.offlineAssessmentDecisionBaseInput
	case "override":
		var input offlineAssessmentOverrideInput
		if decodeLearningData(data, &input) != nil || input.Reason == nil || strings.TrimSpace(*input.Reason) == "" ||
			!utf8.ValidString(*input.Reason) || utf8.RuneCountInString(*input.Reason) > learning.MaxOfflineAssessmentDecisionReasonRunes ||
			input.Items == nil || len(*input.Items) < 1 || len(*input.Items) > learning.MaxRubricItems {
			writeLearningInvalid(w, r)
			return
		}
		base, command.Reason = input.offlineAssessmentDecisionBaseInput, strings.TrimSpace(*input.Reason)
		for _, raw := range *input.Items {
			if raw.RubricItemID == nil || raw.Conclusion == nil || !utf8.ValidString(*raw.RubricItemID) || strings.TrimSpace(*raw.RubricItemID) == "" ||
				utf8.RuneCountInString(*raw.RubricItemID) > learning.MaxOfflineAssessmentRubricItemIDRunes ||
				(*raw.Conclusion != learning.ConclusionPass && *raw.Conclusion != learning.ConclusionPartial && *raw.Conclusion != learning.ConclusionFail) {
				writeLearningInvalid(w, r)
				return
			}
			candidate := ""
			if raw.MisconceptionCandidate != nil {
				candidate = strings.TrimSpace(*raw.MisconceptionCandidate)
				if !utf8.ValidString(*raw.MisconceptionCandidate) || utf8.RuneCountInString(*raw.MisconceptionCandidate) > learning.MaxOfflineAssessmentMisconceptionRunes {
					writeLearningInvalid(w, r)
					return
				}
			}
			command.Items = append(command.Items, learning.OfflineAssessmentOverrideItem{
				RubricItemID: *raw.RubricItemID, Conclusion: *raw.Conclusion, MisconceptionCandidate: candidate,
			})
		}
	case "void":
		var input offlineAssessmentVoidInput
		if decodeLearningData(data, &input) != nil || input.Reason == nil || strings.TrimSpace(*input.Reason) == "" ||
			!utf8.ValidString(*input.Reason) || utf8.RuneCountInString(*input.Reason) > learning.MaxOfflineAssessmentDecisionReasonRunes {
			writeLearningInvalid(w, r)
			return
		}
		base, command.Reason = input.offlineAssessmentDecisionBaseInput, strings.TrimSpace(*input.Reason)
	default:
		writeLearningInvalid(w, r)
		return
	}
	if base.OperationID == nil || base.PayloadSchemaVersion == nil || base.AttemptID == nil ||
		base.ExpectedVersion == nil || base.Kind == nil || base.ExpectedDispositionVersion == nil ||
		!validLearningUUID(*base.OperationID) || !validLearningUUID(*base.AttemptID) || *base.PayloadSchemaVersion != 1 || *base.Kind != command.Kind {
		writeLearningInvalid(w, r)
		return
	}
	expectedVersion, err := learning.ParseUint63Decimal(*base.ExpectedVersion)
	if err != nil || expectedVersion == 0 || expectedVersion > uint64(^uint64(0)>>1) {
		writeLearningInvalid(w, r)
		return
	}
	expectedDisposition, err := learning.ParseUint63Decimal(*base.ExpectedDispositionVersion)
	if err != nil || expectedDisposition == 0 || expectedDisposition > uint64(^uint64(0)>>1) {
		writeLearningInvalid(w, r)
		return
	}
	command.OperationID, command.PayloadSchemaVersion, command.AttemptID = *base.OperationID, *base.PayloadSchemaVersion, *base.AttemptID
	command.ExpectedVersion, command.ExpectedDispositionVersion = int64(expectedVersion), int64(expectedDisposition)
	credential, _ := credentialFromContext(r.Context())
	response, err := a.offline.DecideOfflineAssessment(r.Context(), credential.Device.ID, assessmentID, command)
	if err != nil {
		a.writeOfflineFailure(w, r, "offline_assessment_decision", err)
		return
	}
	status := http.StatusCreated
	if response.Replayed {
		status = http.StatusOK
	}
	writeJSON(w, status, response)
}

type offlineAssessmentDecisionBaseInput struct {
	OperationID                *string `json:"operation_id"`
	PayloadSchemaVersion       *int    `json:"payload_schema_version"`
	AttemptID                  *string `json:"attempt_id"`
	ExpectedVersion            *string `json:"expected_version"`
	Kind                       *string `json:"kind"`
	ExpectedDispositionVersion *string `json:"expected_disposition_version"`
}

type offlineAssessmentConfirmInput struct {
	offlineAssessmentDecisionBaseInput
}

type offlineAssessmentOverrideItemInput struct {
	RubricItemID           *string              `json:"rubric_item_id"`
	Conclusion             *learning.Conclusion `json:"conclusion"`
	MisconceptionCandidate *string              `json:"misconception_candidate,omitempty"`
}

type offlineAssessmentOverrideInput struct {
	offlineAssessmentDecisionBaseInput
	Reason *string                               `json:"reason"`
	Items  *[]offlineAssessmentOverrideItemInput `json:"items"`
}

type offlineAssessmentVoidInput struct {
	offlineAssessmentDecisionBaseInput
	Reason *string `json:"reason"`
}

func (a *API) readOfflineData(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	data, err := readJSONBody(w, r, a.maxOfflineRequestBody)
	if err != nil {
		writeLearningDecodeFailure(w, r, err)
		return nil, false
	}
	return data, true
}

func normalizeOfflineAssessmentPage(page *learning.OfflineAssessmentPage) {
	normalizeProjectionMetadata(&page.Metadata)
	if page.Items == nil {
		page.Items = []learning.OfflineAssessmentSummary{}
	}
	for index := range page.Items {
		if page.Items[index].AllowedDecisions == nil {
			page.Items[index].AllowedDecisions = []string{}
		}
	}
}

func normalizeOfflineAssessmentView(view *learning.OfflineAssessmentView) {
	normalizeProjectionMetadata(&view.Metadata)
	if view.AllowedDecisions == nil {
		view.AllowedDecisions = []string{}
	}
	if view.Activity.References == nil {
		view.Activity.References = []learning.KnowledgeReference{}
	}
	if view.Activity.AllowedHelp == nil {
		view.Activity.AllowedHelp = []learning.HelpLevel{}
	}
	if view.Assessment.Items == nil {
		view.Assessment.Items = []learning.AssessmentItem{}
	}
	if view.Assessment.RiskFlags == nil {
		view.Assessment.RiskFlags = []learning.RiskFlag{}
	}
	if view.Assessment.AttemptCategories == nil {
		view.Assessment.AttemptCategories = []string{}
	}
	if view.Decision.Items == nil {
		view.Decision.Items = []learning.AssessmentItem{}
	}
}

func (a *API) decodeOffline(w http.ResponseWriter, r *http.Request, target any) bool {
	if err := decodeJSON(w, r, a.maxOfflineRequestBody, target); err != nil {
		writeLearningDecodeFailure(w, r, err)
		return false
	}
	return true
}

func (a *API) writeOfflineFailure(w http.ResponseWriter, r *http.Request, operation string, err error) {
	var privacyError *privacy.Error
	if errors.As(err, &privacyError) {
		status := http.StatusServiceUnavailable
		message := "Offline learning content is unavailable"
		code := privacyError.Code
		if code == "" {
			code = "internal_error"
			status = http.StatusInternalServerError
			message = "Request could not be completed"
		}
		writeError(w, r, status, code, message)
		return
	}
	a.writeLearningFailure(w, r, operation, err)
}
