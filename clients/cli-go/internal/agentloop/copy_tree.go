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

type treeCopyWorkspace interface {
	CommitCopyTree(context.Context, *workspace.PreparedMutation, securefile.CopyTreeObserver) (workspace.Result, securefile.CopyTreeResult)
}

type fileBatchFailureSink interface {
	FileBatchFailed(context.Context, string, error) error
}

func copyBatchPlan(prepared *workspace.PreparedMutation) fileeffects.BatchPlan {
	items := prepared.CopyTreeItems()
	plan := fileeffects.BatchPlan{Root: prepared.FileEffect(), Items: make([]fileeffects.BatchItem, len(items))}
	for i, item := range items {
		entry := fileeffects.BatchItem{Source: fileeffects.Endpoint{Path: item.Source, Kind: string(item.Kind), Version: item.Version}, Target: fileeffects.Endpoint{Path: item.Destination, Kind: string(item.Kind)}}
		if item.Kind == securefile.EntryFile {
			entry.Bytes = item.Size
		}
		plan.Items[i] = entry
	}
	return plan
}

func copyItemCode(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, context.Canceled):
		return "cancelled"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, securefile.ErrChanged):
		return "content_changed"
	case errors.Is(err, securefile.ErrAlreadyExists):
		return "already_exists"
	case errors.Is(err, securefile.ErrNotFound):
		return "not_found"
	case errors.Is(err, securefile.ErrLink):
		return "link_not_allowed"
	case errors.Is(err, securefile.ErrOutcomeUnknown):
		return "outcome_unknown"
	default:
		return "copy_item_failed"
	}
}

func copyTreeNotStarted(prepared *workspace.PreparedMutation, code string) workspace.Result {
	return workspace.Result{Publication: workspace.PublicationUnchanged, Summary: "目录复制未开始：" + code, Value: map[string]any{"operation": workspace.ToolCopy, "path": prepared.Presentation.Path, "destination": prepared.Presentation.DestinationPath, "entry_type": "directory", "publication_outcome": "unchanged", "error": code, "code": code, "replay": false}}
}

func (s *Session) commitCopyTreeMutation(ctx context.Context, call modelclient.ToolCall, prepared *workspace.PreparedMutation) (workspace.Result, Event, bool, error) {
	executor, available := s.workspace.(treeCopyWorkspace)
	manager, owner := s.options.FileBatches, s.options.ArtifactOwner
	if !available || manager == nil {
		return s.finishCopyTreeResult(ctx, call, prepared, copyTreeNotStarted(prepared, "file_batch_unavailable"), nil, nil)
	}
	if status, ok := s.options.Durability.(interface{ FileBatchHistoryStatus() string }); ok {
		if code := status.FileBatchHistoryStatus(); code != "" {
			return s.finishCopyTreeResult(ctx, call, prepared, copyTreeNotStarted(prepared, code), nil, nil)
		}
	}
	if manager.HasCall(owner, call.ID) {
		return s.finishCopyTreeResult(ctx, call, prepared, copyTreeNotStarted(prepared, "file_batch_call_recorded"), nil, nil)
	}
	toolCtx, cancel := context.WithTimeout(ctx, s.options.ToolTimeout)
	defer cancel()
	if s.options.Durability != nil {
		if err := s.options.Durability.BeforeFilePublication(toolCtx, FileWriteAhead{ToolCallID: call.ID, Effect: prepared.FileEffect()}); err != nil {
			if errors.Is(err, ErrLocalCallRecorded) {
				// A confirmed replay fence is a domain result, not a failed
				// persistence attempt. It must not disable subsequent fresh calls.
				return s.finishCopyTreeResult(ctx, call, prepared, copyTreeNotStarted(prepared, "file_batch_call_recorded"), nil, nil)
			}
			return s.finishCopyTreeResult(ctx, call, prepared, copyTreeNotStarted(prepared, "file_journal_unavailable"), nil, errors.New("无法在复制前确认恢复凭据"))
		}
	}
	id, journalErr := manager.Begin(toolCtx, owner, call.ID, copyBatchPlan(prepared))
	result := copyTreeNotStarted(prepared, artifactErrorCode(journalErr))
	if journalErr == nil {
		invalidated := false
		observer := securefile.CopyTreeObserver{
			Before: func(stepCtx context.Context, index int, _ securefile.CopyTreeItem) error {
				journalErr = manager.Pending(stepCtx, owner, id, index)
				if journalErr == nil && index%64 == 0 {
					s.publishActivity(ctx, Activity{Kind: ActivityTool, Event: Event{ID: call.ID, Tool: workspace.ToolCopy, Summary: fmt.Sprintf("正在复制第%d项；完整逐项日志 %s", index+1, id), Status: EventRunning}, Phase: ActivityExecutingTool, File: fileActivityDetailFromPrepared(prepared)})
				}
				return journalErr
			},
			After: func(stepCtx context.Context, index int, _ securefile.CopyTreeItem, actual securefile.CopyTreeItemResult) error {
				var invalidateErr error
				if actual.Outcome == securefile.PublishCompleted || actual.Outcome == securefile.PublishUnknown {
					s.markFileEffect(call.ID, actual.Outcome == securefile.PublishUnknown)
					if !invalidated {
						invalidateErr = s.invalidateFileEffect(prepared.FileEffect(), true)
						invalidated = invalidateErr == nil
					}
				}
				journalErr = manager.Settle(stepCtx, owner, id, index, fileeffects.BatchActual{Outcome: string(actual.Outcome), Code: copyItemCode(actual.Err), ContentHash: actual.ContentHash, Bytes: actual.Bytes})
				return errors.Join(journalErr, invalidateErr)
			},
		}
		result, _ = executor.CommitCopyTree(toolCtx, prepared, observer)
		if journalErr == nil {
			finishCtx, finishCancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
			journalErr = manager.Finish(finishCtx, owner, id, result.Publication == workspace.PublicationCompleted, copyTreeResultCode(result))
			finishCancel()
		}
	}
	if result.Publication == workspace.PublicationCompleted && journalErr != nil {
		// Execution counts stay true, but an incomplete journal cannot produce
		// a fully confirmed aggregate recovery receipt.
		result.Publication = workspace.PublicationUnknown
		value := result.Value.(map[string]any)
		value["publication_outcome"], value["error"], value["code"] = "unknown", "file_batch_settlement_unsaved", "file_batch_settlement_unsaved"
		result.Summary += "；实际执行已完成，但逐项结算未完整保存，不会自动重放"
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
	return s.finishCopyTreeResult(ctx, call, prepared, result, toolCtx.Err(), settlementErr)
}

func copyTreeResultCode(result workspace.Result) string {
	if value, ok := result.Value.(map[string]any); ok {
		if code, ok := value["code"].(string); ok {
			return code
		}
	}
	return ""
}

func (s *Session) finishCopyTreeResult(ctx context.Context, call modelclient.ToolCall, prepared *workspace.PreparedMutation, result workspace.Result, toolErr, settlementErr error) (workspace.Result, Event, bool, error) {
	result = s.attachMutationArtifact(call.ID, result)
	if err := s.appendWorkspaceToolResult(call.Function.Name, call.ID, result); err != nil {
		return result, Event{}, false, err
	}
	event := eventFromToolOutput(call.Function.Name, result.Summary, result.Value)
	event.ID = call.ID
	if result.Publication == workspace.PublicationUnknown {
		event.Status, event.Detail = EventOutcomeUnknown, workspace.CodeOutcomeUnknown
	}
	s.publishActivity(ctx, Activity{Kind: ActivityTool, Event: event, Phase: ActivityExecutingTool, StableCode: event.Detail, File: mergePreparedFileActivity(fileActivityDetailFromResult(call.Function.Name, result), prepared)})
	if settlementErr != nil {
		operation := "复制"
		if prepared.IsPurgeArchive() {
			operation = "永久清理"
		}
		return result, event, true, fmt.Errorf("%s结算保存失败；已停止后续操作: %w", operation, settlementErr)
	}
	if result.Publication == workspace.PublicationUnchanged && toolErr != nil {
		return result, event, false, preferContextError(ctx, toolErr)
	}
	stop := result.Publication == workspace.PublicationUnknown || result.Publication == workspace.PublicationCompleted && (ctx.Err() != nil || toolErr != nil)
	return result, event, stop, nil
}
