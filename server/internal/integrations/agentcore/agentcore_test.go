package agentcore_test

import (
	"context"
	"errors"
	"testing"
	"time"

	core "github.com/edu-agent/edu-agent/packages/agentcore"
	m "github.com/edu-agent/edu-agent/packages/agentcore/modelclient"
)

// 该契约直接编译并运行公共核心；不启动 CLI，也不依赖 Web 研究或持久层。
func TestServerRunsSharedCoreWithApplicationToolOnly(t *testing.T) {
	executed := 0
	set, err := core.NewToolSet(time.Second, core.ToolRegistration{
		Definition: m.Tool{Type: "function", Function: m.ToolDefinition{Name: "learning_progress", Parameters: []byte(`{"type":"object","additionalProperties":false}`)}},
		Execute: func(_ context.Context, raw string) (core.ToolOutput, error) {
			var args struct{}
			if err := core.DecodeArguments(raw, &args); err != nil {
				return core.ToolOutput{}, err
			}
			executed++
			return core.ToolOutput{Content: `{"source":"正式应用命令的测试适配器","progress":0}`}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	adapter := &applicationAdapter{tools: set, messages: []m.Message{{Role: "user", Content: "查看进度"}}}
	runner := core.Runner[string]{Model: adapter, Context: adapter, History: historyAdapter{adapter}, Tools: adapter}
	if result, err := runner.Run(t.Context()); err != nil || result != "当前尚无学习进度" || executed != 1 {
		t.Fatalf("公共核心未完成服务端工具循环：%q，%d，%v", result, executed, err)
	}
	if _, err := set.Invoke(t.Context(), m.ToolCall{ID: "forged", Type: "function", Function: m.ToolFunction{Name: "shell", Arguments: `{}`}}); err == nil {
		t.Fatal("服务端工具集合意外开放 Shell")
	}
}

type applicationAdapter struct {
	tools    *core.ToolSet
	messages []m.Message
}

func (a *applicationAdapter) Complete(ctx context.Context, request m.Request) (m.Response, error) {
	if len(request.Tools) != 1 || request.Tools[0].Function.Name != "learning_progress" {
		return m.Response{}, errors.New("工具集合不匹配")
	}
	last := request.Messages[len(request.Messages)-1]
	if last.Role == "tool" {
		return m.Response{Message: m.Message{Role: "assistant", Content: "当前尚无学习进度"}}, nil
	}
	return m.Response{Message: m.Message{Role: "assistant", ToolCalls: []m.ToolCall{{ID: "read", Type: "function", Function: m.ToolFunction{Name: "learning_progress", Arguments: `{}`}}}}}, ctx.Err()
}
func (a *applicationAdapter) Prepare(context.Context) (core.ContextPlan, error) {
	return (core.ContextPlanner{ContextWindow: 4096, MaxTokens: 512, Estimator: core.NewTokenEstimator()}).Plan(a.messages, a.tools.Definitions(), nil)
}
func (a *applicationAdapter) ObserveUsage(core.ContextPlan, m.Usage) {}
func (a *applicationAdapter) AppendAssistant(_ context.Context, message m.Message) error {
	a.messages = append(a.messages, message)
	return nil
}

// Go 不支持方法重载，历史端口用独立适配器保持模型协议与存储协议明确。
type historyAdapter struct{ *applicationAdapter }

func (a historyAdapter) Complete(_ context.Context, message m.Message) (string, error) {
	a.messages = append(a.messages, message)
	return message.Content, nil
}

func (a *applicationAdapter) Execute(ctx context.Context, calls []m.ToolCall) (core.ToolStep[string], error) {
	for _, call := range calls {
		output, err := a.tools.Invoke(ctx, call)
		if err != nil {
			return core.ToolStep[string]{}, err
		}
		a.messages = append(a.messages, m.Message{Role: "tool", ToolCallID: call.ID, Content: output.Content})
	}
	return core.ToolStep[string]{Continue: true}, nil
}
