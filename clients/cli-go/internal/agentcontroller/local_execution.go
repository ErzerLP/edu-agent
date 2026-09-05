package agentcontroller

import (
	"context"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/agentloop"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/agentsession"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/localexec"
)

// BeforeLocalExecution records only an intent identity, never executable input.
// Unknown intent outcomes are intentionally not replayable after a crash.
func (c *Controller) BeforeLocalExecution(ctx context.Context, intent agentloop.LocalExecutionIntent) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || c.switching {
		return ErrSwitchUnavailable
	}
	if !c.persistent {
		return nil
	}
	if c.saveFailed != nil {
		return c.saveFailed
	}
	if err := c.ensureDirtyLocked(); err != nil {
		return err
	}
	if c.fileCallExistsLocked(intent.ToolCallID) || c.dirty.Preference != nil && c.dirty.Preference.ToolCallID == intent.ToolCallID {
		return agentsession.ErrCheckpointConflict
	}
	for _, previous := range c.record.PreferenceReceipts {
		if previous.ToolCallID == intent.ToolCallID {
			return agentsession.ErrCheckpointConflict
		}
	}
	for _, previous := range c.dirty.LocalEffects {
		if previous.ToolCallID == intent.ToolCallID {
			return agentsession.ErrCheckpointConflict
		}
	}
	if len(c.dirty.LocalEffects) >= c.limits.ReceiptCount {
		return agentsession.ErrStoreFull
	}
	candidate := *c.dirty
	candidate.MayHaveSideEffect = true
	candidate.LocalEffects = append(append([]agentsession.LocalEffectIntent(nil), candidate.LocalEffects...), agentsession.LocalEffectIntent{
		ToolCallID: intent.ToolCallID, Operation: intent.Operation, TaskID: intent.TaskID,
	})
	updated, err := c.handle.UpdateDirty(ctx, candidate)
	if err != nil {
		c.saveFailed = checkpointPersistenceError(err)
		return c.saveFailed
	}
	c.dirty = &updated
	return nil
}

// These optional UI methods do not enter the model loop and remain usable
// while it waits for a provider. Capturing owner under the lock isolates Sessions.
func (c *Controller) LocalExecutionAvailable() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.localExec != nil && !c.closed
}

func (c *Controller) localExecutionBinding() (*localexec.Manager, string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.localExec == nil || c.closed || c.switching {
		return nil, "", ErrSwitchUnavailable
	}
	return c.localExec, c.localOwner, nil
}

func (c *Controller) LocalTasks() ([]localexec.Snapshot, error) {
	manager, owner, err := c.localExecutionBinding()
	if err != nil {
		return nil, err
	}
	return manager.List(owner), nil
}

func (c *Controller) ReadLocalTask(taskID, stream string, offset int64, limit int) (localexec.OutputPage, error) {
	manager, owner, err := c.localExecutionBinding()
	if err != nil {
		return localexec.OutputPage{}, err
	}
	return manager.Read(owner, taskID, stream, offset, limit)
}

// Stopping is a safety operation: never require a provider, a current model
// turn, a successful history write, or renewed file authorization to stop it.
func (c *Controller) StopLocalTask(ctx context.Context, taskID string) (localexec.Snapshot, error) {
	manager, owner, err := c.localExecutionBinding()
	if err != nil {
		return localexec.Snapshot{}, err
	}
	return manager.Stop(ctx, owner, taskID)
}

var _ agentloop.LocalExecutionDurability = (*Controller)(nil)
