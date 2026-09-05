package agentloop

import (
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

func TestMoveProductionAuthorizationYOLOWALAndLateOutcomes(t *testing.T) {
	for _, mode := range []string{"approve", "decline", "yolo", "wal_failure", "cancel_pending", "late_cancel", "unknown", "samepath"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			if err := os.MkdirAll(filepath.Join(root, "source/child"), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, "source/child/file"), []byte("private-move-body\x00\xff"), 0600); err != nil {
				t.Fatal(err)
			}
			executor, err := workspace.Open(root)
			if err != nil {
				t.Fatal(err)
			}
			stat := executor.Execute(t.Context(), "stat", `{"path":"source"}`)
			version := stat.Value.(map[string]any)["entry_version"].(string)
			destination := "target"
			if mode == "samepath" {
				destination = "source"
			}
			raw, _ := json.Marshal(map[string]any{"source": "source", "destination": destination, "expected_version": version})
			model := &fakeModel{responses: []modelclient.Response{{Message: toolMessage("move-call", "move", string(raw))}, {Message: modelclient.Message{Role: "assistant", Content: "结束"}}}}
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
			result, err := session.Send(ctx, "移动目录")
			if err != nil {
				t.Fatal(err)
			}
			if mode != "yolo" && mode != "samepath" {
				pending := result.PendingFileMutation
				if pending == nil || pending.Path != "source" || pending.DestinationPath != "target" || pending.EntryKind != "directory" || pending.BaseVersion != version || !strings.Contains(pending.Preview, version) {
					t.Fatal("bad authorization", result)
				}
				entries, _ := os.ReadDir(root)
				if len(entries) != 1 || entries[0].Name() != "source" {
					t.Fatal("prepare side effect", entries)
				}
				if mode == "cancel_pending" {
					_, err = session.CancelPendingFileMutation("move-call")
				} else {
					resolution := FileMutationApprove
					if mode == "decline" {
						resolution = FileMutationDecline
					}
					result, err = session.ResolveFileMutation(ctx, "move-call", resolution)
				}
				if mode == "wal_failure" {
					if err == nil {
						t.Fatal("WAL failed open")
					}
				} else if err != nil {
					t.Fatal(err)
				}
			}
			failed := mode == "decline" || mode == "wal_failure" || mode == "cancel_pending" || mode == "samepath"
			if failed {
				entries, _ := os.ReadDir(root)
				if len(entries) != 1 || entries[0].Name() != "source" || wrapped.commits != 0 {
					t.Fatal("unauthorized rename", entries, wrapped.commits)
				}
				if mode != "wal_failure" && len(sink.files) != 0 {
					t.Fatal("unapproved WAL")
				}
				if mode == "samepath" && result.PendingFileMutation != nil {
					t.Fatal("no-op needs authorization")
				}
				return
			}
			if wrapped.commits != 1 || len(sink.files) != 1 || sink.files[0].Effect.Source.Version != version || sink.files[0].Effect.Target.Path != "target" || sink.files[0].Effect.Target.Version != "" {
				t.Fatal("bad WAL", sink.files)
			}
			if _, err = os.Stat(filepath.Join(root, "source")); !os.IsNotExist(err) {
				t.Fatal("source remains")
			}
			data, err := os.ReadFile(filepath.Join(root, "target/child/file"))
			if err != nil || string(data) != "private-move-body\x00\xff" {
				t.Fatal("bad target", err)
			}
			if mode == "unknown" || mode == "late_cancel" {
				if len(model.requests) != 1 || !strings.Contains(result.Text, "source") || !strings.Contains(result.Text, "target") || strings.Contains(result.Text, "未修改源") {
					t.Fatal("lost true late outcome", result)
				}
			}
			if mode == "unknown" && !strings.Contains(result.Text, "不会自动重试") {
				t.Fatal(result.Text)
			}
			for _, req := range model.requests {
				b, _ := json.Marshal(req)
				if strings.Contains(string(b), "private-move-body") {
					t.Fatal("body leaked")
				}
			}
			history := session.toolHistory["move-call"]
			if !strings.Contains(history, version) || !strings.Contains(history, "target") || !fileOperationFact(history) {
				t.Fatal(history)
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
			if restored.toolHistory["move-call"] != history || restored.FileAuthorizationMode() != FileAuthorizationConfirm {
				t.Fatal("restore lost fact")
			}
		})
	}
}
func TestMoveBothSubtreeObservationsExpireAndFactsSurvive(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "source/child"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "source/child/file"), []byte("input"), 0600); err != nil {
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
	for _, c := range [][3]string{{"src", "stat", `{"path":"source"}`}, {"child", "read", `{"path":"source/child/file"}`}, {"dst", "stat", `{"path":"target/child/file"}`}, {"list", "list", `{}`}, {"find", "find", `{"pattern":"**"}`}, {"search", "search", `{"query":"input"}`}} {
		calls = append(calls, modelclient.ToolCall{ID: c[0], Type: "function", Function: modelclient.ToolFunction{Name: c[1], Arguments: c[2]}})
	}
	model := &fakeModel{responses: []modelclient.Response{{Message: modelclient.Message{Role: "assistant", ToolCalls: calls}}, {Message: modelclient.Message{Role: "assistant", Content: "观察"}}, {Message: toolMessage("move-call", "move", string(raw))}, {Message: modelclient.Message{Role: "assistant", Content: "移动"}}, {Message: toolMessage("new-stat", "stat", `{"path":"target"}`)}, {Message: modelclient.Message{Role: "assistant", Content: "检查"}}}}
	s := newDurableTestSession(t, model, &fakeServer{}, executor, &durabilitySink{})
	defer s.Close()
	if _, err = s.Send(t.Context(), "观察"); err != nil {
		t.Fatal(err)
	}
	if err = s.SetFileAuthorizationMode(FileAuthorizationYOLO); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Send(t.Context(), "移动"); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"src", "child", "dst", "list", "find", "search"} {
		if !strings.Contains(s.toolHistory[id], "superseded") {
			t.Fatal("not invalidated", id, s.toolHistory[id])
		}
	}
	fact := s.toolHistory["move-call"]
	if _, err = s.Send(t.Context(), "检查"); err != nil {
		t.Fatal(err)
	}
	if s.toolHistory["move-call"] != fact {
		t.Fatal("lost historical move")
	}
}
func TestMoveProjectionKeepsEndpointsAndVersion(t *testing.T) {
	e := fileeffects.New("move", strings.Repeat("a", 180)+"/source", strings.Repeat("b", 180)+"/target", "directory")
	e.Source.Version = "entry-v1:" + strings.Repeat("c", 64)
	v := map[string]any{"file_effect": e, "operation": "move", "path": e.Source.Path, "source": e.Source.Path, "destination": e.Target.Path, "publication_outcome": "unknown", "error": workspace.CodeOutcomeUnknown, "code": workspace.CodeOutcomeUnknown, "preview": strings.Repeat("preview", 1000)}
	for _, text := range append(workspaceBudgetProjectionCandidates("move", v), boundedProjectionJSON("move", v, maxHistoryToolOutputBytes, "test"), projectToolResult("move", v).History) {
		var decoded struct {
			Effect fileeffects.Effect `json:"file_effect"`
		}
		if json.Unmarshal([]byte(text), &decoded) != nil || decoded.Effect != e {
			t.Fatal("truncated fact", text)
		}
	}
	if !groupHasSideEffects([]modelclient.Message{toolMessage("move-call", "move", `{}`)}) {
		t.Fatal("unprotected move")
	}
	if !strings.Contains(workspaceSystemPrompt, "move:stat expected_version") || !strings.Contains(workspaceSystemPrompt, "never copy+delete") {
		t.Fatal("missing move safety prompt")
	}
}
