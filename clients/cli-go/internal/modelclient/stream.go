package modelclient

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"sort"
	"strings"
	"unicode/utf8"
)

const (
	maxSSELineBytes            = 512 << 10
	maxSSEEventBytes           = 512 << 10
	maxSSELines                = 8192
	maxSSEEvents               = 4096
	maxStreamTextDeltaBytes    = 64 << 10
	maxStreamHiddenReasoning   = 64 << 10
	maxStreamTextBytes         = 1 << 20
	maxStreamToolCalls         = 32
	maxStreamToolIDBytes       = 512
	maxStreamToolNameBytes     = 256
	maxStreamArgumentDelta     = 128 << 10
	maxStreamToolArgumentBytes = 1 << 20
)

type streamChunk struct {
	Choices []streamChoice `json:"choices"`
	Usage   *Usage         `json:"usage,omitempty"`
	Error   *providerError `json:"error,omitempty"`
}

type streamChoice struct {
	Index        *int        `json:"index"`
	Delta        streamDelta `json:"delta"`
	FinishReason *string     `json:"finish_reason"`
}

type streamDelta struct {
	Role             string                `json:"role,omitempty"`
	Content          *string               `json:"content"`
	ToolCalls        []streamToolCallDelta `json:"tool_calls,omitempty"`
	ReasoningContent json.RawMessage       `json:"reasoning_content,omitempty"`
	Reasoning        json.RawMessage       `json:"reasoning,omitempty"`
	ReasoningDetails json.RawMessage       `json:"reasoning_details,omitempty"`
	Thinking         json.RawMessage       `json:"thinking,omitempty"`
}

type streamToolCallDelta struct {
	Index    *int                `json:"index"`
	ID       string              `json:"id,omitempty"`
	Type     string              `json:"type,omitempty"`
	Function streamFunctionDelta `json:"function"`
}

type streamFunctionDelta struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

type assembledToolCall struct {
	index     int
	id        strings.Builder
	typeName  strings.Builder
	name      strings.Builder
	arguments strings.Builder
}

type streamAssembler struct {
	roleSeen     bool
	text         strings.Builder
	tools        map[int]*assembledToolCall
	finishReason string
	finishSeen   bool
	usage        *Usage
	sawIncrement bool
}

type sseEvent struct {
	name string
	data []byte
}

type sseReader struct {
	reader *bufio.Reader
	total  int64
	lines  int
	events int
}

type streamCompatibilityFallback uint8

const (
	streamCompatibilityNone streamCompatibilityFallback = iota
	streamCompatibilityWithoutOptions
	streamCompatibilityComplete
)

func (c *Client) Stream(ctx context.Context, request Request, observe func(StreamEvent) error) (Response, error) {
	if observe == nil {
		return Response{}, errors.New("模型流式观察器不能为空")
	}
	effort, err := normalizeReasoningEffort(request.ReasoningEffort)
	if err != nil {
		return Response{}, err
	}
	if len(request.Messages) == 0 {
		return Response{}, errors.New("模型消息不能为空")
	}

	includeStreamOptions := true
	compatibilityFallback := false
	responseStarted := false
	for {
		response, fallback, attemptErr := c.streamAttempt(ctx, request, effort, includeStreamOptions, &responseStarted, observe)
		switch fallback {
		case streamCompatibilityWithoutOptions:
			if err := notifyCompatibilityFallback(ctx, observe); err != nil {
				return Response{}, err
			}
			includeStreamOptions = false
			compatibilityFallback = true
			continue
		case streamCompatibilityComplete:
			return c.completeFallback(ctx, request, observe, !compatibilityFallback)
		default:
			if attemptErr == nil && compatibilityFallback {
				response.CompatibilityFallback = true
			}
			return response, attemptErr
		}
	}
}

func (c *Client) streamAttempt(
	ctx context.Context,
	request Request,
	effort ReasoningEffort,
	includeStreamOptions bool,
	responseStarted *bool,
	observe func(StreamEvent) error,
) (Response, streamCompatibilityFallback, error) {
	var options *streamOptions
	if includeStreamOptions {
		options = &streamOptions{IncludeUsage: true}
	}
	body, err := json.Marshal(completionRequest{
		Model:           c.model,
		Messages:        request.Messages,
		Tools:           request.Tools,
		MaxTokens:       request.MaxTokens,
		Stream:          true,
		StreamOptions:   options,
		ReasoningEffort: effort,
	})
	if err != nil {
		return Response{}, streamCompatibilityNone, fmt.Errorf("编码模型请求: %w", err)
	}
	requestCtx, inactivity := newInactivityTimeout(ctx, c.timeout)
	defer inactivity.Stop()
	httpRequest, err := c.newRequest(requestCtx, body, "text/event-stream")
	if err != nil {
		return Response{}, streamCompatibilityNone, err
	}
	response, err := c.http.Do(httpRequest)
	if err != nil {
		if contextErr := requestContextError(ctx, requestCtx); contextErr != nil {
			return Response{}, streamCompatibilityNone, contextErr
		}
		return Response{}, streamCompatibilityNone, errors.New("无法连接模型服务")
	}
	defer response.Body.Close()
	responseBody := activityReader{reader: response.Body, touch: inactivity.Touch}

	if response.StatusCode < 200 || response.StatusCode >= 300 {
		data, readErr := io.ReadAll(io.LimitReader(responseBody, maxResponseBytes+1))
		if readErr != nil {
			if contextErr := requestContextError(ctx, requestCtx); contextErr != nil {
				return Response{}, streamCompatibilityNone, contextErr
			}
			return Response{}, streamCompatibilityNone, errors.New("读取模型响应失败")
		}
		if int64(len(data)) > maxResponseBytes {
			return Response{}, streamCompatibilityNone, clientError(ErrorCodeStreamResponseTooLarge, "模型流式响应超过大小限制")
		}
		if !utf8.Valid(data) {
			return Response{}, streamCompatibilityNone, clientError(ErrorCodeStreamProtocol, "模型流式响应不是有效 UTF-8")
		}
		provider := decodeProviderError(data)
		if includeStreamOptions && isStreamOptionsUnsupported(provider) {
			return Response{}, streamCompatibilityWithoutOptions, nil
		}
		if isStreamUnsupported(provider) {
			return Response{}, streamCompatibilityComplete, nil
		}
		return Response{}, streamCompatibilityNone, providerHTTPError(response.StatusCode, data)
	}
	mediaType, _, parseErr := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if parseErr != nil || !strings.EqualFold(mediaType, "text/event-stream") {
		return Response{}, streamCompatibilityNone, clientError(ErrorCodeStreamProtocol, "模型服务返回了非 SSE 响应")
	}
	if contextErr := requestContextError(ctx, requestCtx); contextErr != nil {
		return Response{}, streamCompatibilityNone, contextErr
	}

	reader := &sseReader{reader: bufio.NewReaderSize(io.LimitReader(responseBody, maxResponseBytes+1), 32<<10)}
	assembler := streamAssembler{tools: make(map[int]*assembledToolCall)}
	for {
		if contextErr := requestContextError(ctx, requestCtx); contextErr != nil {
			return Response{}, streamCompatibilityNone, contextErr
		}
		event, readErr := reader.next()
		if readErr != nil {
			if contextErr := requestContextError(ctx, requestCtx); contextErr != nil {
				return Response{}, streamCompatibilityNone, contextErr
			}
			if errors.Is(readErr, io.EOF) {
				return Response{}, streamCompatibilityNone, clientError(ErrorCodeStreamProtocol, "模型流式响应在完成前中断")
			}
			return Response{}, streamCompatibilityNone, readErr
		}
		if bytes.Equal(bytes.TrimSpace(event.data), []byte("[DONE]")) {
			if event.name == "error" || !assembler.finishSeen {
				return Response{}, streamCompatibilityNone, clientError(ErrorCodeStreamProtocol, "模型流式响应终止顺序无效")
			}
			assembled, err := assembler.response()
			return assembled, streamCompatibilityNone, err
		}
		provider, hasProviderError := decodeProviderErrorFrame(event.data)
		if event.name == "error" || hasProviderError {
			if !assembler.sawIncrement {
				if includeStreamOptions && isStreamOptionsUnsupported(provider) {
					return Response{}, streamCompatibilityWithoutOptions, nil
				}
				if isStreamUnsupported(provider) {
					return Response{}, streamCompatibilityComplete, nil
				}
			}
			if isReasoningEffortUnsupported(provider) {
				return Response{}, streamCompatibilityNone, clientError(ErrorCodeReasoningEffortUnsupported, "模型不支持当前推理强度")
			}
			return Response{}, streamCompatibilityNone, clientError(ErrorCodeStreamProtocol, "模型流式响应包含错误事件")
		}
		text, applyErr := assembler.apply(event.data)
		if applyErr != nil {
			return Response{}, streamCompatibilityNone, applyErr
		}
		if !*responseStarted {
			if err := observe(StreamEvent{Kind: StreamEventResponseStarted}); err != nil {
				return Response{}, streamCompatibilityNone, err
			}
			*responseStarted = true
		}
		if text != "" {
			if err := observe(StreamEvent{Kind: StreamEventTextDelta, Text: text}); err != nil {
				return Response{}, streamCompatibilityNone, err
			}
			if contextErr := requestContextError(ctx, requestCtx); contextErr != nil {
				return Response{}, streamCompatibilityNone, contextErr
			}
		}
	}
}

func isStreamOptionsUnsupported(value providerError) bool {
	if strings.TrimSpace(strings.ToLower(value.Param)) != "stream_options" {
		return false
	}
	return isExplicitUnsupportedCode(value.Code) || isExplicitUnsupportedCode(value.Type)
}

func notifyCompatibilityFallback(ctx context.Context, observe func(StreamEvent) error) error {
	if err := observe(StreamEvent{Kind: StreamEventCompatibilityFallback}); err != nil {
		return err
	}
	return ctx.Err()
}

func (c *Client) completeFallback(ctx context.Context, request Request, observe func(StreamEvent) error, notify bool) (Response, error) {
	if notify {
		if err := notifyCompatibilityFallback(ctx, observe); err != nil {
			return Response{}, err
		}
	}
	response, err := c.Complete(ctx, request)
	if err != nil {
		return Response{}, err
	}
	response.CompatibilityFallback = true
	return response, nil
}

func (r *sseReader) next() (sseEvent, error) {
	var event sseEvent
	var data bytes.Buffer
	for {
		line, err := r.readLine()
		if err != nil {
			if errors.Is(err, io.EOF) && data.Len() == 0 && event.name == "" {
				return sseEvent{}, io.EOF
			}
			if errors.Is(err, io.EOF) {
				return sseEvent{}, clientError(ErrorCodeStreamProtocol, "模型 SSE 事件未完整终止")
			}
			return sseEvent{}, err
		}
		line = bytes.TrimSuffix(line, []byte{'\n'})
		line = bytes.TrimSuffix(line, []byte{'\r'})
		if len(line) == 0 {
			if data.Len() == 0 {
				event.name = ""
				continue
			}
			r.events++
			if r.events > maxSSEEvents {
				return sseEvent{}, clientError(ErrorCodeStreamProtocol, "模型 SSE 事件数量超过限制")
			}
			event.data = append([]byte(nil), data.Bytes()...)
			return event, nil
		}
		if line[0] == ':' {
			continue
		}
		field, value, found := bytes.Cut(line, []byte{':'})
		if !found {
			field = line
			value = nil
		}
		if len(value) > 0 && value[0] == ' ' {
			value = value[1:]
		}
		switch string(field) {
		case "data":
			if data.Len() > 0 {
				_ = data.WriteByte('\n')
			}
			if data.Len()+len(value) > maxSSEEventBytes {
				return sseEvent{}, clientError(ErrorCodeStreamResponseTooLarge, "模型 SSE 事件超过大小限制")
			}
			_, _ = data.Write(value)
		case "event":
			if event.name != "" || len(value) > 64 || !utf8.Valid(value) {
				return sseEvent{}, clientError(ErrorCodeStreamProtocol, "模型 SSE 事件名称无效")
			}
			event.name = string(value)
		case "id", "retry":
			if len(value) > 256 {
				return sseEvent{}, clientError(ErrorCodeStreamProtocol, "模型 SSE 元数据超过限制")
			}
		default:
			return sseEvent{}, clientError(ErrorCodeStreamProtocol, "模型 SSE 包含未知字段")
		}
	}
}

func (r *sseReader) readLine() ([]byte, error) {
	var line []byte
	for {
		fragment, err := r.reader.ReadSlice('\n')
		r.total += int64(len(fragment))
		if r.total > maxResponseBytes {
			return nil, clientError(ErrorCodeStreamResponseTooLarge, "模型流式响应超过大小限制")
		}
		if len(line)+len(fragment) > maxSSELineBytes {
			return nil, clientError(ErrorCodeStreamResponseTooLarge, "模型 SSE 行超过大小限制")
		}
		line = append(line, fragment...)
		if err == nil {
			r.lines++
			if r.lines > maxSSELines {
				return nil, clientError(ErrorCodeStreamProtocol, "模型 SSE 行数超过限制")
			}
			return line, nil
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if errors.Is(err, io.EOF) && len(line) > 0 {
			r.lines++
			if r.lines > maxSSELines {
				return nil, clientError(ErrorCodeStreamProtocol, "模型 SSE 行数超过限制")
			}
			return line, nil
		}
		return nil, err
	}
}

func (a *streamAssembler) apply(data []byte) (string, error) {
	if len(data) == 0 || len(data) > maxSSEEventBytes || !utf8.Valid(data) {
		return "", clientError(ErrorCodeStreamProtocol, "模型 SSE 数据无效")
	}
	var chunk streamChunk
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(&chunk); err != nil {
		return "", clientError(ErrorCodeStreamProtocol, "模型 SSE JSON 无效")
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return "", clientError(ErrorCodeStreamProtocol, "模型 SSE JSON 包含多余数据")
	}
	if chunk.Error != nil {
		return "", clientError(ErrorCodeStreamProtocol, "模型流式响应包含错误事件")
	}
	if chunk.Usage != nil {
		if a.usage != nil || !validUsage(*chunk.Usage) {
			return "", clientError(ErrorCodeStreamProtocol, "模型流式 usage 无效")
		}
		usage := *chunk.Usage
		a.usage = &usage
	}
	if len(chunk.Choices) == 0 {
		if chunk.Usage == nil || !a.finishSeen {
			return "", clientError(ErrorCodeStreamProtocol, "模型 SSE 缺少 choice")
		}
		return "", nil
	}
	if len(chunk.Choices) != 1 || a.finishSeen {
		return "", clientError(ErrorCodeStreamProtocol, "模型 SSE choice 数量无效")
	}
	choice := chunk.Choices[0]
	if choice.Index == nil || *choice.Index != 0 {
		return "", clientError(ErrorCodeStreamProtocol, "模型 SSE choice index 无效")
	}
	if choice.Delta.Role != "" {
		if choice.Delta.Role != "assistant" || a.roleSeen {
			return "", clientError(ErrorCodeStreamProtocol, "模型 SSE role 无效")
		}
		a.roleSeen = true
		a.sawIncrement = true
	}
	if hidden, err := validateHiddenReasoning(choice.Delta); err != nil {
		return "", err
	} else if hidden {
		a.sawIncrement = true
	}
	var text string
	if choice.Delta.Content != nil {
		text = *choice.Delta.Content
		if !utf8.ValidString(text) || len(text) > maxStreamTextDeltaBytes || a.text.Len()+len(text) > maxStreamTextBytes {
			return "", clientError(ErrorCodeStreamResponseTooLarge, "模型流式文本超过限制")
		}
		_, _ = a.text.WriteString(text)
		a.sawIncrement = true
	}
	for _, delta := range choice.Delta.ToolCalls {
		if err := a.applyToolDelta(delta); err != nil {
			return "", err
		}
		a.sawIncrement = true
	}
	if choice.FinishReason != nil {
		reason := strings.TrimSpace(*choice.FinishReason)
		if !validFinishReason(reason) {
			return "", clientError(ErrorCodeStreamProtocol, "模型 SSE finish_reason 无效")
		}
		a.finishReason = reason
		a.finishSeen = true
	}
	return text, nil
}

func validateHiddenReasoning(delta streamDelta) (bool, error) {
	total := 0
	hasValue := false
	for _, value := range []json.RawMessage{delta.ReasoningContent, delta.Reasoning, delta.ReasoningDetails, delta.Thinking} {
		trimmed := bytes.TrimSpace(value)
		if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
			continue
		}
		hasValue = true
		total += len(trimmed)
		if total > maxStreamHiddenReasoning {
			return false, clientError(ErrorCodeStreamResponseTooLarge, "模型隐藏推理增量超过限制")
		}
	}
	return hasValue, nil
}

func (a *streamAssembler) applyToolDelta(delta streamToolCallDelta) error {
	if delta.Index == nil || *delta.Index < 0 || *delta.Index >= maxStreamToolCalls {
		return clientError(ErrorCodeStreamProtocol, "模型工具调用 index 无效")
	}
	if delta.ID == "" && delta.Type == "" && delta.Function.Name == "" && delta.Function.Arguments == "" {
		return clientError(ErrorCodeStreamProtocol, "模型工具调用增量为空")
	}
	call := a.tools[*delta.Index]
	if call == nil {
		if len(a.tools) >= maxStreamToolCalls {
			return clientError(ErrorCodeStreamResponseTooLarge, "模型工具调用数量超过限制")
		}
		call = &assembledToolCall{index: *delta.Index}
		a.tools[*delta.Index] = call
	}
	if !utf8.ValidString(delta.ID) || call.id.Len()+len(delta.ID) > maxStreamToolIDBytes {
		return clientError(ErrorCodeStreamResponseTooLarge, "模型工具调用 ID 超过限制")
	}
	if !utf8.ValidString(delta.Type) || call.typeName.Len()+len(delta.Type) > 32 {
		return clientError(ErrorCodeStreamProtocol, "模型工具调用类型无效")
	}
	if !utf8.ValidString(delta.Function.Name) || call.name.Len()+len(delta.Function.Name) > maxStreamToolNameBytes {
		return clientError(ErrorCodeStreamResponseTooLarge, "模型工具名称超过限制")
	}
	if !utf8.ValidString(delta.Function.Arguments) || len(delta.Function.Arguments) > maxStreamArgumentDelta || call.arguments.Len()+len(delta.Function.Arguments) > maxStreamToolArgumentBytes {
		return clientError(ErrorCodeStreamResponseTooLarge, "模型工具参数超过限制")
	}
	_, _ = call.id.WriteString(delta.ID)
	_, _ = call.typeName.WriteString(delta.Type)
	_, _ = call.name.WriteString(delta.Function.Name)
	_, _ = call.arguments.WriteString(delta.Function.Arguments)
	return nil
}

func (a *streamAssembler) response() (Response, error) {
	if !a.finishSeen {
		return Response{}, clientError(ErrorCodeStreamProtocol, "模型流式响应缺少 finish_reason")
	}
	if err := finishReasonError(a.finishReason, true); err != nil {
		return Response{}, err
	}
	indices := make([]int, 0, len(a.tools))
	for index := range a.tools {
		indices = append(indices, index)
	}
	sort.Ints(indices)
	calls := make([]ToolCall, 0, len(indices))
	ids := make(map[string]struct{}, len(indices))
	for expected, index := range indices {
		if index != expected {
			return Response{}, clientError(ErrorCodeStreamProtocol, "模型工具调用 index 不连续")
		}
		assembled := a.tools[index]
		id := assembled.id.String()
		typeName := assembled.typeName.String()
		name := assembled.name.String()
		arguments := assembled.arguments.String()
		if id == "" || typeName != "function" || name == "" || !validJSONObject(arguments) {
			return Response{}, clientError(ErrorCodeStreamProtocol, "模型工具调用格式无效")
		}
		if _, exists := ids[id]; exists {
			return Response{}, clientError(ErrorCodeStreamProtocol, "模型工具调用 ID 重复")
		}
		ids[id] = struct{}{}
		calls = append(calls, ToolCall{ID: id, Type: typeName, Function: ToolFunction{Name: name, Arguments: arguments}})
	}
	if len(calls) > 0 && a.finishReason != "tool_calls" || len(calls) == 0 && a.finishReason == "tool_calls" {
		return Response{}, clientError(ErrorCodeStreamProtocol, "模型 finish_reason 与工具调用不一致")
	}
	if a.text.Len() == 0 && len(calls) == 0 {
		return Response{}, clientError(ErrorCodeStreamProtocol, "模型响应不包含文本或工具调用")
	}
	return Response{
		Message:      Message{Role: "assistant", Content: a.text.String(), ToolCalls: calls},
		FinishReason: a.finishReason,
		Usage:        a.usage,
	}, nil
}

func decodeProviderErrorFrame(data []byte) (providerError, bool) {
	if len(data) == 0 || !utf8.Valid(data) {
		return providerError{}, false
	}
	var envelope errorResponse
	if json.Unmarshal(data, &envelope) == nil && hasProviderError(envelope.Error) {
		return envelope.Error, true
	}
	var direct providerError
	if json.Unmarshal(data, &direct) == nil && hasProviderError(direct) {
		return direct, true
	}
	return providerError{}, false
}

func hasProviderError(value providerError) bool {
	return value.Code != "" || value.Type != "" || value.Param != "" || value.Message != ""
}

func validUsage(usage Usage) bool {
	if usage.PromptTokens < 0 || usage.CompletionTokens < 0 || usage.TotalTokens < 0 {
		return false
	}
	var nested *int
	if usage.PromptTokensDetails != nil {
		nested = usage.PromptTokensDetails.CachedTokens
	}
	if nested != nil && (*nested < 0 || *nested > usage.PromptTokens) {
		return false
	}
	if usage.PromptCacheHitTokens != nil && (*usage.PromptCacheHitTokens < 0 || *usage.PromptCacheHitTokens > usage.PromptTokens) {
		return false
	}
	if nested != nil && usage.PromptCacheHitTokens != nil && *nested != *usage.PromptCacheHitTokens {
		return false
	}
	sum := usage.PromptTokens + usage.CompletionTokens
	return usage.TotalTokens == 0 || usage.TotalTokens >= sum
}

func validFinishReason(value string) bool {
	switch value {
	case "stop", "tool_calls", "length", "content_filter":
		return true
	default:
		return false
	}
}

func validJSONObject(value string) bool {
	if value == "" || !utf8.ValidString(value) {
		return false
	}
	var object map[string]json.RawMessage
	decoder := json.NewDecoder(strings.NewReader(value))
	if err := decoder.Decode(&object); err != nil || object == nil {
		return false
	}
	var trailing any
	return errors.Is(decoder.Decode(&trailing), io.EOF)
}
