package workspace

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestWorkspaceArchiveFilesDirectoriesAndProtection(t *testing.T) {
	for _, kind := range []string{"text", "binary", "directory", "empty_directory"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			w, err := Open(root)
			if err != nil {
				t.Fatal(err)
			}
			defer w.Close()
			source := "nested/待归档"
			physical := filepath.Join(root, filepath.FromSlash(source))
			if err := os.MkdirAll(filepath.Dir(physical), 0o700); err != nil {
				t.Fatal(err)
			}
			content := []byte("archive keeps the original bytes\r\n")
			if kind == "binary" {
				content = bytes.Repeat([]byte{0, 0xff, 0xfe, 1}, 300000)
			}
			child := ""
			if strings.Contains(kind, "directory") {
				if err := os.MkdirAll(physical, 0o700); err != nil {
					t.Fatal(err)
				}
				if kind == "directory" {
					child = "child.dat"
					if err := os.WriteFile(filepath.Join(physical, child), content, 0o600); err != nil {
						t.Fatal(err)
					}
				}
			} else if err := os.WriteFile(physical, content, 0o600); err != nil {
				t.Fatal(err)
			}
			raw, _ := json.Marshal(map[string]string{"path": source})
			prepared, failure := w.PrepareMutation(t.Context(), ToolArchive, string(raw))
			if prepared == nil || failure.Value != nil {
				t.Fatalf("prepare: %+v", failure)
			}
			if _, err := os.Stat(filepath.Join(root, ArchiveDirectory)); !os.IsNotExist(err) {
				t.Fatalf("prepare created archive: %v", err)
			}
			if !strings.HasPrefix(prepared.Presentation.ArchivePath, ArchiveDirectory+"/") || !strings.HasSuffix(prepared.Presentation.ArchivePath, "/"+source) {
				t.Fatalf("destination: %+v", prepared.Presentation)
			}
			if denied := MutationDenied(prepared); denied.Publication != PublicationUnchanged {
				t.Fatalf("denied: %+v", denied)
			}
			if _, err := os.Stat(physical); err != nil {
				t.Fatal(err)
			}
			result := w.CommitMutation(t.Context(), prepared)
			if result.Publication != PublicationCompleted || resultCode(t, result) != "" {
				t.Fatalf("commit: %+v", result)
			}
			object := resultObject(t, result)
			destination := object["archive_path"].(string)
			if result.Reference == nil || !result.Reference.IsArchive() || !result.Reference.InvalidateObserved || result.Reference.ContentHash != "" {
				t.Fatalf("reference: %+v", result.Reference)
			}
			if _, err := os.Stat(physical); !os.IsNotExist(err) {
				t.Fatalf("source remains: %v", err)
			}
			archived := filepath.Join(root, filepath.FromSlash(destination))
			if kind != "empty_directory" {
				if child != "" {
					archived = filepath.Join(archived, child)
				}
				got, err := os.ReadFile(archived)
				if err != nil || !bytes.Equal(got, content) {
					t.Fatalf("archive bytes differ: %v", err)
				}
			}
			writeRaw, _ := json.Marshal(map[string]string{"path": destination + "/forbidden.txt", "mode": "create", "content": "no"})
			if candidate, failure := w.PrepareMutation(t.Context(), ToolWrite, string(writeRaw)); candidate != nil || resultCode(t, failure) != CodeArchiveProtected {
				t.Fatalf("write bypassed archive protection: %+v", failure)
			}
			archiveRaw, _ := json.Marshal(map[string]string{"path": destination})
			if candidate, failure := w.PrepareMutation(t.Context(), ToolArchive, string(archiveRaw)); candidate != nil || resultCode(t, failure) != CodeArchiveProtected {
				t.Fatalf("rearchive: %+v", failure)
			}
			if duplicate := w.CommitMutation(t.Context(), prepared); resultCode(t, duplicate) != CodeInvalidArguments {
				t.Fatalf("replayed: %+v", duplicate)
			}
		})
	}
}

func TestWorkspaceArchiveUniqueDestinationsAndReadOnlyAccess(t *testing.T) {
	root := t.TempDir()
	w, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	var destinations []string
	for range 2 {
		if err := os.WriteFile(filepath.Join(root, "note.txt"), []byte("archivedneedle\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		prepared, failure := w.PrepareMutation(t.Context(), ToolArchive, `{"path":"note.txt"}`)
		if prepared == nil {
			t.Fatalf("prepare: %+v", failure)
		}
		result := w.CommitMutation(t.Context(), prepared)
		if result.Publication != PublicationCompleted {
			t.Fatalf("archive: %+v", result)
		}
		destinations = append(destinations, prepared.Presentation.ArchivePath)
	}
	if destinations[0] == destinations[1] {
		t.Fatal("reused destination")
	}
	for _, destination := range destinations {
		raw, _ := json.Marshal(map[string]string{"path": destination})
		read := resultObject(t, w.Execute(t.Context(), ToolRead, string(raw)))
		if read["content"] != "archivedneedle\n" {
			t.Fatalf("read archive: %+v", read)
		}
		for _, tool := range []string{ToolWrite, ToolEdit} {
			args := map[string]any{"path": destination, "expected_hash": read["content_hash"]}
			if tool == ToolWrite {
				args["mode"], args["content"] = "replace", "changed"
			} else {
				args["edits"] = []map[string]string{{"old_text": "archivedneedle", "new_text": "changed"}}
			}
			raw, _ := json.Marshal(args)
			if prepared, failure := w.PrepareMutation(t.Context(), tool, string(raw)); prepared != nil || resultCode(t, failure) != CodeArchiveProtected {
				t.Fatalf("%s protection: %+v", tool, failure)
			}
		}
	}
	ordinary := resultObject(t, w.Execute(t.Context(), ToolSearch, `{"query":"archivedneedle"}`))
	if ordinary["returned"] != 0 {
		t.Fatalf("default search included archives: %+v", ordinary)
	}
	explicit := resultObject(t, w.Execute(t.Context(), ToolSearch, `{"query":"archivedneedle","path":".edu-agent-archive"}`))
	if explicit["returned"] != 2 {
		t.Fatalf("explicit search: %+v", explicit)
	}
	// Only the user/test fixture, never the archive executor, removes old archives.
	if err := os.RemoveAll(filepath.Join(root, ArchiveDirectory)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "note.txt"), []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	prepared, failure := w.PrepareMutation(t.Context(), ToolArchive, `{"path":"note.txt"}`)
	if prepared == nil {
		t.Fatalf("prepare after manual cleanup: %+v", failure)
	}
	if result := w.CommitMutation(t.Context(), prepared); result.Publication != PublicationCompleted {
		t.Fatalf("recreate: %+v", result)
	}
}

func TestWorkspaceArchiveConflictCancellationAndFrozenCandidate(t *testing.T) {
	for _, scenario := range []string{"changed", "cancelled", "tampered", "collision"} {
		t.Run(scenario, func(t *testing.T) {
			root := t.TempDir()
			w, err := Open(root)
			if err != nil {
				t.Fatal(err)
			}
			defer w.Close()
			physical := filepath.Join(root, "note.txt")
			if err := os.WriteFile(physical, []byte("old"), 0o600); err != nil {
				t.Fatal(err)
			}
			prepared, failure := w.PrepareMutation(t.Context(), ToolArchive, `{"path":"note.txt"}`)
			if prepared == nil {
				t.Fatalf("prepare: %+v", failure)
			}
			ctx := t.Context()
			want := CodeContentChanged
			switch scenario {
			case "changed":
				if err := os.WriteFile(physical, []byte("external changed"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "cancelled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
				want = CodeCancelled
			case "tampered":
				prepared.Presentation.ArchivePath += "-different"
				want = CodeInvalidArguments
			case "collision":
				destination := filepath.Join(root, filepath.FromSlash(prepared.Presentation.ArchivePath))
				if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(destination, []byte("keep-existing"), 0o600); err != nil {
					t.Fatal(err)
				}
				want = CodeAlreadyExists
			}
			result := w.CommitMutation(ctx, prepared)
			if resultCode(t, result) != want {
				t.Fatalf("got %+v want %s", result, want)
			}
			if _, err := os.Stat(physical); err != nil {
				t.Fatalf("source lost: %v", err)
			}
			if scenario == "collision" {
				data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(prepared.Presentation.ArchivePath)))
				if err != nil || string(data) != "keep-existing" {
					t.Fatalf("overwrote archive: %q %v", data, err)
				}
			}
		})
	}
}

func TestWorkspaceArchiveRejectsUnsafeInputsAndKeepsInternalLinks(t *testing.T) {
	root := t.TempDir()
	w, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	for _, raw := range []string{`{"path":"."}`, `{"path":"../outside"}`, `{"path":"/absolute"}`, `{"path":".edu-agent-archive"}`, `{"path":".EDU-AGENT-ARCHIVE/x"}`, `{"path":"x","force":true}`, `{"path":"x","destination":"y"}`, `{"path":"x","permanent":true}`} {
		if prepared, failure := w.PrepareMutation(t.Context(), ToolArchive, raw); prepared != nil || resultCode(t, failure) == "" {
			t.Fatalf("accepted %s", raw)
		}
	}
	if runtime.GOOS == "windows" {
		return
	} // Windows reparse security is tested in securefile's native fixtures.
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("untouched"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "tree"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "tree", "link")); err != nil {
		t.Fatal(err)
	}
	if prepared, failure := w.PrepareMutation(t.Context(), ToolArchive, `{"path":"tree/link"}`); prepared != nil || resultCode(t, failure) != CodeLinkNotAllowed {
		t.Fatalf("accepted source link: %+v", failure)
	}
	prepared, failure := w.PrepareMutation(t.Context(), ToolArchive, `{"path":"tree"}`)
	if prepared == nil {
		t.Fatalf("prepare: %+v", failure)
	}
	if result := w.CommitMutation(t.Context(), prepared); result.Publication != PublicationCompleted {
		t.Fatalf("directory move: %+v", result)
	}
	link, err := os.Readlink(filepath.Join(root, filepath.FromSlash(prepared.Presentation.ArchivePath), "link"))
	if err != nil || link != outside {
		t.Fatalf("link changed: %q %v", link, err)
	}
	data, err := os.ReadFile(filepath.Join(outside, "secret"))
	if err != nil || string(data) != "untouched" {
		t.Fatalf("outside changed: %q %v", data, err)
	}
}

func TestWorkspaceArchiveInvalidatesAffectedSnapshotsOnly(t *testing.T) {
	archive := &Reference{Path: "notes/topic", Kind: "archive_directory", InvalidateObserved: true}
	for _, previous := range []*Reference{
		{Path: "notes/topic/file.md", Kind: "file"}, {Path: "notes/topic/sub", Kind: "directory_listing"},
		{Path: "notes", Kind: "directory_listing"}, {Path: ".", Kind: "search_result"}, {Path: "notes", Kind: "search_result"},
	} {
		if !archive.Supersedes(previous) {
			t.Fatalf("not invalidated: %+v", previous)
		}
	}
	for _, previous := range []*Reference{{Path: "notes/topic-other/file.md", Kind: "file"}, {Path: "other", Kind: "directory_listing"}, {Path: "notes/topic", Kind: "archive_directory"}} {
		if archive.Supersedes(previous) {
			t.Fatalf("unrelated evidence invalidated: %+v", previous)
		}
	}
}
