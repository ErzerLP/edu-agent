package agentcontroller

import (
	"context"
	"fmt"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/agentcontext"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/agentloop"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/api"
)

func bindLearningDependencies(ctx context.Context, d *Dependencies, binding agentcontext.Binding) error {
	binding = binding.Normalize()
	if !binding.Valid() {
		return fmt.Errorf("invalid_agent_binding")
	}
	d.LoopOptions.LearningBinding = binding
	var client *agentcontext.Client
	switch server := d.Server.(type) {
	case *api.Client:
		client = agentcontext.New(server, binding)
	case *agentcontext.Client:
		client = server.Rebind(binding)
	default:
		return nil
	}
	d.Server = client
	base := d.Model
	if base == nil {
		return fmt.Errorf("Agent 模型未配置")
	}
	if guarded, ok := base.(*agentcontext.GuardedModel); ok {
		base = guarded.Base
	}
	d.Model = &agentcontext.GuardedModel{Base: base, Client: client}
	return nil
}

func (c *Controller) LearningBinding() agentcontext.Binding {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.record.LearningBinding.Normalize()
}

// 切学习区不隐式改变本地工作区；新聊天沿用原路径，失败时不回退 cwd。
func (c *Controller) LearningWorkspaceRoot() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.workspaceRoot
}

func (c *Controller) LearningDescription(ctx context.Context) (string, error) {
	c.mu.Lock()
	server, binding := c.server, c.record.LearningBinding
	c.mu.Unlock()
	if client, ok := server.(*agentcontext.Client); ok {
		return client.Description(ctx)
	}
	return binding.Label(), nil
}

func (c *Controller) ResolveLearningWorkflow(ctx context.Context, id string, outcome agentloop.WorkflowOutcome) (agentloop.Result, error) {
	loop, err := c.beginRuntimeOperation()
	if err != nil {
		return agentloop.Result{}, err
	}
	result, err := loop.ResolveLearningWorkflow(ctx, id, outcome)
	return c.finishOperation(ctx, result, err)
}
