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
	Code              string
	RequestID         string
	Status            int
	CurrentRevisionID *string
	IdentityReview    *IdentityReview
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

func (c *Client) doJSON(ctx context.Context, method, path string, authenticated bool, requestValue any, success map[int]bool, retry bool, target any) error {
	var body []byte
	var err error
	if requestValue != nil {
		body, err = json.Marshal(requestValue)
		if err != nil {
			return &ProtocolError{Category: "request_encoding_failed"}
		}
	}
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	attempts := 1
	if retry {
		attempts = 2
	}
	for attempt := 1; attempt <= attempts; attempt++ {
		result, retryDelay, err := c.attempt(ctx, method, path, authenticated, body, requestValue != nil, success, target)
		if result {
			return nil
		}
		if attempt == attempts || !shouldRetryError(err) {
			return err
		}
		if retryDelay > 0 {
			if deadline, ok := ctx.Deadline(); ok && !c.now().Add(retryDelay).Before(deadline) {
				return err
			}
			if sleepErr := c.sleep(ctx, retryDelay); sleepErr != nil {
				return &TransportError{Category: "deadline_exceeded"}
			}
		}
	}
	return &TransportError{Category: "retry_exhausted"}
}

func (c *Client) attempt(ctx context.Context, method, path string, authenticated bool, body []byte, hasBody bool, success map[int]bool, target any) (bool, time.Duration, error) {
	endpoint := strings.TrimSuffix(c.baseURL, "/") + path
	var reader io.Reader
	if hasBody {
		reader = bytes.NewReader(body)
	}
	request, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return false, 0, &ProtocolError{Category: "request_creation_failed"}
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
		return false, 0, classifyTransport(err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, c.maxBody+1))
	if err != nil {
		return false, 0, classifyTransport(err)
	}
	if int64(len(data)) > c.maxBody {
		return false, 0, &ProtocolError{Category: "response_too_large"}
	}
	if success[response.StatusCode] {
		if response.StatusCode == http.StatusNoContent {
			if len(bytes.TrimSpace(data)) != 0 {
				return false, 0, &ProtocolError{Category: "unexpected_response_body"}
			}
			return true, 0, nil
		}
		if err := requireJSON(response.Header.Get("Content-Type")); err != nil {
			return false, 0, err
		}
		if err := decodeStrict(data, target); err != nil {
			return false, 0, &ProtocolError{Category: "malformed_success_response"}
		}
		if err := validateDecoded(target); err != nil {
			return false, 0, &ProtocolError{Category: "invalid_success_response"}
		}
		return true, 0, nil
	}
	if response.StatusCode >= 300 && response.StatusCode < 400 {
		return false, 0, &ProtocolError{Category: "redirect_refused"}
	}
	if response.StatusCode == http.StatusBadGateway || response.StatusCode == http.StatusGatewayTimeout {
		return false, 0, &APIError{Code: "dependency_unavailable", Status: response.StatusCode}
	}
	if err := requireJSON(response.Header.Get("Content-Type")); err != nil {
		return false, 0, err
	}
	var envelope ErrorResponse
	if err := decodeStrict(data, &envelope); err != nil || validateErrorResponse(envelope) != nil {
		return false, 0, &ProtocolError{Category: "malformed_error_response"}
	}
	apiErr := &APIError{
		Code: envelope.Error.Code, RequestID: envelope.Error.RequestID, Status: response.StatusCode,
		CurrentRevisionID: envelope.CurrentRevisionID, IdentityReview: envelope.IdentityReview,
	}
	if response.StatusCode == http.StatusTooManyRequests {
		delay, ok := parseRetryAfter(response.Header.Get("Retry-After"), c.now())
		if ok {
			return false, delay, apiErr
		}
	}
	if response.StatusCode == http.StatusServiceUnavailable && transient503[apiErr.Code] {
		return false, 0, apiErr
	}
	return false, 0, nonRetryable{err: apiErr}
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

func validateDecoded(target any) error {
	switch value := target.(type) {
	case *IssuedCredential:
		if value.Token == "" || validateDevice(value.Device) != nil {
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

func validateErrorResponse(value ErrorResponse) error {
	if value.Error.Code == "" || value.Error.Message == "" || value.Error.RequestID == "" {
		return errors.New("error envelope is incomplete")
	}
	if value.IdentityReview != nil {
		review := value.IdentityReview
		if len(review.BasisHash) != 64 || review.OperationID == "" || len(review.Receipt) != 64 || review.Documents == nil || review.Nodes == nil {
			return errors.New("identity review is incomplete")
		}
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
