package agentloop

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/localartifact"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/modelclient"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/securefile"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/workspace"
)

type purgeTestExecutor struct {
	*workspace.Workspace
	cancel  context.CancelFunc
	commits int
}

func (w *purgeTestExecutor) CommitPurge(ctx context.Context, p *workspace.PreparedMutation, observer securefile.PurgeObserver) (workspace.Result, securefile.PurgeResult) {
	w.commits++
	if w.cancel != nil {
		after := observer.After
		observer.After = func(ctx context.Context, index int, item securefile.PurgeItem, actual securefile.PurgeItemResult) error {
			err := after(ctx, index, item, actual)
			if index == 0 {
				w.cancel()
			}
			return err
		}
	}
	return w.Workspace.CommitPurge(ctx, p, observer)
}

type purgeTestSink struct {
	durabilitySink
	actual          []workspace.Result
	settlementError bool
}

func (s *purgeTestSink) AfterFilePublication(_ context.Context, _ string, actual workspace.Result) error {
	s.actual = append(s.actual, actual)
	if s.settlementError {
		return errors.New("settlement unavailable")
	}
	return nil
}

func TestArchivePurgeModelApprovalSettlementAndPrivacy(t *testing.T) {
	for _, mode := range []string{"approve", "yolo_forced_confirmation", "decline", "cancel_pending", "source_changed", "wal_failure", "cancel_after_first", "model_failure", "settlement_failure", "retention_failure"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			path := ".edu-agent-archive/selected"
			if err := os.MkdirAll(filepath.Join(root, path, "empty"), 0700); err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"a.bin", "z.bin"} {
				if err := os.WriteFile(filepath.Join(root, path, name), []byte("PRIVATE_PURGE_BODY\x00\xff"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			w, err := workspace.Open(root)
			if err != nil {
				t.Fatal(err)
			}
			version := w.Execute(t.Context(), "stat", `{"path":"`+path+`"}`).Value.(map[string]any)["entry_version"].(string)
			raw, _ := json.Marshal(map[string]any{"path": path, "expected_version": version})
			model := &fakeModel{responses: []modelclient.Response{{Message: toolMessage("purge-call", "purge_archive", string(raw))}, {Message: modelclient.Message{Role: "assistant", Content: "done"}}}}
			if mode == "model_failure" {
				model.responses = model.responses[:1]
				model.err = errors.New("provider stopped")
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			executor := &purgeTestExecutor{Workspace: w}
			if mode == "cancel_after_first" {
				executor.cancel = cancel
			}
			sink := &purgeTestSink{settlementError: mode == "settlement_failure"}
			if mode == "wal_failure" {
				sink.fileErr = errors.New("WAL unavailable")
			}
			s := newDurableTestSession(t, model, &fakeServer{}, executor, sink)
			defer s.Close()
			if mode == "yolo_forced_confirmation" {
				if err := s.SetFileAuthorizationMode(FileAuthorizationYOLO); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "retention_failure" {
				s.options.Artifacts = localartifact.New(localartifact.Options{MaxArtifactBytes: 1})
			}
			result, err := s.Send(ctx, "永久清理所选归档")
			if err != nil {
				t.Fatal(err)
			}
			if mode == "retention_failure" {
				if result.PendingFileMutation != nil || executor.commits != 0 || len(sink.files) != 0 {
					t.Fatal("retention failure allowed execution", result)
				}
				if _, err := os.Stat(filepath.Join(root, path, "z.bin")); err != nil {
					t.Fatal(err)
				}
				return
			}
			pending := result.PendingFileMutation
			if pending == nil || pending.PlanID == "" || pending.DiffID != "" || pending.Operation != "purge_archive" || pending.BaseVersion != version {
				t.Fatal("missing explicit purge approval", pending)
			}
			if executor.commits != 0 || len(sink.files) != 0 {
				t.Fatal("deleted before explicit confirmation")
			}
			var manifest []byte
			for offset := int64(0); ; {
				page, err := s.artifactCatalog().Read(t.Context(), pending.PlanID, offset, 83)
				if err != nil {
					t.Fatal(err)
				}
				manifest = append(manifest, page.Data...)
				if !page.More {
					break
				}
				offset = page.NextOffset
			}
			if int64(len(manifest)) != pending.PlanBytes || !bytes.Contains(manifest, []byte(path+"/empty")) || !bytes.Contains(manifest, []byte("plan_only_not_executed")) {
				t.Fatal("missing frozen plan")
			}
			if _, err := os.Stat(filepath.Join(root, path, "z.bin")); err != nil {
				t.Fatal("prepared deletion", err)
			}
			if mode == "source_changed" {
				if err := os.WriteFile(filepath.Join(root, path, "new"), nil, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "cancel_pending" {
				_, err = s.CancelPendingFileMutation("purge-call")
			} else {
				resolution := FileMutationApprove
				if mode == "decline" {
					resolution = FileMutationDecline
				}
				result, err = s.ResolveFileMutation(ctx, "purge-call", resolution)
			}
			if mode == "wal_failure" {
				if err == nil {
					t.Fatal("WAL failure did not stop turn")
				}
			} else if err != nil {
				t.Fatal(err)
			}
			noExecution := mode == "decline" || mode == "cancel_pending" || mode == "wal_failure" || mode == "source_changed"
			if noExecution {
				if _, err := os.Stat(filepath.Join(root, path, "z.bin")); err != nil {
					t.Fatal("unapproved/changed scope deleted", err)
				}
				if mode != "source_changed" && executor.commits != 0 {
					t.Fatal(executor.commits)
				}
				if mode == "source_changed" && (len(sink.actual) != 1 || sink.actual[0].Publication != workspace.PublicationUnchanged || sink.actual[0].Effect != nil) {
					t.Fatal(sink.actual)
				}
				return
			}
			if executor.commits != 1 || len(sink.files) != 1 || !sink.files[0].Effect.IsArchivePurge() || len(sink.actual) != 1 {
				t.Fatal("root WAL/actual missing", sink.files, sink.actual)
			}
			actual := sink.actual[0]
			if actual.Effect == nil || actual.Effect.Target.Kind != "absent" || actual.Effect.Target.Version != "" || actual.Reference == nil || actual.Reference.ContentHash != "" || !actual.Reference.InvalidateObserved {
				t.Fatal(actual)
			}
			items, err := s.options.FileBatches.List(t.Context(), s.options.ArtifactOwner)
			if err != nil || len(items) != 1 {
				t.Fatal(items, err)
			}
			status, err := s.options.FileBatches.Status(s.options.ArtifactOwner, items[0].ID)
			if err != nil || status.Items != 4 || !status.Finished {
				t.Fatal(status, err)
			}
			if mode == "cancel_after_first" {
				if actual.Publication != workspace.PublicationUnknown || status.Completed != 1 || status.Started != 1 {
					t.Fatal(actual, status)
				}
				if _, err := os.Stat(filepath.Join(root, path, "z.bin")); !os.IsNotExist(err) {
					t.Fatal("first deletion lost", err)
				}
				if _, err := os.Stat(filepath.Join(root, path, "a.bin")); err != nil {
					t.Fatal("later item deleted", err)
				}
			} else {
				if actual.Publication != workspace.PublicationCompleted || status.Completed != 4 {
					t.Fatal(actual, status)
				}
				if _, err := os.Stat(filepath.Join(root, path)); !os.IsNotExist(err) {
					t.Fatal("scope remains", err)
				}
			}
			history := s.toolHistory["purge-call"]
			for _, field := range []string{"file_effect", "plan_id", "batch_id", "receipt_saved_bytes", "item_count", "not_started", "logical_bytes_removed", "physical_bytes_reclaimed"} {
				if !strings.Contains(history, `"`+field+`"`) {
					t.Fatal("lost projection", field, history)
				}
			}
			args, _ := json.Marshal(map[string]any{"action": "read", "id": items[0].ID, "limit": 4096})
			read := s.executeArtifactTool(t.Context(), modelclient.ToolCall{Function: modelclient.ToolFunction{Arguments: string(args)}})
			if read.Code != "" || read.Page == nil || !bytes.Contains(read.Page.Data, []byte(`"version":2`)) {
				t.Fatal(read)
			}
			args, _ = json.Marshal(map[string]any{"action": "search", "id": items[0].ID, "needle": `"type":"actual"`, "limit": 1})
			search := s.executeArtifactTool(t.Context(), modelclient.ToolCall{Function: modelclient.ToolFunction{Arguments: string(args)}})
			if search.Code != "" || search.Search == nil || len(search.Search.Offsets) != 1 {
				t.Fatal(search)
			}
			cp, err := s.ExportCheckpoint()
			if err != nil {
				t.Fatal(err)
			}
			encoded, _ := json.Marshal(cp)
			for _, secret := range []string{"PRIVATE_PURGE_BODY", "plan_only_not_executed", `\"type\":\"actual\"`, string(raw)} {
				if bytes.Contains(encoded, []byte(secret)) {
					t.Fatal("private input/plan/log in checkpoint", secret)
				}
			}
			if mode == "model_failure" || mode == "settlement_failure" || mode == "cancel_after_first" {
				if !strings.Contains(result.Text, "永久") || !strings.Contains(result.Text, path) || strings.Contains(result.Text, "源未修改") {
					t.Fatal("lost permanent cleanup fact", result)
				}
			}
		})
	}
}
