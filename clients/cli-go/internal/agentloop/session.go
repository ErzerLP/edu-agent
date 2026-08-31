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
	model                        Model
	server                       Server
	options                      Options
	appendMu                     sync.Mutex
	activityMu                   sync.Mutex
	activityStarts               map[string]time.Time
	messages                     []modelclient.Message
	messageTurnIDs               []string
	turns                        map[string]*sessionTurn
	turnOrder                    []string
	currentTurnID                string
	activeTurnID                 string
	turnSequence                 int
	reasoningEffort              modelclient.ReasoningEffort
	hotRawTokenLimit             int
	contextRuntime               *ContextRuntime
	estimator                    *ConservativeTokenEstimator
	toolHistory                  map[string]string
	toolReferences               map[string]*ServerReference
	currentToolResultTokens      int
	currentToolResultBudget      int
	remaining                    int
	toolCallsRemaining           int
	pendingKind                  pendingInteractionKind
	pendingCalls                 []modelclient.ToolCall
	pendingIndex                 int
	pendingArgs                  preferenceArgs
	pendingQuestion              *PendingQuestion
	pendingResolving             bool
	pendingEvents                []Event
	pendingOperationID           string
	pendingDecisionOperationID   string
	pendingRejectOperationID     string
	pendingCandidateID           string
	pendingCandidateRevision     int64
	pendingPreferenceFailureCode string
	activitySequence             int
}

type sessionTurn struct {
	ID             string
	Completed      bool
	Protected      bool
	OutcomeUnknown bool
	SourceIDs      []string
	QuestionsAsked int
	QuestionIDs    map[string]struct{}
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
	if options.ReasoningEffort == "" {
		options.ReasoningEffort = modelclient.ReasoningEffortAuto
	}
	if !validReasoningEffort(options.ReasoningEffort) {
		return nil, errors.New("agent reasoning effort is invalid")
	}
	if options.ContextCompaction != ContextCompactionAuto && options.ContextCompaction != ContextCompactionRecentOnly && options.ContextCompaction != ContextCompactionOff {
		return nil, errors.New("agent context compaction mode is invalid")
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	if options.ModelTimeout <= 0 {
		options.ModelTimeout = 90 * time.Second
	}
	if options.ToolTimeout <= 0 {
		options.ToolTimeout = 30 * time.Second
	}
	estimator := NewTokenEstimator()
	session := &Session{
		model: model, server: server, options: options,
		messages:                []modelclient.Message{{Role: "system", Content: systemPrompt}},
		messageTurnIDs:          []string{""},
		turns:                   make(map[string]*sessionTurn),
		turnOrder:               []string{},
		reasoningEffort:         options.ReasoningEffort,
		activityStarts:          make(map[string]time.Time),
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
	if s.activeTurnID != "" {
		return "", ErrActiveTurn
	}
	s.turnSequence++
	turnID := fmt.Sprintf("turn-%d", s.turnSequence)
	s.turns[turnID] = &sessionTurn{ID: turnID, QuestionIDs: make(map[string]struct{})}
	s.turnOrder = append(s.turnOrder, turnID)
	s.currentTurnID = turnID
	s.activeTurnID = turnID
	return turnID, nil
}

func (s *Session) discardTurn(turnID string) {
	s.appendMu.Lock()
	turn := s.turns[turnID]
	delete(s.turns, turnID)
	filteredOrder := s.turnOrder[:0]
	for _, current := range s.turnOrder {
		if current != turnID {
			filteredOrder = append(filteredOrder, current)
		}
	}
	s.turnOrder = filteredOrder
	messages := make([]modelclient.Message, 0, len(s.messages))
	turnIDs := make([]string, 0, len(s.messageTurnIDs))
	for index, message := range s.messages {
		messageTurnID := ""
		if index < len(s.messageTurnIDs) {
			messageTurnID = s.messageTurnIDs[index]
		}
		if messageTurnID == turnID {
			if message.Role == "tool" {
				delete(s.toolHistory, message.ToolCallID)
				delete(s.toolReferences, message.ToolCallID)
			}
			continue
		}
		messages = append(messages, message)
		turnIDs = append(turnIDs, messageTurnID)
	}
	s.messages = messages
	s.messageTurnIDs = turnIDs
	if s.activeTurnID == turnID {
		s.activeTurnID = ""
		s.clearPendingLocked()
		s.remaining = 0
		s.toolCallsRemaining = 0
		s.currentToolResultTokens = 0
	}
	if s.currentTurnID == turnID {
		s.currentTurnID = ""
	}
	s.appendMu.Unlock()
	if turn != nil {
		s.contextRuntime.discardIncompleteTurn(turnID)
	}
}

func (s *Session) clearPendingLocked() {
	s.pendingKind = pendingNone
	s.pendingCalls = nil
	s.pendingEvents = nil
	s.pendingIndex = 0
	s.pendingArgs = preferenceArgs{}
	s.pendingQuestion = nil
	s.pendingResolving = false
	s.clearPreferenceWriteStateLocked()
}

func (s *Session) clearPreferenceWriteStateLocked() {
	s.pendingOperationID = ""
	s.pendingDecisionOperationID = ""
	s.pendingRejectOperationID = ""
	s.pendingCandidateID = ""
	s.pendingCandidateRevision = 0
	s.pendingPreferenceFailureCode = ""
}

func (s *Session) appendCapturedMessage(turnID string, message modelclient.Message, recall string, kind SourceKind, authority AuthorityClass, freshness FreshnessClass, reference *ServerReference) error {
	s.appendMu.Lock()
	defer s.appendMu.Unlock()
	if s.contextRuntime.isClosed() {
		return ErrSessionClosed
	}
	return s.appendCapturedMessageLocked(turnID, message, recall, kind, authority, freshness, reference)
}

func (s *Session) appendCapturedMessageLocked(turnID string, message modelclient.Message, recall string, kind SourceKind, authority AuthorityClass, freshness FreshnessClass, reference *ServerReference) error {
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
	s.finishSuccessfulTurnLocked()
	s.appendMu.Unlock()
	s.afterSuccessfulTurn()
}

func (s *Session) finishSuccessfulTurnLocked() {
	turnID := s.activeTurnID
	if turnID == "" {
		turnID = s.currentTurnID
	}
	if turn, exists := s.turns[turnID]; exists {
		turn.Completed = true
		if !turn.OutcomeUnknown {
			turn.Protected = false
		}
	}
	if s.activeTurnID == turnID {
		s.activeTurnID = ""
	}
	s.clearPendingLocked()
}

func (s *Session) afterSuccessfulTurn() {
	s.contextRuntime.setPreferencePending(false)
	s.trimRawHistory()
	s.contextRuntime.triggerConsolidation()
}

// commitFinalAnswer is the ordinary-answer linearization point. Cancellation
// observed while appendMu is held wins and leaves no assistant/source entry;
// once this check succeeds, the completed answer wins over any later cancel.
func (s *Session) commitFinalAnswer(ctx context.Context, message modelclient.Message, text string) error {
	s.appendMu.Lock()
	if err := ctx.Err(); err != nil {
		s.appendMu.Unlock()
		return err
	}
	if s.contextRuntime.isClosed() {
		s.appendMu.Unlock()
		return ErrSessionClosed
	}
	turnID := s.activeTurnID
	if turnID == "" {
		turnID = s.currentTurnID
	}
	if turnID == "" || s.turns[turnID] == nil {
		s.appendMu.Unlock()
		return errors.New("Agent轮次已失效")
	}
	if err := s.appendCapturedMessageLocked(turnID, message, text, SourceAssistant, AuthoritySessionStatement, FreshnessSessionCurrent, nil); err != nil {
		s.appendMu.Unlock()
		return err
	}
	s.finishSuccessfulTurnLocked()
	s.appendMu.Unlock()
	s.afterSuccessfulTurn()
	return nil
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
	s.activeTurnID = ""
	s.toolHistory = make(map[string]string)
	s.toolReferences = make(map[string]*ServerReference)
	s.clearPendingLocked()
	s.appendMu.Unlock()
	s.activityMu.Lock()
	s.activityStarts = make(map[string]time.Time)
	s.activityMu.Unlock()
}

func (s *Session) Send(ctx context.Context, input string) (Result, error) {
	if s.contextRuntime.isClosed() {
		return Result{}, ErrSessionClosed
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
	ctx = withActivityTurn(ctx, turnID)
	userMessage := modelclient.Message{Role: "user", Content: input}
	if err := s.appendCapturedMessage(turnID, userMessage, input, SourceUser, AuthoritySessionStatement, FreshnessSessionCurrent, nil); err != nil {
		s.discardTurn(turnID)
		return Result{}, err
	}
	s.activityMu.Lock()
	s.activityStarts = make(map[string]time.Time)
	s.activityMu.Unlock()
	s.activitySequence = 0
	s.currentToolResultTokens = 0
	s.remaining = s.options.MaxToolRounds
	s.toolCallsRemaining = maxToolCallsPerTurn
	result, runErr := s.run(ctx, nil)
	if runErr != nil {
		runErr = preferContextError(ctx, runErr)
		if ctx.Err() != nil {
			s.publishActivity(ctx, Activity{Kind: ActivityThinking, Event: Event{ID: "turn-stop", Summary: "正在停止当前回答", Status: EventRunning}, Phase: ActivityStopping, StableCode: stableActivityCode(runErr, "turn_stopping")})
		}
		s.discardTurn(turnID)
		if ctx.Err() != nil {
			s.publishActivity(ctx, Activity{Kind: ActivityThinking, Event: Event{ID: "turn-stop", Summary: "当前回答已停止", Status: EventSucceeded}, Phase: ActivityStopped, StableCode: stableActivityCode(runErr, "turn_stopped")})
		}
		return Result{}, runErr
	}
	return cloneResult(result), nil
}

func cloneResult(value Result) Result {
	value.Events = append([]Event(nil), value.Events...)
	if value.Pending != nil {
		pending := *value.Pending
		value.Pending = &pending
	}
	if value.PendingQuestion != nil {
		question := *value.PendingQuestion
		question.Options = append([]QuestionOption(nil), value.PendingQuestion.Options...)
		value.PendingQuestion = &question
	}
	return value
}

func validateModelMessage(message modelclient.Message) error {
	if len(message.Content) > maxAssistantTextBytes {
		return errors.New("模型回答超过客户端安全上限")
	}
	if len(message.ToolCalls) > maxToolCallsPerResponse {
		return errors.New("模型单轮请求的工具调用数超过安全上限")
	}
	totalArguments := 0
	seenCallIDs := make(map[string]struct{}, len(message.ToolCalls))
	for _, call := range message.ToolCalls {
		if call.ID == "" || call.Type != "function" || call.Function.Name == "" {
			return errors.New("模型工具调用身份无效")
		}
		if _, duplicate := seenCallIDs[call.ID]; duplicate {
			return errors.New("模型工具调用ID重复")
		}
		seenCallIDs[call.ID] = struct{}{}
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

func (s *Session) createPreference(ctx context.Context, args preferenceArgs, createOperationID, decisionOperationID string) (any, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s.preferenceCompensationPending() {
		failureCode, err := s.rejectPendingPreferenceCandidate(ctx)
		if err != nil {
			return nil, err
		}
		return nil, preferenceSaveFailure(failureCode)
	}
	validUntil := s.options.Now().UTC().Add(90 * 24 * time.Hour)
	if args.Stability == "stable" {
		validUntil = s.options.Now().UTC().AddDate(10, 0, 0)
	}
	result, err := s.server.CreateMemoryCandidate(ctx, api.MemoryCandidateRequest{
		OperationID: createOperationID, PayloadSchemaVersion: 1, Content: args.Content, Reason: args.Reason,
		Category: args.Category, Sensitivity: args.Sensitivity, Stability: args.Stability, ValidUntil: validUntil,
	})
	if ctx.Err() != nil {
		return nil, fmt.Errorf("%w，请使用相同操作ID重试核对", ErrPreferenceOutcomeUnknown)
	}
	if err != nil {
		var apiErr *api.APIError
		if errors.As(err, &apiErr) && apiErr.Status >= 400 && apiErr.Status < 500 {
			return nil, preferenceSaveFailure(apiErr.Code)
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
		return nil, preferenceSaveFailure("candidate_" + candidate.Status)
	default:
		return nil, fmt.Errorf("%w，服务端返回未知候选状态", ErrPreferenceOutcomeUnknown)
	}

	decisionResult, err := s.server.DecideMemoryCandidate(ctx, candidate.ID, api.MemoryCandidateDecisionRequest{
		OperationID: decisionOperationID, PayloadSchemaVersion: 1, ExpectedRevision: candidate.Revision,
		Decision: "admit", Reason: "user_confirmed_preference_save",
	})
	if ctx.Err() != nil {
		return nil, fmt.Errorf("%w，请使用相同操作ID重试核对", ErrPreferenceOutcomeUnknown)
	}
	if err != nil {
		var apiErr *api.APIError
		if errors.As(err, &apiErr) && apiErr.Status >= 400 && apiErr.Status < 500 {
			s.setPreferenceCompensation(candidate.ID, candidate.Revision, apiErr.Code)
			failureCode, rejectErr := s.rejectPendingPreferenceCandidate(ctx)
			if rejectErr != nil {
				return nil, rejectErr
			}
			return nil, preferenceSaveFailure(failureCode)
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

func preferenceSaveFailure(code string) error {
	code = strings.TrimSpace(code)
	if code == "" {
		code = "request_rejected"
	}
	return fmt.Errorf("长期偏好未保存（%s）", code)
}

func (s *Session) preferenceCompensationPending() bool {
	s.appendMu.Lock()
	defer s.appendMu.Unlock()
	return s.pendingCandidateID != ""
}

func (s *Session) setPreferenceCompensation(candidateID string, revision int64, failureCode string) {
	s.appendMu.Lock()
	s.pendingCandidateID = candidateID
	s.pendingCandidateRevision = revision
	s.pendingPreferenceFailureCode = strings.TrimSpace(failureCode)
	s.appendMu.Unlock()
}

func (s *Session) ensurePreferenceRejectOperationID() error {
	s.appendMu.Lock()
	if s.pendingCandidateID == "" {
		s.appendMu.Unlock()
		return errors.New("没有需要补偿拒绝的长期偏好候选")
	}
	if s.pendingRejectOperationID != "" {
		s.appendMu.Unlock()
		return nil
	}
	createOperationID := s.pendingOperationID
	decisionOperationID := s.pendingDecisionOperationID
	s.appendMu.Unlock()

	rejectOperationID, err := s.options.NewUUID()
	if err != nil || rejectOperationID == "" || rejectOperationID == createOperationID || rejectOperationID == decisionOperationID {
		return fmt.Errorf("%w，无法生成独立的候选补偿操作ID", ErrPreferenceOutcomeUnknown)
	}
	s.appendMu.Lock()
	if s.pendingCandidateID == "" {
		s.appendMu.Unlock()
		return errors.New("需要补偿拒绝的长期偏好候选已失效")
	}
	if s.pendingRejectOperationID == "" {
		s.pendingRejectOperationID = rejectOperationID
	}
	s.appendMu.Unlock()
	return nil
}

func (s *Session) rejectPendingPreferenceCandidate(ctx context.Context) (string, error) {
	if err := s.ensurePreferenceRejectOperationID(); err != nil {
		return "", err
	}
	s.appendMu.Lock()
	candidateID := s.pendingCandidateID
	candidateRevision := s.pendingCandidateRevision
	failureCode := s.pendingPreferenceFailureCode
	rejectOperationID := s.pendingRejectOperationID
	s.appendMu.Unlock()

	result, err := s.server.DecideMemoryCandidate(ctx, candidateID, api.MemoryCandidateDecisionRequest{
		OperationID: rejectOperationID, PayloadSchemaVersion: 1, ExpectedRevision: candidateRevision,
		Decision: "reject", Reason: "compensate_failed_preference_admission",
	})
	if ctx.Err() != nil {
		return "", fmt.Errorf("%w，候选补偿结果未知，请使用相同操作ID重试核对", ErrPreferenceOutcomeUnknown)
	}
	if err != nil {
		return "", fmt.Errorf("%w，长期偏好候选补偿拒绝未确认", ErrPreferenceOutcomeUnknown)
	}
	if result.Candidate == nil || result.Candidate.Candidate.ID != candidateID ||
		result.Candidate.Candidate.Status != "rejected" || result.Candidate.Candidate.Revision <= candidateRevision {
		return "", fmt.Errorf("%w，服务端未确认长期偏好候选已拒绝", ErrPreferenceOutcomeUnknown)
	}
	return failureCode, nil
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

func (s *Session) appendSessionToolResult(tool, callID string, value any) error {
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
	message := modelclient.Message{Role: "tool", ToolCallID: callID, Content: live}
	sourceID, err := s.contextRuntime.appendSource(sourceDraft{
		TurnID: s.currentTurnID, Kind: SourceTool, CreatedAt: s.options.Now().UTC(), ModelMessage: message,
		RecallText: projection.Recall, Authority: AuthoritySessionStatement, Freshness: FreshnessSessionCurrent,
	})
	if err != nil {
		return err
	}
	s.toolHistory[callID] = projection.History
	delete(s.toolReferences, callID)
	s.messages = append(s.messages, message)
	s.messageTurnIDs = append(s.messageTurnIDs, s.currentTurnID)
	if sourceID != "" {
		if turn := s.turns[s.currentTurnID]; turn != nil {
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
4. remember_preference 会触发专用本地确认；未获得“保存为长期记忆”的明确确认前，不得声称偏好已经保存。“仅本次会话”和“不采用”都不是服务端写授权。
5. 只有继续学习确实需要用户做当前会话决定时才调用 ask_user_question。单选返回一个稳定 option ID，多选返回稳定 option ID 数组，也可接收有界自定义回答；普通问询答案只用于当前会话，不构成长期记忆、外部写入、删除或发布授权。
6. ask_user_question 不得索取密码、API Key、token、私钥、恢复码或助记词；不得把选项文案描述成持久写入授权。
7. 不得请求、显示或保存 API Key、设备令牌、服务密钥等秘密。不要把普通聊天原文当作长期偏好。
8. 工具失败时如实说明，并继续提供不依赖该工具的有限帮助。`
