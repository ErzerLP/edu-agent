package agentloop

import (
	"context"
	"errors"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/modelclient"
)

func (s *Session) ResolvePreference(ctx context.Context, resolution PreferenceResolution) (Result, error) {
	if s.contextRuntime.isClosed() {
		return Result{}, ErrSessionClosed
	}
	s.appendMu.Lock()
	if s.pendingKind != pendingPreference || len(s.pendingCalls) == 0 || s.pendingIndex >= len(s.pendingCalls) {
		s.appendMu.Unlock()
		return Result{}, errors.New("当前没有待处理的长期偏好")
	}
	if s.pendingResolving {
		s.appendMu.Unlock()
		return Result{}, errors.New("长期偏好决定正在处理中")
	}
	outcomeUnknown := s.pendingOperationID != "" || s.pendingDecisionOperationID != "" || s.pendingRejectOperationID != "" || s.pendingCandidateID != ""
	if outcomeUnknown && resolution != PreferenceSave && resolution != PreferenceRetry {
		s.appendMu.Unlock()
		return Result{}, ErrPreferenceOutcomeUnknown
	}
	if resolution == PreferenceRetry && !outcomeUnknown {
		s.appendMu.Unlock()
		return Result{}, errors.New("当前长期偏好没有需要核对的未知结果")
	}
	if resolution != PreferenceSave && resolution != PreferenceRetry && resolution != PreferenceSessionOnly && resolution != PreferenceDecline {
		s.appendMu.Unlock()
		return Result{}, errors.New("长期偏好处理方式无效")
	}
	s.pendingResolving = true
	turnID := s.activeTurnID
	calls := append([]modelclient.ToolCall(nil), s.pendingCalls...)
	index := s.pendingIndex
	args := s.pendingArgs
	events := append([]Event(nil), s.pendingEvents...)
	createOperationID := s.pendingOperationID
	decisionOperationID := s.pendingDecisionOperationID
	s.appendMu.Unlock()
	ctx = withActivityTurn(ctx, turnID)

	call := calls[index]
	var output any
	var event Event
	saved := false
	if resolution == PreferenceSave || resolution == PreferenceRetry {
		s.publishActivity(ctx, Activity{Kind: ActivityTool, Event: Event{ID: call.ID, Tool: call.Function.Name, Summary: "正在保存长期偏好", Status: EventRunning}, Phase: ActivityExecutingTool})
		if createOperationID == "" {
			var err error
			createOperationID, err = s.options.NewUUID()
			if err != nil {
				s.releasePendingResolution()
				return Result{}, errors.New("无法生成偏好创建操作ID")
			}
			decisionOperationID, err = s.options.NewUUID()
			if err != nil || decisionOperationID == createOperationID {
				s.releasePendingResolution()
				return Result{}, errors.New("无法生成独立的偏好确认操作ID")
			}
			s.appendMu.Lock()
			s.pendingOperationID = createOperationID
			s.pendingDecisionOperationID = decisionOperationID
			s.appendMu.Unlock()
		}
		value, err := s.createPreference(ctx, args, createOperationID, decisionOperationID)
		if err != nil {
			status, code := EventFailed, "preference_write_failed"
			if errors.Is(err, ErrPreferenceOutcomeUnknown) {
				status, code = EventOutcomeUnknown, "outcome_unknown"
				s.markPreferenceOutcomeUnknown()
			} else {
				s.appendMu.Lock()
				resolvedTurnID := s.activeTurnID
				if resolvedTurnID == "" {
					resolvedTurnID = s.currentTurnID
				}
				if turn := s.turns[resolvedTurnID]; turn != nil {
					turn.Protected = true
					turn.OutcomeUnknown = false
				}
				s.clearPreferenceWriteStateLocked()
				s.pendingResolving = false
				s.appendMu.Unlock()
			}
			s.publishActivity(ctx, Activity{Kind: ActivityTool, Event: Event{ID: call.ID, Tool: call.Function.Name, Summary: "长期偏好未确认保存", Status: status, Detail: code}, Phase: ActivityWaitingUser, StableCode: code})
			if errors.Is(err, ErrPreferenceOutcomeUnknown) {
				s.releasePendingResolution()
			}
			return Result{}, err
		}
		s.appendMu.Lock()
		resolvedTurnID := s.activeTurnID
		if resolvedTurnID == "" {
			resolvedTurnID = s.currentTurnID
		}
		if turn := s.turns[resolvedTurnID]; turn != nil {
			turn.Protected = true
			turn.OutcomeUnknown = false
		}
		s.appendMu.Unlock()
		output = value
		event = Event{ID: call.ID, Tool: call.Function.Name, Summary: "长期偏好已保存", Status: EventSucceeded}
		saved = true
	} else if resolution == PreferenceSessionOnly {
		output = map[string]any{"submitted": false, "saved": false, "session_only": true, "reason": "user_session_only"}
		event = Event{ID: call.ID, Tool: call.Function.Name, Summary: "偏好仅用于当前会话", Status: EventSucceeded}
	} else {
		output = map[string]any{"submitted": false, "saved": false, "session_only": false, "reason": "user_declined"}
		event = Event{ID: call.ID, Tool: call.Function.Name, Summary: "用户拒绝保存长期偏好", Status: EventSucceeded}
	}

	s.publishActivity(ctx, Activity{Kind: ActivityTool, Event: event, Phase: ActivityContinuingAfterTool})
	events = append(events, event)
	var appendErr error
	if saved {
		appendErr = s.appendToolResult(call.Function.Name, call.ID, output)
	} else {
		appendErr = s.appendSessionToolResult(call.Function.Name, call.ID, output)
	}
	if appendErr != nil {
		if saved {
			return s.preferenceCompletionFallback(events, appendErr)
		}
		return s.finishAfterTurnFailure(turnID, events, appendErr)
	}
	s.clearPendingAfterResolution()
	result, err := s.processCalls(ctx, calls, index+1, events)
	if err == nil {
		return cloneResult(result), nil
	}
	err = preferContextError(ctx, err)
	if saved {
		return s.preferenceCompletionFallback(events, err)
	}
	return s.finishAfterTurnFailure(turnID, events, err)
}

func (s *Session) ResolveQuestion(ctx context.Context, answer QuestionAnswer) (Result, error) {
	if s.contextRuntime.isClosed() {
		return Result{}, ErrSessionClosed
	}
	s.appendMu.Lock()
	if s.pendingKind != pendingQuestion || s.pendingQuestion == nil || len(s.pendingCalls) == 0 || s.pendingIndex >= len(s.pendingCalls) {
		s.appendMu.Unlock()
		return Result{}, errors.New("当前没有待回答的问题")
	}
	if s.pendingResolving {
		s.appendMu.Unlock()
		return Result{}, errors.New("问题回答正在处理中")
	}
	value, err := validateQuestionAnswer(s.pendingQuestion, answer)
	if err != nil {
		s.appendMu.Unlock()
		return Result{}, err
	}
	s.pendingResolving = true
	turnID := s.activeTurnID
	calls := append([]modelclient.ToolCall(nil), s.pendingCalls...)
	index := s.pendingIndex
	events := append([]Event(nil), s.pendingEvents...)
	s.appendMu.Unlock()
	ctx = withActivityTurn(ctx, turnID)

	call := calls[index]
	if err := s.appendSessionToolResult(call.Function.Name, call.ID, value); err != nil {
		return s.finishAfterTurnFailure(turnID, events, err)
	}
	event := Event{ID: call.ID, Tool: call.Function.Name, Summary: questionResolutionSummary(answer.Status), Status: EventSucceeded}
	s.publishActivity(ctx, Activity{Kind: ActivityTool, Event: event, Phase: ActivityContinuingAfterTool})
	events = append(events, event)
	s.clearPendingAfterResolution()
	result, err := s.processCalls(ctx, calls, index+1, events)
	if err != nil {
		err = preferContextError(ctx, err)
		return s.finishAfterTurnFailure(turnID, events, err)
	}
	return cloneResult(result), nil
}

func (s *Session) releasePendingResolution() {
	s.appendMu.Lock()
	s.pendingResolving = false
	s.appendMu.Unlock()
}

func (s *Session) clearPendingAfterResolution() {
	s.appendMu.Lock()
	s.clearPendingLocked()
	s.appendMu.Unlock()
	s.contextRuntime.setPreferencePending(false)
}

func (s *Session) preferenceCompletionFallback(events []Event, _ error) (Result, error) {
	text := "长期偏好已保存；Agent后续回答暂时失败，你可以继续提问。"
	message := modelclient.Message{Role: "assistant", Content: text}
	if err := s.appendCapturedMessage(s.currentTurnID, message, text, SourceAssistant, AuthoritySessionStatement, FreshnessSessionCurrent, nil); err != nil {
		return Result{}, err
	}
	s.finishSuccessfulTurn()
	return Result{Events: events, Text: text}, nil
}
