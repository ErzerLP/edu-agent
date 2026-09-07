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

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/modelclient"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/securefile"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/workspace"
)

type recursiveCopyExecutor struct {
	*workspace.Workspace
	cancel  context.CancelFunc
	commits int
}

func (w *recursiveCopyExecutor) CommitCopyTree(ctx context.Context, p *workspace.PreparedMutation, observer securefile.CopyTreeObserver) (workspace.Result, securefile.CopyTreeResult) {
	w.commits++
	if w.cancel != nil {
		after := observer.After
		observer.After = func(ctx context.Context, index int, item securefile.CopyTreeItem, actual securefile.CopyTreeItemResult) error {
			err := after(ctx, index, item, actual)
			if index == 0 {
				w.cancel()
			}
			return err
		}
	}
	return w.Workspace.CommitCopyTree(ctx, p, observer)
}

type recursiveCopySink struct {
	durabilitySink
	actual []workspace.Result
}

func (s *recursiveCopySink) AfterFilePublication(_ context.Context, _ string, actual workspace.Result) error {
	s.actual = append(s.actual, actual)
	return nil
}

func TestRecursiveCopyProductionAuthorizationAndSettlement(t *testing.T) {
	for _, mode := range []string{"approve", "decline", "yolo", "wal_failure", "source_changed", "cancel_pending", "cancel_after_root", "model_failure"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			if err := os.MkdirAll(filepath.Join(root, "source", "empty"), 0700); err != nil {
				t.Fatal(err)
			}
			body := []byte("PRIVATE_TREE_FILE_BODY\x00\xff")
			for _, name := range []string{"a.bin", "b.bin"} {
				if err := os.WriteFile(filepath.Join(root, "source", name), body, 0640); err != nil {
					t.Fatal(err)
				}
			}
			w, err := workspace.Open(root)
			if err != nil {
				t.Fatal(err)
			}
			version := w.Execute(t.Context(), "stat", `{"path":"source"}`).Value.(map[string]any)["entry_version"].(string)
			raw, _ := json.Marshal(map[string]any{"source": "source", "destination": "target", "expected_version": version})
			model := &fakeModel{responses: []modelclient.Response{{Message: toolMessage("tree-copy", "copy", string(raw))}, {Message: modelclient.Message{Role: "assistant", Content: "done"}}}}
			if mode == "model_failure" {
				model.responses = model.responses[:1]
				model.err = errors.New("model stopped")
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			executor := &recursiveCopyExecutor{Workspace: w}
			if mode == "cancel_after_root" {
				executor.cancel = cancel
			}
			sink := &recursiveCopySink{}
			if mode == "wal_failure" {
				sink.fileErr = errors.New("WAL unavailable")
			}
			s := newDurableTestSession(t, model, &fakeServer{}, executor, sink)
			defer s.Close()
			if mode == "yolo" {
				if err := s.SetFileAuthorizationMode(FileAuthorizationYOLO); err != nil {
					t.Fatal(err)
				}
			}
			result, err := s.Send(ctx, "复制这个目录")
			if err != nil {
				t.Fatal(err)
			}
			if mode != "yolo" {
				pending := result.PendingFileMutation
				if pending == nil || pending.PlanID == "" || pending.DiffID != "" || pending.EntryKind != "directory" || pending.BaseVersion != version {
					t.Fatalf("missing frozen plan: %+v", pending)
				}
				var manifest []byte
				for offset := int64(0); ; {
					page, err := s.artifactCatalog().Read(t.Context(), pending.PlanID, offset, 71)
					if err != nil {
						t.Fatal(err)
					}
					manifest = append(manifest, page.Data...)
					if !page.More {
						break
					}
					offset = page.NextOffset
				}
				if int64(len(manifest)) != pending.PlanBytes || !bytes.Contains(manifest, []byte("source/empty")) || !bytes.Contains(manifest, []byte("plan_only_not_executed")) {
					t.Fatal("incomplete manifest")
				}
				if _, err := os.Stat(filepath.Join(root, "target")); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("prepare changed destination", err)
				}
				if mode == "source_changed" {
					if err := os.WriteFile(filepath.Join(root, "source", "a.bin"), []byte("changed"), 0600); err != nil {
						t.Fatal(err)
					}
				}
				if mode == "cancel_pending" {
					_, err = s.CancelPendingFileMutation("tree-copy")
				} else {
					resolution := FileMutationApprove
					if mode == "decline" {
						resolution = FileMutationDecline
					}
					result, err = s.ResolveFileMutation(ctx, "tree-copy", resolution)
				}
				if mode == "wal_failure" {
					if err == nil {
						t.Fatal("WAL failure must stop this turn")
					}
				} else if err != nil {
					t.Fatal(err)
				}
			}
			noExecution := mode == "decline" || mode == "wal_failure" || mode == "cancel_pending" || mode == "source_changed"
			if noExecution {
				if _, err := os.Stat(filepath.Join(root, "target")); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("unapproved/changed tree published", err)
				}
				if mode != "source_changed" && executor.commits != 0 {
					t.Fatal("unauthorized commit")
				}
				if mode == "source_changed" && (len(sink.actual) != 1 || sink.actual[0].Publication != workspace.PublicationUnchanged) {
					t.Fatal("preflight conflict lost", sink.actual)
				}
				return
			}
			if executor.commits != 1 || len(sink.files) != 1 || len(sink.actual) != 1 || !sink.files[0].Effect.IsDirectoryCopy() {
				t.Fatalf("bad root WAL/settlement: %+v %+v", sink.files, sink.actual)
			}
			actual := sink.actual[0]
			if actual.Effect == nil || actual.Effect.Target.Version != "" || actual.Reference.ContentHash != "" || !actual.Reference.InvalidateObserved {
				t.Fatal("directory invented a content hash")
			}
			items, err := s.options.FileBatches.List(t.Context(), s.options.ArtifactOwner)
			if err != nil || len(items) != 1 {
				t.Fatalf("missing journal: %+v %v", items, err)
			}
			status, err := s.options.FileBatches.Status(s.options.ArtifactOwner, items[0].ID)
			if err != nil || status.Items != 4 || !status.Finished {
				t.Fatalf("bad journal: %+v %v", status, err)
			}
			if mode == "cancel_after_root" {
				if actual.Publication != workspace.PublicationUnknown || status.Completed != 1 || status.Started != 1 || len(model.requests) != 1 {
					t.Fatalf("partial copy lost: %+v %+v", actual, status)
				}
				if _, err := os.Stat(filepath.Join(root, "target")); err != nil {
					t.Fatal("completed root rolled back")
				}
				if _, err := os.Stat(filepath.Join(root, "target", "a.bin")); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("later item executed", err)
				}
			} else {
				if actual.Publication != workspace.PublicationCompleted || status.Completed != 4 {
					t.Fatalf("copy incomplete: %+v %+v", actual, status)
				}
				for _, prefix := range []string{"source", "target"} {
					got, err := os.ReadFile(filepath.Join(root, prefix, "a.bin"))
					if err != nil || !bytes.Equal(got, body) {
						t.Fatal("binary copy changed", prefix, err)
					}
				}
			}
			history := s.toolHistory["tree-copy"]
			for _, field := range []string{"plan_id", "batch_id", "receipt_saved_bytes", "item_count", "not_started"} {
				if !strings.Contains(history, `"`+field+`"`) {
					t.Fatal("projection lost field", field, history)
				}
			}
			args, _ := json.Marshal(map[string]any{"action": "read", "id": items[0].ID, "limit": 4096})
			read := s.executeArtifactTool(t.Context(), modelclient.ToolCall{Function: modelclient.ToolFunction{Arguments: string(args)}})
			if read.Code != "" || read.Page == nil || !bytes.Contains(read.Page.Data, []byte(`"type":"plan"`)) {
				t.Fatalf("model read route: %+v", read)
			}
			args, _ = json.Marshal(map[string]any{"action": "search", "id": items[0].ID, "needle": `"type":"actual"`, "limit": 1})
			search := s.executeArtifactTool(t.Context(), modelclient.ToolCall{Function: modelclient.ToolFunction{Arguments: string(args)}})
			if search.Code != "" || search.Search == nil || len(search.Search.Offsets) != 1 {
				t.Fatalf("model search route: %+v", search)
			}
			cp, err := s.ExportCheckpoint()
			if err != nil {
				t.Fatal(err)
			}
			encoded, _ := json.Marshal(cp)
			if bytes.Contains(encoded, []byte("PRIVATE_TREE_FILE_BODY")) || bytes.Contains(encoded, []byte(`\"type\":\"actual\"`)) || bytes.Contains(encoded, []byte("plan_only_not_executed")) {
				t.Fatal("full copy content or logs entered checkpoint")
			}
			if mode == "model_failure" && !strings.Contains(result.Text, "target") {
				t.Fatal("model failure lost actual copy", result)
			}
		})
	}
}
