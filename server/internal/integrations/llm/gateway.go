package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/edu-agent/edu-agent/packages/agentcore"
	"github.com/edu-agent/edu-agent/packages/agentcore/modelclient"
)

const GatewayMaxBody = 16 << 20

type GatewayMessage = modelclient.Message
type GatewayRequest struct {
	Model           string                      `json:"model"`
	Messages        []GatewayMessage            `json:"messages"`
	Tools           []modelclient.Tool          `json:"tools,omitempty"`
	MaxTokens       int                         `json:"max_tokens,omitempty"`
	ReasoningEffort modelclient.ReasoningEffort `json:"reasoning_effort,omitempty"`
	Stream          bool                        `json:"stream"`
	StreamOptions   *struct {
		IncludeUsage bool `json:"include_usage"`
	} `json:"stream_options,omitempty"`
}
type GatewayDelta struct {
	Role             string             `json:"role,omitempty"`
	Content          string             `json:"content,omitempty"`
	ReasoningContent string             `json:"reasoning_content,omitempty"`
	ToolCalls        []GatewayToolDelta `json:"tool_calls,omitempty"`
}
type GatewayToolDelta struct {
	Index    int                      `json:"index"`
	ID       string                   `json:"id,omitempty"`
	Type     string                   `json:"type,omitempty"`
	Function modelclient.ToolFunction `json:"function"`
}
type GatewayChunk struct {
	Delta        GatewayDelta
	FinishReason string
	Usage        map[string]json.RawMessage
}
type GatewayResult struct {
	Message      GatewayMessage
	FinishReason string
	Usage        map[string]json.RawMessage
}

// GatewayError contains only a stable category, never provider text or URLs.
type GatewayError string

func (e GatewayError) Error() string { return string(e) }
func (e GatewayError) Timeout() bool {
	return e == "model_idle_timeout" || e == "model_request_timeout"
}

var gatewayName = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,256}$`)

// DecodeGatewayRequest is the external input boundary. The shared core types
// preserve its protocol without allowing arbitrary proxy parameters.
func DecodeGatewayRequest(reader io.Reader, window, output int) (GatewayRequest, error) {
	var r GatewayRequest
	raw, err := io.ReadAll(io.LimitReader(reader, GatewayMaxBody+1))
	if err != nil {
		return r, GatewayError("invalid_request")
	}
	if len(raw) > GatewayMaxBody {
		return r, GatewayError("request_too_large")
	}
	if !utf8.Valid(raw) {
		return r, GatewayError("invalid_request")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(&r) != nil || d.Decode(new(any)) != io.EOF || len(r.Messages) == 0 {
		return r, GatewayError("invalid_request")
	}
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(raw, &fields)
	if _, ok := fields["max_tokens"]; ok && r.MaxTokens <= 0 {
		return r, GatewayError("invalid_request")
	}
	if r.MaxTokens == 0 {
		r.MaxTokens = output
	}
	if r.MaxTokens > output {
		return r, GatewayError("output_limit_exceeded")
	}
	switch r.ReasoningEffort {
	case "", "auto", "none", "minimal", "low", "medium", "high", "xhigh", "max":
	default:
		return r, GatewayError("invalid_request")
	}
	calls := map[string]bool{}
	for _, m := range r.Messages {
		switch m.Role {
		case "system", "user":
			if m.Content == "" || len(m.ToolCalls) > 0 || m.ToolCallID != "" {
				return r, GatewayError("invalid_request")
			}
		case "assistant":
			if m.ToolCallID != "" || m.Content == "" && len(m.ToolCalls) == 0 {
				return r, GatewayError("invalid_request")
			}
			for _, c := range m.ToolCalls {
				if c.ID == "" || len(c.ID) > 512 || calls[c.ID] || c.Type != "function" || !gatewayName.MatchString(c.Function.Name) || len(c.Function.Arguments) > 1<<20 || !json.Valid([]byte(c.Function.Arguments)) {
					return r, GatewayError("invalid_request")
				}
				calls[c.ID] = true
			}
		case "tool":
			if !calls[m.ToolCallID] || len(m.ToolCalls) > 0 {
				return r, GatewayError("invalid_request")
			}
			delete(calls, m.ToolCallID)
		default:
			return r, GatewayError("invalid_request")
		}
	}
	names := map[string]bool{}
	for _, tool := range r.Tools {
		var schema map[string]any
		if tool.Type != "function" || !gatewayName.MatchString(tool.Function.Name) || names[tool.Function.Name] || json.Unmarshal(tool.Function.Parameters, &schema) != nil || schema == nil {
			return r, GatewayError("invalid_request")
		}
		names[tool.Function.Name] = true
	}
	estimate := agentcore.NewTokenEstimator().EstimateRequest(modelclient.Request{Messages: r.Messages, Tools: r.Tools})
	if estimate+r.MaxTokens+agentcore.PercentRoundUp(window, 5) > window {
		return r, GatewayError("context_limit_exceeded")
	}
	return r, nil
}

func (c *Client) gatewayPayload(r GatewayRequest, stream, options bool) map[string]any {
	p := map[string]any{"model": c.model, "messages": r.Messages, "stream": stream, "max_tokens": r.MaxTokens}
	if len(r.Tools) > 0 {
		p["tools"] = r.Tools
	}
	if r.ReasoningEffort != "" && r.ReasoningEffort != "auto" {
		p["reasoning_effort"] = r.ReasoningEffort
	}
	if stream && options {
		p["stream_options"] = map[string]bool{"include_usage": true}
	}
	if c.wireAPI != Responses {
		return p
	}
	input := []any{}
	for _, m := range r.Messages {
		if m.Role == "tool" {
			input = append(input, map[string]any{"type": "function_call_output", "call_id": m.ToolCallID, "output": m.Content})
			continue
		}
		if m.Content != "" {
			input = append(input, map[string]any{"role": m.Role, "content": m.Content})
		}
		for _, call := range m.ToolCalls {
			input = append(input, map[string]any{"type": "function_call", "call_id": call.ID, "name": call.Function.Name, "arguments": call.Function.Arguments})
		}
	}
	p = map[string]any{"model": c.model, "input": input, "stream": stream, "store": false, "max_output_tokens": r.MaxTokens}
	if r.ReasoningEffort != "" && r.ReasoningEffort != "auto" {
		p["reasoning"] = map[string]any{"effort": r.ReasoningEffort}
	}
	if len(r.Tools) > 0 {
		tools := []any{}
		for _, t := range r.Tools {
			var schema any
			_ = json.Unmarshal(t.Function.Parameters, &schema)
			tools = append(tools, map[string]any{"type": "function", "name": t.Function.Name, "description": t.Function.Description, "parameters": responsesSchema(schema)})
		}
		p["tools"] = tools
	}
	return p
}

// Gateway owns only protocol conversion. The injected transport owns each real
// attempt's authorization, Dispatch/Observe and accounting before conversion.
func (c *Client) Gateway(ctx context.Context, r GatewayRequest, emit func(GatewayChunk) error) (GatewayResult, error) {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	timer := time.AfterFunc(c.httpClient.Timeout, func() { cancel(GatewayError("model_idle_timeout")) })
	defer timer.Stop()
	progress := func() { timer.Reset(c.httpClient.Timeout) }
	client := *c.httpClient
	client.Timeout = 0
	stream, options := r.Stream, true
	if r.MaxTokens == 0 {
		r.MaxTokens = c.maxOutputTokens
	}
	for {
		body, _ := json.Marshal(c.gatewayPayload(r, stream, options))
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
		if err != nil {
			return GatewayResult{}, GatewayError("upstream_error")
		}
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Accept", "application/json")
		if stream {
			request.Header.Set("Accept", "text/event-stream")
		}
		if c.apiKey != "" {
			request.Header.Set("Authorization", "Bearer "+c.apiKey)
		}
		response, err := client.Do(request)
		if err != nil {
			if cause := context.Cause(ctx); cause != nil {
				return GatewayResult{}, cause
			}
			var gate interface{ GatewayCode() string }
			if errors.As(err, &gate) {
				return GatewayResult{}, GatewayError(gate.GatewayCode())
			}
			return GatewayResult{}, GatewayError("upstream_error")
		}
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			raw, _ := io.ReadAll(io.LimitReader(response.Body, 64<<10))
			_ = response.Body.Close()
			if cause := context.Cause(ctx); cause != nil {
				return GatewayResult{}, cause
			}
			var rejection struct {
				Error struct {
					Param string `json:"param"`
					Code  string `json:"code"`
					Type  string `json:"type"`
				} `json:"error"`
			}
			_ = json.Unmarshal(raw, &rejection)
			if response.StatusCode == 400 || response.StatusCode == 422 {
				unsupported := explicitlyUnsupported(rejection.Error.Code) || explicitlyUnsupported(rejection.Error.Type)
				if unsupported && stream && options && rejection.Error.Param == "stream_options" && c.wireAPI == ChatCompletions {
					options = false
					progress()
					continue
				}
				if unsupported && stream && rejection.Error.Param == "stream" {
					stream = false
					progress()
					continue
				}
				if rejection.Error.Code == "context_length_exceeded" || rejection.Error.Code == "context_window_exceeded" {
					return GatewayResult{}, GatewayError("context_limit_exceeded")
				}
				return GatewayResult{}, GatewayError("upstream_incompatible")
			}
			if response.StatusCode == 429 {
				return GatewayResult{}, GatewayError("upstream_rate_limited")
			}
			return GatewayResult{}, GatewayError("upstream_error")
		}
		progress()
		var result GatewayResult
		if stream {
			if !strings.Contains(response.Header.Get("Content-Type"), "text/event-stream") {
				_ = response.Body.Close()
				return result, GatewayError("upstream_incompatible")
			}
			result, err = c.gatewayStream(response.Body, emit, progress)
		} else {
			result, err = c.gatewayComplete(gatewayProgressReader{reader: response.Body, progress: progress})
			if err == nil && r.Stream {
				err = emitComplete(result, emit)
			}
		}
		_ = response.Body.Close()
		if cause := context.Cause(ctx); cause != nil {
			return result, cause
		}
		return result, err
	}
}

type gatewayProgressReader struct {
	reader   io.Reader
	progress func()
}

func (r gatewayProgressReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	if n > 0 {
		r.progress()
	}
	return n, err
}

func emitComplete(r GatewayResult, emit func(GatewayChunk) error) error {
	if err := emit(GatewayChunk{Delta: GatewayDelta{Role: "assistant"}}); err != nil {
		return err
	}
	// Each delta stays within the shared consumer's limits, including fallback.
	for remaining := r.Message.Content; remaining != ""; {
		n := min(len(remaining), 64<<10)
		for !utf8.ValidString(remaining[:n]) {
			n--
		}
		if err := emit(GatewayChunk{Delta: GatewayDelta{Content: remaining[:n]}}); err != nil {
			return err
		}
		remaining = remaining[n:]
	}
	for i, c := range r.Message.ToolCalls {
		if err := emit(GatewayChunk{Delta: GatewayDelta{ToolCalls: []GatewayToolDelta{{Index: i, ID: c.ID, Type: "function", Function: modelclient.ToolFunction{Name: c.Function.Name}}}}}); err != nil {
			return err
		}
		for rest := c.Function.Arguments; rest != ""; {
			n := min(len(rest), 128<<10)
			for !utf8.ValidString(rest[:n]) {
				n--
			}
			if err := emit(GatewayChunk{Delta: GatewayDelta{ToolCalls: []GatewayToolDelta{{Index: i, Function: modelclient.ToolFunction{Arguments: rest[:n]}}}}}); err != nil {
				return err
			}
			rest = rest[n:]
		}
	}
	if err := emit(GatewayChunk{FinishReason: r.FinishReason}); err != nil {
		return err
	}
	if len(r.Usage) > 0 {
		return emit(GatewayChunk{Usage: r.Usage})
	}
	return nil
}

func cleanUsage(raw map[string]json.RawMessage, responses bool) map[string]json.RawMessage {
	out := map[string]json.RawMessage{}
	for _, key := range []string{"prompt_tokens", "completion_tokens", "total_tokens", "prompt_tokens_details", "completion_tokens_details", "prompt_cache_hit_tokens", "prompt_cache_miss_tokens"} {
		up := key
		if responses {
			switch key {
			case "prompt_tokens":
				up = "input_tokens"
			case "completion_tokens":
				up = "output_tokens"
			case "prompt_tokens_details":
				up = "input_tokens_details"
			case "completion_tokens_details":
				up = "output_tokens_details"
			}
		}
		v, ok := raw[up]
		if !ok || bytes.Equal(v, []byte("null")) {
			continue
		}
		if strings.HasSuffix(key, "_details") {
			var fields map[string]json.RawMessage
			if json.Unmarshal(v, &fields) != nil {
				continue
			}
			clean := map[string]int64{}
			for _, name := range []string{"cached_tokens", "cache_creation_tokens", "cache_write_tokens", "reasoning_tokens"} {
				var n int64
				if b, ok := fields[name]; ok && json.Unmarshal(b, &n) == nil && n >= 0 {
					clean[name] = n
				}
			}
			if len(clean) > 0 {
				out[key], _ = json.Marshal(clean)
			}
		} else {
			var n int64
			if json.Unmarshal(v, &n) == nil && n >= 0 {
				out[key] = v
			}
		}
	}
	return out
}

func validGatewayResult(r GatewayResult) error {
	if r.FinishReason == "stop" && r.Message.Content == "" {
		return GatewayError("upstream_incompatible")
	}
	if len(r.Message.Content) > 1<<20 || !utf8.ValidString(r.Message.Content) {
		return GatewayError("upstream_incompatible")
	}
	switch r.FinishReason {
	case "stop", "tool_calls", "length", "content_filter":
	default:
		return GatewayError("upstream_incompatible")
	}
	ids := map[string]bool{}
	for _, c := range r.Message.ToolCalls {
		if c.ID == "" || ids[c.ID] || len(c.ID) > 512 || c.Type != "function" || !gatewayName.MatchString(c.Function.Name) || len(c.Function.Arguments) > 1<<20 || r.FinishReason == "tool_calls" && !json.Valid([]byte(c.Function.Arguments)) {
			return GatewayError("upstream_incompatible")
		}
		ids[c.ID] = true
	}
	if r.FinishReason == "tool_calls" && len(r.Message.ToolCalls) == 0 || r.FinishReason == "stop" && len(r.Message.ToolCalls) > 0 {
		return GatewayError("upstream_incompatible")
	}
	return nil
}

func gatewayReadJSON(reader io.Reader, v any) error {
	raw, err := io.ReadAll(io.LimitReader(reader, 16<<20+1))
	if err != nil {
		return err
	}
	if len(raw) > 16<<20 || !utf8.Valid(raw) || json.Unmarshal(raw, v) != nil {
		return GatewayError("upstream_incompatible")
	}
	return nil
}

func (c *Client) gatewayComplete(reader io.Reader) (GatewayResult, error) {
	if c.wireAPI == Responses {
		return gatewayResponsesComplete(reader)
	}
	var body struct {
		Choices []struct {
			Message      GatewayMessage `json:"message"`
			FinishReason string         `json:"finish_reason"`
		} `json:"choices"`
		Usage map[string]json.RawMessage `json:"usage"`
	}
	if err := gatewayReadJSON(reader, &body); err != nil {
		return GatewayResult{}, err
	}
	if len(body.Choices) != 1 {
		return GatewayResult{}, GatewayError("upstream_incompatible")
	}
	r := GatewayResult{Message: body.Choices[0].Message, FinishReason: body.Choices[0].FinishReason, Usage: cleanUsage(body.Usage, false)}
	if r.Message.Role != "" && r.Message.Role != "assistant" {
		return r, GatewayError("upstream_incompatible")
	}
	r.Message.Role = "assistant"
	return r, validGatewayResult(r)
}

func explicitlyUnsupported(code string) bool {
	switch strings.ToLower(strings.TrimSpace(code)) {
	case "unsupported", "unsupported_feature", "unsupported_parameter", "unsupported_value", "parameter_not_supported", "not_supported":
		return true
	}
	return false
}
