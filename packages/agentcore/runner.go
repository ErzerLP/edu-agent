// Package agentcore 提供不依赖终端或本机工具的模型循环与上下文预算。
package agentcore

import (
	"context"
	"errors"
	"strings"
	"unicode/utf8"

	"github.com/edu-agent/edu-agent/packages/agentcore/modelclient"
)

type Model interface {
	Complete(context.Context, modelclient.Request) (modelclient.Response, error)
}

type StreamingModel interface {
	Stream(context.Context, modelclient.Request, func(modelclient.StreamEvent) error) (modelclient.Response, error)
}

// ContextSource 读取已提交历史并构造有预算的请求。身份、系统规则与工具目录由宿主固定。
type ContextSource interface {
	Prepare(context.Context) (ContextPlan, error)
	ObserveUsage(ContextPlan, modelclient.Usage)
}

// HistorySink 由宿主实现提交的线性化点、持久化与取消语义，不执行历史工具。
// R 可包含宿主专有结果，避免核心依赖 CLI 工作区或服务端领域 DTO。
type HistorySink[R any] interface {
	AppendAssistant(context.Context, modelclient.Message) error
	Complete(context.Context, modelclient.Message) (R, error)
}

// ToolExecutor 只执行当前已校验响应。宿主负责参数解码、正式命令权限、
// 工具结果投影和副作用日志；交互暂停时返回 Continue=false 并保留待处理调用。
type ToolExecutor[R any] interface {
	Execute(context.Context, []modelclient.ToolCall) (ToolStep[R], error)
}

type ToolStep[R any] struct {
	Continue bool
	Result   R
}

// RoundBudget 属于单个用户轮次；暂停后沿用剩余额度。Limit=0 不限制轮数。
type RoundBudget struct {
	Limit     int
	Remaining int
}

func (b *RoundBudget) take() error {
	if b == nil || b.Limit == 0 {
		return nil
	}
	if b.Limit < 0 || b.Remaining <= 0 {
		return errors.New("Agent已达到用户配置的工具轮数保护值；将最大工具轮数设为0可取消该限制")
	}
	b.Remaining--
	return nil
}

type RunEventKind string

const (
	RunPreparing       RunEventKind = "preparing_context"
	RunWaitingModel    RunEventKind = "waiting_model"
	RunValidating      RunEventKind = "validating_response"
	RunContextFailed   RunEventKind = "context_prepare_failed"
	RunModelFailed     RunEventKind = "model_request_failed"
	RunInvalidResponse RunEventKind = "invalid_model_response"
	RunToolsReady      RunEventKind = "assembling_tools"
	RunCompleted       RunEventKind = "completed"
	RunStream          RunEventKind = "stream"
)

// RunEvent 是临时输出协议。流式正文（尤其可读推理）不能作为历史或授权保存。
type RunEvent struct {
	Kind            RunEventKind
	ReasoningEffort modelclient.ReasoningEffort
	Stream          modelclient.StreamEvent `json:"-"`
	Err             error                   `json:"-"`
}

type EventSink interface {
	Publish(context.Context, RunEvent)
}

// Runner 串行驱动一次 send 或交互恢复；不创建后台 worker，不拥有注入资源。
// 同一宿主会话的 send、resume、close 应由宿主串行化并通过 context 取消。
type Runner[R any] struct {
	Model   Model
	Context ContextSource
	History HistorySink[R]
	Tools   ToolExecutor[R]
	Events  EventSink
	Budget  *RoundBudget
}

func (r Runner[R]) Run(ctx context.Context) (R, error) {
	var zero R
	if r.Model == nil || r.Context == nil || r.History == nil {
		return zero, errors.New("agent core dependencies are incomplete")
	}
	for {
		if err := ctx.Err(); err != nil {
			return zero, err
		}
		if err := r.Budget.take(); err != nil {
			return zero, err
		}
		r.publish(ctx, RunEvent{Kind: RunPreparing})
		plan, err := r.Context.Prepare(ctx)
		if err != nil {
			r.publish(ctx, RunEvent{Kind: RunContextFailed, Err: err})
			return zero, err
		}
		r.publish(ctx, RunEvent{Kind: RunWaitingModel, ReasoningEffort: plan.Request.ReasoningEffort})
		response, err := r.foreground(ctx, plan.Request)
		if ctx.Err() != nil {
			return zero, ctx.Err()
		}
		if err != nil {
			r.publish(ctx, RunEvent{Kind: RunModelFailed, Err: err})
			return zero, err
		}
		r.publish(ctx, RunEvent{Kind: RunValidating})
		if response.Message.Role != "assistant" || !utf8.ValidString(response.Message.Content) {
			err := &modelclient.ClientError{Code: modelclient.ErrorCodeResponseProtocol, Message: "模型响应角色或 UTF-8 无效"}
			r.publish(ctx, RunEvent{Kind: RunInvalidResponse, Err: err})
			return zero, err
		}
		if err := ValidateModelMessage(response.Message); err != nil {
			r.publish(ctx, RunEvent{Kind: RunInvalidResponse, Err: err})
			return zero, err
		}
		recordUsage := func() {
			if response.Usage != nil {
				r.Context.ObserveUsage(plan, *response.Usage)
			}
		}
		if len(response.Message.ToolCalls) == 0 {
			if strings.TrimSpace(response.Message.Content) == "" {
				return zero, errors.New("模型没有返回可显示的回答")
			}
			result, err := r.History.Complete(ctx, response.Message)
			if err != nil {
				return zero, err
			}
			recordUsage()
			r.publish(ctx, RunEvent{Kind: RunCompleted})
			return result, nil
		}
		r.publish(ctx, RunEvent{Kind: RunToolsReady})
		if r.Tools == nil {
			return zero, errors.New("当前运行未注入工具执行器")
		}
		if err := r.History.AppendAssistant(ctx, response.Message); err != nil {
			return zero, err
		}
		recordUsage()
		step, err := r.Tools.Execute(ctx, response.Message.ToolCalls)
		if err != nil {
			return zero, err
		}
		if !step.Continue {
			return step.Result, nil
		}
	}
}

func (r Runner[R]) foreground(ctx context.Context, request modelclient.Request) (modelclient.Response, error) {
	streaming, ok := r.Model.(StreamingModel)
	if !ok {
		return r.Model.Complete(ctx, request)
	}
	return streaming.Stream(ctx, request, func(event modelclient.StreamEvent) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		switch event.Kind {
		case modelclient.StreamEventResponseStarted, modelclient.StreamEventTextDelta,
			modelclient.StreamEventReasoningDelta, modelclient.StreamEventResponseActivity,
			modelclient.StreamEventCompatibilityFallback:
			r.publish(ctx, RunEvent{Kind: RunStream, ReasoningEffort: request.ReasoningEffort, Stream: event})
			return nil
		default:
			return errors.New("模型返回未知流式事件")
		}
	})
}

func (r Runner[R]) publish(ctx context.Context, event RunEvent) {
	if r.Events == nil {
		return
	}
	// 与原 CLI 一致，展示接收器故障不能改变已经发生的业务结果。
	defer func() { _ = recover() }()
	r.Events.Publish(ctx, event)
}
