package modelclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"
)

const maxResponseBytes = int64(4 << 20)

type Client struct {
	baseURL string
	model   string
	apiKey  string
	timeout time.Duration
	http    *http.Client
}

type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type completionRequest struct {
	Model           string          `json:"model"`
	Messages        []Message       `json:"messages"`
	Tools           []Tool          `json:"tools,omitempty"`
	MaxTokens       int             `json:"max_tokens,omitempty"`
	Stream          bool            `json:"stream"`
	StreamOptions   *streamOptions  `json:"stream_options,omitempty"`
	ReasoningEffort ReasoningEffort `json:"reasoning_effort,omitempty"`
}

type completionResponse struct {
	Choices []struct {
		Message      Message `json:"message"`
		FinishReason string  `json:"finish_reason"`
	} `json:"choices"`
	Usage *Usage `json:"usage,omitempty"`
}

type providerError struct {
	Code    string `json:"code"`
	Type    string `json:"type"`
	Param   string `json:"param"`
	Message string `json:"message"`
}

type errorResponse struct {
	Error providerError `json:"error"`
}

func New(baseURL, model, apiKey string, timeout time.Duration, source *http.Client) (*Client, error) {
	parsed, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("模型服务地址无效")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, errors.New("模型服务地址必须使用 HTTP 或 HTTPS")
	}
	model = strings.TrimSpace(model)
	if model == "" {
		return nil, errors.New("模型名称不能为空")
	}
	if timeout <= 0 {
		return nil, errors.New("模型无响应超时必须为正数")
	}
	transport := http.DefaultTransport
	if source != nil && source.Transport != nil {
		transport = source.Transport
	}
	httpClient := &http.Client{
		Transport:     transport,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	return &Client{baseURL: strings.TrimSuffix(parsed.String(), "/"), model: model, apiKey: apiKey, timeout: timeout, http: httpClient}, nil
}

func (c *Client) Complete(ctx context.Context, request Request) (Response, error) {
	effort, err := normalizeReasoningEffort(request.ReasoningEffort)
	if err != nil {
		return Response{}, err
	}
	if len(request.Messages) == 0 {
		return Response{}, errors.New("模型消息不能为空")
	}
	body, err := json.Marshal(completionRequest{
		Model:           c.model,
		Messages:        request.Messages,
		Tools:           request.Tools,
		MaxTokens:       request.MaxTokens,
		Stream:          false,
		ReasoningEffort: effort,
	})
	if err != nil {
		return Response{}, fmt.Errorf("编码模型请求: %w", err)
	}
	requestCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	httpRequest, err := c.newRequest(requestCtx, body, "application/json")
	if err != nil {
		return Response{}, err
	}
	response, err := c.http.Do(httpRequest)
	if err != nil {
		if contextErr := requestContextError(ctx, requestCtx); contextErr != nil {
			return Response{}, contextErr
		}
		return Response{}, errors.New("无法连接模型服务")
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil {
		if contextErr := requestContextError(ctx, requestCtx); contextErr != nil {
			return Response{}, contextErr
		}
		return Response{}, errors.New("读取模型响应失败")
	}
	if int64(len(data)) > maxResponseBytes {
		return Response{}, errors.New("模型响应超过大小限制")
	}
	if !utf8.Valid(data) {
		return Response{}, clientError(ErrorCodeResponseProtocol, "模型响应不是有效 UTF-8")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return Response{}, providerHTTPError(response.StatusCode, data)
	}
	if !strings.HasPrefix(strings.ToLower(response.Header.Get("Content-Type")), "application/json") {
		return Response{}, errors.New("模型服务返回了非 JSON 响应")
	}
	var envelope completionResponse
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(&envelope); err != nil || len(envelope.Choices) == 0 {
		return Response{}, errors.New("模型响应格式无效")
	}
	if envelope.Usage != nil && !validUsage(*envelope.Usage) {
		return Response{}, clientError(ErrorCodeResponseProtocol, "模型 usage 无效")
	}
	finishReason := strings.TrimSpace(envelope.Choices[0].FinishReason)
	if err := finishReasonError(finishReason, false); err != nil {
		return Response{}, err
	}
	message := envelope.Choices[0].Message
	if message.Role == "" {
		message.Role = "assistant"
	}
	if message.Content == "" && len(message.ToolCalls) == 0 {
		return Response{}, errors.New("模型响应不包含文本或工具调用")
	}
	for _, call := range message.ToolCalls {
		if call.ID == "" || call.Type != "function" || call.Function.Name == "" || !json.Valid([]byte(call.Function.Arguments)) {
			return Response{}, errors.New("模型工具调用格式无效")
		}
	}
	if len(message.ToolCalls) > 0 && finishReason != "tool_calls" || len(message.ToolCalls) == 0 && finishReason == "tool_calls" {
		return Response{}, clientError(ErrorCodeResponseProtocol, "模型 finish_reason 与工具调用不一致")
	}
	return Response{Message: message, FinishReason: finishReason, Usage: envelope.Usage}, nil
}

func (c *Client) newRequest(ctx context.Context, body []byte, accept string) (*http.Request, error) {
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("创建模型请求: %w", err)
	}
	httpRequest.Header.Set("Content-Type", "application/json")
	httpRequest.Header.Set("Accept", accept)
	if c.apiKey != "" {
		httpRequest.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	return httpRequest, nil
}

func normalizeReasoningEffort(value ReasoningEffort) (ReasoningEffort, error) {
	value = ReasoningEffort(strings.TrimSpace(string(value)))
	switch value {
	case "", ReasoningEffortAuto:
		return "", nil
	case ReasoningEffortNone, ReasoningEffortMinimal, ReasoningEffortLow, ReasoningEffortMedium, ReasoningEffortHigh, ReasoningEffortXHigh, ReasoningEffortMax:
		return value, nil
	default:
		return "", clientError(ErrorCodeInvalidReasoningEffort, "模型推理强度无效")
	}
}

func providerHTTPError(statusCode int, data []byte) error {
	provider := decodeProviderError(data)
	if isReasoningEffortUnsupported(provider) {
		return clientError(ErrorCodeReasoningEffortUnsupported, "模型不支持当前推理强度")
	}
	code := strings.TrimSpace(provider.Code)
	if code == "" {
		code = strings.TrimSpace(provider.Type)
	}
	if code == "" {
		return fmt.Errorf("模型服务返回 HTTP %d", statusCode)
	}
	return fmt.Errorf("模型服务返回 HTTP %d (%s)", statusCode, safeCode(code))
}

func decodeProviderError(data []byte) providerError {
	var envelope errorResponse
	if !json.Valid(data) || json.Unmarshal(data, &envelope) != nil {
		return providerError{}
	}
	return envelope.Error
}

func isReasoningEffortUnsupported(value providerError) bool {
	if strings.TrimSpace(strings.ToLower(value.Param)) != "reasoning_effort" {
		return false
	}
	return isExplicitUnsupportedCode(value.Code) || isExplicitUnsupportedCode(value.Type)
}

func isStreamUnsupported(value providerError) bool {
	code := normalizeProviderCode(value.Code)
	typeCode := normalizeProviderCode(value.Type)
	if code == "stream_not_supported" || code == "streaming_not_supported" || typeCode == "stream_not_supported" || typeCode == "streaming_not_supported" {
		return true
	}
	if strings.TrimSpace(strings.ToLower(value.Param)) != "stream" {
		return false
	}
	return isExplicitUnsupportedCode(code) || isExplicitUnsupportedCode(typeCode)
}

func finishReasonError(value string, streaming bool) error {
	switch strings.TrimSpace(value) {
	case "stop", "tool_calls":
		return nil
	case "length":
		return clientError(ErrorCodeResponseTruncated, "模型回答因长度限制而截断")
	case "content_filter":
		return clientError(ErrorCodeContentFiltered, "模型回答被内容策略过滤")
	default:
		if streaming {
			return clientError(ErrorCodeStreamProtocol, "模型 SSE finish_reason 无效")
		}
		return clientError(ErrorCodeResponseProtocol, "模型响应 finish_reason 无效")
	}
}

func isExplicitUnsupportedCode(value string) bool {
	switch normalizeProviderCode(value) {
	case "unsupported", "unsupported_feature", "unsupported_parameter", "unsupported_value", "parameter_not_supported", "not_supported":
		return true
	default:
		return false
	}
}

func normalizeProviderCode(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

func safeCode(value string) string {
	var builder strings.Builder
	for _, character := range value {
		if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' || character == '_' || character == '-' || character == '.' {
			builder.WriteRune(character)
		}
		if builder.Len() >= 64 {
			break
		}
	}
	if builder.Len() == 0 {
		return "unknown"
	}
	return builder.String()
}
