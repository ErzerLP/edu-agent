package agentcontroller

import (
	"context"
	"errors"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/agentsession"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/fileeffects"
)

func (c *Controller) bindFileBatchesLocked() {
	if c.fileBatches == nil || !c.persistent || c.handle == nil {
		return
	}
	c.fileBatchErr = ""
	if err := c.fileBatches.Bind(context.Background(), c.artifactOwner, fileArtifactStore{c.handle}); err != nil {
		c.fileBatchErr = "file_batch_unavailable"
		var stable *fileeffects.BatchError
		if errors.As(err, &stable) {
			c.fileBatchErr = stable.Code
		}
		c.appendStatusNoticeLocked("[file_batch_unavailable] 复制分段日志无法安全加载；不把它当空历史，不会继续或重放旧复制。")
	}
}
func (c *Controller) FileBatchHistoryStatus() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.fileBatchErr
}

// The root executor is settled first when possible. A failed item journal
// then fences further side effects AND dirty consumption, retaining the root
// and the last confirmed segment prefix for an honest subsequent recovery.
func (c *Controller) FileBatchFailed(_ context.Context, callID string, cause error) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.persistent {
		return nil
	}
	if c.fileJournalErr != nil {
		return c.fileJournalErr
	}
	if c.dirty == nil || !c.fileCallExistsLocked(callID) {
		return c.failFileJournalLocked(agentsession.ErrCheckpointConflict)
	}
	if cause == nil {
		cause = agentsession.ErrInvalid
	}
	return c.failFileJournalLocked(cause)
}
