package agentcontroller

import (
	"encoding/json"
	"testing"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/agentloop"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/agentsession"
)

// This compatibility helper currently has no production caller. Exercise it
// against a real production directory-copy checkpoint without changing the
// executor-backed settlement path or inferring new outcomes during recovery.
func TestDirectoryCopyCheckpointReceiptMatchesExecutorContract(t *testing.T) {
	_, c, model := recursiveCopyFixture(t, false)
	pending, err := c.Send(t.Context(), "复制目录以核对结算")
	if err != nil || pending.PendingFileMutation == nil {
		t.Fatal(pending, err)
	}
	result, err := c.ResolveFileMutation(t.Context(), model.callID, agentloop.FileMutationApprove)
	if err != nil {
		t.Fatal(err)
	}
	cp, err := c.loop.ExportCheckpoint()
	if err != nil {
		t.Fatal(err)
	}
	if len(c.record.FileReceipts) != 1 || !c.record.FileReceipts[0].Effect.IsDirectoryCopy() {
		t.Fatal(c.record.FileReceipts)
	}
	original := c.record.FileReceipts[0]
	ahead := agentsession.FileWriteAhead{ToolCallID: model.callID, Effect: original.Effect}
	encoded, err := json.Marshal(cp)
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"completed", "unknown", "unknown_forged_hash", "unknown_no_invalidation", "wrong_source"} {
		t.Run(mode, func(t *testing.T) {
			var checkpoint agentloop.SessionCheckpoint
			if err := json.Unmarshal(encoded, &checkpoint); err != nil {
				t.Fatal(err)
			}
			events := append([]agentloop.Event(nil), result.Events...)
			wal := ahead
			expected := original
			if mode != "completed" && mode != "wrong_source" {
				for i := range checkpoint.Turns {
					if checkpoint.Turns[i].FileEffectCallID == model.callID {
						checkpoint.Turns[i].FileEffectUnknown = true
					}
				}
				for i := range events {
					if events[i].ID == model.callID {
						events[i].Status = agentloop.EventOutcomeUnknown
					}
				}
				expected.Outcome, expected.StableCode = agentsession.NoticeOutcomeUnknown, agentsession.FilePublicationUnknownCode
			}
			for i := range checkpoint.WorkspaceReferences {
				if checkpoint.WorkspaceReferences[i].Key != model.callID {
					continue
				}
				switch mode {
				case "unknown_forged_hash":
					checkpoint.WorkspaceReferences[i].Value.ContentHash = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
				case "unknown_no_invalidation":
					checkpoint.WorkspaceReferences[i].Value.InvalidateObserved = false
				}
			}
			if mode == "wrong_source" {
				wal.Effect.Source.Path = "another-source"
			}
			actual, found, err := fileReceiptFromCheckpoint(wal, checkpoint, events)
			if mode == "completed" || mode == "unknown" {
				if err != nil || !found || actual != expected || actual.Effect.Target.Version != "" {
					t.Fatal("directory required an invented hash or lost fact", actual, found, err)
				}
			} else if err == nil {
				t.Fatal("inconsistent checkpoint accepted", actual, found)
			}
		})
	}
}
