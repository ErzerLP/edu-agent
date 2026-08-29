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
)

const maxResponseBytes = int64(4 << 20)

type Client struct {
	baseURL string
	model   string
	apiKey  string
	http    *http.Client
}

type completionRequest struct {
	Model    string    `json:"model"`
	Messages []Message `json:"messages"`
	Tools    []Tool    `json:"tools,omitempty"`
	Stream   bool      `json:"stream"`
}

type completionResponse struct {
	Choices []struct {
		Message      Message `json:"message"`
		FinishReason string  `json:"finish_reason"`
	} `json:"choices"`
}

type errorResponse struct {
	Error struct {
		Code string `json:"code"`
		Type string `json:"type"`
	} `json:"error"`
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
		return nil, errors.New("模型请求超时必须为正数")
	}
	transport := http.DefaultTransport
	if source != nil && source.Transport != nil {
		transport = source.Transport
	}
	httpClient := &http.Client{
		Transport: transport, Timeout: timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	return &Client{baseURL: strings.TrimSuffix(parsed.String(), "/"), model: model, apiKey: apiKey, http: httpClient}, nil
}

func (c *Client) Complete(ctx context.Context, request Request) (Response, error) {
	if len(request.Messages) == 0 {
		return Response{}, errors.New("模型消息不能为空")
	}
	body, err := json.Marshal(completionRequest{Model: c.model, Messages: request.Messages, Tools: request.Tools, Stream: false})
	if err != nil {
		return Response{}, fmt.Errorf("编码模型请求: %w", err)
	}
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return Response{}, fmt.Errorf("创建模型请求: %w", err)
	}
	httpRequest.Header.Set("Content-Type", "application/json")
	httpRequest.Header.Set("Accept", "application/json")
	if c.apiKey != "" {
		httpRequest.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	response, err := c.http.Do(httpRequest)
	if err != nil {
		if ctx.Err() != nil {
			return Response{}, ctx.Err()
		}
		return Response{}, errors.New("无法连接模型服务")
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil {
		return Response{}, errors.New("读取模型响应失败")
	}
	if int64(len(data)) > maxResponseBytes {
		return Response{}, errors.New("模型响应超过大小限制")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		var envelope errorResponse
		_ = json.Unmarshal(data, &envelope)
		code := strings.TrimSpace(envelope.Error.Code)
		if code == "" {
			code = strings.TrimSpace(envelope.Error.Type)
		}
		if code == "" {
			return Response{}, fmt.Errorf("模型服务返回 HTTP %d", response.StatusCode)
		}
		return Response{}, fmt.Errorf("模型服务返回 HTTP %d (%s)", response.StatusCode, safeCode(code))
	}
	if !strings.HasPrefix(strings.ToLower(response.Header.Get("Content-Type")), "application/json") {
		return Response{}, errors.New("模型服务返回了非 JSON 响应")
	}
	var envelope completionResponse
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(&envelope); err != nil || len(envelope.Choices) == 0 {
		return Response{}, errors.New("模型响应格式无效")
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
	return Response{Message: message, FinishReason: envelope.Choices[0].FinishReason}, nil
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
