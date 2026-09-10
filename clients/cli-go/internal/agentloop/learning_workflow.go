package agentloop

import (
	"context"
	"fmt"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/agentcontext"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/api"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/modelclient"
)

type LearningWorkflow struct {
	CallID  string
	Kind    string `json:"workflow"`
	Binding agentcontext.Binding
}

// WorkflowOutcome 仅由客户端正式页面生成，模型参数中不存在批准字段。
type WorkflowOutcome struct {
	Status   string                `json:"status"`
	Code     string                `json:"code,omitempty"`
	Data     any                   `json:"data,omitempty"`
	Navigate *agentcontext.Binding `json:"-"`
}

func (s *Session) beginLearningWorkflow(call modelclient.ToolCall, calls []modelclient.ToolCall, index int, events []Event) (Result, error) {
	var args struct {
		Kind string `json:"workflow"`
	}
	err := decodeArguments(call.Function.Arguments, &args)
	if err != nil || (args.Kind != "import" && args.Kind != "goal" && args.Kind != "planning" && args.Kind != "select_context") {
		if err := s.appendSessionToolResult(call.Function.Name, call.ID, toolError("invalid_arguments")); err != nil {
			return Result{}, err
		}
		return Result{}, nil
	}
	flow := &LearningWorkflow{CallID: call.ID, Kind: args.Kind, Binding: s.options.LearningBinding.Normalize()}
	s.appendMu.Lock()
	s.pendingKind = pendingLearningWorkflow
	s.pendingCalls, s.pendingIndex, s.pendingEvents = append([]modelclient.ToolCall(nil), calls...), index, append([]Event(nil), events...)
	s.pendingWorkflow = flow
	s.appendMu.Unlock()
	event := Event{ID: call.ID, Tool: call.Function.Name, Status: EventConfirmationRequired, Summary: "等待用户打开正式流程；尚未修改业务数据"}
	return Result{Events: append(events, event), Workflow: flow}, nil
}

func (s *Session) ResolveLearningWorkflow(ctx context.Context, callID string, outcome WorkflowOutcome) (Result, error) {
	s.appendMu.Lock()
	if s.pendingKind != pendingLearningWorkflow || s.pendingWorkflow == nil || s.pendingWorkflow.CallID != callID || s.pendingResolving {
		s.appendMu.Unlock()
		return Result{}, fmt.Errorf("正式流程已失效或不属于当前会话")
	}
	s.pendingResolving = true
	calls, index, events := append([]modelclient.ToolCall(nil), s.pendingCalls...), s.pendingIndex, append([]Event(nil), s.pendingEvents...)
	turnID := s.activeTurnID
	s.appendMu.Unlock()
	call := calls[index]
	if err := s.appendToolResult(call.Function.Name, call.ID, outcome); err != nil {
		return s.finishAfterTurnFailure(turnID, events, err)
	}
	event := Event{ID: call.ID, Tool: call.Function.Name, Status: EventSucceeded, Summary: "正式流程返回：" + outcome.Status, Detail: outcome.Code}
	if outcome.Status == "failed" || outcome.Status == "unavailable" {
		event.Status = EventFailed
	}
	events = append(events, event)
	s.publishActivity(ctx, Activity{Kind: ActivityTool, Event: event, Phase: ActivityContinuingAfterTool})
	s.clearPendingAfterResolution()
	if outcome.Navigate != nil {
		s.appendMu.Lock()
		s.sanitizeIncompleteToolCallsLocked(s.currentTurnID)
		s.appendMu.Unlock()
		text := "已明确选择新的学习上下文，将进入对应聊天。原会话保留原绑定。"
		if err := s.commitFinalAnswer(ctx, modelclient.Message{Role: "assistant", Content: text}, text); err != nil {
			return Result{}, err
		}
		return Result{Text: text, Events: events, Navigate: outcome.Navigate}, nil
	}
	result, err := s.resumeAfterCalls(withActivityTurn(ctx, turnID), calls, index+1, events)
	if err != nil {
		return s.finishAfterTurnFailure(turnID, events, preferContextError(ctx, err))
	}
	return cloneResult(result), nil
}

func (s *Session) readLearningContext(ctx context.Context, call modelclient.ToolCall) (any, string) {
	c, ok := s.server.(*agentcontext.Client)
	if !ok {
		return toolError("learning_context_unavailable"), "正式学习上下文不可用"
	}
	if _, _, err := c.Validate(ctx); err != nil {
		return toolFailure(err, "learning_context_unavailable"), "学习上下文已失效"
	}
	switch call.Function.Name {
	case "list_learning_goals":
		var args struct {
			Cursor string `json:"cursor"`
		}
		if decodeArguments(call.Function.Arguments, &args) != nil || len(args.Cursor) > 4096 {
			return toolError("invalid_arguments"), "目标查询参数无效"
		}
		v, err := c.Goals(ctx, "", "", args.Cursor, 20)
		if err != nil {
			return toolFailure(err, "goals_unavailable"), "目标查询失败"
		}
		return v, "已读取本区目标；绑定须由用户明确选择"
	case "get_bound_goal":
		if requireEmptyArguments(call.Function.Arguments) != nil {
			return toolError("invalid_arguments"), "目标查询参数无效"
		}
		if c.Binding.GoalID == "" {
			return toolError("goal_selection_required"), "请打开正式目标选择流程"
		}
		v, err := c.Goal(ctx, c.Binding.GoalID)
		if err != nil {
			return toolFailure(err, "goal_unavailable"), "目标查询失败"
		}
		return v, "已读取绑定目标及结构化草稿"
	case "get_planning_drafts":
		if requireEmptyArguments(call.Function.Arguments) != nil {
			return toolError("invalid_arguments"), "规划查询参数无效"
		}
		if c.Binding.GoalID == "" {
			return toolError("goal_selection_required"), "请打开正式目标选择流程"
		}
		v, err := c.PlanningList(ctx, c.Binding.GoalID)
		if err != nil {
			return toolFailure(err, "planning_unavailable"), "规划查询失败"
		}
		if v == nil {
			v = []api.PlanningDraft{}
		}
		return map[string]any{"items": v}, "已读取正式规划草稿和采用结果"
	}
	return toolError("unknown_tool"), "未知学习工具"
}
