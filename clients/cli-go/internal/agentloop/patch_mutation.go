package agentloop

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/modelclient"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/workspace"
)

type patchItemReceipt struct {
	Path             string `json:"path"`
	Operation        string `json:"operation"`
	ArchivePath      string `json:"archive_path,omitempty"`
	Outcome          string `json:"outcome"`
	ExpectedVersion  string `json:"expected_version,omitempty"`
	ResultVersion    string `json:"result_version,omitempty"`
	PersistenceError string `json:"persistence_error,omitempty"`
	Code             string `json:"code,omitempty"`
}
type patchReceipt struct {
	Operation    string             `json:"operation"`
	Outcome      string             `json:"outcome"`
	Completed    int                `json:"completed"`
	Unknown      int                `json:"unknown"`
	Items        []patchItemReceipt `json:"items"`
	DiffID       string             `json:"diff_id,omitempty"`
	ReceiptID    string             `json:"receipt_id,omitempty"`
	ReceiptError string             `json:"receipt_error,omitempty"`
}

func patchItemCallID(callID string, index int) string {
	digest := sha256.Sum256([]byte(fmt.Sprintf("patch-item-v1\x00%s\x00%d", callID, index)))
	return "patch_" + hex.EncodeToString(digest[:])
}

func (s *Session) commitPatchMutation(ctx context.Context, call modelclient.ToolCall, prepared *workspace.PreparedMutation) (workspace.Result, Event, bool, error) {
	items, err := prepared.ClaimPatchItems()
	if err != nil {
		return workspace.Result{Publication: workspace.PublicationUnchanged}, Event{}, false, err
	}
	receipt := patchReceipt{Operation: workspace.ToolPatch, Outcome: "completed", Items: make([]patchItemReceipt, len(items))}
	s.appendMu.Lock()
	receipt.DiffID = s.mutationArtifacts[call.ID].ID
	s.appendMu.Unlock()
	for i, item := range items {
		p := item.Presentation
		receipt.Items[i] = patchItemReceipt{Path: p.Path, Operation: p.Operation, ArchivePath: p.ArchivePath, ExpectedVersion: p.BaseVersion, Outcome: "not_started"}
	}
	var failure error
	for i, item := range items {
		if err := ctx.Err(); err != nil {
			failure = err
			receipt.Items[i].Code = "canceled"
			break
		}
		childID := patchItemCallID(call.ID, i)
		s.publishActivity(ctx, Activity{Kind: ActivityTool, Event: Event{ID: childID, Tool: workspace.ToolPatch, Summary: fmt.Sprintf("发布补丁文件 %d/%d", i+1, len(items)), Status: EventRunning}, Phase: ActivityExecutingTool, File: fileActivityDetailFromPrepared(item)})
		result, toolErr, settleErr, beforeErr := s.publishPreparedFileItem(ctx, childID, item)
		if beforeErr != nil {
			failure = beforeErr
			receipt.Items[i].Code = "file_journal_unavailable"
			break
		}
		receipt.Items[i].Outcome = string(result.Publication)
		if value, ok := result.Value.(map[string]any); ok {
			if code, ok := value["error"].(string); ok {
				receipt.Items[i].Code = code
			}
		}
		if result.Publication == workspace.PublicationCompleted {
			receipt.Completed++
			if result.Reference != nil {
				receipt.Items[i].ResultVersion = result.Reference.ContentHash
			}
		}
		if result.Publication == workspace.PublicationUnknown {
			receipt.Unknown++
		}
		if result.Publication == workspace.PublicationCompleted || result.Publication == workspace.PublicationUnknown {
			s.markFileEffect(call.ID, receipt.Unknown > 0)
		}
		if result.Effect != nil {
			if err := s.invalidateFileEffect(*result.Effect, true); err != nil {
				failure = err
			}
		}
		event := eventFromToolOutput(item.Presentation.Tool, result.Summary, result.Value)
		event.ID = childID
		s.publishActivity(ctx, Activity{Kind: ActivityTool, Event: event, Phase: ActivityExecutingTool, StableCode: event.Detail, File: mergePreparedFileActivity(fileActivityDetailFromResult(item.Presentation.Tool, result), item)})
		if settleErr != nil {
			failure = settleErr
			receipt.Items[i].PersistenceError = "file_settlement_unsaved"
		}
		if toolErr != nil {
			failure = preferContextError(ctx, toolErr)
			if receipt.Items[i].Code == "" {
				receipt.Items[i].Code = workspaceToolContextFailureEvent(childID, workspace.ToolPatch, toolErr).Detail
			}
		}
		if failure != nil || result.Publication != workspace.PublicationCompleted {
			break
		}
	}
	publication := workspace.PublicationUnchanged
	if receipt.Completed > 0 {
		publication = workspace.PublicationCompleted
	}
	if receipt.Unknown > 0 {
		publication = workspace.PublicationUnknown
		receipt.Outcome = "unknown"
	} else if receipt.Completed < len(items) {
		receipt.Outcome = "partial"
	}
	// This document is a receipt of observations, never an executable replay plan.
	data, marshalErr := json.Marshal(receipt)
	if marshalErr == nil && s.options.Artifacts != nil {
		saveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		info, saveErr := s.options.Artifacts.Put(saveCtx, s.options.ArtifactOwner, "receipt", data)
		cancel()
		if saveErr == nil {
			receipt.ReceiptID = info.ID
		} else {
			receipt.ReceiptError = artifactErrorCode(saveErr)
		}
	} else {
		receipt.ReceiptError = "artifact_unavailable"
	}
	result := workspace.Result{Publication: publication, Summary: fmt.Sprintf("补丁已完成 %d/%d 个文件，未知 %d；其余项未继续，无自动回滚", receipt.Completed, len(items), receipt.Unknown)}
	result = s.attachMutationArtifact(call.ID, result)
	if err := s.appendPatchReceipt(call.ID, receipt); err != nil {
		return result, Event{}, true, err
	}
	event := Event{ID: call.ID, Tool: workspace.ToolPatch, Summary: result.Summary, Status: EventSucceeded}
	if receipt.Outcome != "completed" {
		event.Status = EventFailed
		event.Detail = "patch_partial"
	}
	if receipt.Unknown > 0 {
		event.Status = EventOutcomeUnknown
		event.Detail = workspace.CodeOutcomeUnknown
	}
	if receipt.ReceiptError != "" {
		event.Detail = receipt.ReceiptError
	}
	s.publishActivity(ctx, Activity{Kind: ActivityTool, Event: event, Phase: ActivityExecutingTool, StableCode: event.Detail})
	stop := publication != workspace.PublicationUnchanged && (receipt.Outcome != "completed" || failure != nil || ctx.Err() != nil || receipt.ReceiptError != "")
	return result, event, stop, failure
}

func (s *Session) appendPatchReceipt(callID string, receipt patchReceipt) error {
	s.appendMu.Lock()
	defer s.appendMu.Unlock()
	if s.contextRuntime.isClosed() {
		return ErrSessionClosed
	}
	allowed := min(max(32, s.currentToolResultBudget/max(1, s.currentToolResultShares)-30), max(32, s.currentToolResultBudget-s.currentToolResultTokens-6))
	project := func(n int) string {
		v := map[string]any{"operation": receipt.Operation, "outcome": receipt.Outcome, "completed": receipt.Completed, "unknown": receipt.Unknown, "total": len(receipt.Items), "diff_id": receipt.DiffID, "receipt_id": receipt.ReceiptID}
		if receipt.ReceiptError != "" {
			v["receipt_error"] = receipt.ReceiptError
		}
		if n > 0 {
			v["items"] = receipt.Items[:n]
		}
		if n < len(receipt.Items) {
			v["items_omitted"] = true
		}
		data, _ := json.Marshal(v)
		return string(data)
	}
	history := project(0)
	live := history
	for n := len(receipt.Items); n >= 0; n-- {
		candidate := project(n)
		if len(candidate) <= maxToolOutputBytes && s.estimator.EstimateText(candidate) <= allowed {
			live = candidate
			break
		}
	}
	return s.appendLocalDataProjectionLocked(callID, live, history)
}
