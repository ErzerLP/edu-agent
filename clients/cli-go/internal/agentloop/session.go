package agentloop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/api"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/modelclient"
)

const (
	maxToolOutputBytes        = 8 << 10
	maxUserInputBytes         = 8 << 10
	maxUserInputRunes         = 8000
	maxAssistantTextBytes     = 64 << 10
	maxToolCallsPerResponse   = 4
	maxToolCallsPerTurn       = 16
	maxToolCallArgumentsBytes = 8 << 10
	maxToolCallArgumentsTotal = 16 << 10
)

type Session struct {
	model              Model
	server             Server
	options            Options
	messages           []modelclient.Message
	remaining          int
	toolCallsRemaining int
	pendingCalls       []modelclient.ToolCall
	pendingIndex       int
	pendingArgs        preferenceArgs
	pendingEvents      []Event
	pendingOperationID string
}

type preferenceArgs struct {
	Content     string `json:"content"`
	Reason      string `json:"reason"`
	Category    string `json:"category"`
	Sensitivity string `json:"sensitivity"`
	Stability   string `json:"stability"`
}

func New(model Model, server Server, options Options) (*Session, error) {
	if model == nil || server == nil || options.NewUUID == nil {
		return nil, errors.New("agent loop dependencies are incomplete")
	}
	if options.ContextWindow < 4096 || options.MaxToolRounds < 1 || options.MaxToolRounds > 16 {
		return nil, errors.New("agent loop limits are invalid")
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	return &Session{
		model: model, server: server, options: options,
		messages: []modelclient.Message{{Role: "system", Content: systemPrompt}},
	}, nil
}

func (s *Session) Send(ctx context.Context, input string) (Result, error) {
	if len(s.pendingCalls) != 0 {
		return Result{}, errors.New("请先确认或拒绝待保存的长期偏好")
	}
	input = strings.TrimSpace(input)
	if input == "" {
		return Result{}, errors.New("请输入学习问题")
	}
	if len(input) > maxUserInputBytes || utf8.RuneCountInString(input) > maxUserInputRunes || containsUnsafeControl(input) {
		return Result{}, errors.New("输入内容过长或包含不支持的控制字符")
	}
	s.messages = append(s.messages, modelclient.Message{Role: "user", Content: input})
	s.remaining = s.options.MaxToolRounds
	s.toolCallsRemaining = maxToolCallsPerTurn
	return s.run(ctx, nil)
}

func (s *Session) ResolvePreference(ctx context.Context, approved bool) (Result, error) {
	if len(s.pendingCalls) == 0 || s.pendingIndex >= len(s.pendingCalls) {
		return Result{}, errors.New("当前没有待确认的长期偏好")
	}
	if !approved && s.pendingOperationID != "" {
		return Result{}, ErrPreferenceOutcomeUnknown
	}
	events := append([]Event(nil), s.pendingEvents...)
	call := s.pendingCalls[s.pendingIndex]
	var output any
	if approved {
		if s.pendingOperationID == "" {
			operationID, err := s.options.NewUUID()
			if err != nil {
				return Result{}, errors.New("无法生成偏好操作ID")
			}
			s.pendingOperationID = operationID
		}
		created, err := s.createPreference(ctx, s.pendingArgs, s.pendingOperationID)
		if err != nil {
			return Result{}, err
		}
		output = created
		events = append(events, Event{Tool: call.Function.Name, Summary: "长期偏好候选已提交到服务端"})
	} else {
		output = map[string]any{"saved": false, "reason": "user_declined"}
		events = append(events, Event{Tool: call.Function.Name, Summary: "用户取消了长期偏好保存"})
	}
	s.appendToolResult(call.ID, output)
	calls, index := s.pendingCalls, s.pendingIndex+1
	s.pendingCalls, s.pendingEvents = nil, nil
	s.pendingIndex, s.pendingArgs, s.pendingOperationID = 0, preferenceArgs{}, ""
	result, err := s.processCalls(ctx, calls, index, events)
	if err == nil {
		return result, nil
	}
	if approved {
		return Result{Events: events, Text: "长期偏好候选已提交；Agent后续回答暂时失败，你可以继续提问。"}, nil
	}
	return Result{Events: events, Text: "已取消长期偏好保存；Agent后续回答暂时失败，你可以继续提问。"}, nil
}

func (s *Session) run(ctx context.Context, events []Event) (Result, error) {
	for s.remaining > 0 {
		s.remaining--
		messages, err := s.contextMessages()
		if err != nil {
			return Result{}, err
		}
		response, err := s.model.Complete(ctx, modelclient.Request{Messages: messages, Tools: Tools()})
		if err != nil {
			return Result{}, err
		}
		if err := validateModelMessage(response.Message); err != nil {
			return Result{}, err
		}
		if len(response.Message.ToolCalls) > s.toolCallsRemaining {
			return Result{}, errors.New("模型请求的工具调用总数超过安全上限")
		}
		s.toolCallsRemaining -= len(response.Message.ToolCalls)
		s.messages = append(s.messages, response.Message)
		if len(response.Message.ToolCalls) == 0 {
			text := strings.TrimSpace(response.Message.Content)
			if text == "" {
				return Result{}, errors.New("模型没有返回可显示的回答")
			}
			return Result{Text: text, Events: events}, nil
		}
		return s.processCalls(ctx, response.Message.ToolCalls, 0, events)
	}
	return Result{}, errors.New("Agent工具轮数已达到上限，请缩小问题范围后重试")
}

func validateModelMessage(message modelclient.Message) error {
	if len(message.Content) > maxAssistantTextBytes {
		return errors.New("模型回答超过客户端安全上限")
	}
	if len(message.ToolCalls) > maxToolCallsPerResponse {
		return errors.New("模型单轮请求的工具调用数超过安全上限")
	}
	totalArguments := 0
	for _, call := range message.ToolCalls {
		if len(call.Function.Arguments) > maxToolCallArgumentsBytes {
			return errors.New("模型工具参数超过客户端安全上限")
		}
		totalArguments += len(call.Function.Arguments)
	}
	if totalArguments > maxToolCallArgumentsTotal {
		return errors.New("模型单轮工具参数总量超过安全上限")
	}
	return nil
}

func containsUnsafeControl(value string) bool {
	for _, current := range value {
		if current == '\n' || current == '\t' {
			continue
		}
		if unicode.IsControl(current) {
			return true
		}
	}
	return false
}

func (s *Session) processCalls(ctx context.Context, calls []modelclient.ToolCall, start int, events []Event) (Result, error) {
	for index := start; index < len(calls); index++ {
		call := calls[index]
		if call.Function.Name == "remember_preference" {
			args, err := decodePreferenceArgs(call.Function.Arguments)
			if err != nil {
				s.appendToolResult(call.ID, map[string]any{"error": err.Error()})
				events = append(events, Event{Tool: call.Function.Name, Summary: "偏好候选参数无效"})
				continue
			}
			s.pendingCalls, s.pendingIndex, s.pendingArgs = calls, index, args
			s.pendingEvents = append([]Event(nil), events...)
			return Result{Events: events, Pending: &PreferenceConfirmation{
				Content: args.Content, Reason: args.Reason, Category: args.Category,
				Sensitivity: args.Sensitivity, Stability: args.Stability,
			}}, nil
		}
		output, summary := s.executeReadTool(ctx, call)
		s.appendToolResult(call.ID, output)
		events = append(events, Event{Tool: call.Function.Name, Summary: summary})
	}
	return s.run(ctx, events)
}

func (s *Session) executeReadTool(ctx context.Context, call modelclient.ToolCall) (any, string) {
	switch call.Function.Name {
	case "search_knowledge":
		var args struct {
			Query string `json:"query"`
		}
		if err := decodeArguments(call.Function.Arguments, &args); err != nil || strings.TrimSpace(args.Query) == "" {
			return toolError("query is required"), "知识检索参数无效"
		}
		result, err := s.server.RetrieveKnowledge(ctx, api.KnowledgeRetrievalRequest{
			Query: strings.TrimSpace(args.Query), QueryContextSchemaVersion: api.ProposalContextSchemaVersion,
			Context: map[string]any{"surface": "client_agent"},
			Limits:  &api.KnowledgeQueryLimits{MaxDepth: 4, CandidatesPerLayer: 8, MaxHits: 8, TotalCandidates: 32},
		})
		if err != nil {
			return toolError("knowledge unavailable"), "知识库检索失败"
		}
		return boundedValue(result), fmt.Sprintf("检索到 %d 条知识片段", len(result.Hits))
	case "get_learning_progress":
		if err := requireEmptyArguments(call.Function.Arguments); err != nil {
			return toolError(err.Error()), "学习进度参数无效"
		}
		result, err := s.server.CurrentSession(ctx)
		if err != nil {
			return toolError("current session unavailable"), "当前学习进度不可用"
		}
		return boundedValue(result), "已读取当前学习状态"
	case "get_learning_route":
		if err := requireEmptyArguments(call.Function.Arguments); err != nil {
			return toolError(err.Error()), "学习路线参数无效"
		}
		result, err := s.server.Routes(ctx, "", 10, true)
		if err != nil {
			return toolError("learning route unavailable"), "学习路线不可用"
		}
		return boundedValue(result), fmt.Sprintf("已读取 %d 条当前路线", len(result.Items))
	case "get_due_reviews":
		if err := requireEmptyArguments(call.Function.Arguments); err != nil {
			return toolError(err.Error()), "复习任务参数无效"
		}
		due := s.options.Now().UTC()
		result, err := s.server.Reviews(ctx, "", 20, &due)
		if err != nil {
			return toolError("reviews unavailable"), "复习任务不可用"
		}
		return boundedValue(result), fmt.Sprintf("已读取 %d 项到期复习", len(result.Items))
	case "list_long_term_preferences":
		if err := requireEmptyArguments(call.Function.Arguments); err != nil {
			return toolError(err.Error()), "长期偏好参数无效"
		}
		result, err := s.server.MemoryCandidates(ctx, "", 100)
		if err != nil {
			return toolError("preferences unavailable"), "长期偏好不可用"
		}
		items := make([]map[string]any, 0)
		for _, item := range result.Items {
			candidate := item.Candidate
			if candidate.Status != "admitted" || item.ProposedContent == "" || !preferenceCategory(candidate.Category) {
				continue
			}
			items = append(items, map[string]any{
				"content": item.ProposedContent, "category": candidate.Category,
				"stability": candidate.Stability, "valid_until": candidate.ValidUntil,
			})
		}
		return boundedValue(map[string]any{"preferences": items}), fmt.Sprintf("已读取 %d 条长期偏好", len(items))
	default:
		return toolError("unknown tool"), "模型请求了未知工具"
	}
}

func (s *Session) createPreference(ctx context.Context, args preferenceArgs, operationID string) (any, error) {
	validUntil := s.options.Now().UTC().Add(90 * 24 * time.Hour)
	if args.Stability == "stable" {
		validUntil = s.options.Now().UTC().AddDate(10, 0, 0)
	}
	result, err := s.server.CreateMemoryCandidate(ctx, api.MemoryCandidateRequest{
		OperationID: operationID, PayloadSchemaVersion: 1, Content: args.Content, Reason: args.Reason,
		Category: args.Category, Sensitivity: args.Sensitivity, Stability: args.Stability, ValidUntil: validUntil,
	})
	if err != nil {
		return nil, fmt.Errorf("%w，请使用相同操作ID重试核对", ErrPreferenceOutcomeUnknown)
	}
	if result.Candidate == nil {
		return nil, fmt.Errorf("%w，服务端成功响应缺少候选结果", ErrPreferenceOutcomeUnknown)
	}
	return map[string]any{
		"saved": true, "candidate_id": result.Candidate.Candidate.ID,
		"status": result.Candidate.Candidate.Status,
	}, nil
}

func (s *Session) appendToolResult(callID string, value any) {
	data, err := json.Marshal(boundedValue(value))
	if err != nil {
		data = []byte(`{"error":"tool result encoding failed"}`)
	}
	s.messages = append(s.messages, modelclient.Message{Role: "tool", ToolCallID: callID, Content: string(data)})
}

func (s *Session) contextMessages() ([]modelclient.Message, error) {
	maxBytes := s.options.ContextWindow * 3
	if maxBytes < 12<<10 {
		maxBytes = 12 << 10
	}
	if messagesSize(s.messages) <= maxBytes {
		return append([]modelclient.Message(nil), s.messages...), nil
	}
	groups := messageGroups(s.messages[1:])
	selected := make([][]modelclient.Message, 0, len(groups))
	total := messagesSize(s.messages[:1])
	for index := len(groups) - 1; index >= 0; index-- {
		size := messagesSize(groups[index])
		if total+size > maxBytes {
			if len(selected) == 0 {
				return nil, errors.New("当前对话轮次超过上下文上限，请缩短输入或减少工具结果后重试")
			}
			break
		}
		selected = append(selected, groups[index])
		total += size
	}
	result := append([]modelclient.Message(nil), s.messages[0])
	for index := len(selected) - 1; index >= 0; index-- {
		result = append(result, selected[index]...)
	}
	return result, nil
}

func messageGroups(messages []modelclient.Message) [][]modelclient.Message {
	groups := make([][]modelclient.Message, 0)
	for _, message := range messages {
		if message.Role == "user" || len(groups) == 0 {
			groups = append(groups, []modelclient.Message{message})
		} else {
			groups[len(groups)-1] = append(groups[len(groups)-1], message)
		}
	}
	return groups
}

func messagesSize(messages []modelclient.Message) int {
	total := 0
	for _, message := range messages {
		total += len(message.Content) + len(message.Role) + len(message.ToolCallID)
		for _, call := range message.ToolCalls {
			total += len(call.ID) + len(call.Function.Name) + len(call.Function.Arguments)
		}
	}
	return total
}

func decodePreferenceArgs(raw string) (preferenceArgs, error) {
	var args preferenceArgs
	if err := decodeArguments(raw, &args); err != nil {
		return args, err
	}
	args.Content, args.Reason = strings.TrimSpace(args.Content), strings.TrimSpace(args.Reason)
	args.Category, args.Sensitivity = strings.TrimSpace(args.Category), strings.TrimSpace(args.Sensitivity)
	args.Stability = strings.TrimSpace(args.Stability)
	if args.Content == "" || utf8.RuneCountInString(args.Content) > 2000 || args.Reason == "" || utf8.RuneCountInString(args.Reason) > 500 || containsUnsafeControl(args.Content) || containsUnsafeControl(args.Reason) {
		return args, errors.New("content or reason is invalid")
	}
	if !preferenceCategory(args.Category) || args.Sensitivity != "non_sensitive" && args.Sensitivity != "sensitive" || args.Stability != "stable" && args.Stability != "transient" {
		return args, errors.New("preference classification is invalid")
	}
	return args, nil
}

func preferenceCategory(value string) bool {
	return value == "interaction_preference" || value == "time_constraint" || value == "personal_context"
}

func decodeArguments(raw string, target any) error {
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if decoder.More() {
		return errors.New("multiple argument values")
	}
	return nil
}

func requireEmptyArguments(raw string) error {
	var args map[string]json.RawMessage
	if err := decodeArguments(raw, &args); err != nil {
		return err
	}
	if len(args) != 0 {
		return errors.New("tool accepts no arguments")
	}
	return nil
}

func boundedValue(value any) any {
	data, err := json.Marshal(value)
	if err != nil {
		return toolError("result encoding failed")
	}
	if len(data) <= maxToolOutputBytes {
		return value
	}
	return map[string]any{
		"truncated": true,
		"content":   truncateUTF8(string(data), maxToolOutputBytes-128),
	}
}

func truncateUTF8(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	value = value[:limit]
	for !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value
}

func toolError(message string) map[string]any { return map[string]any{"error": message} }

const systemPrompt = `你是 edu-agent 客户端中的中文学习助手。你本身没有持久状态，服务端知识库、学习进度和长期偏好才是权威事实。

工作规则：
1. 回答学习问题前，按需使用知识库、当前学习状态、路线、复习任务和长期偏好工具，不要臆造服务端内容。
2. 以中文为主，表达清楚、具体、可执行。尊重用户已经保存的交互偏好。
3. 只有用户明确表达希望长期保留的偏好、时间约束或个人学习背景时，才调用 remember_preference。
4. remember_preference 会触发本地用户确认；未获得确认前，不得声称偏好已经保存。
5. 不得请求、显示或保存 API Key、设备令牌、服务密钥等秘密。不要把普通聊天原文当作长期偏好。
6. 工具失败时如实说明，并继续提供不依赖该工具的有限帮助。`
