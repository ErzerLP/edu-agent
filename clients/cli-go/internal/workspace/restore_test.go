package workspace

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func restoreWorkspaceFixture(t *testing.T) (*Workspace, string, string, string) {
	t.Helper()
	root := t.TempDir()
	source := ArchiveDirectory + "/legacy-container/nested/item"
	if err := os.MkdirAll(filepath.Dir(filepath.Join(root, source)), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, source), []byte("private-restore-body\x00\xff"), 0600); err != nil {
		t.Fatal(err)
	}
	w, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = w.Close() })
	stat := w.Execute(t.Context(), ToolStat, `{"path":"`+source+`"}`)
	version := stat.Value.(map[string]any)["entry_version"].(string)
	raw, _ := json.Marshal(map[string]any{"source": source, "destination": "recovered", "expected_version": version})
	return w, root, source, string(raw)
}

func TestArchiveRestoreWorkspaceStrictPlanAndPublication(t *testing.T) {
	w, root, source, raw := restoreWorkspaceFixture(t)
	for _, bad := range []string{`null`, `{}`, strings.Replace(raw, `"source":`, `"Source":`, 1), strings.TrimSuffix(raw, "}") + `,"overwrite":true}`, strings.TrimSuffix(raw, "}") + `,"source":"other"}`, strings.Replace(raw, `"destination":"recovered"`, `"destination":null`, 1)} {
		if p, r := w.PrepareMutation(t.Context(), ToolRestoreArchive, bad); p != nil || resultCode(t, r) != CodeInvalidArguments {
			t.Fatal("bad arguments accepted", bad, r)
		}
	}
	if r := w.Execute(t.Context(), ToolRestoreArchive, raw); r.Publication == PublicationCompleted {
		t.Fatal("execute bypassed approval")
	}
	p, r := w.PrepareMutation(t.Context(), ToolRestoreArchive, raw)
	if p == nil {
		t.Fatal(r)
	}
	if len(p.candidate) != 0 || p.FullDiff() != "" || p.FileEffect().Validate() != nil || p.FileEffect().SchemaVersion != 3 || p.Presentation.DestinationPath != "recovered" {
		t.Fatal(p)
	}
	if denied := MutationDenied(p); denied.Effect != nil || denied.Publication != PublicationUnchanged || denied.Value.(map[string]any)["destination"] != "recovered" {
		t.Fatal(denied)
	}
	if _, err := os.Stat(filepath.Join(root, source)); err != nil {
		t.Fatal("prepare changed source", err)
	}
	r = w.CommitMutation(t.Context(), p)
	if r.Publication != PublicationCompleted || r.Effect == nil || !r.Effect.IsArchiveRestore() || r.Effect.Source.Path != source || r.Effect.Target.Path != "recovered" || r.Effect.Target.Version != "" || r.Reference.Kind != "restore_file" || !r.Reference.InvalidateObserved || r.Reference.ContentHash != "" {
		t.Fatal(r)
	}
	data, err := os.ReadFile(filepath.Join(root, "recovered"))
	if err != nil || !bytes.Equal(data, []byte("private-restore-body\x00\xff")) {
		t.Fatal(data, err)
	}
	if _, err := os.Stat(filepath.Join(root, source)); !os.IsNotExist(err) {
		t.Fatal("source remains", err)
	}
	encoded, _ := json.Marshal(r.Value)
	if bytes.Contains(encoded, []byte("private-restore-body")) {
		t.Fatal("body leaked")
	}
	if r := w.CommitMutation(t.Context(), p); r.Publication != PublicationUnchanged || r.Effect != nil {
		t.Fatal("reused plan", r)
	}
	if _, err := os.Stat(filepath.Join(root, ArchiveDirectory, "legacy-container")); err != nil {
		t.Fatal("container cleaned", err)
	}
}

func TestArchiveRestoreWorkspaceFrozenAuthorization(t *testing.T) {
	for _, field := range []string{"path", "target", "version", "kind", "tool", "operation", "preview", "truncated", "conflict"} {
		t.Run(field, func(t *testing.T) {
			w, root, source, raw := restoreWorkspaceFixture(t)
			p, r := w.PrepareMutation(t.Context(), ToolRestoreArchive, raw)
			if p == nil {
				t.Fatal(r)
			}
			switch field {
			case "path":
				p.Presentation.Path = "other"
			case "target":
				p.Presentation.DestinationPath = "other"
			case "version":
				p.Presentation.BaseVersion = "other"
			case "kind":
				p.Presentation.EntryKind = "directory"
			case "tool":
				p.Presentation.Tool = ToolMove
			case "operation":
				p.Presentation.Operation = ToolMove
			case "preview":
				p.Presentation.Preview = "other"
			case "truncated":
				p.Presentation.Truncated = true
			case "conflict":
				if err := os.WriteFile(filepath.Join(root, "recovered"), []byte("keep"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			r = w.CommitMutation(t.Context(), p)
			if r.Publication != PublicationUnchanged || r.Effect != nil || resultCode(t, r) == "" {
				t.Fatal("unsafe restore", r)
			}
			if _, err := os.Stat(filepath.Join(root, source)); err != nil {
				t.Fatal(err)
			}
			if field == "conflict" {
				data, _ := os.ReadFile(filepath.Join(root, "recovered"))
				if string(data) != "keep" {
					t.Fatal("overwrote target")
				}
			}
		})
	}
}

func TestArchiveRestoreLocateBeyondDirectoryWindow(t *testing.T) {
	root := t.TempDir()
	scope := ArchiveDirectory + "/old-container"
	if err := os.MkdirAll(filepath.Join(root, scope), 0700); err != nil {
		t.Fatal(err)
	}
	want := make([]string, 2501)
	for i := range want {
		want[i] = fmt.Sprintf("%s/item-%04d", scope, i)
		if err := os.WriteFile(filepath.Join(root, want[i]), []byte("body\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	w, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	for _, tool := range []string{ToolList, ToolFind} {
		args := map[string]any{"path": scope}
		if tool == ToolFind {
			args["pattern"], args["type"] = "**", "file"
		}
		got, final, pages := drainQueryTest(t, w, tool, args)
		if !slices.Equal(got, want) || pages < 2 || final["scan_complete"] != true {
			t.Fatalf("%s got=%d pages=%d final=%v", tool, len(got), pages, final)
		}
	}
	read := w.Execute(t.Context(), ToolRead, `{"path":"`+want[len(want)-1]+`"}`)
	if resultCode(t, read) != "" || read.Value.(map[string]any)["content"] != "body\n" {
		t.Fatal(read)
	}
}
