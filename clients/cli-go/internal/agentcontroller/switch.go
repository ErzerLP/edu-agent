package agentcontroller

import (
	"context"
	"errors"
	"strings"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/agentloop"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/agentsession"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/workspace"
)

// CommitSwitch performs target preflight against an independently opened store,
// publishes the current stable checkpoint, stops the old runtime, and only then
// exposes the new active Session.
func (c *Controller) CommitSwitch(ctx context.Context, plan SwitchPlan, confirmation SwitchConfirmation) (uint64, error) {
	c.mu.Lock()
	if plan.SameTarget && plan.SessionID == c.record.SessionID {
		generation := c.generation
		c.mu.Unlock()
		return generation, nil
	}
	gate := c.switchGateLocked()
	if !gate.Allowed || plan.controllerGeneration != c.generation || plan.currentSessionID != c.record.SessionID || plan.SessionID == "" {
		c.mu.Unlock()
		return 0, ErrSwitchUnavailable
	}
	if plan.NeedWorkspaceConfirm && !confirmation.Workspace {
		c.mu.Unlock()
		return 0, ErrWorkspaceConfirmationRequired
	}
	c.switching = true
	baseGeneration := c.generation
	currentWorkspace := c.record.WorkspaceRoot
	dependencies := Dependencies{
		Model: c.model, Server: c.server, Provider: c.provider, LoopOptions: c.loopOptions,
		WorkspaceRoot: currentWorkspace, Now: c.now,
	}
	store := c.store
	c.mu.Unlock()

	peer, err := store.Reopen(ctx)
	if err != nil {
		c.resetSwitching(baseGeneration)
		return 0, err
	}
	dependencies.Store = peer
	target, err := Resume(ctx, dependencies, ResumeOptions{
		SessionID: plan.SessionID, CurrentWorkspace: currentWorkspace, PrepareOnly: true,
		ConfirmWorkspace: func(WorkspaceBinding) (bool, error) { return confirmation.Workspace, nil },
	})
	if err != nil {
		c.resetSwitching(baseGeneration)
		return 0, err
	}
	target.mu.Lock()
	if target.record.RecordRevision != plan.ExpectedRevision {
		target.mu.Unlock()
		target.abort()
		c.resetSwitching(baseGeneration)
		return 0, agentsession.ErrCheckpointConflict
	}
	target.mu.Unlock()
	if target.providerBlocked && confirmation.Provider {
		if err := target.ConfirmProvider(ctx); err != nil {
			target.abort()
			c.resetSwitching(baseGeneration)
			return 0, err
		}
	}

	c.mu.Lock()
	if c.closed || c.generation != baseGeneration || !c.switching || c.record.SessionID != plan.currentSessionID {
		c.mu.Unlock()
		target.abort()
		return 0, ErrSwitchUnavailable
	}
	previousLifecycle := c.record.Lifecycle
	c.record.Lifecycle = "closed"
	if err := c.saveCheckpointLocked(ctx, false); err != nil {
		c.record.Lifecycle = previousLifecycle
		c.switching = false
		c.mu.Unlock()
		target.abort()
		return 0, err
	}
	c.mu.Unlock()

	target.mu.Lock()
	target.prepared = false
	if err := target.saveCheckpointLocked(ctx, target.dirty != nil); err != nil {
		target.mu.Unlock()
		target.abort()
		c.restoreCurrentAfterFailedSwitch(ctx, baseGeneration)
		return 0, err
	}
	target.mu.Unlock()

	generation, err := c.installPreparedTarget(target, baseGeneration)
	if err != nil {
		target.abort()
		c.restoreCurrentAfterFailedSwitch(ctx, baseGeneration)
		return 0, err
	}
	return generation, nil
}

func (c *Controller) NewSession(ctx context.Context) (uint64, error) {
	c.mu.Lock()
	gate := c.switchGateLocked()
	if !gate.Allowed {
		c.mu.Unlock()
		return 0, ErrSwitchUnavailable
	}
	c.switching = true
	baseGeneration := c.generation
	currentID := c.record.SessionID
	binding := WorkspaceBinding{
		Root: c.record.WorkspaceRoot, Label: c.record.WorkspaceLabel, PathHash: c.record.WorkspacePathHash,
		RootIdentityHash: c.record.WorkspaceRootIdentityHash,
	}
	loopOptions := c.loopOptions
	loopOptions.Workspace = nil
	loopOptions.WorkspaceStatus = workspace.Status{Code: workspace.CodeWorkspaceUnavailable}
	if binding.Root != "" {
		if actual, bindErr := BindWorkspace(binding.Root); bindErr == nil && (binding.RootIdentityHash == "" || actual.RootIdentityHash == binding.RootIdentityHash) {
			if opened, openErr := workspace.Open(binding.Root); openErr == nil {
				loopOptions.Workspace = opened
				loopOptions.WorkspaceStatus = opened.Status()
				binding.Available = true
			}
		}
	}
	peer, err := c.store.Reopen(ctx)
	if err != nil {
		c.switching = false
		c.mu.Unlock()
		if loopOptions.Workspace != nil {
			_ = loopOptions.Workspace.Close()
		}
		return 0, err
	}
	previousLifecycle := c.record.Lifecycle
	c.record.Lifecycle = "closed"
	if err := c.saveCheckpointLocked(ctx, false); err != nil {
		c.record.Lifecycle = previousLifecycle
		c.switching = false
		c.mu.Unlock()
		_ = peer.Close()
		if loopOptions.Workspace != nil {
			_ = loopOptions.Workspace.Close()
		}
		return 0, err
	}
	dependencies := Dependencies{
		Store: peer, Model: c.model, Server: c.server, Provider: c.provider, LoopOptions: loopOptions,
		WorkspaceRoot: binding.Root, WorkspaceBinding: &binding, Now: c.now,
	}
	c.mu.Unlock()

	target, err := Start(ctx, dependencies, false)
	if err != nil {
		c.restoreCurrentAfterFailedSwitch(ctx, baseGeneration)
		return 0, err
	}
	if !target.Status().Persistent {
		reason := target.Status().DegradedReason
		target.abort()
		c.restoreCurrentAfterFailedSwitch(ctx, baseGeneration)
		if strings.Contains(reason, "session_store_full") {
			return 0, agentsession.ErrStoreFull
		}
		return 0, agentsession.ErrCheckpointSaveFailed
	}
	c.mu.Lock()
	valid := !c.closed && c.switching && c.generation == baseGeneration && c.record.SessionID == currentID
	c.mu.Unlock()
	if !valid {
		target.abort()
		return 0, ErrSwitchUnavailable
	}
	generation, err := c.installPreparedTarget(target, baseGeneration)
	if err != nil {
		target.abort()
		c.restoreCurrentAfterFailedSwitch(ctx, baseGeneration)
		return 0, err
	}
	return generation, nil
}

func (c *Controller) installPreparedTarget(target *Controller, baseGeneration uint64) (uint64, error) {
	target.mu.Lock()
	if target.closed || target.loop == nil || target.handle == nil || target.store == nil {
		target.mu.Unlock()
		return 0, ErrSwitchUnavailable
	}
	if target.contextCancel != nil {
		target.contextCancel()
		target.contextCancel = nil
	}
	newLoop := target.loop
	if err := newLoop.SetDurabilitySink(c); err != nil {
		target.mu.Unlock()
		return 0, err
	}

	c.mu.Lock()
	if c.closed || !c.switching || c.generation != baseGeneration {
		c.mu.Unlock()
		target.mu.Unlock()
		return 0, ErrSwitchUnavailable
	}
	if c.titleCancel != nil {
		c.titleCancel()
		c.titleCancel = nil
	}
	if c.contextCancel != nil {
		c.contextCancel()
		c.contextCancel = nil
	}
	oldLoop, oldHandle, oldStore := c.loop, c.handle, c.store
	oldLoop.Close()

	c.loop, c.handle, c.store = newLoop, target.handle, target.store
	c.record, c.transcript, c.dirty = target.record, target.transcript, target.dirty
	c.loopOptions, c.workspaceRoot = target.loopOptions, target.record.WorkspaceRoot
	c.persistent, c.degradedReason, c.providerBlocked, c.resumed, c.prepared = target.persistent, target.degradedReason, target.providerBlocked, true, false
	c.notices, c.pendingUser, c.saveFailed = append([]string(nil), target.notices...), "", target.saveFailed
	c.generation++
	c.switching = false
	generation := c.generation

	target.loop, target.handle, target.store = nil, nil, nil
	target.persistent = false
	target.closed = true
	target.mu.Unlock()
	c.mu.Unlock()

	if oldHandle != nil {
		_ = oldHandle.Close()
	}
	if oldStore != nil {
		_ = oldStore.Close()
	}
	c.startContextWorker()
	return generation, nil
}

func (c *Controller) restoreCurrentAfterFailedSwitch(ctx context.Context, generation uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || c.generation != generation {
		return
	}
	c.record.Lifecycle = "active"
	if err := c.saveCheckpointLocked(ctx, false); err != nil {
		c.saveFailed = checkpointPersistenceError(err)
	}
	c.switching = false
}

func (c *Controller) resetSwitching(generation uint64) {
	c.mu.Lock()
	if !c.closed && c.generation == generation {
		c.switching = false
	}
	c.mu.Unlock()
}

var _ = errors.Is
var _ = agentloop.FileAuthorizationConfirm
