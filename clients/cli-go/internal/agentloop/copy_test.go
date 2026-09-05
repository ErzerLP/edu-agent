package agentloop

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/fileeffects"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/modelclient"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/workspace"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type copyOutcomeExecutor struct {
	workspace.Executor
	unknown bool
	cancel  context.CancelFunc
	commits int
}

func (w *copyOutcomeExecutor) CommitMutation(ctx context.Context, p *workspace.PreparedMutation) workspace.Result {
	w.commits++
	r := w.Executor.CommitMutation(ctx, p)
	if r.Publication == workspace.PublicationCompleted {
		if w.unknown {
			r.Publication = workspace.PublicationUnknown
			r.Effect.Target.Version = ""
			r.Reference.ContentHash = ""
			r.Reference.InvalidateObserved = true
			v := r.Value.(map[string]any)
			v["publication_outcome"] = "unknown"
			v["complete"] = false
			v["file_effect"] = *r.Effect
			v["error"], v["code"] = workspace.CodeOutcomeUnknown, workspace.CodeOutcomeUnknown
			delete(v, "content_hash")
		}
		if w.cancel != nil {
			w.cancel()
		}
	}
	return r
}
func TestCopyProductionAuthorizationYOLOWALAndLateOutcomes(t *testing.T) {
	for _, mode := range []string{"approve", "decline", "yolo", "wal_failure", "cancel_pending", "late_cancel", "unknown"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			data := bytes.Repeat([]byte("private-binary-body\x00\xff"), 60000)
			if err := os.WriteFile(filepath.Join(root, "source.bin"), data, 0640); err != nil {
				t.Fatal(err)
			}
			executor, err := workspace.Open(root)
			if err != nil {
				t.Fatal(err)
			}
			stat := executor.Execute(t.Context(), "stat", `{"path":"source.bin"}`)
			version := stat.Value.(map[string]any)["entry_version"].(string)
			raw, _ := json.Marshal(map[string]any{"source": "source.bin", "destination": "copy.bin", "expected_version": version})
			model := &fakeModel{responses: []modelclient.Response{{Message: toolMessage("copy-call", "copy", string(raw))}, {Message: modelclient.Message{Role: "assistant", Content: "复制结束"}}}}
			sink := &durabilitySink{}
			if mode == "wal_failure" {
				sink.fileErr = errors.New("storage failed")
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			wrapped := &copyOutcomeExecutor{Executor: executor, unknown: mode == "unknown"}
			if mode == "late_cancel" {
				wrapped.cancel = cancel
			}
			session := newDurableTestSession(t, model, &fakeServer{}, wrapped, sink)
			defer session.Close()
			if mode == "yolo" {
				if err = session.SetFileAuthorizationMode(FileAuthorizationYOLO); err != nil {
					t.Fatal(err)
				}
			}
			result, err := session.Send(ctx, "复制文件")
			if err != nil {
				t.Fatal(err)
			}
			if mode != "yolo" {
				pending := result.PendingFileMutation
				if pending == nil || pending.Path != "source.bin" || pending.DestinationPath != "copy.bin" || pending.BaseVersion != version || !strings.Contains(pending.Preview, version) {
					t.Fatal("bad confirmation", result)
				}
				entries, _ := os.ReadDir(root)
				if len(entries) != 1 {
					t.Fatal("prepare side effect", entries)
				}
				if mode == "cancel_pending" {
					_, err = session.CancelPendingFileMutation("copy-call")
				} else {
					resolution := FileMutationApprove
					if mode == "decline" {
						resolution = FileMutationDecline
					}
					result, err = session.ResolveFileMutation(ctx, "copy-call", resolution)
				}
				if mode == "wal_failure" {
					if err == nil {
						t.Fatal("WAL fail open")
					}
				} else if err != nil {
					t.Fatal(err)
				}
			}
			failed := mode == "wal_failure" || mode == "decline" || mode == "cancel_pending"
			if failed {
				entries, _ := os.ReadDir(root)
				if len(entries) != 1 || wrapped.commits != 0 {
					t.Fatal("unauthorized commit", entries, wrapped.commits)
				}
				if mode != "wal_failure" && len(sink.files) != 0 {
					t.Fatal("unapproved WAL")
				}
				return
			}
			if wrapped.commits != 1 || len(sink.files) != 1 || sink.files[0].Effect.Source.Version != version || sink.files[0].Effect.Target.Path != "copy.bin" || sink.files[0].Effect.Target.Version != "" {
				t.Fatal("bad WAL", sink.files)
			}
			for _, path := range []string{"source.bin", "copy.bin"} {
				actual, err := os.ReadFile(filepath.Join(root, path))
				if err != nil || !bytes.Equal(actual, data) {
					t.Fatal("changed bytes", path, err)
				}
			}
			if mode == "unknown" || mode == "late_cancel" {
				if len(model.requests) != 1 || !strings.Contains(result.Text, "copy.bin") || !strings.Contains(result.Text, "source.bin") || !strings.Contains(result.Text, "未修改源") {
					t.Fatal("lost late effect", result, len(model.requests))
				}
			}
			if mode == "unknown" && !strings.Contains(result.Text, "不会自动重试") {
				t.Fatal(result.Text)
			}
			for _, req := range model.requests {
				encoded, _ := json.Marshal(req)
				if bytes.Contains(encoded, []byte("private-binary-body")) {
					t.Fatal("binary body entered model")
				}
			}
			history := session.toolHistory["copy-call"]
			if !strings.Contains(history, version) || !strings.Contains(history, "source.bin") || !strings.Contains(history, "copy.bin") {
				t.Fatal("lost history", history)
			}
			cp, err := session.ExportCheckpoint()
			if err != nil {
				t.Fatal(err)
			}
			restored := newDurableTestSession(t, &fakeModel{}, &fakeServer{}, nil, &durabilitySink{})
			defer restored.Close()
			if err = restored.RestoreCheckpoint(cp); err != nil {
				t.Fatal(err)
			}
			if restored.FileAuthorizationMode() != FileAuthorizationConfirm || restored.toolHistory["copy-call"] != history {
				t.Fatal("restore changed effect or restored YOLO")
			}
		})
	}
}
func TestCopyOnlyTargetObservationsExpireAndOperationFactsSurvive(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "source"), []byte("input"), 0600); err != nil {
		t.Fatal(err)
	}
	executor, err := workspace.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	stat := executor.Execute(t.Context(), "stat", `{"path":"source"}`)
	version := stat.Value.(map[string]any)["entry_version"].(string)
	raw, _ := json.Marshal(map[string]any{"source": "source", "destination": "target", "expected_version": version})
	calls := []modelclient.ToolCall{}
	for _, call := range [][3]string{{"source-stat", "stat", `{"path":"source"}`}, {"target-stat", "stat", `{"path":"target"}`}, {"list", "list", `{}`}, {"find", "find", `{"pattern":"**"}`}, {"search", "search", `{"query":"input"}`}} {
		calls = append(calls, modelclient.ToolCall{ID: call[0], Type: "function", Function: modelclient.ToolFunction{Name: call[1], Arguments: call[2]}})
	}
	model := &fakeModel{responses: []modelclient.Response{{Message: modelclient.Message{Role: "assistant", ToolCalls: calls}}, {Message: modelclient.Message{Role: "assistant", Content: "已观察"}}, {Message: toolMessage("copy-call", "copy", string(raw))}, {Message: modelclient.Message{Role: "assistant", Content: "已复制"}}, {Message: toolMessage("new-stat", "stat", `{"path":"target"}`)}, {Message: modelclient.Message{Role: "assistant", Content: "已检查"}}}}
	session := newDurableTestSession(t, model, &fakeServer{}, executor, &durabilitySink{})
	defer session.Close()
	if _, err = session.Send(t.Context(), "观察"); err != nil {
		t.Fatal(err)
	}
	sourceHistory := session.toolHistory["source-stat"]
	if err = session.SetFileAuthorizationMode(FileAuthorizationYOLO); err != nil {
		t.Fatal(err)
	}
	if _, err = session.Send(t.Context(), "复制"); err != nil {
		t.Fatal(err)
	}
	if sourceHistory != session.toolHistory["source-stat"] {
		t.Fatal("source was invalidated")
	}
	for _, id := range []string{"target-stat", "list", "find", "search"} {
		if !strings.Contains(session.toolHistory[id], "superseded") {
			t.Fatal("not invalidated", id, session.toolHistory[id])
		}
	}
	fact := session.toolHistory["copy-call"]
	if _, err = session.Send(t.Context(), "重新检查"); err != nil {
		t.Fatal(err)
	}
	if session.toolHistory["copy-call"] != fact || !fileOperationFact(fact) {
		t.Fatal("operation fact erased")
	}
}
func TestCopyProjectionPreservesCompleteEffectInSmallBudgets(t *testing.T) {
	source := strings.Repeat("a", 180) + "/source"
	destination := strings.Repeat("b", 180) + "/destination"
	e := fileeffects.New("copy", source, destination, "file")
	e.Source.Version = "entry-v1:" + strings.Repeat("a", 64)
	v := map[string]any{"file_effect": e, "operation": "copy", "path": source, "source": source, "destination": destination, "source_unchanged": true, "publication_outcome": "unknown", "error": workspace.CodeOutcomeUnknown, "code": workspace.CodeOutcomeUnknown, "preview": strings.Repeat("preview", 1000)}
	for _, text := range append(workspaceBudgetProjectionCandidates("copy", v), boundedProjectionJSON("copy", v, maxHistoryToolOutputBytes, "test"), projectToolResult("copy", v).History) {
		var decoded struct {
			Effect fileeffects.Effect `json:"file_effect"`
		}
		if json.Unmarshal([]byte(text), &decoded) != nil || decoded.Effect != e {
			t.Fatal("truncated effect", text)
		}
	}
	if !groupHasSideEffects([]modelclient.Message{toolMessage("copy-call", "copy", `{}`)}) {
		t.Fatal("unprotected copy round")
	}
}
