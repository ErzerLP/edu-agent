package agentloop

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/modelclient"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/workspace"
)

type continueAgentLoopError struct {
	events []Event
}

func (*continueAgentLoopError) Error() string {
	return "continue agent loop"
}

func (s *Session) run(ctx context.Context, events []Event) (Result, error) {
	for {
		result, err := s.runOnce(ctx, events)
		var continuation *continueAgentLoopError
		if !errors.As(err, &continuation) {
			return result, err
		}
		events = continuation.events
	}
}

func (s *Session) runOnce(ctx context.Context, events []Event) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	if s.options.MaxToolRounds > 0 {
		if s.remaining <= 0 {
			return Result{}, errors.New("Agent已达到用户配置的工具轮数保护值；将最大工具轮数设为0可取消该限制")
		}
		s.remaining--
	}
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
	if err := validateModelMessage(response.Message); err != nil {
		s.publishActivity(ctx, Activity{Kind: ActivityThinking, Event: Event{ID: thinkingID, Summary: "模型响应不符合协议", Status: EventFailed, Detail: "invalid_model_response"}, Phase: ActivityValidatingResponse, StableCode: "invalid_model_response"})
		return Result{}, err
	}
	recordUsage := func() {
		if response.Usage == nil {
			return
		}
		s.estimator.ObserveActual(plan.EstimatedInput, *response.Usage)
		s.contextRuntime.UpdateUsageStatus(*response.Usage)
	}
	if len(response.Message.ToolCalls) == 0 {
		text := strings.TrimSpace(response.Message.Content)
		if text == "" {
			return Result{}, errors.New("模型没有返回可显示的回答")
		}
		if err := s.commitFinalAnswer(ctx, response.Message, text); err != nil {
			return Result{}, err
		}
		recordUsage()
		s.publishActivity(ctx, Activity{Kind: ActivityThinking, Event: Event{ID: thinkingID, Summary: "已完成回答组织", Status: EventSucceeded}, Phase: ActivityValidatingResponse})
		return Result{Text: text, Events: events}, nil
	}
	s.publishActivity(ctx, Activity{Kind: ActivityThinking, Event: Event{ID: thinkingID, Summary: "已确定下一步工具操作", Status: EventSucceeded}, Phase: ActivityAssemblingTools})
	if err := s.appendTurnMessage(s.currentTurnID, response.Message); err != nil {
		return Result{}, err
	}
	recordUsage()
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
	s.currentToolResultShares = max(s.currentToolResultShares, toolResultBudgetShares, len(calls))
	for index := start; index < len(calls); index++ {
		if err := ctx.Err(); err != nil {
			return Result{}, err
		}
		call := calls[index]
		initialFile := (*FileActivityDetail)(nil)
		runningSummary := toolRunningSummary(call.Function.Name)
		if s.workspace != nil && s.workspaceStatus.Available && (workspace.IsReadTool(call.Function.Name) || workspace.IsMutationTool(call.Function.Name)) {
			initialFile = initialWorkspaceFileActivity(call.Function.Name, call.Function.Arguments)
			if initialFile != nil {
				runningSummary = workspaceProgressSummary(call.Function.Name, initialFile)
			}
		}
		s.publishActivity(ctx, Activity{Kind: ActivityTool, Event: Event{ID: call.ID, Tool: call.Function.Name, Summary: runningSummary, Status: EventRunning}, Phase: ActivityExecutingTool, File: initialFile})
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
				if appendErr := s.appendSessionToolResult(call.Function.Name, call.ID, map[string]any{"error": "question_id_conflict"}); appendErr != nil {
					return Result{}, appendErr
				}
				event := Event{ID: call.ID, Tool: call.Function.Name, Summary: "用户问询标识重复或当前轮次不可用", Status: EventInvalid, Detail: "question_id_conflict"}
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
			if workspace.IsMutationTool(call.Function.Name) && s.workspace != nil && s.workspaceStatus.Available {
				toolCtx, cancel := context.WithTimeout(ctx, s.options.ToolTimeout)
				prepared, preparationResult := s.workspace.PrepareMutation(toolCtx, call.Function.Name, call.Function.Arguments)
				toolErr := toolCtx.Err()
				cancel()
				if toolErr != nil {
					event := workspaceToolContextFailureEvent(call.ID, call.Function.Name, toolErr)
					s.publishActivity(ctx, Activity{Kind: ActivityTool, Event: event, Phase: ActivityStopped, StableCode: event.Detail, File: mergePreparedFileActivity(initialFile, prepared)})
					return Result{}, preferContextError(ctx, toolErr)
				}
				if prepared == nil {
					if err := s.appendWorkspaceToolResult(call.Function.Name, call.ID, preparationResult); err != nil {
						return Result{}, err
					}
					event := eventFromToolOutput(call.Function.Name, preparationResult.Summary, preparationResult.Value)
					event.ID = call.ID
					finalFile := mergeFileActivityDetail(fileActivityDetailFromResult(call.Function.Name, preparationResult), initialFile)
					s.publishActivity(ctx, Activity{Kind: ActivityTool, Event: event, Phase: ActivityExecutingTool, StableCode: event.Detail, File: finalFile})
					events = append(events, event)
					continue
				}
				if s.FileAuthorizationMode() == FileAuthorizationConfirm {
					pending := pendingFileMutationFrom(call.ID, prepared)
					s.appendMu.Lock()
					s.pendingKind = pendingFileMutation
					s.pendingCalls = append([]modelclient.ToolCall(nil), calls...)
					s.pendingIndex = index
					s.pendingFileMutation = pending
					s.pendingPreparedMutation = prepared
					s.pendingEvents = append([]Event(nil), events...)
					s.pendingResolving = false
					s.appendMu.Unlock()
					event := Event{ID: call.ID, Tool: call.Function.Name, Summary: "等待用户授权文件修改", Status: EventConfirmationRequired}
					s.publishActivity(ctx, Activity{Kind: ActivityTool, Event: event, Phase: ActivityWaitingUser, File: fileActivityDetailFromPrepared(prepared)})
					return Result{Events: append(events, event), PendingFileMutation: pending}, nil
				}
				commitResult, event, stop, err := s.commitPreparedFileMutation(ctx, call, prepared)
				if err != nil {
					return Result{}, err
				}
				events = append(events, event)
				if stop || commitResult.Publication == workspace.PublicationUnknown {
					return s.fileMutationCompletionFallback(s.currentTurnID, events)
				}
				continue
			}
			if workspace.IsReadTool(call.Function.Name) && s.workspace != nil && s.workspaceStatus.Available {
				lastFile := initialFile
				toolCtx, cancel := context.WithTimeout(ctx, s.options.ToolTimeout)
				progressCtx := workspace.WithProgressReporter(toolCtx, func(progress workspace.Progress) {
					detail := fileActivityDetailFromProgress(progress)
					lastFile = mergeFileActivityDetail(detail, lastFile)
					s.publishActivity(toolCtx, Activity{
						Kind:  ActivityTool,
						Event: Event{ID: call.ID, Tool: call.Function.Name, Summary: workspaceProgressSummary(call.Function.Name, lastFile), Status: EventRunning},
						Phase: ActivityExecutingTool, File: lastFile,
					})
				})
				workspaceResult := s.workspace.Execute(progressCtx, call.Function.Name, call.Function.Arguments)
				toolErr := toolCtx.Err()
				cancel()
				if toolErr != nil {
					event := workspaceToolContextFailureEvent(call.ID, call.Function.Name, toolErr)
					s.publishActivity(ctx, Activity{Kind: ActivityTool, Event: event, Phase: ActivityStopped, StableCode: event.Detail, File: lastFile})
					return Result{}, preferContextError(ctx, toolErr)
				}
				if err := s.appendWorkspaceToolResult(call.Function.Name, call.ID, workspaceResult); err != nil {
					return Result{}, err
				}
				event := eventFromToolOutput(call.Function.Name, workspaceResult.Summary, workspaceResult.Value)
				event.ID = call.ID
				finalFile := mergeFileActivityDetail(fileActivityDetailFromResult(call.Function.Name, workspaceResult), lastFile)
				s.publishActivity(ctx, Activity{Kind: ActivityTool, Event: event, Phase: ActivityExecutingTool, StableCode: event.Detail, File: finalFile})
				events = append(events, event)
				continue
			}
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
	return Result{}, &continueAgentLoopError{events: events}
}

func (s *Session) resumeAfterCalls(ctx context.Context, calls []modelclient.ToolCall, start int, events []Event) (Result, error) {
	result, err := s.processCalls(ctx, calls, start, events)
	var continuation *continueAgentLoopError
	if errors.As(err, &continuation) {
		return s.run(ctx, continuation.events)
	}
	return result, err
}

func (s *Session) registerQuestion(args questionArgs) (*PendingQuestion, bool) {
	s.appendMu.Lock()
	defer s.appendMu.Unlock()
	turn := s.turns[s.activeTurnID]
	if turn == nil {
		return nil, false
	}
	if _, duplicate := turn.QuestionIDs[args.QuestionID]; duplicate {
		return nil, false
	}
	turn.QuestionsAsked++
	turn.QuestionIDs[args.QuestionID] = struct{}{}
	return pendingQuestionFromArgs(args), true
}
