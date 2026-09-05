package agentcontroller

import (
	"context"
	"errors"
	"fmt"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/agentloop"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/agentsession"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/workspace"
)

func dirtyFileEntries(marker agentsession.DirtyMarker) []agentsession.FileJournalEntry {
	entries := append([]agentsession.FileJournalEntry(nil), marker.FileJournal...)
	if marker.File != nil {
		entries = append(entries, agentsession.FileJournalEntry{WriteAhead: *marker.File})
	}
	return entries
}

func journalReceipt(entry agentsession.FileJournalEntry) (agentsession.FileReceipt, bool) {
	if entry.Unchanged {
		return agentsession.FileReceipt{}, false
	}
	if entry.Result != nil {
		return *entry.Result, true
	}
	return agentsession.FileReceipt{ToolCallID: entry.WriteAhead.ToolCallID, Effect: entry.WriteAhead.Effect,
		InvalidateObserved: true, StableCode: agentsession.FilePublicationUnknownCode, Outcome: agentsession.NoticeOutcomeUnknown}, true
}

func (c *Controller) failFileJournalLocked(err error) error {
	c.fileJournalErr = checkpointPersistenceError(err)
	c.saveFailed = c.fileJournalErr
	return c.fileJournalErr
}

func (c *Controller) updateFileJournalLocked(ctx context.Context, candidate agentsession.DirtyMarker) error {
	updated, err := c.handle.UpdateDirty(ctx, candidate)
	if err != nil {
		return c.failFileJournalLocked(err)
	}
	c.dirty = &updated
	return nil
}

func (c *Controller) fileCallExistsLocked(callID string) bool {
	for _, entry := range dirtyFileEntries(*c.dirty) {
		if entry.WriteAhead.ToolCallID == callID {
			return true
		}
	}
	for _, receipt := range c.record.FileReceipts {
		if receipt.ToolCallID == callID {
			return true
		}
	}
	return false
}

// AfterFilePublication is called synchronously by the Loop with the actual
// executor result. The active marker binds the turn, and its current WAL binds
// the call and frozen plan. No model message, partial event or unstable
// checkpoint is allowed to manufacture settlement evidence.
func (c *Controller) AfterFilePublication(ctx context.Context, callID string, result workspace.Result) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.persistent {
		return nil
	}
	if err := c.ensureDirtyLocked(); err != nil {
		return err
	}
	if c.dirty.File == nil || c.dirty.File.ToolCallID != callID {
		return c.failFileJournalLocked(agentsession.ErrCheckpointConflict)
	}
	entry := agentsession.FileJournalEntry{WriteAhead: *c.dirty.File}
	if result.Publication == workspace.PublicationUnchanged {
		// A no-effect executor result needs no file receipt. Keep the settled
		// identity until checkpoint so the same call cannot be executed twice.
		if result.Effect != nil {
			return c.failFileJournalLocked(agentsession.ErrInvalid)
		}
		entry.Unchanged = true
	} else {
		receipt, err := fileReceiptFromExecution(entry.WriteAhead, result)
		if err != nil {
			return c.failFileJournalLocked(err)
		}
		entry.Result = &receipt
	}
	candidate := *c.dirty
	candidate.File = nil
	candidate.FileJournal = append(append([]agentsession.FileJournalEntry(nil), candidate.FileJournal...), entry)
	return c.updateFileJournalLocked(ctx, candidate)
}

func fileReceiptFromExecution(wal agentsession.FileWriteAhead, result workspace.Result) (agentsession.FileReceipt, error) {
	if result.Publication != workspace.PublicationCompleted && result.Publication != workspace.PublicationUnknown {
		return agentsession.FileReceipt{}, agentsession.ErrInvalid
	}
	unknown := result.Publication == workspace.PublicationUnknown
	ref := result.Reference
	if ref == nil || ref.Path != wal.Effect.ReferencePath() || ref.Kind != wal.Effect.ReferenceKind() {
		return agentsession.FileReceipt{}, errors.New("文件结算缺少匹配的执行器引用")
	}
	effect := wal.Effect
	if result.Effect != nil {
		if result.Effect.Validate() != nil || !effect.SamePlan(*result.Effect) {
			return agentsession.FileReceipt{}, errors.New("文件结算结果与预写计划不一致")
		}
		effect = *result.Effect
	} else if effect.Operation == workspace.ToolMkdir || effect.Operation == workspace.ToolCopy || effect.Operation == workspace.ToolMove {
		return agentsession.FileReceipt{}, errors.New("文件结算缺少完整执行器副作用事实")
	}
	if effect.Operation == workspace.ToolCopy && !unknown && (effect.Target.Version == "" || effect.Target.Version != ref.ContentHash) {
		return agentsession.FileReceipt{}, errors.New("复制结算缺少匹配的实际目标哈希")
	}
	if effect.Operation == workspace.ToolMove && (ref.ContentHash != "" || !ref.InvalidateObserved) {
		return agentsession.FileReceipt{}, errors.New("移动结算不能伪造目标版本或省略双端失效")
	}
	if !unknown && effect.Operation != workspace.ToolArchive && effect.Operation != workspace.ToolMkdir && effect.Operation != workspace.ToolMove {
		effect.Target.Version = ref.ContentHash
	}
	r := agentsession.FileReceipt{ToolCallID: wal.ToolCallID, Effect: effect, InvalidateObserved: ref.InvalidateObserved,
		StableCode: agentsession.FilePublicationCompletedCode, Outcome: agentsession.NoticeOutcomeCompleted}
	if unknown {
		r.Effect.Target.Version = ""
		r.InvalidateObserved = true
		r.StableCode, r.Outcome = agentsession.FilePublicationUnknownCode, agentsession.NoticeOutcomeUnknown
	}
	return r, nil // UpdateDirty validates the receipt and WAL again before I/O.
}

// Only already-checkpointed receipts may age out. At most ReceiptCount pending
// identities are admitted; appending this whole batch therefore retains every
// not-yet-checkpointed fact, including unresolved WALs on ordinary failures.
func (c *Controller) mergeFileJournalLocked(marker agentsession.DirtyMarker) error {
	entries := dirtyFileEntries(marker)
	if len(entries) > c.limits.ReceiptCount {
		return agentsession.ErrStoreFull
	}
	for _, entry := range entries {
		if receipt, ok := journalReceipt(entry); ok {
			c.record.FileReceipts = appendBoundedFile(c.record.FileReceipts, receipt, c.limits.ReceiptCount)
		}
	}
	return nil
}

func fileJournalRecoveryLabel(receipt agentsession.FileReceipt) string {
	if receipt.Outcome == agentsession.NoticeOutcomeUnknown {
		return fileEffectRecoveryLabel(receipt.Effect)
	}
	return fmt.Sprintf("文件操作已完成：%s %s → %s；已保留实际结果，不会恢复重放", receipt.Effect.Operation, receipt.Effect.Source.Path, receipt.Effect.Target.Path)
}

var _ agentloop.DurabilitySink = (*Controller)(nil)
