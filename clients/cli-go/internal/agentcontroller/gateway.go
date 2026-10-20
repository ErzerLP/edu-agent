package agentcontroller

import (
	"context"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/agentloop"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/id"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/modelgateway"
)

func (c *Controller) prepareGatewayTurn(ctx context.Context, loop *agentloop.Session) error {
	c.mu.Lock()
	gateway, ok := c.model.(*modelgateway.Model)
	if !ok {
		c.mu.Unlock()
		return nil
	}
	sessionID := c.record.GatewaySessionID
	space := c.loopOptions.LearningBinding.SpaceID
	c.mu.Unlock()
	turnID, err := id.NewUUID()
	if err != nil {
		return err
	}
	bound, err := gateway.Prepare(ctx, modelgateway.Identity{SessionID: sessionID, TurnID: turnID, SpaceID: space})
	if err != nil {
		return err
	}
	loop.SetGatewayModel(bound)
	c.mu.Lock()
	c.model = bound
	c.mu.Unlock()
	return nil
}
