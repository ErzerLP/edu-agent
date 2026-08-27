package api

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/url"
)

func (c *Client) PrepareOffline(ctx context.Context, request OfflinePrepareRequest) (OfflinePrepareResponse, int, error) {
	if err := validateOfflinePrepareRequest(request); err != nil {
		return OfflinePrepareResponse{}, 0, &ProtocolError{Category: "invalid_offline_prepare_request"}
	}
	var response OfflinePrepareResponse
	status, err := c.doJSONStatus(ctx, http.MethodPost, "/v1/learning/offline/packs", true, request, map[int]bool{
		http.StatusOK: true, http.StatusCreated: true,
	}, true, &response)
	if err != nil {
		return response, status, err
	}
	if err := validateOfflinePrepareBinding(response, request, status); err != nil {
		return response, status, &ProtocolError{Category: "invalid_success_response"}
	}
	return response, status, nil
}

func (c *Client) SyncOfflineCanonical(ctx context.Context, canonicalBody []byte) (OfflineSyncResponse, error) {
	request, err := decodeOfflineSyncCanonical(canonicalBody)
	if err != nil {
		return OfflineSyncResponse{}, &ProtocolError{Category: "invalid_offline_sync_request"}
	}
	var response OfflineSyncResponse
	if _, err := c.doCanonicalJSON(ctx, http.MethodPost, "/v1/learning/offline/sync", true, canonicalBody, map[int]bool{http.StatusOK: true}, true, &response); err != nil {
		return response, err
	}
	if err := validateOfflineSyncBinding(response, request); err != nil {
		return response, &ProtocolError{Category: "invalid_success_response"}
	}
	return response, nil
}

func (c *Client) OfflineOperationStatus(ctx context.Context, operationID string) (OfflineOperationStatus, error) {
	if !validLearningUUID(operationID) {
		return OfflineOperationStatus{}, &ProtocolError{Category: "invalid_offline_operation_id"}
	}
	var response OfflineOperationStatus
	path := "/v1/learning/offline/operations/" + url.PathEscape(operationID)
	if err := c.doJSON(ctx, http.MethodGet, path, true, nil, map[int]bool{http.StatusOK: true}, true, &response); err != nil {
		return response, err
	}
	if response.OperationID != operationID {
		return response, &ProtocolError{Category: "invalid_success_response"}
	}
	return response, nil
}

func (c *Client) OfflineAssessments(ctx context.Context, cursor string, limit int, status string) (OfflineAssessmentPage, error) {
	if validatePageRequest(cursor, limit) != nil || status != "provisional" {
		return OfflineAssessmentPage{}, &ProtocolError{Category: "invalid_offline_assessment_query"}
	}
	values := url.Values{}
	setPageQuery(values, cursor, limit)
	values.Set("status", status)
	var response OfflineAssessmentPage
	if err := c.doJSON(ctx, http.MethodGet, withQuery("/v1/learning/offline/assessments", values), true, nil, map[int]bool{http.StatusOK: true}, true, &response); err != nil {
		return response, err
	}
	if err := validateOfflineAssessmentPageContract(response); err != nil {
		return response, &ProtocolError{Category: "invalid_success_response"}
	}
	return response, nil
}

func (c *Client) OfflineAssessment(ctx context.Context, assessmentID string) (OfflineAssessmentView, error) {
	if !validLearningUUID(assessmentID) {
		return OfflineAssessmentView{}, &ProtocolError{Category: "invalid_offline_assessment_id"}
	}
	var response OfflineAssessmentView
	path := "/v1/learning/offline/assessments/" + url.PathEscape(assessmentID)
	if err := c.doJSON(ctx, http.MethodGet, path, true, nil, map[int]bool{http.StatusOK: true}, true, &response); err != nil {
		return response, err
	}
	if err := validateOfflineAssessmentViewContract(response); err != nil || response.Assessment.AssessmentID != assessmentID {
		return response, &ProtocolError{Category: "invalid_success_response"}
	}
	return response, nil
}

func (c *Client) DecideOfflineAssessment(ctx context.Context, assessmentID string, request OfflineAssessmentDecisionRequest) (OfflineAssessmentDecisionReceipt, error) {
	if err := validateOfflineAssessmentDecisionRequestContract(assessmentID, request); err != nil {
		return OfflineAssessmentDecisionReceipt{}, &ProtocolError{Category: "invalid_offline_assessment_decision_request"}
	}
	var response OfflineAssessmentDecisionReceipt
	path := "/v1/learning/offline/assessments/" + url.PathEscape(assessmentID) + "/decisions"
	status, err := c.doJSONStatus(ctx, http.MethodPost, path, true, request, map[int]bool{http.StatusOK: true, http.StatusCreated: true}, true, &response)
	if err != nil {
		return response, err
	}
	if err := validateOfflineAssessmentDecisionReceiptContract(response, assessmentID, request, status); err != nil {
		return response, &ProtocolError{Category: "invalid_success_response"}
	}
	return response, nil
}

func (c *Client) OfflineDevicePurgeTask(ctx context.Context, erasureID string) (*OfflinePurgeTask, error) {
	if !validLearningUUID(erasureID) {
		return nil, &ProtocolError{Category: "invalid_privacy_erasure_id"}
	}
	var response OfflinePurgeTask
	path := "/v1/privacy/erasures/" + url.PathEscape(erasureID) + "/offline-device-purge"
	status, err := c.doJSONStatus(ctx, http.MethodGet, path, true, nil, map[int]bool{http.StatusOK: true, http.StatusNoContent: true}, true, &response)
	if err != nil {
		return nil, err
	}
	if status == http.StatusNoContent {
		return nil, nil
	}
	if response.ErasureID != erasureID || !validLearningUUID(response.DeviceID) || response.Status != "pending" || !validPurgeChallenge(response.ChallengeRevision, response.OldGeneration, response.CurrentGeneration, response.Challenge) {
		return nil, &ProtocolError{Category: "invalid_success_response"}
	}
	return &response, nil
}

func (c *Client) AckOfflineDevicePurge(ctx context.Context, task OfflinePurgeTask, request OfflinePurgeAckRequest) (OfflinePurgeAckResponse, error) {
	if !validLearningUUID(task.ErasureID) || !validLearningUUID(task.DeviceID) || request.ChallengeRevision != task.ChallengeRevision || request.Challenge != task.Challenge || (request.Outcome != "succeeded" && request.Outcome != "failed") {
		return OfflinePurgeAckResponse{}, &ProtocolError{Category: "invalid_offline_purge_ack_request"}
	}
	if request.Outcome == "succeeded" && (request.ManagedObjectsAbsent == nil || !*request.ManagedObjectsAbsent || request.FailureCode != "") || request.Outcome == "failed" && (request.ManagedObjectsAbsent != nil || !validPurgeFailureCode(request.FailureCode)) {
		return OfflinePurgeAckResponse{}, &ProtocolError{Category: "invalid_offline_purge_ack_request"}
	}
	var response OfflinePurgeAckResponse
	path := "/v1/privacy/erasures/" + url.PathEscape(task.ErasureID) + "/offline-device-purge/ack"
	if err := c.doJSON(ctx, http.MethodPost, path, true, request, map[int]bool{http.StatusOK: true}, false, &response); err != nil {
		return response, err
	}
	expectedStatus := "succeeded"
	if request.Outcome == "failed" {
		expectedStatus = "failed"
	}
	if response.ErasureID != task.ErasureID || response.DeviceID != task.DeviceID || response.ChallengeRevision != task.ChallengeRevision || response.Status != expectedStatus || response.StableReason == "" {
		return response, &ProtocolError{Category: "invalid_success_response"}
	}
	return response, nil
}

func validPurgeChallenge(revision, oldGeneration, currentGeneration int64, challenge string) bool {
	if revision <= 0 || oldGeneration <= 0 || currentGeneration <= oldGeneration {
		return false
	}
	decoded, err := base64.RawURLEncoding.Strict().DecodeString(challenge)
	return err == nil && len(decoded) == 32 && base64.RawURLEncoding.EncodeToString(decoded) == challenge
}

func validPurgeFailureCode(value string) bool {
	switch value {
	case "profile_busy", "key_delete_failed", "path_delete_failed", "verification_failed":
		return true
	default:
		return false
	}
}
