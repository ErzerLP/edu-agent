package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	defaultMaxResponseBody = int64(8 << 20)
	userAgent              = "edu-agent-go-cli/1"
)

type APIError struct {
	Code               string
	RequestID          string
	Status             int
	CurrentRevisionID  *string
	IdentityReview     *IdentityReview
	Conflict           *LearningConflict
	CurrentDisposition string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("api request failed: code=%s status=%d request_id=%s", e.Code, e.Status, e.RequestID)
}

type ProtocolError struct{ Category string }

func (e *ProtocolError) Error() string { return "api protocol error: " + e.Category }

type TransportError struct{ Category string }

func (e *TransportError) Error() string { return "api transport error: " + e.Category }

type Client struct {
	baseURL string
	token   string
	timeout time.Duration
	http    *http.Client
	maxBody int64
	now     func() time.Time
	sleep   func(context.Context, time.Duration) error
}

func NewClient(baseURL, token string, timeout time.Duration, source *http.Client) *Client {
	if source == nil {
		source = &http.Client{}
	}
	copyClient := *source
	copyClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	copyClient.Timeout = 0
	return &Client{
		baseURL: baseURL,
		token:   token,
		timeout: timeout,
		http:    &copyClient,
		maxBody: defaultMaxResponseBody,
		now:     time.Now,
		sleep: func(ctx context.Context, delay time.Duration) error {
			timer := time.NewTimer(delay)
			defer timer.Stop()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-timer.C:
				return nil
			}
		},
	}
}

func (c *Client) Pair(ctx context.Context, code, displayName string) (IssuedCredential, error) {
	var response IssuedCredential
	err := c.doJSON(ctx, http.MethodPost, "/v1/pairings/exchange", false, struct {
		Code        string `json:"code"`
		DisplayName string `json:"display_name"`
	}{code, displayName}, map[int]bool{http.StatusCreated: true}, false, &response)
	return response, err
}

func (c *Client) Devices(ctx context.Context) (DevicesResponse, error) {
	var response DevicesResponse
	err := c.doJSON(ctx, http.MethodGet, "/v1/devices", true, nil, map[int]bool{http.StatusOK: true}, true, &response)
	return response, err
}

func (c *Client) Readiness(ctx context.Context) (Readiness, error) {
	var response Readiness
	err := c.doJSON(ctx, http.MethodGet, "/readyz", false, nil, map[int]bool{http.StatusOK: true, http.StatusServiceUnavailable: true}, true, &response)
	return response, err
}

func (c *Client) ModelCapabilities(ctx context.Context) (ModelCapabilities, error) {
	var response ModelCapabilities
	err := c.doJSON(ctx, http.MethodGet, "/v1/model/capabilities", true, nil, map[int]bool{http.StatusOK: true}, true, &response)
	return response, err
}

func (c *Client) RevokeDevice(ctx context.Context, deviceID string) error {
	path := "/v1/devices/" + url.PathEscape(deviceID)
	return c.doJSON(ctx, http.MethodDelete, path, true, nil, map[int]bool{http.StatusNoContent: true}, false, nil)
}

func (c *Client) KnowledgeHead(ctx context.Context) (KnowledgeRevision, error) {
	var response HeadResponse
	err := c.doJSON(ctx, http.MethodGet, "/v1/knowledge/revisions/head", true, nil, map[int]bool{http.StatusOK: true}, true, &response)
	return response.Revision, err
}

func (c *Client) ImportKnowledge(ctx context.Context, request ImportRequest) (ImportResult, error) {
	var response ImportResult
	err := c.doJSON(ctx, http.MethodPost, "/v1/knowledge/imports", true, request, map[int]bool{http.StatusOK: true, http.StatusCreated: true}, true, &response)
	return response, err
}

func (c *Client) RetrieveKnowledge(ctx context.Context, request KnowledgeRetrievalRequest) (KnowledgeRetrievalResult, error) {
	if err := validateKnowledgeRetrievalRequest(request); err != nil {
		return KnowledgeRetrievalResult{}, &ProtocolError{Category: "invalid_retrieval_request"}
	}
	var response KnowledgeRetrievalResult
	err := c.doJSON(ctx, http.MethodPost, "/v1/knowledge/retrievals", true, request, map[int]bool{http.StatusOK: true}, true, &response)
	return response, err
}

func (c *Client) CreateGoal(ctx context.Context, request LearningGoalRequest) (GoalOperationResult, error) {
	if err := validateGoalRequest(request); err != nil {
		return GoalOperationResult{}, &ProtocolError{Category: "invalid_goal_request"}
	}
	var response GoalOperationResult
	if err := c.doJSON(ctx, http.MethodPost, "/v1/learning/goals", true, request, map[int]bool{http.StatusOK: true, http.StatusCreated: true}, true, &response); err != nil {
		return response, err
	}
	return response, protocolSuccessError(validateGoalOperationBinding(response, request))
}

func (c *Client) CreateSession(ctx context.Context, request TutoringSessionRequest) (SessionOperationResult, error) {
	if err := validateSessionRequest(request); err != nil {
		return SessionOperationResult{}, &ProtocolError{Category: "invalid_session_request"}
	}
	var response SessionOperationResult
	if err := c.doJSON(ctx, http.MethodPost, "/v1/tutoring/sessions", true, request, map[int]bool{http.StatusOK: true, http.StatusCreated: true}, true, &response); err != nil {
		return response, err
	}
	return response, protocolSuccessError(validateSessionCreateBinding(response, request))
}

func (c *Client) CreateProposal(ctx context.Context, request TutoringProposalRequest) (TutoringProposal, error) {
	if err := validateProposalRequest(request); err != nil {
		return TutoringProposal{}, &ProtocolError{Category: "invalid_proposal_request"}
	}
	var response TutoringProposal
	if err := c.doJSON(ctx, http.MethodPost, "/v1/tutoring/proposals", true, request, map[int]bool{http.StatusCreated: true}, true, &response); err != nil {
		return response, err
	}
	return response, protocolSuccessError(validateProposalBinding(response, request))
}

func (c *Client) ApplySessionAction(ctx context.Context, sessionID string, request TutoringAction) (SessionOperationResult, error) {
	if err := validateTutoringActionRequest(sessionID, request); err != nil {
		return SessionOperationResult{}, &ProtocolError{Category: "invalid_action_request"}
	}
	var response SessionOperationResult
	path := "/v1/tutoring/sessions/" + url.PathEscape(sessionID) + "/actions"
	if err := c.doJSON(ctx, http.MethodPost, path, true, request, map[int]bool{http.StatusOK: true, http.StatusCreated: true}, true, &response); err != nil {
		return response, err
	}
	return response, protocolSuccessError(validateSessionOperationBinding(response, sessionID))
}

func (c *Client) DecideAssessment(ctx context.Context, assessmentID string, request AssessmentDecisionRequest) (AssessmentDecisionOperationResult, error) {
	if err := validateAssessmentDecisionRequest(assessmentID, request); err != nil {
		return AssessmentDecisionOperationResult{}, &ProtocolError{Category: "invalid_assessment_decision_request"}
	}
	var response AssessmentDecisionOperationResult
	path := "/v1/learning/assessments/" + url.PathEscape(assessmentID) + "/decisions"
	if err := c.doJSON(ctx, http.MethodPost, path, true, request, map[int]bool{http.StatusOK: true, http.StatusCreated: true}, true, &response); err != nil {
		return response, err
	}
	return response, protocolSuccessError(validateAssessmentDecisionOperationBinding(response, assessmentID, assessmentDecisionAggregateID(request)))
}

func (c *Client) CurrentSession(ctx context.Context) (SessionView, error) {
	var response SessionView
	err := c.doJSON(ctx, http.MethodGet, "/v1/tutoring/sessions/current", true, nil, map[int]bool{http.StatusOK: true}, true, &response)
	return response, err
}

func (c *Client) Session(ctx context.Context, sessionID string) (SessionView, error) {
	if !validLearningUUID(sessionID) {
		return SessionView{}, &ProtocolError{Category: "invalid_session_id"}
	}
	var response SessionView
	path := "/v1/tutoring/sessions/" + url.PathEscape(sessionID)
	err := c.doJSON(ctx, http.MethodGet, path, true, nil, map[int]bool{http.StatusOK: true}, true, &response)
	return response, err
}

func (c *Client) Timeline(ctx context.Context, cursor string, limit int, sessionID string) (TimelinePage, error) {
	if err := validateTimelineRequest(cursor, limit, sessionID); err != nil {
		return TimelinePage{}, &ProtocolError{Category: "invalid_timeline_request"}
	}
	var response TimelinePage
	values := url.Values{}
	setPageQuery(values, cursor, limit)
	if sessionID != "" {
		values.Set("session_id", sessionID)
	}
	err := c.doJSON(ctx, http.MethodGet, withQuery("/v1/learning/timeline", values), true, nil, map[int]bool{http.StatusOK: true}, true, &response)
	return response, err
}

func (c *Client) Routes(ctx context.Context, cursor string, limit int, currentOnly bool) (RoutesPage, error) {
	if err := validatePageRequest(cursor, limit); err != nil {
		return RoutesPage{}, &ProtocolError{Category: "invalid_routes_request"}
	}
	var response RoutesPage
	values := url.Values{}
	setPageQuery(values, cursor, limit)
	values.Set("current_only", strconv.FormatBool(currentOnly))
	err := c.doJSON(ctx, http.MethodGet, withQuery("/v1/learning/routes", values), true, nil, map[int]bool{http.StatusOK: true}, true, &response)
	return response, err
}

func (c *Client) Node(ctx context.Context, nodeRevisionID string) (NodeView, error) {
	if !validLearningUUID(nodeRevisionID) {
		return NodeView{}, &ProtocolError{Category: "invalid_node_revision_id"}
	}
	var response NodeView
	path := "/v1/learning/nodes/" + url.PathEscape(nodeRevisionID)
	err := c.doJSON(ctx, http.MethodGet, path, true, nil, map[int]bool{http.StatusOK: true}, true, &response)
	return response, err
}

func (c *Client) Evidence(ctx context.Context, cursor string, limit int, nodeRevisionID string) (EvidencePage, error) {
	if err := validateEvidenceRequest(cursor, limit, nodeRevisionID); err != nil {
		return EvidencePage{}, &ProtocolError{Category: "invalid_evidence_request"}
	}
	var response EvidencePage
	values := url.Values{}
	setPageQuery(values, cursor, limit)
	if nodeRevisionID != "" {
		values.Set("node_revision_id", nodeRevisionID)
	}
	err := c.doJSON(ctx, http.MethodGet, withQuery("/v1/learning/evidence", values), true, nil, map[int]bool{http.StatusOK: true}, true, &response)
	return response, err
}

func (c *Client) Reviews(ctx context.Context, cursor string, limit int, dueBefore *time.Time) (ReviewsPage, error) {
	if err := validateReviewsRequest(cursor, limit, dueBefore); err != nil {
		return ReviewsPage{}, &ProtocolError{Category: "invalid_reviews_request"}
	}
	var response ReviewsPage
	values := url.Values{}
	setPageQuery(values, cursor, limit)
	if dueBefore != nil {
		values.Set("due_before", dueBefore.UTC().Format(time.RFC3339Nano))
	}
	err := c.doJSON(ctx, http.MethodGet, withQuery("/v1/learning/reviews", values), true, nil, map[int]bool{http.StatusOK: true}, true, &response)
	return response, err
}

func (c *Client) ProjectionStatus(ctx context.Context) (ProjectionStatus, error) {
	var response ProjectionStatus
	err := c.doJSON(ctx, http.MethodGet, "/v1/learning/projections/status", true, nil, map[int]bool{http.StatusOK: true}, true, &response)
	return response, err
}

func protocolSuccessError(err error) error {
	if err == nil {
		return nil
	}
	return &ProtocolError{Category: "invalid_success_response"}
}

func setPageQuery(values url.Values, cursor string, limit int) {
	if cursor != "" {
		values.Set("cursor", cursor)
	}
	if limit > 0 {
		values.Set("limit", strconv.Itoa(limit))
	}
}

func withQuery(path string, values url.Values) string {
	if len(values) == 0 {
		return path
	}
	return path + "?" + values.Encode()
}

func (c *Client) doJSON(ctx context.Context, method, path string, authenticated bool, requestValue any, success map[int]bool, retry bool, target any) error {
	_, err := c.doJSONStatus(ctx, method, path, authenticated, requestValue, success, retry, target)
	return err
}

func (c *Client) doJSONStatus(ctx context.Context, method, path string, authenticated bool, requestValue any, success map[int]bool, retry bool, target any) (int, error) {
	var body []byte
	var err error
	if requestValue != nil {
		body, err = json.Marshal(requestValue)
		if err != nil {
			return 0, &ProtocolError{Category: "request_encoding_failed"}
		}
	}
	return c.doJSONBody(ctx, method, path, authenticated, body, requestValue != nil, success, retry, target)
}

// doCanonicalJSON sends the caller-provided JSON bytes without marshaling or
// otherwise rewriting them. The same immutable slice is read on every retry.
func (c *Client) doCanonicalJSON(ctx context.Context, method, path string, authenticated bool, body []byte, success map[int]bool, retry bool, target any) (int, error) {
	if len(body) == 0 {
		return 0, &ProtocolError{Category: "request_encoding_failed"}
	}
	return c.doJSONBody(ctx, method, path, authenticated, body, true, success, retry, target)
}

func (c *Client) doJSONBody(ctx context.Context, method, path string, authenticated bool, body []byte, hasBody bool, success map[int]bool, retry bool, target any) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	attempts := 1
	if retry {
		attempts = 2
	}
	for attempt := 1; attempt <= attempts; attempt++ {
		result, status, retryDelay, err := c.attempt(ctx, method, path, authenticated, body, hasBody, success, target)
		if result {
			return status, nil
		}
		if attempt == attempts || !shouldRetryError(err) {
			return status, err
		}
		if retryDelay > 0 {
			if deadline, ok := ctx.Deadline(); ok && !c.now().Add(retryDelay).Before(deadline) {
				return status, err
			}
			if sleepErr := c.sleep(ctx, retryDelay); sleepErr != nil {
				return status, &TransportError{Category: "deadline_exceeded"}
			}
		}
	}
	return 0, &TransportError{Category: "retry_exhausted"}
}

func (c *Client) attempt(ctx context.Context, method, path string, authenticated bool, body []byte, hasBody bool, success map[int]bool, target any) (bool, int, time.Duration, error) {
	endpoint := strings.TrimSuffix(c.baseURL, "/") + path
	var reader io.Reader
	if hasBody {
		reader = bytes.NewReader(body)
	}
	request, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return false, 0, 0, &ProtocolError{Category: "request_creation_failed"}
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", userAgent)
	if hasBody {
		request.Header.Set("Content-Type", "application/json")
	}
	if authenticated {
		request.Header.Set("Authorization", "Bearer "+c.token)
	}
	response, err := c.http.Do(request)
	if err != nil {
		return false, 0, 0, classifyTransport(err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, c.maxBody+1))
	if err != nil {
		return false, response.StatusCode, 0, classifyTransport(err)
	}
	if int64(len(data)) > c.maxBody {
		return false, response.StatusCode, 0, &ProtocolError{Category: "response_too_large"}
	}
	if success[response.StatusCode] {
		if response.StatusCode == http.StatusNoContent {
			if len(bytes.TrimSpace(data)) != 0 {
				return false, response.StatusCode, 0, &ProtocolError{Category: "unexpected_response_body"}
			}
			return true, response.StatusCode, 0, nil
		}
		if err := requireJSON(response.Header.Get("Content-Type")); err != nil {
			return false, response.StatusCode, 0, err
		}
		if err := decodeStrict(data, target); err != nil {
			switch target.(type) {
			case *OfflinePrepareResponse:
				return false, response.StatusCode, 0, &ProtocolError{Category: "malformed_offline_prepare_response: " + err.Error()}
			case *OfflineSyncResponse:
				return false, response.StatusCode, 0, &ProtocolError{Category: "malformed_offline_sync_response: " + err.Error()}
			case *OfflineOperationStatus:
				return false, response.StatusCode, 0, &ProtocolError{Category: "malformed_offline_status_response: " + err.Error()}
			}
			return false, response.StatusCode, 0, &ProtocolError{Category: "malformed_success_response"}
		}
		if err := validateDecoded(target); err != nil {
			switch target.(type) {
			case *OfflinePrepareResponse:
				return false, response.StatusCode, 0, &ProtocolError{Category: "invalid_offline_prepare_response: " + err.Error()}
			case *OfflineSyncResponse:
				return false, response.StatusCode, 0, &ProtocolError{Category: "invalid_offline_sync_response: " + err.Error()}
			case *OfflineOperationStatus:
				return false, response.StatusCode, 0, &ProtocolError{Category: "invalid_offline_status_response: " + err.Error()}
			}
			return false, response.StatusCode, 0, &ProtocolError{Category: "invalid_success_response"}
		}
		return true, response.StatusCode, 0, nil
	}
	if response.StatusCode >= 300 && response.StatusCode < 400 {
		return false, response.StatusCode, 0, &ProtocolError{Category: "redirect_refused"}
	}
	if response.StatusCode == http.StatusBadGateway || response.StatusCode == http.StatusGatewayTimeout {
		return false, response.StatusCode, 0, &APIError{Code: "dependency_unavailable", Status: response.StatusCode}
	}
	if err := requireJSON(response.Header.Get("Content-Type")); err != nil {
		return false, response.StatusCode, 0, err
	}
	var envelope ErrorResponse
	if err := decodeStrict(data, &envelope); err != nil || validateErrorResponse(method, path, response.StatusCode, envelope) != nil || validateErrorBinding(method, path, envelope, body, hasBody) != nil {
		return false, response.StatusCode, 0, &ProtocolError{Category: "malformed_error_response"}
	}
	apiErr := &APIError{
		Code: envelope.Error.Code, RequestID: envelope.Error.RequestID, Status: response.StatusCode,
		CurrentRevisionID: envelope.CurrentRevisionID, IdentityReview: envelope.IdentityReview,
		Conflict: envelope.Conflict, CurrentDisposition: envelope.CurrentDisposition,
	}
	if response.StatusCode == http.StatusTooManyRequests {
		delay, ok := parseRetryAfter(response.Header.Get("Retry-After"), c.now())
		if ok {
			return false, response.StatusCode, delay, apiErr
		}
	}
	if response.StatusCode == http.StatusServiceUnavailable && transient503[apiErr.Code] {
		return false, response.StatusCode, 0, apiErr
	}
	return false, response.StatusCode, 0, nonRetryable{err: apiErr}
}

type nonRetryable struct{ err error }

func (e nonRetryable) Error() string { return e.err.Error() }
func (e nonRetryable) Unwrap() error { return e.err }

func shouldRetryError(err error) bool {
	var stopped nonRetryable
	if errors.As(err, &stopped) {
		return false
	}
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr.Status == http.StatusBadGateway || apiErr.Status == http.StatusGatewayTimeout ||
			(apiErr.Status == http.StatusTooManyRequests) ||
			(apiErr.Status == http.StatusServiceUnavailable && transient503[apiErr.Code])
	}
	var transportErr *TransportError
	return errors.As(err, &transportErr) && transportErr.Category == "connection_failed"
}

func classifyTransport(err error) error {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return &TransportError{Category: "deadline_exceeded"}
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return &TransportError{Category: "connection_failed"}
	}
	var netErr *net.OpError
	if errors.As(err, &netErr) {
		return &TransportError{Category: "connection_failed"}
	}
	return &TransportError{Category: "transport_failed"}
}

func requireJSON(value string) error {
	mediaType, _, err := mime.ParseMediaType(value)
	if err != nil || !strings.EqualFold(mediaType, "application/json") {
		return &ProtocolError{Category: "unexpected_content_type"}
	}
	return nil
}

func decodeStrict(data []byte, target any) error {
	if err := rejectDuplicateJSONFields(data); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	decoder.UseNumber()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	return validateRequiredJSONFields(data, target)
}

func rejectDuplicateJSONFields(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := scanJSONValue(decoder); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	return nil
}

func scanJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delimiter {
	case '{':
		seen := map[string]struct{}{}
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return errors.New("JSON object key is not a string")
			}
			if _, exists := seen[key]; exists {
				return fmt.Errorf("duplicate JSON field %q", key)
			}
			seen[key] = struct{}{}
			if err := scanJSONValue(decoder); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil || end != json.Delim('}') {
			return errors.New("invalid JSON object")
		}
	case '[':
		for decoder.More() {
			if err := scanJSONValue(decoder); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil || end != json.Delim(']') {
			return errors.New("invalid JSON array")
		}
	default:
		return errors.New("invalid JSON delimiter")
	}
	return nil
}

// validateDecoded applies semantic checks after strict JSON shape validation.
func validateDecoded(target any) error {
	switch value := target.(type) {
	case *IssuedCredential:
		if value.Token == "" || validateDevice(value.Device) != nil || (value.Offline != nil && validateOfflinePairingBootstrap(*value.Offline) != nil) {
			return errors.New("issued credential is incomplete")
		}
	case *DevicesResponse:
		if value.Devices == nil {
			return errors.New("devices is required")
		}
		for _, device := range value.Devices {
			if err := validateDevice(device); err != nil {
				return err
			}
		}
	case *Readiness:
		if !validHealthStatus(value.Status) || value.Components == nil {
			return errors.New("readiness is incomplete")
		}
		for _, component := range value.Components {
			if !validHealthStatus(component.Status) {
				return errors.New("readiness component status is invalid")
			}
		}
	case *ModelCapabilities:
		if value.Profile != "openai-chat-completions-v1" || value.ContextWindow < 0 || value.MinimumContextWindow < 0 || value.IncompatibilityReasons == nil {
			return errors.New("model capabilities are incomplete")
		}
	case *HeadResponse:
		return validateRevision(value.Revision)
	case *ImportResult:
		return validateRevision(value.Revision)
	case *KnowledgeRetrievalResult:
		return validateRetrieval(*value)
	case *GoalOperationResult:
		return validateGoalOperationResult(*value)
	case *SessionOperationResult:
		return validateSessionOperationResult(*value)
	case *AssessmentDecisionOperationResult:
		return validateAssessmentDecisionOperationResult(*value)
	case *TutoringProposal:
		return validateTutoringProposal(*value)
	case *SessionView:
		return validateSessionView(*value)
	case *TimelinePage:
		return validateProjectionPageCursor[TimelineItem](value.Metadata, value.Items, value.NextCursor)
	case *RoutesPage:
		return validateProjectionPageCursor[RouteProjection](value.Metadata, value.Items, value.NextCursor)
	case *NodeView:
		return validateProjectionMetadata(value.Metadata)
	case *EvidencePage:
		return validateProjectionPageCursor[AcceptedEvidence](value.Metadata, value.Items, value.NextCursor)
	case *ReviewsPage:
		return validateProjectionPageCursor[ReviewSchedule](value.Metadata, value.Items, value.NextCursor)
	case *ProjectionStatus:
		if err := validateProjectionMetadata(value.Metadata); err != nil {
			return err
		}
		if value.ActiveGenerationID == "" || len(value.Fingerprint) != 64 {
			return errors.New("projection status is incomplete")
		}
	case *OfflinePrepareResponse:
		return validateOfflinePrepareResponse(*value)
	case *OfflineSyncResponse:
		return validateOfflineSyncResponse(*value)
	case *OfflineOperationStatus:
		return validateOfflineOperationStatus(*value)
	}
	return nil
}

func validateDevice(value Device) error {
	if value.ID == "" || value.DisplayName == "" || value.CreatedAt.IsZero() {
		return errors.New("device is incomplete")
	}
	return nil
}

func validateRevision(value KnowledgeRevision) error {
	if value.RevisionID == "" || value.RevisionNo < 1 || len(value.ManifestHash) != 64 || value.Source == "" || value.CreatedByDeviceID == "" || value.CreatedAt.IsZero() ||
		value.CanonicalizerVersion != "edu-markdown-v1" || value.ParserVersion != "goldmark-v1.8.5-commonmark-0.31.2-gfm" ||
		value.IndexerVersion != "knowledge-indexer-v1" || value.IdentityPolicyVersion != "identity-policy-v1" {
		return errors.New("knowledge revision is incomplete")
	}
	return nil
}

func validateErrorResponse(method, path string, status int, value ErrorResponse) error {
	if value.Error.Code == "" || value.Error.Message == "" || value.Error.RequestID == "" {
		return errors.New("error envelope is incomplete")
	}
	if status != http.StatusConflict && (value.CurrentRevisionID != nil || value.IdentityReview != nil || value.Conflict != nil || value.CurrentDisposition != "") {
		return errors.New("non-conflict response contains conflict fields")
	}
	if value.CurrentRevisionID != nil && !validLearningUUID(*value.CurrentRevisionID) {
		return errors.New("current revision ID is invalid")
	}
	if value.Conflict != nil {
		conflict := value.Conflict
		if (conflict.AggregateType != "goal" && conflict.AggregateType != "session") || !validLearningUUID(conflict.AggregateID) || conflict.ExpectedVersion < 0 || conflict.CurrentVersion < 0 || conflict.AsOfEventSeq < 0 {
			return errors.New("learning conflict is invalid")
		}
		if value.Error.Code != "version_conflict" && value.Error.Code != "idempotency_conflict" {
			return errors.New("unexpected learning conflict details")
		}
	}
	if value.Error.Code == "version_conflict" && value.Conflict == nil {
		return errors.New("version conflict details are missing")
	}
	if value.Error.Code == "assessment_disposition_conflict" {
		switch assessmentDispositionConflictEndpoint(method, path) {
		case "action":
			if value.CurrentDisposition != "" && !validAssessmentDisposition(value.CurrentDisposition) {
				return errors.New("assessment conflict disposition is invalid")
			}
		case "decision":
			if !validAssessmentDisposition(value.CurrentDisposition) {
				return errors.New("assessment conflict disposition is invalid")
			}
		default:
			return errors.New("assessment conflict is invalid for endpoint")
		}
	} else if value.CurrentDisposition != "" {
		return errors.New("unexpected assessment disposition")
	}
	if value.Error.Code == "identity_review_required" {
		if value.IdentityReview == nil || validateIdentityReview(*value.IdentityReview) != nil {
			return errors.New("identity review is incomplete")
		}
	} else if value.IdentityReview != nil {
		return errors.New("unexpected identity review")
	}
	if value.Error.Code == "revision_conflict" {
		if value.CurrentRevisionID == nil {
			return errors.New("revision conflict head is missing")
		}
	} else if value.CurrentRevisionID != nil {
		return errors.New("unexpected current revision")
	}
	return nil
}

func assessmentDispositionConflictEndpoint(method, path string) string {
	if method != http.MethodPost {
		return ""
	}
	path, _, _ = strings.Cut(path, "?")
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) != 5 || parts[0] != "v1" || !validLearningUUID(parts[3]) {
		return ""
	}
	if parts[1] == "tutoring" && parts[2] == "sessions" && parts[4] == "actions" {
		return "action"
	}
	if parts[1] == "learning" && parts[2] == "assessments" && parts[4] == "decisions" {
		return "decision"
	}
	return ""
}

func validateIdentityReview(value IdentityReview) error {
	if !validSHA256(value.BasisHash) || !validLearningUUID(value.OperationID) || !validSHA256(value.Receipt) || value.Documents == nil || value.Nodes == nil {
		return errors.New("identity review envelope is invalid")
	}
	for _, document := range value.Documents {
		if document.Path == "" || !validSHA256(document.Locator) || document.ReasonCode == "" || document.Candidates == nil || !validIdentityCandidates(document.Candidates) {
			return errors.New("document identity review is invalid")
		}
	}
	for _, node := range value.Nodes {
		if node.Path == "" || !validSHA256(node.Locator) || node.Preorder < 0 || node.ReasonCode == "" || node.Candidates == nil || !validIdentityCandidates(node.Candidates) {
			return errors.New("node identity review is invalid")
		}
	}
	return nil
}

func validIdentityCandidates(values []IdentityCandidate) bool {
	for _, candidate := range values {
		if !validLearningUUID(candidate.StableID) || !validLearningUUID(candidate.RevisionID) || candidate.ReasonCode == "" || candidate.Score < 0 || candidate.Score > 1000000 {
			return false
		}
		if candidate.Evidence != nil {
			if _, err := json.Marshal(candidate.Evidence); err != nil {
				return false
			}
		}
	}
	return true
}

func validateErrorBinding(method, path string, value ErrorResponse, body []byte, hasBody bool) error {
	if value.Conflict == nil {
		return nil
	}
	if !hasBody {
		return errors.New("learning conflict has no request body to bind")
	}
	if method == http.MethodPost && path == "/v1/learning/offline/packs" {
		var request struct {
			ExpectedSessionVersion string `json:"expected_session_version"`
		}
		if err := json.Unmarshal(body, &request); err != nil || request.ExpectedSessionVersion == "" || value.Conflict.AggregateType != "session" {
			return errors.New("offline prepare conflict request binding is unavailable")
		}
		expected, err := strconv.ParseInt(request.ExpectedSessionVersion, 10, 64)
		if err != nil || expected != value.Conflict.ExpectedVersion {
			return errors.New("offline prepare conflict does not belong to the request")
		}
		return nil
	}
	var request struct {
		AggregateType   string `json:"aggregate_type"`
		AggregateID     string `json:"aggregate_id"`
		ExpectedVersion int64  `json:"expected_version"`
	}
	if err := json.Unmarshal(body, &request); err != nil || request.AggregateType == "" || request.AggregateID == "" {
		return errors.New("learning conflict request binding is unavailable")
	}
	if value.Conflict.AggregateType != request.AggregateType || value.Conflict.AggregateID != request.AggregateID || value.Conflict.ExpectedVersion != request.ExpectedVersion {
		return errors.New("learning conflict does not belong to the request")
	}
	return nil
}

func validHealthStatus(value string) bool {
	return value == "healthy" || value == "degraded" || value == "not_ready"
}

func parseRetryAfter(value string, now time.Time) (time.Duration, bool) {
	value = strings.TrimSpace(value)
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil && seconds >= 0 {
		const maxRetryAfterSeconds = int64(^uint64(0)>>1) / int64(time.Second)
		if seconds > maxRetryAfterSeconds {
			return 0, false
		}
		return time.Duration(seconds) * time.Second, true
	}
	when, err := http.ParseTime(value)
	if err != nil || when.Before(now) {
		return 0, false
	}
	return when.Sub(now), true
}

var transient503 = map[string]bool{
	"temporarily_unavailable": true,
	"dependency_unavailable":  true,
	"upstream_unavailable":    true,
	"service_unavailable":     true,
	"unavailable":             true,
}
