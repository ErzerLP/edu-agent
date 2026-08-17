package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
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
	if !c.probeAt.IsZero() && age >= 0 && age < c.probeCacheTTL {
		return cloneCapabilities(c.probeResult)
	}
	result := c.probe(ctx)
	if ctx.Err() == nil {
		c.probeAt = time.Now()
		c.probeResult = cloneCapabilities(result)
	}
	return result
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
	if expected, _ := schema["type"].(string); expected != "" && !matchesType(expected, value) {
		return fmt.Errorf("%s must be %s", path, expected)
	}
	object, isObject := value.(map[string]any)
	if isObject {
		properties, _ := schema["properties"].(map[string]any)
		if required, ok := schema["required"].([]any); ok {
			for _, item := range required {
				name, _ := item.(string)
				if _, exists := object[name]; !exists {
					return fmt.Errorf("%s.%s is required", path, name)
				}
			}
		}
		for name, rawChild := range properties {
			childValue, exists := object[name]
			childSchema, ok := rawChild.(map[string]any)
			if exists && ok {
				if err := validateValue(childSchema, childValue, path+"."+name); err != nil {
					return err
				}
			}
		}
		if additional, ok := schema["additionalProperties"].(bool); ok && !additional {
			for name := range object {
				if _, declared := properties[name]; !declared {
					return fmt.Errorf("%s.%s is not allowed", path, name)
				}
			}
		}
	}
	return nil
}

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
		return ok && number == float64(int64(number))
	case "null":
		return value == nil
	default:
		return false
	}
}
