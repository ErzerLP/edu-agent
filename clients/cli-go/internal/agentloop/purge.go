package agentloop

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/fileeffects"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/modelclient"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/securefile"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/workspace"
)

type purgeWorkspace interface {
	CommitPurge(context.Context, *workspace.PreparedMutation, securefile.PurgeObserver) (workspace.Result, securefile.PurgeResult)
}

func purgeBatchPlan(p *workspace.PreparedMutation) fileeffects.BatchPlan {
	items := p.PurgeItems()
	plan := fileeffects.BatchPlan{Root: p.FileEffect(), Items: make([]fileeffects.BatchItem, len(items))}
	for i, item := range items {
		entry := fileeffects.BatchItem{Source: fileeffects.Endpoint{Path: item.Path, Kind: string(item.Kind), Version: item.Version}, Target: fileeffects.Endpoint{Path: item.Path, Kind: "absent"}}
		if item.Kind == securefile.EntryFile {
			entry.Bytes = item.Size
		}
		plan.Items[i] = entry
	}
	return plan
}

func purgeItemCode(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, context.Canceled):
		return "cancelled"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, securefile.ErrChanged):
		return "content_changed"
	case errors.Is(err, securefile.ErrNotFound):
		return "not_found"
	case errors.Is(err, securefile.ErrCrossDevice):
		return "purge_cross_mount"
	case errors.Is(err, securefile.ErrOutcomeUnknown):
		return "outcome_unknown"
	default:
		return "purge_item_failed"
	}
}

func purgeNotStarted(p *workspace.PreparedMutation, code string) workspace.Result {
	value := map[string]any{"operation": workspace.ToolPurgeArchive, "path": p.Presentation.Path, "entry_type": p.Presentation.EntryKind, "publication_outcome": "unchanged", "purge_state": "not_started", "error": code, "code": code, "replay": false, "physical_bytes_reclaimed": "unknown"}
	if code == "file_batch_call_recorded" {
		value["operation_outcome"] = "unknown"
	}
	return workspace.Result{Publication: workspace.PublicationUnchanged, Summary: "永久归档清理未开始：" + code, Value: value}
}

func (s *Session) commitPurgeMutation(ctx context.Context, call modelclient.ToolCall, p *workspace.PreparedMutation) (workspace.Result, Event, bool, error) {
	executor, ok := s.workspace.(purgeWorkspace)
	manager, owner := s.options.FileBatches, s.options.ArtifactOwner
	if !ok || manager == nil {
		return s.finishCopyTreeResult(ctx, call, p, purgeNotStarted(p, "file_batch_unavailable"), nil, nil)
	}
	if state, ok := s.options.Durability.(interface{ FileBatchHistoryStatus() string }); ok {
		if code := state.FileBatchHistoryStatus(); code != "" {
			return s.finishCopyTreeResult(ctx, call, p, purgeNotStarted(p, code), nil, nil)
		}
	}
	if manager.HasCall(owner, call.ID) {
		return s.finishCopyTreeResult(ctx, call, p, purgeNotStarted(p, "file_batch_call_recorded"), nil, nil)
	}
	toolCtx, cancel := context.WithTimeout(ctx, s.options.ToolTimeout)
	defer cancel()
	if s.options.Durability != nil {
		if err := s.options.Durability.BeforeFilePublication(toolCtx, FileWriteAhead{ToolCallID: call.ID, Effect: p.FileEffect()}); err != nil {
			if errors.Is(err, ErrLocalCallRecorded) {
				return s.finishCopyTreeResult(ctx, call, p, purgeNotStarted(p, "file_batch_call_recorded"), nil, nil)
			}
			return s.finishCopyTreeResult(ctx, call, p, purgeNotStarted(p, "file_journal_unavailable"), nil, errors.New("无法在永久清理前确认恢复凭据"))
		}
	}
	id, journalErr := manager.Begin(toolCtx, owner, call.ID, purgeBatchPlan(p))
	result := purgeNotStarted(p, artifactErrorCode(journalErr))
	if journalErr == nil {
		invalidated := false
		observer := securefile.PurgeObserver{
			Before: func(stepCtx context.Context, index int, _ securefile.PurgeItem) error {
				journalErr = manager.Pending(stepCtx, owner, id, index)
				if journalErr == nil && index%64 == 0 {
					s.publishActivity(ctx, Activity{Kind: ActivityTool, Event: Event{ID: call.ID, Tool: workspace.ToolPurgeArchive, Summary: fmt.Sprintf("正在永久清理第%d项；完整逐项日志 %s", index+1, id), Status: EventRunning}, Phase: ActivityExecutingTool, File: fileActivityDetailFromPrepared(p)})
				}
				return journalErr
			},
			After: func(stepCtx context.Context, index int, _ securefile.PurgeItem, actual securefile.PurgeItemResult) error {
				var invalidationErr error
				if actual.Outcome == securefile.PublishCompleted || actual.Outcome == securefile.PublishUnknown {
					s.markFileEffect(call.ID, actual.Outcome == securefile.PublishUnknown)
					if !invalidated {
						invalidationErr = s.invalidateFileEffect(p.FileEffect(), true)
						invalidated = invalidationErr == nil
					}
				}
				journalErr = manager.Settle(stepCtx, owner, id, index, fileeffects.BatchActual{Outcome: string(actual.Outcome), Code: purgeItemCode(actual.Err), Bytes: actual.Bytes})
				return errors.Join(journalErr, invalidationErr)
			},
		}
		result, _ = executor.CommitPurge(toolCtx, p, observer)
		if journalErr == nil {
			finishCtx, finishCancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
			journalErr = manager.Finish(finishCtx, owner, id, result.Publication == workspace.PublicationCompleted, copyTreeResultCode(result))
			finishCancel()
		}
	}
	if result.Publication == workspace.PublicationCompleted && journalErr != nil {
		result.Publication = workspace.PublicationUnknown
		value := result.Value.(map[string]any)
		value["publication_outcome"], value["purge_state"], value["complete"] = "unknown", "unknown", false
		value["error"], value["code"] = "file_batch_settlement_unsaved", "file_batch_settlement_unsaved"
		result.Summary += "；实际删除完成但日志未完整保存，不会重放或撤销"
	}
	if result.Publication == workspace.PublicationCompleted || result.Publication == workspace.PublicationUnknown {
		s.markFileEffect(call.ID, result.Publication == workspace.PublicationUnknown)
	}
	var settlementErr error
	if s.options.Durability != nil {
		settleCtx, settleCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		settlementErr = s.options.Durability.AfterFilePublication(settleCtx, call.ID, result)
		settleCancel()
		if journalErr != nil {
			if sink, ok := s.options.Durability.(fileBatchFailureSink); ok {
				failureCtx, failureCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
				settlementErr = errors.Join(settlementErr, sink.FileBatchFailed(failureCtx, call.ID, journalErr))
				failureCancel()
			}
		}
	}
	value, ok := result.Value.(map[string]any)
	if !ok {
		value = make(map[string]any)
	}
	if id != "" {
		value["batch_id"], value["receipt_id"] = id, id
		if status, err := manager.Status(owner, id); err == nil {
			value["receipt_bytes"], value["receipt_saved_bytes"] = status.Bytes, status.SavedBytes
			value["receipt_saved"] = status.Bytes == status.SavedBytes && status.Bytes > 0 && status.PersistenceError == ""
		}
	}
	if journalErr != nil {
		value["receipt_error"] = artifactErrorCode(journalErr)
	}
	result.Value = value
	return s.finishCopyTreeResult(ctx, call, p, result, toolCtx.Err(), settlementErr)
}

func purgeReceiptProjection(object map[string]any) map[string]any {
	return preserveFields(object, "file_effect", "operation", "path", "entry_type", "publication_outcome", "operation_outcome", "error", "code", "replay", "plan_id", "plan_bytes", "plan_saved", "plan_hash", "batch_id", "receipt_id", "receipt_bytes", "receipt_saved_bytes", "receipt_saved", "receipt_error", "item_count", "attempted", "completed", "unchanged", "unknown", "not_started", "logical_bytes_removed", "physical_bytes_reclaimed", "purge_state", "complete", "cleanup_incomplete", "execution_error")
}
