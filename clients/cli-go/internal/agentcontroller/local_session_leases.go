package agentcontroller

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/agentsession"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/localexec"
)

// A lease owns the handle AND its store. A prepared target may borrow a reference
// while the parked owner keeps the cross-process lock; abort never drops that
// owner's reference. Installation transfers references without reopening a lock.
type localSessionLease struct {
	mu        sync.Mutex
	refs      int
	handle    *agentsession.Handle
	store     *agentsession.Store
	storageID string
}

func newLocalSessionLease(handle *agentsession.Handle, store *agentsession.Store, storageID string) *localSessionLease {
	return &localSessionLease{refs: 1, handle: handle, store: store, storageID: storageID}
}

// retain is called only while the caller already owns a reference (including a
// parked map entry protected by Controller.mu).
func (l *localSessionLease) retain() {
	l.mu.Lock()
	l.refs++
	l.mu.Unlock()
}

func (l *localSessionLease) release() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.refs--
	if l.refs != 0 {
		return nil
	}
	return errors.Join(l.handle.Close(), l.store.Close())
}

func localTaskUnsettled(task localexec.Snapshot) bool {
	if task.Controllable {
		return true
	}
	switch task.State {
	case localexec.StateStarting, localexec.StateRunning, localexec.StateStopping, localexec.StateFinishing:
		return true
	default:
		return false
	}
}

func (c *Controller) localOwnerUnsettledLocked(owner string) bool {
	if c.localExec != nil {
		for _, task := range c.localExec.List(owner) {
			if localTaskUnsettled(task) {
				return true
			}
		}
	}
	return false
}

const localSessionLeaseReapInterval = 100 * time.Millisecond

// Only one reaper exists per client, and only while parked leases exist. The map
// is bounded by the manager's task budget. Starting and finishing are included:
// an exited shell alone does not prove that its supervisor has retired.
func (c *Controller) parkLocalSessionLocked(owner string, lease *localSessionLease) {
	if c.parkedLocalSessions == nil {
		c.parkedLocalSessions = make(map[string]*localSessionLease)
	}
	c.parkedLocalSessions[owner] = lease
	if c.localLeaseReaperCancel != nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	c.localLeaseReaperCancel = cancel
	go c.reapLocalSessionLeases(ctx)
}

func (c *Controller) reapLocalSessionLeases(ctx context.Context) {
	ticker := time.NewTicker(localSessionLeaseReapInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.mu.Lock()
			if c.closed {
				c.mu.Unlock()
				return
			}
			for owner, lease := range c.parkedLocalSessions {
				if !c.localOwnerUnsettledLocked(owner) {
					delete(c.parkedLocalSessions, owner)
					_ = lease.release()
				}
			}
			if len(c.parkedLocalSessions) == 0 {
				c.localLeaseReaperCancel = nil
				c.mu.Unlock()
				return
			}
			c.mu.Unlock()
		}
	}
}

// prepareSwitchTarget pins a parked lease before leaving Controller.mu. Load
// validates profile privacy generation just like OpenSession, without an unlock
// window. The normal resume pipeline still checks workspace/provider/checkpoint.
func (c *Controller) prepareSwitchTarget(ctx context.Context, dependencies Dependencies, options ResumeOptions) (*Controller, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	c.mu.Lock()
	if c.closed || !c.switching {
		c.mu.Unlock()
		return nil, ErrSwitchUnavailable
	}
	lease := c.parkedLocalSessions[options.SessionID]
	if lease != nil {
		lease.retain()
		c.mu.Unlock()
		loaded, err := lease.handle.Load()
		if err == nil {
			err = ctx.Err()
		}
		if err != nil {
			_ = lease.release()
			return nil, sessionStoreLoadError(err)
		}
		dependencies.Store = lease.store
		return resumeWithLocalSessionLease(ctx, dependencies, options, loaded, lease)
	}
	peer, err := c.store.Reopen(ctx)
	c.mu.Unlock()
	if err != nil {
		return nil, err
	}
	dependencies.Store = peer
	return Resume(ctx, dependencies, options)
}

// Caller cancellation may end Manager.Close's wait, not its cleanup. Never
// release Session locks or stores while that cleanup still uses their owner.
// The manager itself bounds stop/grace/kill/drain; preserve the original error.
func closeLocalExecutionManager(ctx context.Context, manager *localexec.Manager) error {
	err := manager.Close(ctx)
	var localErr *localexec.Error
	if errors.As(err, &localErr) && localErr.Code == "close_wait_canceled" {
		err = errors.Join(err, manager.Close(context.Background()))
	}
	return err
}

func (c *Controller) takeParkedLocalSessionsLocked() map[string]*localSessionLease {
	if c.localLeaseReaperCancel != nil {
		c.localLeaseReaperCancel()
		c.localLeaseReaperCancel = nil
	}
	parked := c.parkedLocalSessions
	c.parkedLocalSessions = nil
	return parked
}

func releaseLocalSessionResources(lease *localSessionLease, handle *agentsession.Handle, store *agentsession.Store) error {
	if lease != nil {
		return lease.release()
	}
	// Unsaved/degraded controllers can own a store without a Session handle.
	return errors.Join(handle.Close(), store.Close())
}
