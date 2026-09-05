package agentloop

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/modelclient"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/workspace"
)

func TestArchiveToolAuthorizationAndWriteAhead(t *testing.T) {
	for _, mode := range []string{"approve", "decline", "yolo", "wal_failure"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			if err := os.Mkdir(filepath.Join(root, "notes"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, "notes", "data.bin"), []byte{0, 0xff, 1}, 0o600); err != nil {
				t.Fatal(err)
			}
			executor, err := workspace.Open(root)
			if err != nil {
				t.Fatal(err)
			}
			model := &fakeModel{responses: []modelclient.Response{
				{Message: toolMessage("archive-call", workspace.ToolArchive, `{"path":"notes"}`)},
				{Message: modelclient.Message{Role: "assistant", Content: "处理结束"}},
			}}
			sink := &durabilitySink{}
			if mode == "wal_failure" {
				sink.fileErr = errors.New("write-ahead unavailable")
			}
			session := newDurableTestSession(t, model, &fakeServer{}, executor, sink)
			defer session.Close()
			if mode == "yolo" {
				if err := session.SetFileAuthorizationMode(FileAuthorizationYOLO); err != nil {
					t.Fatal(err)
				}
			}
			var activities []Activity
			ctx := WithActivityReporter(t.Context(), func(value Activity) { activities = append(activities, value) })
			result, err := session.Send(ctx, "删除 notes 目录，只能归档")
			if err != nil {
				t.Fatal(err)
			}
			destination := ""
			if mode != "yolo" {
				pending := result.PendingFileMutation
				if pending == nil || pending.Tool != workspace.ToolArchive || pending.EntryKind != "directory" || pending.ArchivePath == "" {
					t.Fatalf("pending: %+v", result)
				}
				destination = pending.ArchivePath
				if _, err := os.Stat(filepath.Join(root, workspace.ArchiveDirectory)); !os.IsNotExist(err) {
					t.Fatalf("archive created before authorization: %v", err)
				}
				resolution := FileMutationApprove
				if mode == "decline" {
					resolution = FileMutationDecline
				}
				result, err = session.ResolveFileMutation(ctx, "archive-call", resolution)
				if mode == "wal_failure" {
					if err == nil {
						t.Fatal("archive bypassed failed write-ahead")
					}
				} else if err != nil {
					t.Fatal(err)
				}
			} else if result.PendingFileMutation != nil {
				t.Fatal("YOLO requested authorization")
			}
			if mode == "decline" || mode == "wal_failure" {
				if _, err := os.Stat(filepath.Join(root, "notes", "data.bin")); err != nil {
					t.Fatalf("source changed: %v", err)
				}
				if _, err := os.Stat(filepath.Join(root, workspace.ArchiveDirectory)); !os.IsNotExist(err) {
					t.Fatalf("failed/denied call made archive: %v", err)
				}
				if mode == "decline" && len(sink.files) != 0 {
					t.Fatal("decline wrote mutation WAL")
				}
				return
			}
			if len(sink.files) != 1 || sink.files[0].Operation != workspace.ToolArchive || sink.files[0].Kind != "directory" || sink.files[0].ArchivePath == "" {
				t.Fatalf("write-ahead: %+v", sink.files)
			}
			if destination != "" && sink.files[0].ArchivePath != destination {
				t.Fatal("WAL destination differs from approval")
			}
			destination = sink.files[0].ArchivePath
			if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(destination), "data.bin")); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(filepath.Join(root, "notes")); !os.IsNotExist(err) {
				t.Fatalf("source remains: %v", err)
			}
			found := false
			for _, activity := range activities {
				if activity.Event.ID == "archive-call" && activity.Event.Status == EventSucceeded && activity.File != nil && activity.File.ArchivePath == destination {
					found = true
				}
			}
			if !found {
				t.Fatalf("missing archive terminal detail: %+v", activities)
			}
			checkpoint, err := session.ExportCheckpoint()
			if err != nil {
				t.Fatal(err)
			}
			fresh := false
			for _, source := range checkpoint.Context.Sources {
				if source.WorkspaceReference != nil && source.WorkspaceReference.IsArchive() && source.Freshness == FreshnessWorkspaceObserved {
					fresh = true
				}
			}
			if !fresh {
				t.Fatal("archive receipt incorrectly marked stale")
			}
		})
	}
}

func TestArchiveToolModelFailureRetainsDestinationAndInvalidatesTree(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "notes"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "notes", "a.md"), []byte("historical file body"), 0o600); err != nil {
		t.Fatal(err)
	}
	executor, err := workspace.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	model := &fakeModel{responses: []modelclient.Response{
		{Message: toolMessage("read-call", workspace.ToolRead, `{"path":"notes/a.md"}`)},
		{Message: modelclient.Message{Role: "assistant", Content: "已读"}},
		{Message: toolMessage("archive-call", workspace.ToolArchive, `{"path":"notes"}`)},
	}}
	session := newWorkspaceTestSession(t, model, executor)
	defer session.Close()
	if _, err := session.Send(t.Context(), "读取笔记"); err != nil {
		t.Fatal(err)
	}
	pending, err := session.Send(t.Context(), "归档目录")
	if err != nil || pending.PendingFileMutation == nil {
		t.Fatalf("pending=%+v err=%v", pending, err)
	}
	destination := pending.PendingFileMutation.ArchivePath
	result, err := session.ResolveFileMutation(t.Context(), "archive-call", FileMutationApprove)
	if err != nil || !strings.Contains(result.Text, destination) || !strings.Contains(result.Text, "已归档") {
		t.Fatalf("lost effect after model failure: %+v %v", result, err)
	}
	if strings.Contains(session.toolHistory["read-call"], "historical file body") || !strings.Contains(session.toolHistory["read-call"], "superseded") {
		t.Fatalf("old tree content still current: %s", session.toolHistory["read-call"])
	}
	checkpoint, err := session.ExportCheckpoint()
	if err != nil {
		t.Fatal(err)
	}
	foundHistory := false
	for _, source := range checkpoint.Context.Sources {
		if source.WorkspaceReference != nil && source.WorkspaceReference.Path == "notes/a.md" {
			foundHistory = true
			if source.Freshness != FreshnessWorkspaceSuperseded || !strings.Contains(source.RecallText, "historical file body") {
				t.Fatalf("historical evidence not retained as historical: %+v", source)
			}
		}
	}
	if !foundHistory {
		t.Fatal("lost historical source")
	}
}

func TestArchiveToolUnknownOutcomePreservesBothPathsAndStops(t *testing.T) {
	const destination = ".edu-agent-archive/20260905-id/notes"
	prepared := &workspace.PreparedMutation{Presentation: workspace.MutationPresentation{
		Tool: workspace.ToolArchive, Operation: workspace.ToolArchive, Path: "notes", ArchivePath: destination, EntryKind: "directory", PreviewKind: "archive", Preview: "notes -> " + destination,
	}}
	executor := &fakeWorkspaceExecutor{
		status: workspace.Status{Available: true, Label: "project"}, prepared: map[string]*workspace.PreparedMutation{workspace.ToolArchive: prepared},
		commitResults: []workspace.Result{{
			Value:     map[string]any{"path": "notes", "archive_path": destination, "operation": "archive", "entry_type": "directory", "error": workspace.CodeOutcomeUnknown, "code": workspace.CodeOutcomeUnknown, "publication_outcome": "unknown"},
			Reference: &workspace.Reference{Path: "notes", Kind: "archive_directory", InvalidateObserved: true}, Publication: workspace.PublicationUnknown,
		}},
	}
	model := &fakeModel{responses: []modelclient.Response{{Message: modelclient.Message{Role: "assistant", ToolCalls: []modelclient.ToolCall{
		{ID: "archive-call", Type: "function", Function: modelclient.ToolFunction{Name: workspace.ToolArchive, Arguments: `{"path":"notes"}`}},
		{ID: "sibling", Type: "function", Function: modelclient.ToolFunction{Name: workspace.ToolRead, Arguments: `{"path":"notes/a.md"}`}},
	}}}}}
	session := newWorkspaceTestSession(t, model, executor)
	defer session.Close()
	if err := session.SetFileAuthorizationMode(FileAuthorizationYOLO); err != nil {
		t.Fatal(err)
	}
	result, err := session.Send(context.Background(), "归档目录")
	if err != nil || !strings.Contains(result.Text, destination) || !strings.Contains(result.Text, "不会自动重试") || len(model.requests) != 1 || len(executor.calls) != 0 {
		t.Fatalf("unknown result=%+v err=%v requests=%d siblings=%v", result, err, len(model.requests), executor.calls)
	}
}
