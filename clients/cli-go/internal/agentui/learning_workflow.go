package agentui

import (
	"context"
	"io"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/agentcontext"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/agentcontroller"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/agentloop"
)

type WorkflowRunner func(context.Context, agentloop.LearningWorkflow, io.Reader, io.Writer) agentloop.WorkflowOutcome
type learningWorkflowConversation interface {
	ResolveLearningWorkflow(context.Context, string, agentloop.WorkflowOutcome) (agentloop.Result, error)
}

type NavigateLearningContext struct {
	Binding       agentcontext.Binding
	NoSave        bool
	WorkspaceRoot string
}

func (*NavigateLearningContext) Error() string {
	return "切换至用户明确选择的学习上下文"
}

type workflowMessage struct {
	generation uint64
	id         string
	outcome    agentloop.WorkflowOutcome
}
type workflowCommand struct {
	ctx    context.Context
	flow   agentloop.LearningWorkflow
	run    WorkflowRunner
	in     io.Reader
	out    io.Writer
	result agentloop.WorkflowOutcome
}

func (c *workflowCommand) SetStdin(in io.Reader)   { c.in = in }
func (c *workflowCommand) SetStdout(out io.Writer) { c.out = out }
func (c *workflowCommand) SetStderr(io.Writer)     {}
func (c *workflowCommand) Run() error              { c.result = c.run(c.ctx, c.flow, c.in, c.out); return nil }

func (m model) learningWorkflowKey(key string) (tea.Model, tea.Cmd) {
	flow := *m.pendingWorkflow
	if key == "esc" {
		return m.resolveLearningWorkflow(flow.CallID, agentloop.WorkflowOutcome{Status: "cancelled"})
	}
	if key != "enter" {
		return m, nil
	}
	if m.workflowRunner == nil {
		return m.resolveLearningWorkflow(flow.CallID, agentloop.WorkflowOutcome{Status: "unavailable", Code: "workflow_ui_unavailable"})
	}
	m.status = "正在正式页面操作；退出后回到原聊天"
	c := &workflowCommand{ctx: m.ctx, flow: flow, run: m.workflowRunner}
	generation := m.generation
	return m, tea.Exec(c, func(err error) tea.Msg {
		if err != nil {
			c.result = agentloop.WorkflowOutcome{Status: "failed", Code: "workflow_ui_failed"}
		}
		return workflowMessage{generation: generation, id: flow.CallID, outcome: c.result}
	})
}

func (m model) resolveLearningWorkflow(id string, outcome agentloop.WorkflowOutcome) (tea.Model, tea.Cmd) {
	if id == "" {
		m.pendingWorkflow = nil
		if outcome.Navigate != nil {
			m.navigation = &NavigateLearningContext{Binding: *outcome.Navigate}
			return m, tea.Quit
		}
		m.status = "已返回原学习上下文：" + outcome.Status
		return m, nil
	}
	c, ok := m.session.(learningWorkflowConversation)
	if !ok {
		m.status = "当前会话不支持正式流程"
		return m, nil
	}
	m.pendingWorkflow = nil
	return m.beginTurn(turnKind("workflow"), true, "", nil, nil, "", func(ctx context.Context) (agentloop.Result, error) {
		return c.ResolveLearningWorkflow(ctx, id, outcome)
	})
}

func (m model) chooseLearningContext() (tea.Model, tea.Cmd) {
	if m.busy || m.pending != nil || m.pendingQuestion != nil || m.pendingFileMutation != nil || m.pendingWorkflow != nil {
		m.status = "请先完成或取消当前请求与批准"
		return m, nil
	}
	if m.manager != nil {
		gate := m.manager.SwitchGate()
		if !gate.Allowed && gate.Code != agentcontroller.SwitchBlockUnsaved {
			m.status = gate.Reason
			return m, nil
		}
	}
	bound, ok := m.session.(interface{ LearningBinding() agentcontext.Binding })
	if !ok || m.workflowRunner == nil {
		m.status = "学习上下文选择不可用"
		return m, nil
	}
	m.pendingWorkflow = &agentloop.LearningWorkflow{Kind: "select_context", Binding: bound.LearningBinding()}
	return m.learningWorkflowKey("enter")
}
