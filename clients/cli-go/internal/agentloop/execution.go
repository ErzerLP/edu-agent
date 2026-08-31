package agentloop

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/modelclient"
)

func (s *Session) run(ctx context.Context, events []Event) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	if s.remaining <= 0 {
		return Result{}, errors.New("Agent工具轮数已达到上限，请缩小问题范围后重试")
	}
	s.remaining--
	reasoningEffort := s.frozenReasoningEffort()
	thinkingID := s.nextThinkingActivityID()
	summary := "正在分析问题"
	if len(events) > 0 {
		summary = "正在结合工具结果继续分析"
	}
	s.publishActivity(ctx, Activity{Kind: ActivityThinking, Event: Event{ID: thinkingID, Summary: summary, Status: EventRunning}, Phase: ActivityPreparingContext})
	plan, err := s.contextPlan()
	if err != nil {
		s.publishActivity(ctx, Activity{Kind: ActivityThinking, Event: Event{ID: thinkingID, Summary: "上下文准备失败", Status: EventFailed, Detail: "context_prepare_failed"}, Phase: ActivityValidatingResponse, StableCode: "context_prepare_failed"})
		return Result{}, err
	}
	plan.Request.ReasoningEffort = reasoningEffort
	s.publishActivity(ctx, Activity{Kind: ActivityThinking, Event: Event{ID: thinkingID, Summary: summary, Status: EventRunning}, Phase: ActivityWaitingModel, ReasoningEffort: reasoningEffort})
	response, err := s.foregroundResponse(ctx, plan.Request, thinkingID)
	if err != nil {
		err = preferContextError(ctx, err)
		if ctx.Err() != nil {
			return Result{}, err
		}
		code := stableActivityCode(err, "model_request_failed")
		s.publishActivity(ctx, Activity{Kind: ActivityThinking, Event: Event{ID: thinkingID, Summary: "模型响应失败", Status: EventFailed, Detail: code}, Phase: ActivityValidatingResponse, StableCode: code})
		return Result{}, err
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	s.publishActivity(ctx, Activity{Kind: ActivityThinking, Event: Event{ID: thinkingID, Summary: "正在校验模型响应", Status: EventRunning}, Phase: ActivityValidatingResponse})
	if response.Usage != nil {
		s.estimator.ObserveActual(plan.EstimatedInput, *response.Usage)
	}
	if err := validateModelMessage(response.Message); err != nil {
		s.publishActivity(ctx, Activity{Kind: ActivityThinking, Event: Event{ID: thinkingID, Summary: "模型响应不符合协议", Status: EventFailed, Detail: "invalid_model_response"}, Phase: ActivityValidatingResponse, StableCode: "invalid_model_response"})
		return Result{}, err
	}
	if len(response.Message.ToolCalls) > s.toolCallsRemaining {
		s.publishActivity(ctx, Activity{Kind: ActivityThinking, Event: Event{ID: thinkingID, Summary: "工具调用超过安全上限", Status: EventFailed, Detail: "tool_limit_exceeded"}, Phase: ActivityAssemblingTools, StableCode: "tool_limit_exceeded"})
		return Result{}, errors.New("模型请求的工具调用总数超过安全上限")
	}
	s.toolCallsRemaining -= len(response.Message.ToolCalls)
	if len(response.Message.ToolCalls) == 0 {
		text := strings.TrimSpace(response.Message.Content)
		if text == "" {
			return Result{}, errors.New("模型没有返回可显示的回答")
		}
		if err := s.commitFinalAnswer(ctx, response.Message, text); err != nil {
			return Result{}, err
		}
		s.publishActivity(ctx, Activity{Kind: ActivityThinking, Event: Event{ID: thinkingID, Summary: "已完成回答组织", Status: EventSucceeded}, Phase: ActivityValidatingResponse})
		return Result{Text: text, Events: events}, nil
	}
	s.publishActivity(ctx, Activity{Kind: ActivityThinking, Event: Event{ID: thinkingID, Summary: "已确定下一步工具操作", Status: EventSucceeded}, Phase: ActivityAssemblingTools})
	if err := s.appendTurnMessage(s.currentTurnID, response.Message); err != nil {
		return Result{}, err
	}
	return s.processCalls(ctx, response.Message.ToolCalls, 0, events)
}

func (s *Session) foregroundResponse(ctx context.Context, request modelclient.Request, activityID string) (modelclient.Response, error) {
	streaming, ok := s.model.(StreamingModel)
	if !ok {
		return s.model.Complete(ctx, request)
	}
	return streaming.Stream(ctx, request, func(event modelclient.StreamEvent) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		switch event.Kind {
		case modelclient.StreamEventResponseStarted:
			s.publishActivity(ctx, Activity{Kind: ActivityThinking, Event: Event{ID: activityID, Summary: "正在接收模型响应", Status: EventRunning}, Phase: ActivityReceivingStream, ReasoningEffort: request.ReasoningEffort})
		case modelclient.StreamEventTextDelta:
			delta := safeActivityDelta(event.Text)
			if delta != "" {
				s.publishActivity(ctx, Activity{Kind: ActivityTextDelta, Event: Event{ID: activityID, Summary: "正在生成回答", Status: EventRunning}, Phase: ActivityReceivingStream, ReasoningEffort: request.ReasoningEffort, Delta: delta})
			}
		case modelclient.StreamEventCompatibilityFallback:
			s.publishActivity(ctx, Activity{Kind: ActivityThinking, Event: Event{ID: activityID, Summary: "模型已切换到兼容响应模式", Status: EventRunning, Detail: "stream_compatibility_fallback"}, Phase: ActivityWaitingModel, ReasoningEffort: request.ReasoningEffort, StableCode: "stream_compatibility_fallback"})
		default:
			return errors.New("模型返回未知流式事件")
		}
		return nil
	})
}

func (s *Session) processCalls(ctx context.Context, calls []modelclient.ToolCall, start int, events []Event) (Result, error) {
	for index := start; index < len(calls); index++ {
		if err := ctx.Err(); err != nil {
			return Result{}, err
		}
		call := calls[index]
		s.publishActivity(ctx, Activity{Kind: ActivityTool, Event: Event{ID: call.ID, Tool: call.Function.Name, Summary: toolRunningSummary(call.Function.Name), Status: EventRunning}, Phase: ActivityExecutingTool})
		switch call.Function.Name {
		case "remember_preference":
			args, err := decodePreferenceArgs(call.Function.Arguments)
			if err != nil {
				if appendErr := s.appendSessionToolResult(call.Function.Name, call.ID, map[string]any{"error": "invalid_arguments"}); appendErr != nil {
					return Result{}, appendErr
				}
				event := Event{ID: call.ID, Tool: call.Function.Name, Summary: "偏好候选参数无效", Status: EventInvalid, Detail: "invalid_arguments"}
				s.publishActivity(ctx, Activity{Kind: ActivityTool, Event: event, Phase: ActivityValidatingResponse, StableCode: event.Detail})
				events = append(events, event)
				continue
			}
			s.appendMu.Lock()
			s.pendingKind = pendingPreference
			s.pendingCalls = append([]modelclient.ToolCall(nil), calls...)
			s.pendingIndex = index
			s.pendingArgs = args
			s.pendingEvents = append([]Event(nil), events...)
			s.pendingResolving = false
			s.appendMu.Unlock()
			s.markPreferencePending()
			event := Event{ID: call.ID, Tool: call.Function.Name, Summary: "等待用户决定长期偏好的处理方式", Status: EventConfirmationRequired}
			s.publishActivity(ctx, Activity{Kind: ActivityTool, Event: event, Phase: ActivityWaitingUser})
			return Result{Events: append(events, event), Pending: &PreferenceConfirmation{
				Content: args.Content, Reason: args.Reason, Category: args.Category,
				Sensitivity: args.Sensitivity, Stability: args.Stability,
			}}, nil
		case "ask_user_question":
			args, err := decodeQuestionArgs(call.Function.Arguments)
			if err != nil {
				if appendErr := s.appendSessionToolResult(call.Function.Name, call.ID, map[string]any{"error": "invalid_arguments"}); appendErr != nil {
					return Result{}, appendErr
				}
				event := Event{ID: call.ID, Tool: call.Function.Name, Summary: "用户问询参数无效", Status: EventInvalid, Detail: "invalid_arguments"}
				s.publishActivity(ctx, Activity{Kind: ActivityTool, Event: event, Phase: ActivityValidatingResponse, StableCode: event.Detail})
				events = append(events, event)
				continue
			}
			pending, allowed := s.registerQuestion(args)
			if !allowed {
				if appendErr := s.appendSessionToolResult(call.Function.Name, call.ID, map[string]any{"error": "question_limit_exceeded"}); appendErr != nil {
					return Result{}, appendErr
				}
				event := Event{ID: call.ID, Tool: call.Function.Name, Summary: "当前轮次的用户问询已达到上限", Status: EventInvalid, Detail: "question_limit_exceeded"}
				s.publishActivity(ctx, Activity{Kind: ActivityTool, Event: event, Phase: ActivityValidatingResponse, StableCode: event.Detail})
				events = append(events, event)
				continue
			}
			s.appendMu.Lock()
			s.pendingKind = pendingQuestion
			s.pendingCalls = append([]modelclient.ToolCall(nil), calls...)
			s.pendingIndex = index
			s.pendingQuestion = pending
			s.pendingEvents = append([]Event(nil), events...)
			s.pendingResolving = false
			s.appendMu.Unlock()
			event := Event{ID: call.ID, Tool: call.Function.Name, Summary: "等待用户回答问题", Status: EventConfirmationRequired}
			s.publishActivity(ctx, Activity{Kind: ActivityTool, Event: event, Phase: ActivityWaitingUser})
			return Result{Events: append(events, event), PendingQuestion: clonePendingQuestion(pending)}, nil
		default:
			output, summary := s.executeReadTool(ctx, call)
			if err := ctx.Err(); err != nil {
				return Result{}, err
			}
			if err := s.appendToolResult(call.Function.Name, call.ID, output); err != nil {
				return Result{}, err
			}
			event := eventFromToolOutput(call.Function.Name, summary, output)
			event.ID = call.ID
			s.publishActivity(ctx, Activity{Kind: ActivityTool, Event: event, Phase: ActivityExecutingTool, StableCode: event.Detail})
			events = append(events, event)
		}
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	s.publishActivity(ctx, Activity{Kind: ActivityThinking, Event: Event{ID: fmt.Sprintf("continue-%d", s.activitySequence), Summary: "正在结合工具结果继续分析", Status: EventRunning}, Phase: ActivityContinuingAfterTool})
	return s.run(ctx, events)
}

func (s *Session) registerQuestion(args questionArgs) (*PendingQuestion, bool) {
	s.appendMu.Lock()
	defer s.appendMu.Unlock()
	turn := s.turns[s.activeTurnID]
	if turn == nil || turn.QuestionsAsked >= 4 {
		return nil, false
	}
	if _, duplicate := turn.QuestionIDs[args.QuestionID]; duplicate {
		return nil, false
	}
	turn.QuestionsAsked++
	turn.QuestionIDs[args.QuestionID] = struct{}{}
	return pendingQuestionFromArgs(args), true
}
