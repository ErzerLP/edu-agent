package agentloop

import (
	"context"
	"fmt"
	"strings"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/modelclient"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/modelgateway"
	core "github.com/edu-agent/edu-agent/packages/agentcore"
)

type toolStepPause struct {
	calls  []modelclient.ToolCall
	next   int
	events []Event
}

func (s *Session) ToolStepsPaused() bool {
	s.appendMu.Lock()
	defer s.appendMu.Unlock()
	return s.toolStepPause != nil
}

func (s *Session) toolStepsReached() bool {
	return s.options.MaxToolSteps > 0 && s.toolSteps >= s.options.MaxToolSteps
}

func (s *Session) pauseForToolSteps(ctx context.Context, calls []modelclient.ToolCall, next int, events []Event) (Result, error) {
	s.appendMu.Lock()
	s.toolStepPause = &toolStepPause{calls: append([]modelclient.ToolCall(nil), calls...), next: next, events: append([]Event(nil), events...)}
	s.appendMu.Unlock()
	// The cursor is retained before the summary; a failed summary cannot resume execution.
	text := fmt.Sprintf("本轮已执行 %d 次工具调用。进展说明暂时不可用，已有工具结果见上方。", s.toolSteps)
	plan, err := s.contextPlanFor(true)
	if err == nil {
		plan.Request.ReasoningEffort = s.frozenReasoningEffort()
		// Use the gateway's existing whole-request deadline, as foreground calls do.
		response, callErr := s.model.Complete(modelgateway.WithStage(ctx, "tool_limit_summary"), plan.Request)
		if callErr == nil && response.Message.Role == "assistant" && len(response.Message.ToolCalls) == 0 && strings.TrimSpace(response.Message.Content) != "" && core.ValidateModelMessage(response.Message) == nil {
			text = response.Message.Content
			if response.Usage != nil {
				s.estimator.ObserveActual(plan.EstimatedInput, *response.Usage)
				s.contextRuntime.UpdateUsageStatus(*response.Usage)
			}
		}
	}
	text += "\n\n本轮工具调用已达上限，已暂停。输入“继续”后重新计数并接着执行。"
	return Result{PausedToolSteps: true, Text: text, Events: events}, nil
}

func (s *Session) resumeToolSteps(ctx context.Context) (Result, error) {
	s.appendMu.Lock()
	pause := s.toolStepPause
	s.toolStepPause = nil
	turnID := s.activeTurnID
	s.appendMu.Unlock()
	s.toolSteps = 0
	result, err := s.resumeAfterCalls(withActivityTurn(ctx, turnID), pause.calls, pause.next, pause.events)
	if err != nil {
		return s.finishAfterTurnFailure(turnID, pause.events, preferContextError(ctx, err))
	}
	return cloneResult(result), nil
}

// Work on a projection: the original tool batch and cursor remain intact for resume.
func toolSummaryMessages(messages []modelclient.Message) []modelclient.Message {
	result := make([]modelclient.Message, 0, len(messages)+1)
	for index, message := range messages {
		if len(message.ToolCalls) > 0 {
			completed := make(map[string]bool)
			for next := index + 1; next < len(messages) && messages[next].Role == "tool"; next++ {
				completed[messages[next].ToolCallID] = true
			}
			calls := make([]modelclient.ToolCall, 0, len(message.ToolCalls))
			for _, call := range message.ToolCalls {
				if completed[call.ID] {
					calls = append(calls, call)
				}
			}
			message.ToolCalls = calls
			if len(calls) == 0 && message.Content == "" {
				continue
			}
		}
		result = append(result, message)
	}
	return append(result, modelclient.Message{Role: "user", Content: "本轮工具调用已达上限。请仅根据已有结果简短说明已完成的进展和待办；不要调用工具，不要声称未执行的操作已完成。随后等待用户输入“继续”。"})
}
