package agentcontroller

import (
	"context"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/localexec"
)

// Human terminal interaction is not a model tool call or a replayable chat
// operation. Capture the owner once: a switch cannot reroute accepted input to
// another Session. Nothing records the content in model history or dirty DTOs.
func (c *Controller) InputLocalTask(ctx context.Context, taskID string, content []byte) (localexec.InputResult, error) {
	manager, owner, err := c.localExecutionBinding()
	if err != nil {
		return localexec.InputResult{Outcome: "not_written"}, err
	}
	snapshot, err := manager.Status(owner, taskID)
	if err != nil {
		return localexec.InputResult{Outcome: "not_written"}, err
	}
	if !snapshot.PTY || !snapshot.Controllable {
		return localexec.InputResult{Outcome: "not_written"}, &localexec.Error{Code: "pty_unavailable"}
	}
	return manager.WriteInput(ctx, owner, taskID, content)
}

func (c *Controller) InterruptLocalTask(ctx context.Context, taskID string) (localexec.InputResult, error) {
	manager, owner, err := c.localExecutionBinding()
	if err != nil {
		return localexec.InputResult{Outcome: "not_written"}, err
	}
	return manager.Interrupt(ctx, owner, taskID)
}

func (c *Controller) EOFLocalTask(ctx context.Context, taskID string) (localexec.InputResult, error) {
	manager, owner, err := c.localExecutionBinding()
	if err != nil {
		return localexec.InputResult{Outcome: "not_written"}, err
	}
	return manager.SendEOF(ctx, owner, taskID)
}

func (c *Controller) ResizeLocalTask(taskID string, rows, cols int) (localexec.Snapshot, error) {
	manager, owner, err := c.localExecutionBinding()
	if err != nil {
		return localexec.Snapshot{}, err
	}
	return manager.Resize(owner, taskID, rows, cols)
}
