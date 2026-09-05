package workspace

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/securefile"
)

func TestStatWorkspaceMetadataHashAndNoSideEffects(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "folder"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, ArchiveDirectory), 0700); err != nil {
		t.Fatal(err)
	}
	data := []byte{0, 255, 13, 10, 0xef, 0xbb, 0xbf}
	for name, body := range map[string][]byte{"file": []byte("private body\r\n"), "binary": data, "large": bytes.Repeat([]byte{255}, (1<<20)+1), ArchiveDirectory + "/saved": []byte("archived")} {
		if err := os.WriteFile(filepath.Join(dir, name), body, 0600); err != nil {
			t.Fatal(err)
		}
	}
	w, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	before, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{".", "folder", "file", "binary", "large", ArchiveDirectory, ArchiveDirectory + "/saved", "missing", "missing/child"} {
		t.Run(path, func(t *testing.T) {
			raw, _ := json.Marshal(map[string]any{"path": path})
			result := w.Execute(t.Context(), ToolStat, string(raw))
			value := resultObject(t, result)
			if code := resultCode(t, result); code != "" {
				t.Fatalf("stat: %+v", result)
			}
			if result.Reference == nil || result.Reference.Kind != "entry_metadata" || result.Reference.ContentHash != hashProjection(value) || result.Publication != "" {
				t.Fatalf("reference: %+v", result)
			}
			if strings.HasPrefix(path, "missing") {
				if value["exists"] != false || len(value) != 3 {
					t.Fatalf("missing: %+v", value)
				}
			} else {
				if value["exists"] != true || value["entry_type"] == nil || value["mtime"] == nil || !strings.HasPrefix(value["entry_version"].(string), "entry-v1:") {
					t.Fatalf("metadata: %+v", value)
				}
				if value["entry_type"] == "directory" && value["size"] != nil {
					t.Fatalf("recursive directory size: %+v", value)
				}
				if value["entry_type"] == "file" && value["size"] == nil {
					t.Fatalf("missing file size: %+v", value)
				}
			}
			if value["content_hash"] != nil || value["content"] != nil || value["identity"] != nil || safeResultJSONSize(value) > w.limits.ResultBytes {
				t.Fatalf("unsafe metadata output: %+v", value)
			}
			encoded, _ := json.Marshal(value)
			if strings.Contains(string(encoded), dir) || strings.Contains(string(encoded), "private body") {
				t.Fatalf("private output: %s", encoded)
			}
		})
	}
	for _, path := range []string{"binary", "file", ArchiveDirectory + "/saved"} {
		raw, _ := json.Marshal(map[string]any{"path": path, "hash": true})
		result := w.Execute(t.Context(), ToolStat, string(raw))
		value := resultObject(t, result)
		body, err := os.ReadFile(filepath.Join(dir, path))
		if err != nil {
			t.Fatal(err)
		}
		if value["content_hash"] != fmt.Sprintf("sha256:%x", sha256.Sum256(body)) || value["content"] != nil || result.Reference.ContentHash == value["content_hash"] {
			t.Fatalf("raw hash or metadata reference: %+v", result)
		}
	}
	for _, path := range []string{"large", ".", "folder"} {
		metadataArgs, _ := json.Marshal(map[string]any{"path": path})
		observed := w.Execute(t.Context(), ToolStat, string(metadataArgs))
		hashArgs, _ := json.Marshal(map[string]any{"path": path, "hash": true})
		failed := w.Execute(t.Context(), ToolStat, string(hashArgs))
		code := CodeUnsupportedType
		if path == "large" {
			code = CodeFileTooLarge
		}
		if resultCode(t, failed) != code || !failed.Reference.Supersedes(observed.Reference) {
			t.Fatalf("hash refusal retained old metadata: %+v", failed)
		}
	}
	after, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Fatal("stat created entries")
	}
	if !IsReadTool(ToolStat) || IsMutationTool(ToolStat) {
		t.Fatal("stat registration is not read-only")
	}
	old := w.Execute(t.Context(), ToolStat, `{"path":"binary","hash":true}`)
	mtime := time.Now().Add(time.Hour)
	if err := os.Chtimes(filepath.Join(dir, "binary"), mtime, mtime); err != nil {
		t.Fatal(err)
	}
	fresh := w.Execute(t.Context(), ToolStat, `{"path":"binary","hash":true}`)
	if !fresh.Reference.Supersedes(old.Reference) || resultObject(t, old)["content_hash"] != resultObject(t, fresh)["content_hash"] {
		t.Fatal("touch with identical bytes did not invalidate metadata")
	}
}

func TestStatArgumentsPathsCancellationAndProgress(t *testing.T) {
	w, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	for _, raw := range []string{`null`, `[]`, `{"path":".","unexpected":true}`, `{"path":".","hash":"true"}`, `{"path":"."} {}`} {
		if code := resultCode(t, w.Execute(t.Context(), ToolStat, raw)); code != CodeInvalidArguments {
			t.Fatalf("%s: %s", raw, code)
		}
	}
	for _, path := range []string{"", "../escape", "/etc/passwd", `C:\\x`, "file:stream", "CON", "name.", "name ", "dir//x", "x\u202ey"} {
		raw, _ := json.Marshal(map[string]any{"path": path})
		result := w.Execute(t.Context(), ToolStat, string(raw))
		if code := resultCode(t, result); code != CodeInvalidPath && code != CodePathOutsideWorkspace {
			t.Fatalf("unsafe %q: %+v", path, result)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if code := resultCode(t, w.Execute(ctx, ToolStat, `{"path":"."}`)); code != CodeCancelled {
		t.Fatal(code)
	}
	ctx, stop := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
	defer stop()
	if code := resultCode(t, w.Execute(ctx, ToolStat, `{"path":"."}`)); code != CodeTimeout {
		t.Fatal(code)
	}
	var progress []Progress
	result := w.Execute(WithProgressReporter(t.Context(), func(p Progress) { progress = append(progress, p) }), ToolStat, `{"path":"."}`)
	if resultCode(t, result) != "" || len(progress) != 1 || progress[0].Path != "." || progress[0].Bytes != 0 {
		t.Fatalf("metadata progress: %+v %+v", progress, result)
	}
	if p, ok := InitialProgress(ToolStat, `{"path":"."}`); !ok || p.Path != "." {
		t.Fatalf("initial progress: %+v %v", p, ok)
	}
	// Production errors cannot masquerade as absence.
	for _, err := range []error{securefile.ErrPermission, securefile.ErrOutsideRoot, securefile.ErrLink} {
		if codeForSecureError(err) == CodeNotFound {
			t.Fatalf("unsafe missing classification: %v", err)
		}
	}
}

func TestStatReferenceInvalidationByWriteEditArchive(t *testing.T) {
	w, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	missing := w.Execute(t.Context(), ToolStat, `{"path":"notes/file"}`)
	root := w.Execute(t.Context(), ToolStat, `{"path":"."}`)
	prepared, failure := w.PrepareMutation(t.Context(), ToolWrite, `{"path":"notes/file","mode":"create","content":"old"}`)
	if prepared == nil {
		t.Fatalf("prepare: %+v", failure)
	}
	written := w.CommitMutation(t.Context(), prepared)
	if written.Publication != PublicationCompleted || !written.Reference.Supersedes(missing.Reference) || !written.Reference.Supersedes(root.Reference) {
		t.Fatalf("write invalidation: %+v", written)
	}
	metadata := w.Execute(t.Context(), ToolStat, `{"path":"notes/file"}`)
	raw, _ := json.Marshal(map[string]any{"path": "notes/file", "expected_hash": written.Reference.ContentHash, "edits": []map[string]string{{"old_text": "old", "new_text": "new"}}})
	prepared, failure = w.PrepareMutation(t.Context(), ToolEdit, string(raw))
	if prepared == nil {
		t.Fatalf("prepare: %+v", failure)
	}
	edited := w.CommitMutation(t.Context(), prepared)
	if edited.Publication != PublicationCompleted || !edited.Reference.Supersedes(metadata.Reference) {
		t.Fatalf("edit invalidation: %+v", edited)
	}
	metadata = w.Execute(t.Context(), ToolStat, `{"path":"notes/file"}`)
	prepared, failure = w.PrepareMutation(t.Context(), ToolArchive, `{"path":"notes"}`)
	if prepared == nil {
		t.Fatalf("prepare: %+v", failure)
	}
	archived := w.CommitMutation(t.Context(), prepared)
	if archived.Publication != PublicationCompleted || !archived.Reference.Supersedes(metadata.Reference) || !archived.Reference.Supersedes(root.Reference) || !archived.Reference.Supersedes(&Reference{Kind: "entry_metadata", Path: ArchiveDirectory + "/unknown", ContentHash: hashProjection(false)}) {
		t.Fatalf("archive invalidation: %+v", archived)
	}
	if archived.Reference.Supersedes(&Reference{Kind: "entry_metadata", Path: "unrelated"}) || archived.Reference.Supersedes(&Reference{Kind: "archive_directory", Path: "notes"}) {
		t.Fatal("unrelated/historical evidence invalidated")
	}
	unknown := &Reference{Path: "notes/file", Kind: "file", InvalidateObserved: true}
	if !unknown.Supersedes(metadata.Reference) {
		t.Fatal("unknown publication retained metadata")
	}
}
