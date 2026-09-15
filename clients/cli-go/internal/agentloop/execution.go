package agentloop

import (
	"context"
	"errors"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/modelclient"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/workspace"
	core "github.com/edu-agent/edu-agent/packages/agentcore"
)

type continueAgentLoopError struct{ events []Event }

func (*continueAgentLoopError) Error() string { return "continue agent loop" }

// run 的生产调度唯一实现位于共享核心；适配器只连接 CLI 的状态与展示。
func (s *Session) run(ctx context.Context, events []Event) (Result, error) {
	adapter := &coreAdapter{session: s, events: events}
	budget := &core.RoundBudget{Limit: s.options.MaxToolRounds, Remaining: s.remaining}
	defer func() { s.remaining = budget.Remaining }()
	return (core.Runner[Result]{
		Model: s.model, Context: adapter, History: adapter, Tools: adapter, Events: adapter, Budget: budget,
	}).Run(ctx)
}

type coreAdapter struct {
	session    *Session
	events     []Event
	thinkingID string
	summary    string
	effort     modelclient.ReasoningEffort
}

func (a *coreAdapter) Prepare(context.Context) (core.ContextPlan, error) {
	plan, err := a.session.contextPlan()
	plan.Request.ReasoningEffort = a.effort
	return plan, err
}

func (a *coreAdapter) ObserveUsage(plan core.ContextPlan, usage modelclient.Usage) {
	a.session.estimator.ObserveActual(plan.EstimatedInput, usage)
	a.session.contextRuntime.UpdateUsageStatus(usage)
}

func (a *coreAdapter) AppendAssistant(_ context.Context, message modelclient.Message) error {
	return a.session.appendTurnMessage(a.session.currentTurnID, message)
}

func (a *coreAdapter) Complete(ctx context.Context, message modelclient.Message) (Result, error) {
	if err := a.session.commitFinalAnswer(ctx, message, message.Content); err != nil {
		return Result{}, err
	}
	return Result{Text: message.Content, Events: a.events}, nil
}

func (a *coreAdapter) Execute(ctx context.Context, calls []modelclient.ToolCall) (core.ToolStep[Result], error) {
	result, err := a.session.processCalls(ctx, calls, 0, a.events)
	var continuation *continueAgentLoopError
	if errors.As(err, &continuation) {
		a.events = continuation.events
		return core.ToolStep[Result]{Continue: true}, nil
	}
	return core.ToolStep[Result]{Result: result}, err
}

func (a *coreAdapter) Publish(ctx context.Context, event core.RunEvent) {
	s := a.session
	activity := Activity{Kind: ActivityThinking, Event: Event{ID: a.thinkingID, Status: EventRunning}}
	switch event.Kind {
	case core.RunPreparing:
		a.effort = s.frozenReasoningEffort()
		a.thinkingID = s.nextThinkingActivityID()
		a.summary = "正在分析问题"
		if len(a.events) > 0 {
			a.summary = "正在结合工具结果继续分析"
			s.publishActivity(ctx, Activity{Kind: ActivityThinking, Event: Event{ID: a.thinkingID, Summary: a.summary, Status: EventRunning}, Phase: ActivityContinuingAfterTool})
		}
		activity.Event.ID, activity.Event.Summary, activity.Phase = a.thinkingID, a.summary, ActivityPreparingContext
	case core.RunWaitingModel:
		activity.Event.Summary, activity.Phase, activity.ReasoningEffort = a.summary, ActivityWaitingModel, event.ReasoningEffort
	case core.RunContextFailed:
		activity.Event.Summary, activity.Event.Status, activity.Event.Detail = "上下文准备失败", EventFailed, "context_prepare_failed"
		activity.Phase, activity.StableCode = ActivityValidatingResponse, "context_prepare_failed"
	case core.RunModelFailed:
		code := stableActivityCode(event.Err, "model_request_failed")
		activity.Event.Summary, activity.Event.Status, activity.Event.Detail = "模型响应失败", EventFailed, code
		activity.Phase, activity.StableCode = ActivityValidatingResponse, code
	case core.RunValidating:
		activity.Event.Summary, activity.Phase = "正在校验模型响应", ActivityValidatingResponse
	case core.RunInvalidResponse:
		activity.Event.Summary, activity.Event.Status, activity.Event.Detail = "模型响应不符合协议", EventFailed, "invalid_model_response"
		activity.Phase, activity.StableCode = ActivityValidatingResponse, "invalid_model_response"
	case core.RunCompleted:
		activity.Event.Summary, activity.Event.Status, activity.Phase = "已完成回答组织", EventSucceeded, ActivityValidatingResponse
	case core.RunToolsReady:
		activity.Event.Summary, activity.Event.Status, activity.Phase = "已确定下一步工具操作", EventSucceeded, ActivityAssemblingTools
	case core.RunStream:
		switch event.Stream.Kind {
		case modelclient.StreamEventResponseStarted:
			activity.Event.Summary, activity.Phase, activity.ReasoningEffort = "正在接收模型响应", ActivityReceivingStream, event.ReasoningEffort
		case modelclient.StreamEventTextDelta:
			activity.Delta = safeActivityDelta(event.Stream.Text)
			if activity.Delta == "" {
				return
			}
			activity.Kind, activity.Event.Summary, activity.Phase, activity.ReasoningEffort = ActivityTextDelta, "正在生成回答", ActivityReceivingStream, event.ReasoningEffort
		case modelclient.StreamEventReasoningDelta:
			delta := safeActivityDelta(event.Stream.Text)
			if delta == "" {
				return
			}
			activity = Activity{Kind: ActivityReasoningDelta, Event: Event{ID: a.thinkingID}, Delta: delta}
		case modelclient.StreamEventResponseActivity:
			activity = Activity{Kind: ActivityResponseProgress, Event: Event{ID: a.thinkingID}, ReceivedAt: event.Stream.ReceivedAt}
		case modelclient.StreamEventCompatibilityFallback:
			activity.Event.Summary, activity.Event.Detail = "模型已切换到兼容响应模式", "stream_compatibility_fallback"
			activity.Phase, activity.ReasoningEffort, activity.StableCode = ActivityWaitingModel, event.ReasoningEffort, "stream_compatibility_fallback"
		}
	}
	s.publishActivity(ctx, activity)
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
		case "open_learning_workflow":
			result, err := s.beginLearningWorkflow(call, calls, index, events)
			if err != nil || result.Workflow != nil {
				return result, err
			}
			continue
		case "artifact":
			output := s.executeArtifactTool(ctx, call)
			if err := s.appendArtifactToolResult(call.ID, output); err != nil {
				return Result{}, err
			}
			event := Event{ID: call.ID, Tool: call.Function.Name, Summary: "已读取独立长结果；不代表文件修改已执行", Status: EventSucceeded}
			if output.Code != "" {
				event.Status, event.Detail = EventFailed, output.Code
			}
			s.publishActivity(ctx, Activity{Kind: ActivityTool, Event: event, Phase: ActivityExecutingTool, StableCode: event.Detail})
			events = append(events, event)
		case "shell", "task":
			output := s.executeLocalTool(ctx, call)
			if err := s.appendLocalToolResult(call.ID, output); err != nil {
				return Result{}, err
			}
			event := Event{ID: call.ID, Tool: call.Function.Name, Summary: "本地任务操作已返回；请依据任务状态继续", Status: EventSucceeded}
			if output.Code != "" {
				event.Status, event.Detail = EventFailed, output.Code
			}
			if output.Snapshot != nil {
				event.Summary = "本地任务 " + output.Snapshot.TaskID + "：" + output.Snapshot.State
			}
			s.publishActivity(ctx, Activity{Kind: ActivityTool, Event: event, Phase: ActivityExecutingTool, StableCode: event.Detail})
			events = append(events, event)
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
				diff, retentionErr := s.retainMutationArtifact(ctx, call.ID, prepared)
				if retentionErr != nil {
					prepared = nil
					preparationResult = workspace.Result{
						Publication: workspace.PublicationUnchanged,
						Summary:     "完整差异或计划未能保留；没有发布文件修改或清理",
						Value:       map[string]any{"error": artifactErrorCode(retentionErr), "publication": "unchanged"},
					}
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
				if s.FileAuthorizationMode() == FileAuthorizationConfirm || prepared.IsPurgeArchive() {
					pending := pendingFileMutationFrom(call.ID, prepared)
					if prepared.IsCopyTree() || prepared.IsPurgeArchive() {
						pending.PlanID, pending.PlanBytes, pending.PlanSaved = diff.ID, diff.Bytes, diff.Saved
					} else {
						pending.DiffID, pending.DiffBytes, pending.DiffSaved = diff.ID, diff.Bytes, diff.Saved
					}
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
