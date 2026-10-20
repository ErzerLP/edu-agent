package agentcontroller

import (
	"context"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/agentcontext"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/agentloop"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/id"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/modelgateway"
)

func gatewayModel(model agentloop.Model) *modelgateway.Model {
	if guarded, ok := model.(*agentcontext.GuardedModel); ok {
		model = guarded.Base
	}
	gateway, _ := model.(*modelgateway.Model)
	return gateway
}

func (c *Controller) prepareGatewayTurn(ctx context.Context, loop *agentloop.Session) error {
	c.mu.Lock()
	current := c.model
	gateway := gatewayModel(current)
	if gateway == nil {
		c.mu.Unlock()
		return nil
	}
	sessionID := c.record.GatewaySessionID
	space := c.loopOptions.LearningBinding.SpaceID
	c.mu.Unlock()
	turnID := gateway.Identity.TurnID
	if !loop.ToolStepsPaused() {
		var err error
		turnID, err = id.NewUUID()
		if err != nil {
			return err
		}
	}
	bound, err := gateway.Prepare(ctx, modelgateway.Identity{SessionID: sessionID, TurnID: turnID, SpaceID: space})
	if err != nil {
		return err
	}
	var next agentloop.Model = bound
	if guarded, ok := current.(*agentcontext.GuardedModel); ok {
		next = &agentcontext.GuardedModel{Base: bound, Client: guarded.Client}
	}
	loop.SetGatewayModel(next, bound)
	c.mu.Lock()
	c.model = next
	c.mu.Unlock()
	return nil
}
