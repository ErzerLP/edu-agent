package agentloop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
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
	model                      Model
	server                     Server
	options                    Options
	appendMu                   sync.Mutex
	messages                   []modelclient.Message
	messageTurnIDs             []string
	turns                      map[string]*sessionTurn
	turnOrder                  []string
	currentTurnID              string
	turnSequence               int
	hotRawTokenLimit           int
	contextRuntime             *ContextRuntime
	estimator                  *ConservativeTokenEstimator
	toolHistory                map[string]string
	toolReferences             map[string]*ServerReference
	currentToolResultTokens    int
	currentToolResultBudget    int
	remaining                  int
	toolCallsRemaining         int
	pendingCalls               []modelclient.ToolCall
	pendingIndex               int
	pendingArgs                preferenceArgs
	pendingEvents              []Event
	pendingOperationID         string
	pendingDecisionOperationID string
	activitySequence           int
}

type sessionTurn struct {
	ID             string
	Completed      bool
	Protected      bool
	OutcomeUnknown bool
	SourceIDs      []string
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
	if options.ContextCompaction == "" {
		options.ContextCompaction = ContextCompactionAuto
	}
	if options.ContextCompaction != ContextCompactionAuto && options.ContextCompaction != ContextCompactionRecentOnly && options.ContextCompaction != ContextCompactionOff {
		return nil, errors.New("agent context compaction mode is invalid")
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	estimator := NewTokenEstimator()
	session := &Session{
		model: model, server: server, options: options,
		messages:                []modelclient.Message{{Role: "system", Content: systemPrompt}},
		messageTurnIDs:          []string{""},
		turns:                   make(map[string]*sessionTurn),
		turnOrder:               []string{},
		hotRawTokenLimit:        clampInt(divideRoundUp(options.ContextWindow*55, 100), 1024, options.ContextWindow),
		estimator:               estimator,
		toolHistory:             make(map[string]string),
		toolReferences:          make(map[string]*ServerReference),
		currentToolResultBudget: clampInt(divideRoundUp(options.ContextWindow*8, 100), 512, 2048),
	}
	session.contextRuntime = newContextRuntime(model, options, estimator)
	return session, nil
}

func (s *Session) startTurn() (string, error) {
	s.appendMu.Lock()
	defer s.appendMu.Unlock()
	if s.contextRuntime.isClosed() {
		return "", ErrSessionClosed
	}
	s.turnSequence++
	turnID := fmt.Sprintf("turn-%d", s.turnSequence)
	s.turns[turnID] = &sessionTurn{ID: turnID}
	s.turnOrder = append(s.turnOrder, turnID)
	s.currentTurnID = turnID
	return turnID, nil
}

func (s *Session) discardTurn(turnID string) {
	s.appendMu.Lock()
	defer s.appendMu.Unlock()
	delete(s.turns, turnID)
	filtered := s.turnOrder[:0]
	for _, current := range s.turnOrder {
		if current != turnID {
			filtered = append(filtered, current)
		}
	}
	s.turnOrder = filtered
	if s.currentTurnID == turnID {
		s.currentTurnID = ""
	}
}

func (s *Session) appendCapturedMessage(turnID string, message modelclient.Message, recall string, kind SourceKind, authority AuthorityClass, freshness FreshnessClass, reference *ServerReference) error {
	s.appendMu.Lock()
	defer s.appendMu.Unlock()
	if s.contextRuntime.isClosed() {
		return ErrSessionClosed
	}
	sourceID, err := s.contextRuntime.appendSource(sourceDraft{
		TurnID: turnID, Kind: kind, CreatedAt: s.options.Now().UTC(), ModelMessage: message,
		RecallText: recall, Authority: authority, Freshness: freshness, ServerReference: reference,
	})
	if err != nil {
		return err
	}
	s.messages = append(s.messages, cloneModelMessage(message))
	s.messageTurnIDs = append(s.messageTurnIDs, turnID)
	if sourceID != "" {
		if turn, exists := s.turns[turnID]; exists {
			turn.SourceIDs = append(turn.SourceIDs, sourceID)
		}
	}
	return nil
}

func (s *Session) appendTurnMessage(turnID string, message modelclient.Message) error {
	s.appendMu.Lock()
	defer s.appendMu.Unlock()
	if s.contextRuntime.isClosed() {
		return ErrSessionClosed
	}
	s.messages = append(s.messages, cloneModelMessage(message))
	s.messageTurnIDs = append(s.messageTurnIDs, turnID)
	return nil
}

func (s *Session) markPreferencePending() {
	s.appendMu.Lock()
	if turn, exists := s.turns[s.currentTurnID]; exists {
		turn.Protected = true
	}
	s.appendMu.Unlock()
	s.contextRuntime.setPreferencePending(true)
}

func (s *Session) markPreferenceOutcomeUnknown() {
	s.appendMu.Lock()
	if turn, exists := s.turns[s.currentTurnID]; exists {
		turn.Protected = true
		turn.OutcomeUnknown = true
	}
	s.appendMu.Unlock()
	s.contextRuntime.setPreferencePending(true)
}

func (s *Session) finishSuccessfulTurn() {
	s.appendMu.Lock()
	if turn, exists := s.turns[s.currentTurnID]; exists {
		turn.Completed = true
		if !turn.OutcomeUnknown {
			turn.Protected = false
		}
	}
	s.appendMu.Unlock()
	s.contextRuntime.setPreferencePending(false)
	s.trimRawHistory()
	s.contextRuntime.triggerConsolidation()
}

func (s *Session) trimRawHistory() {
	if s.options.ContextCompaction == ContextCompactionOff {
		return
	}
	s.appendMu.Lock()
	if len(s.messages) != len(s.messageTurnIDs) || len(s.turnOrder) == 0 {
		s.appendMu.Unlock()
		return
	}
	turnMessages := make(map[string][]modelclient.Message, len(s.turnOrder))
	for index, turnID := range s.messageTurnIDs {
		if turnID != "" {
			turnMessages[turnID] = append(turnMessages[turnID], s.messages[index])
		}
	}
	keep := make(map[string]struct{}, len(s.turnOrder))
	for _, turnID := range s.turnOrder {
		turn := s.turns[turnID]
		if turn == nil || !turn.Completed || turn.Protected || turn.OutcomeUnknown {
			keep[turnID] = struct{}{}
		}
	}
	keptCompleted := 0
	for index := len(s.turnOrder) - 1; index >= 0 && keptCompleted < 2; index-- {
		turnID := s.turnOrder[index]
		if turn := s.turns[turnID]; turn != nil && turn.Completed {
			keep[turnID] = struct{}{}
			keptCompleted++
		}
	}
	tokens := 0
	planner := ContextPlanner{Estimator: s.estimator}
	for turnID := range keep {
		tokens += planner.estimateAdditional(turnMessages[turnID])
	}
	for index := len(s.turnOrder) - 1; index >= 0; index-- {
		turnID := s.turnOrder[index]
		if _, exists := keep[turnID]; exists {
			continue
		}
		size := planner.estimateAdditional(turnMessages[turnID])
		if tokens+size <= s.hotRawTokenLimit {
			keep[turnID] = struct{}{}
			tokens += size
			continue
		}
		if s.options.ContextCompaction == ContextCompactionAuto && !s.contextRuntime.turnCovered(turnID) {
			keep[turnID] = struct{}{}
			tokens += size
		}
	}
	dropped := make([]string, 0)
	retainedCompleted := 0
	for _, turnID := range s.turnOrder {
		if _, exists := keep[turnID]; !exists {
			dropped = append(dropped, turnID)
			continue
		}
		if turn := s.turns[turnID]; turn != nil && turn.Completed {
			retainedCompleted++
		}
	}
	if len(dropped) == 0 {
		s.appendMu.Unlock()
		return
	}
	droppedSet := make(map[string]struct{}, len(dropped))
	for _, turnID := range dropped {
		droppedSet[turnID] = struct{}{}
		delete(s.turns, turnID)
	}
	messages := make([]modelclient.Message, 0, len(s.messages))
	turnIDs := make([]string, 0, len(s.messageTurnIDs))
	for index, message := range s.messages {
		turnID := s.messageTurnIDs[index]
		if _, remove := droppedSet[turnID]; remove {
			if message.Role == "tool" {
				delete(s.toolHistory, message.ToolCallID)
				delete(s.toolReferences, message.ToolCallID)
			}
			continue
		}
		messages = append(messages, message)
		turnIDs = append(turnIDs, turnID)
	}
	order := make([]string, 0, len(s.turnOrder)-len(dropped))
	for _, turnID := range s.turnOrder {
		if _, remove := droppedSet[turnID]; !remove {
			order = append(order, turnID)
		}
	}
	s.messages = messages
	s.messageTurnIDs = turnIDs
	s.turnOrder = order
	s.appendMu.Unlock()
	s.contextRuntime.markTurnsWarm(dropped)
	s.contextRuntime.PublishCompacted(len(dropped), retainedCompleted)
}

func (s *Session) ContextStatus() ContextStatus { return s.contextRuntime.ContextStatus() }

func (s *Session) ContextUpdates() <-chan ContextEvent { return s.contextRuntime.ContextUpdates() }

// LearningStatus reads the current authoritative server projection for presentation.
// A missing current session is a valid inactive state, not an error.
func (s *Session) LearningStatus(ctx context.Context) (LearningStatus, error) {
	if s.contextRuntime.isClosed() {
		return LearningStatus{}, ErrSessionClosed
	}
	view, err := s.server.CurrentSession(ctx)
	if err != nil {
		var apiErr *api.APIError
		if isAPINotFound(err) || errors.As(err, &apiErr) && apiErr.Status == 404 {
			return LearningStatus{Active: false}, nil
		}
		return LearningStatus{}, err
	}
	return LearningStatus{Active: true, View: view}, nil
}

// Close is idempotent. It cancels the background consolidator, waits up to a
// fixed boundary for cooperative providers, and clears all process-local context state.
func (s *Session) Close() {
	s.appendMu.Lock()
	s.contextRuntime.beginClose()
	s.appendMu.Unlock()
	s.contextRuntime.waitAndClear()
	s.appendMu.Lock()
	s.messages = nil
	s.messageTurnIDs = nil
	s.turns = make(map[string]*sessionTurn)
	s.turnOrder = nil
	s.currentTurnID = ""
	s.toolHistory = make(map[string]string)
	s.toolReferences = make(map[string]*ServerReference)
	s.pendingCalls = nil
	s.pendingEvents = nil
	s.appendMu.Unlock()
}

func (s *Session) Send(ctx context.Context, input string) (Result, error) {
	if s.contextRuntime.isClosed() {
		return Result{}, ErrSessionClosed
	}
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
	s.trimRawHistory()
	turnID, err := s.startTurn()
	if err != nil {
		return Result{}, err
	}
	userMessage := modelclient.Message{Role: "user", Content: input}
	if err := s.appendCapturedMessage(turnID, userMessage, input, SourceUser, AuthoritySessionStatement, FreshnessSessionCurrent, nil); err != nil {
		s.discardTurn(turnID)
		return Result{}, err
	}
	s.activitySequence = 0
	s.currentToolResultTokens = 0
	s.remaining = s.options.MaxToolRounds
	s.toolCallsRemaining = maxToolCallsPerTurn
	return s.run(ctx, nil)
}

func (s *Session) ResolvePreference(ctx context.Context, approved bool) (Result, error) {
	if s.contextRuntime.isClosed() {
		return Result{}, ErrSessionClosed
	}
	if len(s.pendingCalls) == 0 || s.pendingIndex >= len(s.pendingCalls) {
		return Result{}, errors.New("当前没有待确认的长期偏好")
	}
	if !approved && (s.pendingOperationID != "" || s.pendingDecisionOperationID != "") {
		return Result{}, ErrPreferenceOutcomeUnknown
	}
	events := append([]Event(nil), s.pendingEvents...)
	call := s.pendingCalls[s.pendingIndex]
	var output any
	var preferenceEvent Event
	if approved {
		PublishActivity(ctx, Activity{Kind: ActivityTool, Event: Event{
			ID: call.ID, Tool: call.Function.Name, Summary: "正在保存长期偏好", Status: EventRunning,
		}})
		if s.pendingOperationID == "" {
			createOperationID, err := s.options.NewUUID()
			if err != nil {
				PublishActivity(ctx, Activity{Kind: ActivityTool, Event: Event{
					ID: call.ID, Tool: call.Function.Name, Summary: "无法开始长期偏好保存", Status: EventFailed, Detail: "create_operation_id_failed",
				}})
				return Result{}, errors.New("无法生成偏好创建操作ID")
			}
			decisionOperationID, err := s.options.NewUUID()
			if err != nil || decisionOperationID == createOperationID {
				PublishActivity(ctx, Activity{Kind: ActivityTool, Event: Event{
					ID: call.ID, Tool: call.Function.Name, Summary: "无法开始长期偏好保存", Status: EventFailed, Detail: "decision_operation_id_failed",
				}})
				return Result{}, errors.New("无法生成独立的偏好确认操作ID")
			}
			s.pendingOperationID = createOperationID
			s.pendingDecisionOperationID = decisionOperationID
		}
		saved, err := s.createPreference(ctx, s.pendingArgs, s.pendingOperationID, s.pendingDecisionOperationID)
		if err != nil {
			status, detail := EventFailed, "request_failed"
			if errors.Is(err, ErrPreferenceOutcomeUnknown) {
				status, detail = EventOutcomeUnknown, "outcome_unknown"
			}
			PublishActivity(ctx, Activity{Kind: ActivityTool, Event: Event{
				ID: call.ID, Tool: call.Function.Name, Summary: "长期偏好未确认保存", Status: status, Detail: detail,
			}})
			if errors.Is(err, ErrPreferenceOutcomeUnknown) {
				s.markPreferenceOutcomeUnknown()
			} else {
				s.pendingOperationID = ""
				s.pendingDecisionOperationID = ""
			}
			return Result{}, err
		}
		output = saved
		preferenceEvent = Event{ID: call.ID, Tool: call.Function.Name, Summary: "长期偏好已保存", Status: EventSucceeded}
	} else {
		output = map[string]any{"submitted": false, "saved": false, "reason": "user_declined"}
		preferenceEvent = Event{ID: call.ID, Tool: call.Function.Name, Summary: "用户取消了长期偏好保存", Status: EventSucceeded}
	}
	PublishActivity(ctx, Activity{Kind: ActivityTool, Event: preferenceEvent})
	events = append(events, preferenceEvent)
	if err := s.appendToolResult(call.Function.Name, call.ID, output); err != nil {
		return Result{}, err
	}
	calls, index := s.pendingCalls, s.pendingIndex+1
	s.pendingCalls, s.pendingEvents = nil, nil
	s.pendingIndex, s.pendingArgs, s.pendingOperationID, s.pendingDecisionOperationID = 0, preferenceArgs{}, "", ""
	s.contextRuntime.setPreferencePending(false)
	result, err := s.processCalls(ctx, calls, index, events)
	if err == nil {
		return result, nil
	}
	text := "已取消长期偏好保存；Agent后续回答暂时失败，你可以继续提问。"
	if approved {
		text = "长期偏好已保存；Agent后续回答暂时失败，你可以继续提问。"
	}
	message := modelclient.Message{Role: "assistant", Content: text}
	if appendErr := s.appendCapturedMessage(s.currentTurnID, message, text, SourceAssistant, AuthoritySessionStatement, FreshnessSessionCurrent, nil); appendErr != nil {
		return Result{}, appendErr
	}
	s.finishSuccessfulTurn()
	return Result{Events: events, Text: text}, nil
}

func (s *Session) run(ctx context.Context, events []Event) (Result, error) {
	if s.remaining <= 0 {
		return Result{}, errors.New("Agent工具轮数已达到上限，请缩小问题范围后重试")
	}
	s.remaining--
	plan, err := s.contextPlan()
	if err != nil {
		return Result{}, err
	}
	thinkingID := s.nextThinkingActivityID()
	thinkingSummary := "正在分析问题"
	if len(events) > 0 {
		thinkingSummary = "正在结合工具结果继续分析"
	}
	PublishActivity(ctx, Activity{Kind: ActivityThinking, Event: Event{
		ID: thinkingID, Summary: thinkingSummary, Status: EventRunning,
	}})
	response, err := s.model.Complete(ctx, plan.Request)
	if err != nil {
		PublishActivity(ctx, Activity{Kind: ActivityThinking, Event: Event{
			ID: thinkingID, Summary: "模型响应失败", Status: EventFailed, Detail: "model_request_failed",
		}})
		return Result{}, err
	}
	if response.Usage != nil {
		s.estimator.ObserveActual(plan.EstimatedInput, *response.Usage)
	}
	if err := validateModelMessage(response.Message); err != nil {
		PublishActivity(ctx, Activity{Kind: ActivityThinking, Event: Event{
			ID: thinkingID, Summary: "模型响应不符合协议", Status: EventFailed, Detail: "invalid_model_response",
		}})
		return Result{}, err
	}
	if len(response.Message.ToolCalls) > s.toolCallsRemaining {
		PublishActivity(ctx, Activity{Kind: ActivityThinking, Event: Event{
			ID: thinkingID, Summary: "工具调用超过安全上限", Status: EventFailed, Detail: "tool_limit_exceeded",
		}})
		return Result{}, errors.New("模型请求的工具调用总数超过安全上限")
	}
	s.toolCallsRemaining -= len(response.Message.ToolCalls)
	if len(response.Message.ToolCalls) == 0 {
		text := strings.TrimSpace(response.Message.Content)
		if text == "" {
			PublishActivity(ctx, Activity{Kind: ActivityThinking, Event: Event{
				ID: thinkingID, Summary: "模型没有返回可显示的回答", Status: EventFailed, Detail: "empty_model_response",
			}})
			return Result{}, errors.New("模型没有返回可显示的回答")
		}
		PublishActivity(ctx, Activity{Kind: ActivityThinking, Event: Event{
			ID: thinkingID, Summary: "已完成回答组织", Status: EventSucceeded,
		}})
		if err := s.appendCapturedMessage(s.currentTurnID, response.Message, text, SourceAssistant, AuthoritySessionStatement, FreshnessSessionCurrent, nil); err != nil {
			return Result{}, err
		}
		s.finishSuccessfulTurn()
		return Result{Text: text, Events: events}, nil
	}
	PublishActivity(ctx, Activity{Kind: ActivityThinking, Event: Event{
		ID: thinkingID, Summary: "已确定下一步工具操作", Status: EventSucceeded,
	}})
	if err := s.appendTurnMessage(s.currentTurnID, response.Message); err != nil {
		return Result{}, err
	}
	return s.processCalls(ctx, response.Message.ToolCalls, 0, events)
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
		PublishActivity(ctx, Activity{Kind: ActivityTool, Event: Event{
			ID: call.ID, Tool: call.Function.Name, Summary: toolRunningSummary(call.Function.Name), Status: EventRunning,
		}})
		if call.Function.Name == "remember_preference" {
			args, err := decodePreferenceArgs(call.Function.Arguments)
			if err != nil {
				if appendErr := s.appendToolResult(call.Function.Name, call.ID, map[string]any{"error": err.Error()}); appendErr != nil {
					return Result{}, appendErr
				}
				event := Event{ID: call.ID, Tool: call.Function.Name, Summary: "偏好候选参数无效", Status: EventInvalid, Detail: "invalid_arguments"}
				PublishActivity(ctx, Activity{Kind: ActivityTool, Event: event})
				events = append(events, event)
				continue
			}
			s.pendingCalls, s.pendingIndex, s.pendingArgs = calls, index, args
			s.pendingEvents = append([]Event(nil), events...)
			s.markPreferencePending()
			pendingEvent := Event{ID: call.ID, Tool: call.Function.Name, Summary: "等待用户确认长期偏好候选", Status: EventConfirmationRequired}
			PublishActivity(ctx, Activity{Kind: ActivityTool, Event: pendingEvent})
			return Result{Events: append(events, pendingEvent), Pending: &PreferenceConfirmation{
				Content: args.Content, Reason: args.Reason, Category: args.Category,
				Sensitivity: args.Sensitivity, Stability: args.Stability,
			}}, nil
		}
		output, summary := s.executeReadTool(ctx, call)
		if err := s.appendToolResult(call.Function.Name, call.ID, output); err != nil {
			return Result{}, err
		}
		event := eventFromToolOutput(call.Function.Name, summary, output)
		event.ID = call.ID
		PublishActivity(ctx, Activity{Kind: ActivityTool, Event: event})
		events = append(events, event)
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
			return toolError("invalid_arguments"), "知识检索参数无效"
		}
		result, err := s.server.RetrieveKnowledge(ctx, api.KnowledgeRetrievalRequest{
			Query: strings.TrimSpace(args.Query), QueryContextSchemaVersion: api.QueryContextSchemaVersion,
			Context: map[string]any{"surface": "client_agent"},
			Limits:  &api.KnowledgeQueryLimits{MaxDepth: 4, CandidatesPerLayer: 8, MaxHits: 8, TotalCandidates: 32},
		})
		if err != nil {
			return toolFailure(err, "knowledge_unavailable"), "知识库检索失败"
		}
		hits := make([]map[string]any, 0, len(result.Hits))
		for _, hit := range result.Hits {
			hits = append(hits, map[string]any{
				"path":             hit.Path,
				"node_revision_id": hit.NodeRevisionID,
				"text":             hit.CanonicalSlice,
				"provenance":       hit.Provenance,
			})
		}
		return map[string]any{
			"knowledge_revision_id": result.KnowledgeRevisionID,
			"hits":                  hits,
			"degraded":              result.Degraded,
			"truncated":             result.Truncated,
		}, fmt.Sprintf("检索到 %d 条知识片段", len(hits))
	case "get_learning_progress":
		if err := requireEmptyArguments(call.Function.Arguments); err != nil {
			return toolError("invalid_arguments"), "学习进度参数无效"
		}
		result, err := s.server.CurrentSession(ctx)
		if err != nil {
			if isAPINotFound(err) {
				return map[string]any{"active": false, "reason": "no_current_session"}, "当前没有进行中的学习会话"
			}
			return toolFailure(err, "current_session_unavailable"), "当前学习进度不可用"
		}
		return map[string]any{"active": true, "session": result}, "已读取当前学习状态"
	case "get_learning_route":
		var args struct {
			Offset int `json:"offset"`
		}
		if err := decodeArguments(call.Function.Arguments, &args); err != nil || args.Offset < 0 {
			return toolError("invalid_arguments"), "学习路线参数无效"
		}
		view, err := s.server.CurrentSession(ctx)
		if err != nil {
			if isAPINotFound(err) {
				return map[string]any{"active": false, "reason": "no_current_session"}, "当前没有进行中的学习会话"
			}
			return toolFailure(err, "learning_route_unavailable"), "学习路线不可用"
		}
		if view.WorkItem == nil || view.WorkItem.RouteRevision == nil {
			return map[string]any{"active": false, "reason": "route_not_ready", "session_state": view.Session.State}, "当前学习会话尚未生成路线"
		}
		route := view.WorkItem.RouteRevision
		start := min(args.Offset, len(route.Steps))
		end := min(start+12, len(route.Steps))
		steps := make([]map[string]any, 0, end-start)
		for _, step := range route.Steps[start:end] {
			steps = append(steps, map[string]any{
				"ordinal":              step.Ordinal,
				"node_revision_id":     step.NodeRevisionID,
				"teaching_intent":      step.TeachingIntent,
				"completion_condition": step.CompletionCondition,
			})
		}
		value := map[string]any{
			"active":            true,
			"route_revision_id": route.RouteRevisionID,
			"goal_revision_id":  route.GoalRevisionID,
			"revision":          route.Revision,
			"generation":        view.Metadata.Generation,
			"offset":            start,
			"returned":          len(steps),
			"total_steps":       len(route.Steps),
			"steps":             steps,
			"has_more":          end < len(route.Steps),
		}
		if end < len(route.Steps) {
			value["next_offset"] = end
		}
		return value, fmt.Sprintf("已读取当前路线的 %d/%d 个步骤", len(steps), len(route.Steps))
	case "get_due_reviews":
		var args struct {
			Cursor string `json:"cursor"`
		}
		if err := decodeArguments(call.Function.Arguments, &args); err != nil || len(args.Cursor) > 4096 {
			return toolError("invalid_arguments"), "复习任务参数无效"
		}
		due := s.options.Now().UTC()
		result, err := s.server.Reviews(ctx, args.Cursor, 20, &due)
		if err != nil {
			return toolFailure(err, "reviews_unavailable"), "复习任务不可用"
		}
		value := map[string]any{
			"items":      result.Items,
			"returned":   len(result.Items),
			"due_before": due,
			"generation": result.Metadata.Generation,
			"has_more":   result.NextCursor != "",
		}
		if result.NextCursor != "" {
			value["next_cursor"] = result.NextCursor
		}
		return value, fmt.Sprintf("已读取 %d 项到期复习", len(result.Items))
	case "list_long_term_preferences":
		var args struct {
			Cursor string `json:"cursor"`
		}
		if err := decodeArguments(call.Function.Arguments, &args); err != nil || len(args.Cursor) > 4096 {
			return toolError("invalid_arguments"), "长期偏好参数无效"
		}
		result, err := s.server.ExportMemory(ctx, args.Cursor, 20)
		if err != nil {
			return toolFailure(err, "preferences_unavailable"), "长期偏好不可用"
		}
		degraded := result.Degraded
		reasonCodes := append([]string(nil), result.ReasonCodes...)
		privacyInvalidated := false
		for _, code := range reasonCodes {
			if code == "content_redacted" || code == "privacy_clear_in_progress" {
				privacyInvalidated = true
			}
		}
		now := s.options.Now().UTC()
		items := make([]map[string]any, 0, len(result.Items))
		for _, item := range result.Items {
			if item.ContentStatus == "redacted" {
				degraded = true
				privacyInvalidated = true
				reasonCodes = appendUnique(reasonCodes, "content_redacted")
			}
			if item.ContentStatus != "available" || strings.TrimSpace(item.Content) == "" {
				continue
			}
			candidate, candidateErr := s.server.MemoryCandidate(ctx, item.Record.CandidateID)
			if candidateErr != nil {
				degraded = true
				reasonCodes = appendUnique(reasonCodes, "candidate_metadata_unavailable")
				continue
			}
			if !preferenceCategory(candidate.Candidate.Category) || !candidate.Candidate.ValidUntil.After(now) {
				continue
			}
			items = append(items, map[string]any{
				"memory_id":   item.Record.LogicalMemoryID,
				"revision":    item.Record.Revision,
				"category":    candidate.Candidate.Category,
				"sensitivity": candidate.Candidate.Sensitivity,
				"stability":   candidate.Candidate.Stability,
				"valid_until": candidate.Candidate.ValidUntil,
				"content":     item.Content,
			})
		}
		value := map[string]any{
			"items":               items,
			"read_generation":     result.ReadGeneration,
			"degraded":            degraded,
			"privacy_invalidated": privacyInvalidated,
			"reason_codes":        reasonCodes,
			"has_more":            result.NextCursor != "",
		}
		if result.NextCursor != "" {
			value["next_cursor"] = result.NextCursor
		}
		summary := fmt.Sprintf("已读取 %d 条长期偏好", len(items))
		if degraded {
			summary += "（部分内容暂不可用）"
		}
		return value, summary
	case "recall_session_memory":
		var args struct {
			MemoryID string `json:"memory_id"`
		}
		if err := decodeArguments(call.Function.Arguments, &args); err != nil ||
			!validOpaqueID(args.MemoryID, "obs_") && !validOpaqueID(args.MemoryID, "ref_") {
			return toolError("invalid_arguments"), "会话证据回查参数无效"
		}
		value := s.contextRuntime.recallMemory(args.MemoryID)
		if toolResultCode(value) == ContextSourceUnavailable {
			s.contextRuntime.PublishSourceUnavailable()
		}
		return value, recallSummary(value)
	default:
		return toolError("unknown tool"), "模型请求了未知工具"
	}
}

func (s *Session) createPreference(ctx context.Context, args preferenceArgs, createOperationID, decisionOperationID string) (any, error) {
	validUntil := s.options.Now().UTC().Add(90 * 24 * time.Hour)
	if args.Stability == "stable" {
		validUntil = s.options.Now().UTC().AddDate(10, 0, 0)
	}
	result, err := s.server.CreateMemoryCandidate(ctx, api.MemoryCandidateRequest{
		OperationID: createOperationID, PayloadSchemaVersion: 1, Content: args.Content, Reason: args.Reason,
		Category: args.Category, Sensitivity: args.Sensitivity, Stability: args.Stability, ValidUntil: validUntil,
	})
	if err != nil {
		var apiErr *api.APIError
		if errors.As(err, &apiErr) && apiErr.Status >= 400 && apiErr.Status < 500 {
			return nil, fmt.Errorf("长期偏好未保存（%s）", apiErr.Code)
		}
		return nil, fmt.Errorf("%w，请使用相同操作ID重试核对", ErrPreferenceOutcomeUnknown)
	}
	if result.Candidate == nil {
		return nil, fmt.Errorf("%w，服务端成功响应缺少候选结果", ErrPreferenceOutcomeUnknown)
	}
	candidate := result.Candidate.Candidate
	if candidate.ID == "" {
		return nil, fmt.Errorf("%w，服务端成功响应缺少候选身份", ErrPreferenceOutcomeUnknown)
	}
	switch candidate.Status {
	case "admitted":
		return map[string]any{
			"submitted": true, "saved": true, "candidate_id": candidate.ID,
			"status": candidate.Status, "replayed": result.Replayed,
		}, nil
	case "pending_review":
		if candidate.Revision < 1 {
			return nil, fmt.Errorf("%w，待确认候选缺少有效身份或修订", ErrPreferenceOutcomeUnknown)
		}
	case "rejected", "expired":
		return nil, fmt.Errorf("长期偏好未保存（candidate_%s）", candidate.Status)
	default:
		return nil, fmt.Errorf("%w，服务端返回未知候选状态", ErrPreferenceOutcomeUnknown)
	}

	decisionResult, err := s.server.DecideMemoryCandidate(ctx, candidate.ID, api.MemoryCandidateDecisionRequest{
		OperationID: decisionOperationID, PayloadSchemaVersion: 1, ExpectedRevision: candidate.Revision,
		Decision: "admit", Reason: "user_confirmed_preference_save",
	})
	if err != nil {
		var apiErr *api.APIError
		if errors.As(err, &apiErr) && apiErr.Status >= 400 && apiErr.Status < 500 {
			return nil, fmt.Errorf("长期偏好未保存（%s）", apiErr.Code)
		}
		return nil, fmt.Errorf("%w，请使用相同操作ID重试核对", ErrPreferenceOutcomeUnknown)
	}
	if decisionResult.Candidate == nil || decisionResult.Candidate.Candidate.ID != candidate.ID ||
		decisionResult.Candidate.Candidate.Status != "admitted" || decisionResult.Candidate.Candidate.Revision <= candidate.Revision {
		return nil, fmt.Errorf("%w，服务端未确认长期偏好已接纳", ErrPreferenceOutcomeUnknown)
	}
	return map[string]any{
		"submitted": true, "saved": true, "candidate_id": decisionResult.Candidate.Candidate.ID,
		"status": decisionResult.Candidate.Candidate.Status, "replayed": result.Replayed || decisionResult.Replayed,
	}, nil
}

func (s *Session) appendToolResult(tool, callID string, value any) error {
	if tool == "recall_session_memory" {
		return s.appendUncapturedToolResult(tool, callID, value)
	}
	projection := projectToolResult(tool, value)
	allServerEvidence := toolResultCode(value) == "content_redacted"
	identityEvidence := toolResultInvalidatesIdentity(tool, value)
	s.appendMu.Lock()
	defer s.appendMu.Unlock()
	if s.contextRuntime.isClosed() {
		return ErrSessionClosed
	}
	sessionInvalidation := s.invalidatedSessionToolCallsLocked(projection.ServerReference, allServerEvidence, identityEvidence)
	runtimeInvalidation := s.contextRuntime.invalidateServerEvidenceForAppend(projection.ServerReference, allServerEvidence, identityEvidence)
	invalidation := mergeServerInvalidations(sessionInvalidation, runtimeInvalidation)
	if len(invalidation.ToolCallIDs) > 0 || len(invalidation.TurnIDs) > 0 {
		s.replaceInvalidatedToolResultsLocked(invalidation)
	}
	staleGeneration := s.incomingServerGenerationStaleLocked(projection.ServerReference)
	freshness := FreshnessHistorical
	recall := projection.Recall
	if staleGeneration {
		projection.Live = staleServerGenerationToolResultJSON
		projection.History = staleServerGenerationToolResultJSON
		recall = ""
		freshness = FreshnessInvalidated
	}
	live := projection.Live
	estimated := s.estimator.EstimateText(live) + 6
	if s.currentToolResultTokens+estimated > s.currentToolResultBudget {
		live = currentTurnBudgetProjection(tool, value)
		if staleGeneration {
			live = staleServerGenerationToolResultJSON
		}
		estimated = s.estimator.EstimateText(live) + 6
		projection.Live = live
	}
	s.currentToolResultTokens += estimated
	if s.currentToolResultTokens > s.currentToolResultBudget {
		s.currentToolResultTokens = s.currentToolResultBudget
	}
	message := modelclient.Message{Role: "tool", ToolCallID: callID, Content: live}
	sourceID, err := s.contextRuntime.appendSource(sourceDraft{
		TurnID: s.currentTurnID, Kind: SourceTool, CreatedAt: s.options.Now().UTC(), ModelMessage: message,
		RecallText: recall, Authority: AuthorityServerSnapshot, Freshness: freshness,
		ServerReference: projection.ServerReference,
	})
	if err != nil {
		return err
	}
	s.toolHistory[callID] = projection.History
	s.toolReferences[callID] = cloneServerReference(projection.ServerReference)
	s.messages = append(s.messages, message)
	s.messageTurnIDs = append(s.messageTurnIDs, s.currentTurnID)
	if sourceID != "" {
		if turn, exists := s.turns[s.currentTurnID]; exists {
			turn.SourceIDs = append(turn.SourceIDs, sourceID)
		}
	}
	return nil
}

func (s *Session) appendUncapturedToolResult(tool, callID string, value any) error {
	projection := projectToolResult(tool, value)
	s.appendMu.Lock()
	defer s.appendMu.Unlock()
	if s.contextRuntime.isClosed() {
		return ErrSessionClosed
	}
	live := projection.Live
	estimated := s.estimator.EstimateText(live) + 6
	if s.currentToolResultTokens+estimated > s.currentToolResultBudget {
		live = currentTurnBudgetProjection(tool, value)
		estimated = s.estimator.EstimateText(live) + 6
	}
	s.currentToolResultTokens = min(s.currentToolResultBudget, s.currentToolResultTokens+estimated)
	s.toolHistory[callID] = projection.History
	s.messages = append(s.messages, modelclient.Message{Role: "tool", ToolCallID: callID, Content: live})
	s.messageTurnIDs = append(s.messageTurnIDs, s.currentTurnID)
	return nil
}

const (
	invalidatedToolResultJSON           = `{"invalidated":true,"reason":"server_content_invalidated"}`
	staleServerGenerationToolResultJSON = `{"invalidated":true,"reason":"stale_server_generation"}`
	invalidatedAssistantText            = "早期服务端派生内容已失效，需要重新读取。"
)

func toolResultCode(value any) string {
	object := normalizedProjectionObject(value)
	if object == nil {
		return ""
	}
	code, _ := object["code"].(string)
	return code
}

func (s *Session) invalidatedSessionToolCallsLocked(reference *ServerReference, allServerEvidence, identityEvidence bool) serverInvalidation {
	identity := ""
	if reference != nil {
		identity = reference.Identity()
	}
	if !allServerEvidence && !identityEvidence && identity == "" {
		return serverInvalidation{}
	}
	calls := make(map[string]struct{})
	for callID, previous := range s.toolReferences {
		invalidate := allServerEvidence
		if !invalidate && previous != nil && previous.Identity() == identity {
			invalidate = identityEvidence || serverReferenceNewer(reference, previous)
		}
		if invalidate {
			calls[callID] = struct{}{}
		}
	}
	turns := make(map[string]struct{})
	for index, message := range s.messages {
		if message.Role != "tool" {
			continue
		}
		if _, affected := calls[message.ToolCallID]; affected && index < len(s.messageTurnIDs) {
			turns[s.messageTurnIDs[index]] = struct{}{}
		}
	}
	result := serverInvalidation{
		ToolCallIDs: make([]string, 0, len(calls)),
		TurnIDs:     make([]string, 0, len(turns)),
	}
	for callID := range calls {
		result.ToolCallIDs = append(result.ToolCallIDs, callID)
	}
	for turnID := range turns {
		result.TurnIDs = append(result.TurnIDs, turnID)
	}
	sort.Strings(result.ToolCallIDs)
	sort.Strings(result.TurnIDs)
	return result
}

func mergeServerInvalidations(values ...serverInvalidation) serverInvalidation {
	calls := make(map[string]struct{})
	turns := make(map[string]struct{})
	for _, value := range values {
		for _, callID := range value.ToolCallIDs {
			calls[callID] = struct{}{}
		}
		for _, turnID := range value.TurnIDs {
			turns[turnID] = struct{}{}
		}
	}
	result := serverInvalidation{
		ToolCallIDs: make([]string, 0, len(calls)),
		TurnIDs:     make([]string, 0, len(turns)),
	}
	for callID := range calls {
		result.ToolCallIDs = append(result.ToolCallIDs, callID)
	}
	for turnID := range turns {
		result.TurnIDs = append(result.TurnIDs, turnID)
	}
	sort.Strings(result.ToolCallIDs)
	sort.Strings(result.TurnIDs)
	return result
}

func (s *Session) incomingServerGenerationStaleLocked(reference *ServerReference) bool {
	if reference == nil || reference.Identity() == "" || reference.Generation <= 0 {
		return false
	}
	for _, previous := range s.toolReferences {
		if serverReferenceStale(reference, previous) {
			return true
		}
	}
	return false
}

func (s *Session) replaceInvalidatedToolResultsLocked(invalidation serverInvalidation) {
	invalidatedCalls := make(map[string]struct{}, len(invalidation.ToolCallIDs))
	for _, callID := range invalidation.ToolCallIDs {
		invalidatedCalls[callID] = struct{}{}
		s.toolHistory[callID] = invalidatedToolResultJSON
	}
	invalidatedTurns := make(map[string]struct{}, len(invalidation.TurnIDs))
	for _, turnID := range invalidation.TurnIDs {
		invalidatedTurns[turnID] = struct{}{}
	}
	for index := range s.messages {
		message := &s.messages[index]
		if message.Role == "tool" {
			if _, affected := invalidatedCalls[message.ToolCallID]; affected {
				message.Content = invalidatedToolResultJSON
			}
			continue
		}
		if message.Role == "assistant" && message.Content != "" && index < len(s.messageTurnIDs) {
			if _, affected := invalidatedTurns[s.messageTurnIDs[index]]; affected {
				message.Content = invalidatedAssistantText
			}
		}
	}
}

func (s *Session) contextPlan() (ContextPlan, error) {
	s.appendMu.Lock()
	if s.contextRuntime.isClosed() {
		s.appendMu.Unlock()
		return ContextPlan{}, ErrSessionClosed
	}
	messages := make([]modelclient.Message, len(s.messages))
	for index, message := range s.messages {
		messages[index] = cloneModelMessage(message)
	}
	history := make(map[string]string, len(s.toolHistory))
	for callID, projection := range s.toolHistory {
		history[callID] = projection
	}
	s.appendMu.Unlock()
	plan, err := (ContextPlanner{
		ContextWindow: s.options.ContextWindow,
		Mode:          s.options.ContextCompaction,
		Estimator:     s.estimator,
		Memory:        s.contextRuntime.memoryProjection(),
	}).Plan(messages, Tools(), history)
	if err == nil {
		s.contextRuntime.markSoftPressure(plan.SoftPressure)
		s.contextRuntime.UpdatePlanStatus(plan, s.currentTurnID)
	}
	return plan, err
}

func (s *Session) contextMessages() ([]modelclient.Message, error) {
	plan, err := s.contextPlan()
	if err != nil {
		return nil, err
	}
	return plan.Request.Messages, nil
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

func eventFromToolOutput(tool, summary string, output any) Event {
	event := Event{Tool: tool, Summary: summary, Status: EventSucceeded}
	value, ok := output.(map[string]any)
	if !ok {
		return event
	}
	rawError, failed := value["error"]
	if !failed {
		return event
	}
	event.Status = EventFailed
	if message, ok := rawError.(string); ok {
		event.Detail = message
		if message == "invalid_arguments" || message == "query is required" || message == "unknown tool" {
			event.Status = EventInvalid
		}
	}
	if code, ok := value["code"].(string); ok && code != "" {
		event.Detail = code
	}
	return event
}

func (s *Session) nextThinkingActivityID() string {
	s.activitySequence++
	return fmt.Sprintf("thinking-%d", s.activitySequence)
}

func toolRunningSummary(tool string) string {
	switch tool {
	case "search_knowledge":
		return "正在检索服务端知识库"
	case "get_learning_progress":
		return "正在读取学习进度"
	case "get_learning_route":
		return "正在读取学习路线"
	case "get_due_reviews":
		return "正在读取到期复习"
	case "list_long_term_preferences":
		return "正在读取长期偏好"
	case "recall_session_memory":
		return "正在回查会话证据"
	case "remember_preference":
		return "正在准备长期偏好保存"
	default:
		return "正在调用工具"
	}
}

func preferenceCategory(value string) bool {
	return value == "interaction_preference" || value == "time_constraint" || value == "personal_context"
}

func decodeArguments(raw string, target any) error {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw[0] != '{' {
		return errors.New("tool arguments must be a JSON object")
	}
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple argument values")
		}
		return err
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

func toolFailure(err error, fallback string) map[string]any {
	value := map[string]any{"error": fallback}
	var apiErr *api.APIError
	if errors.As(err, &apiErr) {
		value["code"] = apiErr.Code
		value["status"] = apiErr.Status
		return value
	}
	var protocolErr *api.ProtocolError
	if errors.As(err, &protocolErr) {
		value["code"] = "protocol_error"
		return value
	}
	var transportErr *api.TransportError
	if errors.As(err, &transportErr) {
		value["code"] = "transport_error"
		return value
	}
	return value
}

func isAPINotFound(err error) bool {
	var apiErr *api.APIError
	return errors.As(err, &apiErr) && apiErr.Status == 404 && apiErr.Code == "not_found"
}

func appendUnique(values []string, value string) []string {
	for _, current := range values {
		if current == value {
			return values
		}
	}
	return append(values, value)
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
