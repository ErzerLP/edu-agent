package agentloop

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/modelclient"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/workspace"
)

func TestMkdirProductionAuthorizationYOLOAndWAL(t *testing.T) {
	for _, mode := range []string{"approve", "decline", "yolo", "wal_failure", "cancel_pending"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			executor, err := workspace.Open(root)
			if err != nil {
				t.Fatal(err)
			}
			model := &fakeModel{responses: []modelclient.Response{{Message: toolMessage("mkdir-call", workspace.ToolMkdir, `{"path":"new/deep","parents":true}`)}, {Message: modelclient.Message{Role: "assistant", Content: "处理结束"}}}}
			sink := &durabilitySink{}
			if mode == "wal_failure" {
				sink.fileErr = errors.New("storage unavailable")
			}
			session := newDurableTestSession(t, model, &fakeServer{}, executor, sink)
			defer session.Close()
			if mode == "yolo" {
				if err = session.SetFileAuthorizationMode(FileAuthorizationYOLO); err != nil {
					t.Fatal(err)
				}
			}
			result, err := session.Send(t.Context(), "创建目录")
			if err != nil {
				t.Fatal(err)
			}
			if mode != "yolo" {
				if result.PendingFileMutation == nil || !strings.Contains(result.PendingFileMutation.Preview, "new/deep") || !strings.Contains(result.PendingFileMutation.Preview, "锚点") {
					t.Fatalf("pending=%+v", result)
				}
				if _, err = os.Stat(filepath.Join(root, "new")); !os.IsNotExist(err) {
					t.Fatalf("premature creation: %v", err)
				}
				if mode == "cancel_pending" {
					if _, err = session.CancelPendingFileMutation("mkdir-call"); err != nil {
						t.Fatal(err)
					}
				} else {
					resolution := FileMutationApprove
					if mode == "decline" {
						resolution = FileMutationDecline
					}
					result, err = session.ResolveFileMutation(t.Context(), "mkdir-call", resolution)
					if mode == "wal_failure" {
						if err == nil {
							t.Fatal("failed WAL allowed mkdir")
						}
					} else if err != nil {
						t.Fatal(err)
					}
				}
			}
			if mode == "decline" || mode == "wal_failure" || mode == "cancel_pending" {
				if _, err = os.Stat(filepath.Join(root, "new")); !os.IsNotExist(err) {
					t.Fatal("denied/cancelled mkdir created")
				}
				if mode != "wal_failure" && len(sink.files) != 0 {
					t.Fatal("unapproved mkdir wrote WAL")
				}
				return
			}
			if len(sink.files) != 1 || sink.files[0].Effect.Operation != "mkdir" || sink.files[0].Effect.Directories.Created != 0 || sink.files[0].Effect.Directories.Count != 2 {
				t.Fatalf("WAL=%+v", sink.files)
			}
			if _, err = os.Stat(filepath.Join(root, "new/deep")); err != nil {
				t.Fatal(err)
			}
			checkpoint, err := session.ExportCheckpoint()
			if err != nil {
				t.Fatal(err)
			}
			restored := newDurableTestSession(t, &fakeModel{}, &fakeServer{}, nil, &durabilitySink{})
			defer restored.Close()
			if err = restored.RestoreCheckpoint(checkpoint); err != nil {
				t.Fatal(err)
			}
			if restored.FileAuthorizationMode() != FileAuthorizationConfirm {
				t.Fatal("YOLO restored")
			}
		})
	}
}
func TestMkdirPartialProductionOutcomeRetainedAndNoSiblingReplay(t *testing.T) {
	root := t.TempDir()
	executor, err := workspace.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	path := "partial/" + strings.Repeat("x", 256) + "/last"
	raw, _ := json.Marshal(map[string]any{"path": path, "parents": true})
	model := &fakeModel{responses: []modelclient.Response{{Message: modelclient.Message{Role: "assistant", ToolCalls: []modelclient.ToolCall{
		{ID: "mkdir-call", Type: "function", Function: modelclient.ToolFunction{Name: "mkdir", Arguments: string(raw)}},
		{ID: "later", Type: "function", Function: modelclient.ToolFunction{Name: "mkdir", Arguments: `{"path":"must-not-exist"}`}},
	}}}}}
	session := newDurableTestSession(t, model, &fakeServer{}, executor, &durabilitySink{})
	defer session.Close()
	if err = session.SetFileAuthorizationMode(FileAuthorizationYOLO); err != nil {
		t.Fatal(err)
	}
	result, err := session.Send(t.Context(), "创建多级目录")
	if err != nil || !strings.Contains(result.Text, "结果未知") || len(model.requests) != 1 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if _, err = os.Stat(filepath.Join(root, "partial")); err != nil {
		t.Fatal("lost actual partial creation", err)
	}
	if _, err = os.Stat(filepath.Join(root, "must-not-exist")); !os.IsNotExist(err) {
		t.Fatal("sibling replayed")
	}
	checkpoint, err := session.ExportCheckpoint()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(session.toolHistory["mkdir-call"], `"created":1`) {
		t.Fatalf("lost known prefix: %s", session.toolHistory["mkdir-call"])
	}
	if len(checkpoint.Turns) != 1 || !checkpoint.Turns[0].FileEffectUnknown || !checkpoint.Turns[0].Protected || !groupHasSideEffects(checkpoint.Messages) {
		t.Fatal("partial effect unprotected")
	}
}
func TestMkdirInvalidatesObservationsButKeepsOperationFacts(t *testing.T) {
	root := t.TempDir()
	executor, err := workspace.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	model := &fakeModel{responses: []modelclient.Response{
		{Message: modelclient.Message{Role: "assistant", ToolCalls: []modelclient.ToolCall{
			{ID: "stat", Type: "function", Function: modelclient.ToolFunction{Name: "stat", Arguments: `{"path":"."}`}},
			{ID: "list", Type: "function", Function: modelclient.ToolFunction{Name: "list", Arguments: `{}`}},
			{ID: "find", Type: "function", Function: modelclient.ToolFunction{Name: "find", Arguments: `{"pattern":"**"}`}},
			{ID: "search", Type: "function", Function: modelclient.ToolFunction{Name: "search", Arguments: `{"query":"text"}`}},
		}}},
		{Message: modelclient.Message{Role: "assistant", Content: "已观察"}},
		{Message: toolMessage("mkdir-one", "mkdir", `{"path":"a/b","parents":true}`)},
		{Message: modelclient.Message{Role: "assistant", Content: "已创建"}},
		{Message: toolMessage("mkdir-two", "mkdir", `{"path":"a/b/c"}`)},
		{Message: modelclient.Message{Role: "assistant", Content: "已创建"}},
	}}
	session := newDurableTestSession(t, model, &fakeServer{}, executor, &durabilitySink{})
	defer session.Close()
	if _, err = session.Send(t.Context(), "观察根目录"); err != nil {
		t.Fatal(err)
	}
	if err = session.SetFileAuthorizationMode(FileAuthorizationYOLO); err != nil {
		t.Fatal(err)
	}
	if _, err = session.Send(t.Context(), "创建目录"); err != nil {
		t.Fatal(err)
	}
	for _, call := range []string{"stat", "list", "find", "search"} {
		if !strings.Contains(session.toolHistory[call], "superseded") {
			t.Fatalf("%s not expired: %s", call, session.toolHistory[call])
		}
	}
	before := session.toolHistory["mkdir-one"]
	if _, err = session.Send(t.Context(), "再创建子目录"); err != nil {
		t.Fatal(err)
	}
	if before != session.toolHistory["mkdir-one"] || !strings.Contains(before, `"created":2`) {
		t.Fatal("prior operation fact overwritten")
	}
}
