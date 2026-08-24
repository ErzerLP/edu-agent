package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

type ErrorCategory string

const (
	ErrorUnauthorized    ErrorCategory = "unauthorized"
	ErrorRateLimited     ErrorCategory = "rate_limited"
	ErrorUpstream        ErrorCategory = "upstream_error"
	ErrorTimeout         ErrorCategory = "timeout"
	ErrorUnavailable     ErrorCategory = "unavailable"
	ErrorInvalidResponse ErrorCategory = "invalid_response"
	ErrorSchemaMismatch  ErrorCategory = "schema_mismatch"
	ErrorIncompatible    ErrorCategory = "incompatible"
	ErrorInvalidRequest  ErrorCategory = "invalid_request"
)

type Error struct {
	Category ErrorCategory
	Status   int
	Cause    error
}

func (e *Error) Error() string {
	if e.Status != 0 {
		return fmt.Sprintf("model request failed: category=%s status=%d", e.Category, e.Status)
	}
	return fmt.Sprintf("model request failed: category=%s", e.Category)
}

func (e *Error) Unwrap() error { return e.Cause }

func Category(err error) ErrorCategory {
	var modelErr *Error
	if errors.As(err, &modelErr) {
		return modelErr.Category
	}
	return ErrorUpstream
}

type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
)

type Message struct {
	Role    Role   `json:"role"`
	Content string `json:"content"`
}

type ChatRequest struct {
	Messages            []Message
	Schema              json.RawMessage
	SchemaName          string
	UseNativeJSONSchema bool
}

type ChatResult struct {
	JSON json.RawMessage
}

type Capabilities struct {
	Profile                string   `json:"profile"`
	Compatible             bool     `json:"compatible"`
	ContextWindow          int      `json:"context_window"`
	MinimumContextWindow   int      `json:"minimum_context_window"`
	SystemUserAssistant    bool     `json:"system_user_assistant_messages"`
	NonStreaming           bool     `json:"non_streaming"`
	StructuredJSON         bool     `json:"structured_json"`
	NativeJSONSchema       bool     `json:"native_json_schema"`
	Streaming              bool     `json:"streaming"`
	ToolCalls              bool     `json:"tool_calls"`
	IncompatibilityReasons []string `json:"incompatibility_reasons"`
}

type Options struct {
	BaseURL        *url.URL
	Model          string
	APIKey         string
	ContextWindow  int
	MinimumContext int
	Timeout        time.Duration
	ProbeCacheTTL  time.Duration
	HTTPClient     *http.Client
}

type Client struct {
	endpoint       string
	model          string
	apiKey         string
	contextWindow  int
	minimumContext int
	httpClient     *http.Client
	probeCacheTTL  time.Duration
	probeMu        sync.Mutex
	probeAt        time.Time
	probeResult    Capabilities
}

func New(options Options) (*Client, error) {
	if options.BaseURL == nil || (options.BaseURL.Scheme != "http" && options.BaseURL.Scheme != "https") || options.BaseURL.Host == "" {
		return nil, errors.New("model base URL must be absolute HTTP(S)")
	}
	if strings.TrimSpace(options.Model) == "" || strings.TrimSpace(options.APIKey) == "" {
		return nil, errors.New("model name and API key are required")
	}
	if options.ContextWindow < options.MinimumContext || options.MinimumContext <= 0 {
		return nil, errors.New("configured model context window is below the profile minimum")
	}
	if options.Timeout <= 0 {
		return nil, errors.New("model timeout must be positive")
	}
	base := *options.BaseURL
	path := strings.TrimSuffix(base.Path, "/")
	if strings.HasSuffix(path, "/v1") {
		base.Path = path + "/chat/completions"
	} else {
		base.Path = path + "/v1/chat/completions"
	}
	client := options.HTTPClient
	if client == nil {
		client = &http.Client{}
	}
	clone := *client
	clone.Timeout = options.Timeout
	if options.ProbeCacheTTL <= 0 {
		options.ProbeCacheTTL = 15 * time.Minute
	}
	return &Client{
		endpoint: base.String(), model: options.Model, apiKey: options.APIKey,
		contextWindow: options.ContextWindow, minimumContext: options.MinimumContext,
		httpClient: &clone, probeCacheTTL: options.ProbeCacheTTL,
	}, nil
}

func (c *Client) Chat(ctx context.Context, request ChatRequest) (ChatResult, error) {
	if len(request.Messages) == 0 {
		return ChatResult{}, &Error{Category: ErrorInvalidRequest}
	}
	for _, message := range request.Messages {
		if message.Role != RoleSystem && message.Role != RoleUser && message.Role != RoleAssistant {
			return ChatResult{}, &Error{Category: ErrorInvalidRequest}
		}
		if strings.TrimSpace(message.Content) == "" {
			return ChatResult{}, &Error{Category: ErrorInvalidRequest}
		}
	}
	payload := map[string]any{
		"model": c.model, "messages": request.Messages, "stream": false,
		"response_format": map[string]any{"type": "json_object"},
	}
	if len(request.Schema) != 0 && request.UseNativeJSONSchema {
		var schema any
		if err := json.Unmarshal(request.Schema, &schema); err != nil {
			return ChatResult{}, &Error{Category: ErrorInvalidRequest, Cause: err}
		}
		name := request.SchemaName
		if name == "" {
			name = "edu_agent_response"
		}
		payload["response_format"] = map[string]any{
			"type":        "json_schema",
			"json_schema": map[string]any{"name": name, "strict": true, "schema": schema},
		}
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return ChatResult{}, &Error{Category: ErrorInvalidRequest, Cause: err}
	}
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return ChatResult{}, &Error{Category: ErrorInvalidRequest, Cause: err}
	}
	httpRequest.Header.Set("Content-Type", "application/json")
	httpRequest.Header.Set("Accept", "application/json")
	httpRequest.Header.Set("Authorization", "Bearer "+c.apiKey)
	response, err := c.httpClient.Do(httpRequest)
	if err != nil {
		return ChatResult{}, classifyTransportError(ctx, err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10))
		return ChatResult{}, classifyHTTPError(response.StatusCode)
	}
	var envelope struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, 1<<20))
	if err := decoder.Decode(&envelope); err != nil || len(envelope.Choices) == 0 {
		return ChatResult{}, &Error{Category: ErrorInvalidResponse, Cause: err}
	}
	content := []byte(envelope.Choices[0].Message.Content)
	if !json.Valid(content) {
		return ChatResult{}, &Error{Category: ErrorInvalidResponse}
	}
	if len(request.Schema) != 0 {
		if err := validateJSONSchema(request.Schema, content); err != nil {
			return ChatResult{}, &Error{Category: ErrorSchemaMismatch, Cause: err}
		}
	}
	return ChatResult{JSON: append(json.RawMessage(nil), content...)}, nil
}

func (c *Client) Probe(ctx context.Context) Capabilities {
	c.probeMu.Lock()
	defer c.probeMu.Unlock()
	now := time.Now()
	age := now.Sub(c.probeAt)
	cacheTTL := c.probeCacheTTL
	if !c.probeResult.Compatible && cacheTTL > 30*time.Second {
		cacheTTL = 30 * time.Second
	}
	if !c.probeAt.IsZero() && age >= 0 && age < cacheTTL {
		result := cloneCapabilities(c.probeResult)
		if result.Compatible {
			if err := c.checkAvailability(ctx); err != nil {
				result.Compatible = false
				result.IncompatibilityReasons = []string{string(Category(err))}
			}
		}
		return result
	}
	result := c.probe(ctx)
	if ctx.Err() == nil {
		c.probeAt = time.Now()
		c.probeResult = cloneCapabilities(result)
	}
	return result
}

func (c *Client) checkAvailability(ctx context.Context) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodHead, c.endpoint, nil)
	if err != nil {
		return &Error{Category: ErrorInvalidRequest, Cause: err}
	}
	request.Header.Set("Authorization", "Bearer "+c.apiKey)
	response, err := c.httpClient.Do(request)
	if err != nil {
		return classifyTransportError(ctx, err)
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4<<10))
	switch response.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		return &Error{Category: ErrorUnauthorized}
	case http.StatusTooManyRequests:
		return &Error{Category: ErrorRateLimited}
	}
	if response.StatusCode >= 500 {
		return &Error{Category: ErrorUpstream}
	}
	return nil
}

func (c *Client) probe(ctx context.Context) Capabilities {
	capabilities := Capabilities{
		Profile: "openai-chat-completions-v1", ContextWindow: c.contextWindow,
		MinimumContextWindow: c.minimumContext,
		NativeJSONSchema:     false, Streaming: false, ToolCalls: false,
	}
	schema := json.RawMessage(`{"type":"object","properties":{"capability_probe":{"type":"boolean"}},"required":["capability_probe"],"additionalProperties":false}`)
	request := ChatRequest{
		Messages: []Message{
			{Role: RoleSystem, Content: "Return only JSON matching the requested schema."},
			{Role: RoleAssistant, Content: "I will return the requested JSON object."},
			{Role: RoleUser, Content: "Confirm the core profile."},
		},
		Schema: schema, SchemaName: "capability_probe", UseNativeJSONSchema: true,
	}
	_, nativeErr := c.Chat(ctx, request)
	if nativeErr == nil {
		markCoreCapabilities(&capabilities)
		capabilities.NativeJSONSchema = true
		return capabilities
	}
	if Category(nativeErr) != ErrorIncompatible {
		capabilities.IncompatibilityReasons = []string{string(Category(nativeErr))}
		return capabilities
	}
	request.UseNativeJSONSchema = false
	if _, err := c.Chat(ctx, request); err != nil {
		capabilities.IncompatibilityReasons = []string{string(Category(err))}
		return capabilities
	}
	markCoreCapabilities(&capabilities)
	return capabilities
}

func markCoreCapabilities(capabilities *Capabilities) {
	capabilities.Compatible = true
	capabilities.SystemUserAssistant = true
	capabilities.NonStreaming = true
	capabilities.StructuredJSON = true
}

func cloneCapabilities(value Capabilities) Capabilities {
	value.IncompatibilityReasons = append([]string(nil), value.IncompatibilityReasons...)
	return value
}

func classifyHTTPError(status int) error {
	category := ErrorIncompatible
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		category = ErrorUnauthorized
	case status == http.StatusTooManyRequests:
		category = ErrorRateLimited
	case status >= 500:
		category = ErrorUpstream
	}
	return &Error{Category: category, Status: status}
}

func classifyTransportError(ctx context.Context, err error) error {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) {
		return &Error{Category: ErrorTimeout, Cause: context.DeadlineExceeded}
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return &Error{Category: ErrorTimeout, Cause: netErr}
	}
	return &Error{Category: ErrorUnavailable}
}

func validateJSONSchema(schemaBytes, valueBytes []byte) error {
	var schema map[string]any
	var value any
	if err := json.Unmarshal(schemaBytes, &schema); err != nil {
		return err
	}
	if err := json.Unmarshal(valueBytes, &value); err != nil {
		return err
	}
	return validateValue(schema, value, "$")
}

func validateValue(schema map[string]any, value any, path string) error {
	if expected, exists := schema["type"]; exists {
		typeName, ok := expected.(string)
		if !ok || !matchesType(typeName, value) {
			return fmt.Errorf("%s must be %v", path, expected)
		}
	}
	if rawEnum, exists := schema["enum"]; exists {
		allowed, ok := rawEnum.([]any)
		if !ok {
			return fmt.Errorf("%s has invalid enum schema", path)
		}
		matched := false
		for _, candidate := range allowed {
			if reflect.DeepEqual(candidate, value) {
				matched = true
				break
			}
		}
		if !matched {
			return fmt.Errorf("%s is not an allowed value", path)
		}
	}

	if object, ok := value.(map[string]any); ok {
		properties := map[string]any{}
		if rawProperties, exists := schema["properties"]; exists {
			var valid bool
			properties, valid = rawProperties.(map[string]any)
			if !valid {
				return fmt.Errorf("%s has invalid properties schema", path)
			}
		}
		if rawRequired, exists := schema["required"]; exists {
			required, valid := rawRequired.([]any)
			if !valid {
				return fmt.Errorf("%s has invalid required schema", path)
			}
			for _, item := range required {
				name, valid := item.(string)
				if !valid || name == "" {
					return fmt.Errorf("%s has invalid required schema", path)
				}
				if _, exists := object[name]; !exists {
					return fmt.Errorf("%s.%s is required", path, name)
				}
			}
		}
		for name, rawChild := range properties {
			childValue, exists := object[name]
			if !exists {
				continue
			}
			childSchema, valid := rawChild.(map[string]any)
			if !valid {
				return fmt.Errorf("%s.%s has invalid schema", path, name)
			}
			if err := validateValue(childSchema, childValue, path+"."+name); err != nil {
				return err
			}
		}
		if rawAdditional, exists := schema["additionalProperties"]; exists {
			additional, valid := rawAdditional.(bool)
			if !valid {
				return fmt.Errorf("%s has unsupported additionalProperties schema", path)
			}
			if !additional {
				for name := range object {
					if _, declared := properties[name]; !declared {
						return fmt.Errorf("%s.%s is not allowed", path, name)
					}
				}
			}
		}
	}

	if array, ok := value.([]any); ok {
		if err := validateCountConstraint(schema, "minItems", len(array), path, "items"); err != nil {
			return err
		}
		if err := validateCountConstraint(schema, "maxItems", len(array), path, "items"); err != nil {
			return err
		}
		if rawItems, exists := schema["items"]; exists {
			itemSchema, valid := rawItems.(map[string]any)
			if !valid {
				return fmt.Errorf("%s has invalid items schema", path)
			}
			for index, item := range array {
				if err := validateValue(itemSchema, item, fmt.Sprintf("%s[%d]", path, index)); err != nil {
					return err
				}
			}
		}
	}

	if text, ok := value.(string); ok {
		length := utf8.RuneCountInString(text)
		if err := validateCountConstraint(schema, "minLength", length, path, "characters"); err != nil {
			return err
		}
		if err := validateCountConstraint(schema, "maxLength", length, path, "characters"); err != nil {
			return err
		}
	}

	if number, ok := value.(float64); ok {
		if err := validateNumberConstraint(schema, "minimum", number, path); err != nil {
			return err
		}
		if err := validateNumberConstraint(schema, "maximum", number, path); err != nil {
			return err
		}
	}
	return nil
}

func validateCountConstraint(schema map[string]any, keyword string, actual int, path, unit string) error {
	raw, exists := schema[keyword]
	if !exists {
		return nil
	}
	limit, ok := raw.(float64)
	if !ok || limit < 0 || math.Trunc(limit) != limit || limit > float64(maxInt()) {
		return fmt.Errorf("%s has invalid %s schema", path, keyword)
	}
	if keyword == "minItems" || keyword == "minLength" {
		if actual < int(limit) {
			return fmt.Errorf("%s must contain at least %d %s", path, int(limit), unit)
		}
		return nil
	}
	if actual > int(limit) {
		return fmt.Errorf("%s must contain at most %d %s", path, int(limit), unit)
	}
	return nil
}

func validateNumberConstraint(schema map[string]any, keyword string, actual float64, path string) error {
	raw, exists := schema[keyword]
	if !exists {
		return nil
	}
	limit, ok := raw.(float64)
	if !ok {
		return fmt.Errorf("%s has invalid %s schema", path, keyword)
	}
	if keyword == "minimum" && actual < limit {
		return fmt.Errorf("%s must be at least %v", path, limit)
	}
	if keyword == "maximum" && actual > limit {
		return fmt.Errorf("%s must be at most %v", path, limit)
	}
	return nil
}

func maxInt() int { return int(^uint(0) >> 1) }

func matchesType(expected string, value any) bool {
	switch expected {
	case "object":
		_, ok := value.(map[string]any)
		return ok
	case "array":
		_, ok := value.([]any)
		return ok
	case "string":
		_, ok := value.(string)
		return ok
	case "boolean":
		_, ok := value.(bool)
		return ok
	case "number":
		_, ok := value.(float64)
		return ok
	case "integer":
		number, ok := value.(float64)
		return ok && math.Trunc(number) == number
	case "null":
		return value == nil
	default:
		return false
	}
}
