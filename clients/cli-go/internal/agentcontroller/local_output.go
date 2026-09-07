package agentcontroller

import (
	"context"
	"errors"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/agentsession"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/localexec"
)

// The adapter owns no key or lease and never calls back into Controller. The
// session lease keeps its handle alive through the task's final output write.
type localOutputStore struct{ handle *agentsession.Handle }

func localOutputError(err error) error {
	if err == nil {
		return nil
	}
	code := "output_save_failed"
	switch {
	case errors.Is(err, agentsession.ErrStoreFull):
		code = "output_store_full"
	case errors.Is(err, agentsession.ErrCorrupt):
		code = "output_corrupt"
	case errors.Is(err, agentsession.ErrVersionUnsupported):
		code = "output_version_unsupported"
	case errors.Is(err, agentsession.ErrNotFound), errors.Is(err, agentsession.ErrPrivacyInvalidated), errors.Is(err, agentsession.ErrKeyUnavailable):
		code = "output_unavailable"
	}
	return &localexec.Error{Code: code}
}

func (s localOutputStore) CheckArtifactAccess(ctx context.Context) error {
	return localOutputError(s.handle.CheckArtifactAccess(ctx))
}
func (s localOutputStore) ReadArtifact(ctx context.Context, name string) ([]byte, error) {
	data, err := s.handle.ReadArtifact(ctx, name)
	return data, localOutputError(err)
}
func (s localOutputStore) WriteArtifact(ctx context.Context, name string, data []byte) error {
	return localOutputError(s.handle.WriteArtifact(ctx, name, data))
}
func (s localOutputStore) ListArtifacts(ctx context.Context, prefix string) ([]string, error) {
	items, err := s.handle.ListArtifacts(ctx, prefix)
	return items, localOutputError(err)
}

// Called only when construction/preflight is complete, or after the switch
// commit. A failed target must never replace a live owner's backend binding.
func (c *Controller) bindLocalOutputLocked() {
	if c.localExec == nil || !c.persistent || c.handle == nil {
		return
	}
	c.localOutputErr = ""
	if err := c.localExec.BindArchive(c.localOwner, localOutputStore{handle: c.handle}); err != nil {
		c.localOutputErr = "output_history_unavailable"
		var localErr *localexec.Error
		if errors.As(err, &localErr) {
			c.localOutputErr = localErr.Code
		}
		c.appendStatusNoticeLocked("[output_unavailable] 历史任务产物无法安全加载；不能把缺失列表当作没有历史任务。当前命令输出保存能力需核对，不会重跑旧命令。")
		return
	}
	if c.resumed {
		c.appendStatusNoticeLocked("历史任务仅恢复已认证的输出和结算快照；未保存最终结算的任务为未知，不恢复进程控制，不自动重跑。F5 可查看已保留范围。")
	}
}

// LocalOutputStatus is metadata only: an empty live task list must not hide a
// failed historical catalog load. It never blocks safe access to live tasks.
func (c *Controller) LocalOutputStatus() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.localOutputErr
}

func (c *Controller) SearchLocalTask(ctx context.Context, taskID, stream, needle string, offset int64, limit int) (localexec.SearchPage, error) {
	manager, owner, err := c.localExecutionBinding()
	if err != nil {
		return localexec.SearchPage{}, err
	}
	return manager.Search(ctx, owner, taskID, stream, needle, offset, limit)
}
