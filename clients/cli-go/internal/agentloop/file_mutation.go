package agentloop

import (
	"context"
	"errors"
	"strings"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/modelclient"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/workspace"
)

func pendingFileMutationFrom(callID string, prepared *workspace.PreparedMutation) *PendingFileMutation {
	if prepared == nil {
		return nil
	}
	value := prepared.Presentation
	return &PendingFileMutation{
		CallID: callID, Tool: value.Tool, Operation: value.Operation, Path: value.Path,
		PreviewKind: value.PreviewKind, Preview: value.Preview, Truncated: value.Truncated, BaseVersion: value.BaseVersion,
	}
}

func (s *Session) ResolveFileMutation(ctx context.Context, callID string, resolution FileMutationResolution) (Result, error) {
	if s.contextRuntime.isClosed() {
		return Result{}, ErrSessionClosed
	}
	s.appendMu.Lock()
	if s.pendingKind != pendingFileMutation || s.pendingFileMutation == nil || s.pendingPreparedMutation == nil ||
		len(s.pendingCalls) == 0 || s.pendingIndex >= len(s.pendingCalls) || s.pendingFileMutation.CallID != callID {
		s.appendMu.Unlock()
		return Result{}, errors.New("当前没有匹配的待授权文件修改")
	}
	if s.pendingResolving {
		s.appendMu.Unlock()
		return Result{}, errors.New("文件修改授权正在处理中")
	}
	if resolution != FileMutationApprove && resolution != FileMutationDecline {
		s.appendMu.Unlock()
		return Result{}, errors.New("文件修改授权决定无效")
	}
	s.pendingResolving = true
	turnID := s.activeTurnID
	calls := append([]modelclient.ToolCall(nil), s.pendingCalls...)
	index := s.pendingIndex
	events := append([]Event(nil), s.pendingEvents...)
	prepared := s.pendingPreparedMutation
	s.appendMu.Unlock()
	ctx = withActivityTurn(ctx, turnID)
	call := calls[index]

	if resolution == FileMutationDecline {
		result := workspace.MutationDenied(prepared)
		if err := s.appendWorkspaceToolResult(call.Function.Name, call.ID, result); err != nil {
			s.discardTurn(turnID)
			return Result{}, err
		}
		event := Event{ID: call.ID, Tool: call.Function.Name, Summary: "用户拒绝了文件修改", Status: EventFailed, Detail: workspace.CodeAuthorizationDenied}
		detail := fileActivityDetailFromPrepared(prepared)
		if detail != nil {
			detail.PublicationOutcome = string(workspace.PublicationUnchanged)
		}
		s.publishActivity(ctx, Activity{Kind: ActivityTool, Event: event, Phase: ActivityContinuingAfterTool, StableCode: event.Detail, File: detail})
		events = append(events, event)
		s.clearPendingAfterResolution()
		resultValue, err := s.processCalls(ctx, calls, index+1, events)
		if err == nil {
			return cloneResult(resultValue), nil
		}
		return s.finishAfterTurnFailure(turnID, events, preferContextError(ctx, err))
	}

	result, event, stop, err := s.commitPreparedFileMutation(ctx, call, prepared)
	if err != nil {
		s.discardTurn(turnID)
		return Result{}, err
	}
	events = append(events, event)
	s.clearPendingAfterResolution()
	if stop {
		return s.fileMutationCompletionFallback(turnID, events)
	}
	if result.Publication == workspace.PublicationUnknown {
		return s.fileMutationCompletionFallback(turnID, events)
	}
	continued, runErr := s.processCalls(ctx, calls, index+1, events)
	if runErr == nil {
		return cloneResult(continued), nil
	}
	return s.finishAfterTurnFailure(turnID, events, preferContextError(ctx, runErr))
}

func (s *Session) CancelPendingFileMutation(callID string) (Result, error) {
	if s.contextRuntime.isClosed() {
		return Result{}, ErrSessionClosed
	}
	s.appendMu.Lock()
	if s.pendingKind != pendingFileMutation || s.pendingFileMutation == nil || s.pendingFileMutation.CallID != callID {
		s.appendMu.Unlock()
		return Result{}, errors.New("当前没有匹配的待授权文件修改")
	}
	if s.pendingResolving {
		s.appendMu.Unlock()
		return Result{}, errors.New("文件修改授权正在处理中")
	}
	turnID := s.activeTurnID
	events := append([]Event(nil), s.pendingEvents...)
	hasEffect := false
	if turn := s.turns[turnID]; turn != nil {
		hasEffect = turn.FileEffectCallID != ""
	}
	s.appendMu.Unlock()
	if hasEffect {
		return s.fileMutationCompletionFallback(turnID, events)
	}
	s.discardTurn(turnID)
	return Result{}, nil
}

func (s *Session) commitPreparedFileMutation(ctx context.Context, call modelclient.ToolCall, prepared *workspace.PreparedMutation) (workspace.Result, Event, bool, error) {
	s.publishActivity(ctx, Activity{Kind: ActivityTool, Event: Event{ID: call.ID, Tool: call.Function.Name, Summary: "正在安全发布文件修改", Status: EventRunning}, Phase: ActivityExecutingTool, File: fileActivityDetailFromPrepared(prepared)})
	toolCtx, cancel := context.WithTimeout(ctx, s.options.ToolTimeout)
	result := s.workspace.CommitMutation(toolCtx, prepared)
	toolErr := toolCtx.Err()
	cancel()
	if result.Publication == workspace.PublicationUnchanged && toolErr != nil {
		return result, Event{}, false, preferContextError(ctx, toolErr)
	}
	if err := s.appendWorkspaceToolResult(call.Function.Name, call.ID, result); err != nil {
		return result, Event{}, false, err
	}
	if result.Publication == workspace.PublicationCompleted || result.Publication == workspace.PublicationUnknown {
		s.markFileEffect(call.ID, result.Publication == workspace.PublicationUnknown)
	}
	event := eventFromToolOutput(call.Function.Name, result.Summary, result.Value)
	event.ID = call.ID
	if result.Publication == workspace.PublicationUnknown {
		event.Status = EventOutcomeUnknown
		event.Detail = workspace.CodeOutcomeUnknown
	}
	s.publishActivity(ctx, Activity{
		Kind: ActivityTool, Event: event, Phase: ActivityExecutingTool, StableCode: event.Detail,
		File: mergePreparedFileActivity(fileActivityDetailFromResult(call.Function.Name, result), prepared),
	})
	stop := result.Publication == workspace.PublicationUnknown || result.Publication == workspace.PublicationCompleted && (ctx.Err() != nil || toolErr != nil)
	return result, event, stop, nil
}

func (s *Session) markFileEffect(callID string, unknown bool) {
	s.appendMu.Lock()
	turnID := s.activeTurnID
	if turnID == "" {
		turnID = s.currentTurnID
	}
	if turn := s.turns[turnID]; turn != nil {
		turn.FileEffectCallID = callID
		turn.FileEffectUnknown = unknown
		turn.Protected = true
		if unknown {
			turn.OutcomeUnknown = true
		}
	}
	s.appendMu.Unlock()
}

func (s *Session) finishAfterTurnFailure(turnID string, events []Event, err error) (Result, error) {
	if effect, _ := s.fileEffectState(turnID); effect {
		return s.fileMutationCompletionFallback(turnID, events)
	}
	s.discardTurn(turnID)
	return Result{}, err
}

func (s *Session) fileEffectState(turnID string) (bool, bool) {
	s.appendMu.Lock()
	defer s.appendMu.Unlock()
	turn := s.turns[turnID]
	if turn == nil || turn.FileEffectCallID == "" {
		return false, false
	}
	return true, turn.FileEffectUnknown
}

func (s *Session) fileMutationCompletionFallback(turnID string, events []Event) (Result, error) {
	s.appendMu.Lock()
	turn := s.turns[turnID]
	if turn == nil || turn.FileEffectCallID == "" {
		s.appendMu.Unlock()
		return Result{}, errors.New("文件修改副作用记录不可用")
	}
	unknown := turn.FileEffectUnknown
	s.sanitizeIncompleteToolCallsLocked(turnID)
	text := "文件修改已完成；后续处理已停止。"
	if unknown {
		text = "文件发布结果无法确认；后续处理已停止，请重新读取目标文件。"
	}
	message := modelclient.Message{Role: "assistant", Content: text}
	if err := s.appendCapturedMessageLocked(turnID, message, text, SourceAssistant, AuthoritySessionStatement, FreshnessSessionCurrent, nil); err != nil {
		s.appendMu.Unlock()
		return Result{}, err
	}
	s.finishSuccessfulTurnLocked()
	s.appendMu.Unlock()
	s.afterSuccessfulTurn()
	return Result{Text: text, Events: append([]Event(nil), events...)}, nil
}

func (s *Session) sanitizeIncompleteToolCallsLocked(turnID string) {
	completed := make(map[string]struct{})
	for index, message := range s.messages {
		if index < len(s.messageTurnIDs) && s.messageTurnIDs[index] == turnID && message.Role == "tool" {
			completed[message.ToolCallID] = struct{}{}
		}
	}
	messages := make([]modelclient.Message, 0, len(s.messages))
	turnIDs := make([]string, 0, len(s.messageTurnIDs))
	for index, message := range s.messages {
		messageTurnID := ""
		if index < len(s.messageTurnIDs) {
			messageTurnID = s.messageTurnIDs[index]
		}
		if messageTurnID == turnID && message.Role == "assistant" && len(message.ToolCalls) > 0 {
			filtered := make([]modelclient.ToolCall, 0, len(message.ToolCalls))
			for _, call := range message.ToolCalls {
				if _, exists := completed[call.ID]; exists {
					filtered = append(filtered, call)
				}
			}
			if len(filtered) == 0 && strings.TrimSpace(message.Content) == "" {
				continue
			}
			message.ToolCalls = filtered
		}
		messages = append(messages, message)
		turnIDs = append(turnIDs, messageTurnID)
	}
	s.messages = messages
	s.messageTurnIDs = turnIDs
}
