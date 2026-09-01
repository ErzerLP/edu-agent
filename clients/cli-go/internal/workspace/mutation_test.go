package workspace

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestWorkspaceWriteCreateReplaceAndConflict(t *testing.T) {
	root := t.TempDir()
	value, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer value.Close()

	prepared, failure := value.PrepareMutation(t.Context(), ToolWrite, `{"path":"nested/note.txt","mode":"create","content":"first\n"}`)
	if prepared == nil || failure.Value != nil || prepared.Presentation.Operation != "write_create" || prepared.Presentation.Path != "nested/note.txt" {
		t.Fatalf("prepared=%+v failure=%+v", prepared, failure)
	}
	if _, err := os.Stat(filepath.Join(root, "nested", "note.txt")); !os.IsNotExist(err) {
		t.Fatalf("prepare changed disk: %v", err)
	}
	created := value.CommitMutation(t.Context(), prepared)
	if created.Publication != PublicationCompleted || resultCode(t, created) != "" {
		t.Fatalf("create result=%+v", created)
	}
	if data, err := os.ReadFile(filepath.Join(root, "nested", "note.txt")); err != nil || string(data) != "first\n" {
		t.Fatalf("created data=%q err=%v", data, err)
	}
	if repeated := value.CommitMutation(t.Context(), prepared); resultCode(t, repeated) != CodeInvalidArguments || repeated.Publication != PublicationUnchanged {
		t.Fatalf("repeated commit=%+v", repeated)
	}

	if err := os.Chmod(filepath.Join(root, "nested", "note.txt"), 0o600); err != nil {
		t.Fatal(err)
	}
	read := resultObject(t, value.Execute(t.Context(), ToolRead, `{"path":"nested/note.txt"}`))
	baseHash := read["content_hash"].(string)
	replaceArgs, _ := json.Marshal(map[string]any{
		"path": "nested/note.txt", "mode": "replace", "content": "approved\n", "expected_hash": baseHash,
	})
	replacement, failure := value.PrepareMutation(t.Context(), ToolWrite, string(replaceArgs))
	if replacement == nil || failure.Value != nil || replacement.Presentation.PreviewKind != "diff" {
		t.Fatalf("replacement=%+v failure=%+v", replacement, failure)
	}
	if err := os.WriteFile(filepath.Join(root, "nested", "note.txt"), []byte("external\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	conflict := value.CommitMutation(t.Context(), replacement)
	if resultCode(t, conflict) != CodeContentChanged || conflict.Publication != PublicationUnchanged || conflict.Reference == nil || !conflict.Reference.InvalidateObserved || conflict.Reference.ContentHash != baseHash {
		t.Fatalf("conflict=%+v", conflict)
	}
	if data, _ := os.ReadFile(filepath.Join(root, "nested", "note.txt")); string(data) != "external\n" {
		t.Fatalf("conflict overwrote external data: %q", data)
	}

	external := resultObject(t, value.Execute(t.Context(), ToolRead, `{"path":"nested/note.txt"}`))
	replaceArgs, _ = json.Marshal(map[string]any{
		"path": "nested/note.txt", "mode": "replace", "content": "approved\n", "expected_hash": external["content_hash"],
	})
	replacement, failure = value.PrepareMutation(t.Context(), ToolWrite, string(replaceArgs))
	if replacement == nil || failure.Value != nil {
		t.Fatalf("second replacement=%+v failure=%+v", replacement, failure)
	}
	replaced := value.CommitMutation(t.Context(), replacement)
	if replaced.Publication != PublicationCompleted || resultCode(t, replaced) != "" {
		t.Fatalf("replace result=%+v", replaced)
	}
	if data, err := os.ReadFile(filepath.Join(root, "nested", "note.txt")); err != nil || string(data) != "approved\n" {
		t.Fatalf("replaced data=%q err=%v", data, err)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(filepath.Join(root, "nested", "note.txt"))
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("replace mode=%v err=%v", info.Mode().Perm(), err)
		}
	}
}

func TestWorkspaceReplaceUsesCommitTimePermissionBits(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits are not a Windows ACL assertion")
	}
	root := t.TempDir()
	path := filepath.Join(root, "notes.txt")
	if err := os.WriteFile(path, []byte("old\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	value, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer value.Close()
	read := resultObject(t, value.Execute(t.Context(), ToolRead, `{"path":"notes.txt"}`))
	arguments, _ := json.Marshal(map[string]any{
		"path": "notes.txt", "mode": "replace", "content": "new\n", "expected_hash": read["content_hash"],
	})
	prepared, failure := value.PrepareMutation(t.Context(), ToolWrite, string(arguments))
	if prepared == nil || failure.Value != nil {
		t.Fatalf("prepared=%+v failure=%+v", prepared, failure)
	}
	if err := os.Chmod(path, 0o640); err != nil {
		t.Fatal(err)
	}
	if result := value.CommitMutation(t.Context(), prepared); result.Publication != PublicationCompleted {
		t.Fatalf("commit=%+v", result)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o640 {
		t.Fatalf("mode=%v err=%v", info.Mode().Perm(), err)
	}
}

func TestWorkspaceMutationQueueSerializesHardlinkAliases(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("hardlink alias identity is covered by native Windows security tests")
	}
	root := t.TempDir()
	original := filepath.Join(root, "original.txt")
	alias := filepath.Join(root, "alias.txt")
	if err := os.WriteFile(original, []byte("old\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(original, alias); err != nil {
		t.Skipf("hardlinks unavailable: %v", err)
	}
	value, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer value.Close()
	read := resultObject(t, value.Execute(t.Context(), ToolRead, `{"path":"alias.txt"}`))
	arguments, _ := json.Marshal(map[string]any{
		"path": "alias.txt", "mode": "replace", "content": "new\n", "expected_hash": read["content_hash"],
	})
	prepared, failure := value.PrepareMutation(t.Context(), ToolWrite, string(arguments))
	if prepared == nil || failure.Value != nil {
		t.Fatalf("prepared=%+v failure=%+v", prepared, failure)
	}
	snapshot, err := value.root.ReadSnapshot("original.txt", value.limits.FileBytes, false)
	if err != nil || snapshot.Identity == "" {
		t.Fatalf("snapshot=%+v err=%v", snapshot, err)
	}
	release, err := value.queues.acquire(t.Context(), "file:"+snapshot.Identity)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan Result, 1)
	go func() { done <- value.CommitMutation(context.Background(), prepared) }()
	select {
	case result := <-done:
		release()
		t.Fatalf("hardlink alias bypassed queue: %+v", result)
	case <-time.After(30 * time.Millisecond):
	}
	release()
	select {
	case result := <-done:
		if result.Publication != PublicationCompleted {
			t.Fatalf("commit=%+v", result)
		}
	case <-time.After(time.Second):
		t.Fatal("hardlink alias commit did not resume")
	}
}

func TestWorkspaceEditIsExactUniqueAndPreservesEncoding(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "notes.txt")
	original := append([]byte(nil), utf8BOM...)
	original = append(original, []byte("alpha\r\nbeta\r\n")...)
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	value, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer value.Close()

	read := resultObject(t, value.Execute(t.Context(), ToolRead, `{"path":"notes.txt"}`))
	arguments, _ := json.Marshal(map[string]any{
		"path": "notes.txt", "expected_hash": read["content_hash"],
		"edits": []map[string]string{
			{"old_text": "alpha\r\n", "new_text": "ALPHA\n"},
			{"old_text": "beta", "new_text": "BETA"},
		},
	})
	prepared, failure := value.PrepareMutation(t.Context(), ToolEdit, string(arguments))
	if prepared == nil || failure.Value != nil || prepared.Presentation.PreviewKind != "diff" || prepared.Presentation.Preview == "" {
		t.Fatalf("prepared=%+v failure=%+v", prepared, failure)
	}
	result := value.CommitMutation(t.Context(), prepared)
	if result.Publication != PublicationCompleted || resultCode(t, result) != "" {
		t.Fatalf("edit result=%+v", result)
	}
	want := append([]byte(nil), utf8BOM...)
	want = append(want, []byte("ALPHA\r\nBETA\r\n")...)
	if got, err := os.ReadFile(path); err != nil || !bytes.Equal(got, want) {
		t.Fatalf("edited bytes=%q want=%q err=%v", got, want, err)
	}

	tests := []struct {
		name  string
		text  string
		edits []map[string]string
		code  string
	}{
		{name: "missing", text: "abcdef", edits: []map[string]string{{"old_text": "zzz", "new_text": "x"}}, code: CodeReplacementMissing},
		{name: "not unique", text: "same same", edits: []map[string]string{{"old_text": "same", "new_text": "x"}}, code: CodeReplacementNotUnique},
		{name: "overlapping occurrences", text: "aaa", edits: []map[string]string{{"old_text": "aa", "new_text": "x"}}, code: CodeReplacementNotUnique},
		{name: "overlap", text: "abcdef", edits: []map[string]string{{"old_text": "abc", "new_text": "x"}, {"old_text": "bcd", "new_text": "y"}}, code: CodeReplacementOverlap},
		{name: "no changes", text: "abcdef", edits: []map[string]string{{"old_text": "abc", "new_text": "abc"}}, code: CodeNoChanges},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			caseRoot := t.TempDir()
			if err := os.WriteFile(filepath.Join(caseRoot, "case.txt"), []byte(test.text), 0o600); err != nil {
				t.Fatal(err)
			}
			caseWorkspace, err := Open(caseRoot)
			if err != nil {
				t.Fatal(err)
			}
			defer caseWorkspace.Close()
			caseRead := resultObject(t, caseWorkspace.Execute(t.Context(), ToolRead, `{"path":"case.txt"}`))
			caseArgs, _ := json.Marshal(map[string]any{"path": "case.txt", "expected_hash": caseRead["content_hash"], "edits": test.edits})
			candidate, failed := caseWorkspace.PrepareMutation(t.Context(), ToolEdit, string(caseArgs))
			if candidate != nil || resultCode(t, failed) != test.code || failed.Publication != PublicationUnchanged {
				t.Fatalf("candidate=%+v failure=%+v", candidate, failed)
			}
		})
	}
}

func TestWorkspaceMutationDoesNotFollowIntermediateLink(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "linked")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	value, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer value.Close()

	prepared, failure := value.PrepareMutation(t.Context(), ToolWrite, `{"path":"linked/escape.txt","mode":"create","content":"blocked"}`)
	if prepared != nil {
		t.Fatalf("link traversal prepared a mutation: %+v", prepared)
	}
	if code := resultCode(t, failure); code != CodeLinkNotAllowed && code != CodeNotDirectory {
		t.Fatalf("link traversal code=%q result=%+v", code, failure)
	}
	if _, err := os.Stat(filepath.Join(outside, "escape.txt")); !os.IsNotExist(err) {
		t.Fatalf("outside file changed during prepare: %v", err)
	}
}

func TestMutationQueuesAreFIFOAndCancellationSafe(t *testing.T) {
	var queues mutationQueues
	releaseFirst, err := queues.acquire(t.Context(), "file.txt")
	if err != nil {
		t.Fatal(err)
	}
	order := make(chan int, 2)
	releases := make(chan func(), 2)
	start := func(id int, path string, ctx context.Context) {
		go func() {
			release, err := queues.acquire(ctx, path)
			if err != nil {
				order <- -id
				return
			}
			order <- id
			releases <- release
		}()
	}
	start(1, "file.txt", t.Context())
	waitForQueueLength(t, &queues, "file.txt", 1)
	start(2, "file.txt", t.Context())
	waitForQueueLength(t, &queues, "file.txt", 2)
	releaseFirst()
	if got := <-order; got != 1 {
		t.Fatalf("first waiter=%d", got)
	}
	(<-releases)()
	if got := <-order; got != 2 {
		t.Fatalf("second waiter=%d", got)
	}
	(<-releases)()

	releaseHeld, err := queues.acquire(t.Context(), "cancel.txt")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	start(3, "cancel.txt", ctx)
	waitForQueueLength(t, &queues, "cancel.txt", 1)
	cancel()
	if got := <-order; got != -3 {
		t.Fatalf("cancelled waiter=%d", got)
	}
	releaseHeld()
	if release, err := queues.acquire(t.Context(), "cancel.txt"); err != nil {
		t.Fatal(err)
	} else {
		release()
	}
}

func waitForQueueLength(t *testing.T, queues *mutationQueues, path string, want int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		queues.mu.Lock()
		length := 0
		if target := queues.targets[path]; target != nil {
			length = len(target.waiters)
		}
		queues.mu.Unlock()
		if length == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("queue %q did not reach length %d", path, want)
}

func TestWorkspaceMutationPreviewIsBoundedAndFrozen(t *testing.T) {
	limits := DefaultLimits()
	limits.MutationPreviewBytes = 1024
	value, err := OpenWithLimits(t.TempDir(), limits)
	if err != nil {
		t.Fatal(err)
	}
	defer value.Close()
	content := strings.Repeat("界", 2000)
	arguments, _ := json.Marshal(map[string]any{"path": ".env", "mode": "create", "content": content})
	prepared, failure := value.PrepareMutation(t.Context(), ToolWrite, string(arguments))
	if prepared == nil || failure.Value != nil || !prepared.Presentation.Truncated || len(prepared.Presentation.Preview) > limits.MutationPreviewBytes {
		t.Fatalf("prepared=%+v failure=%+v previewBytes=%d", prepared, failure, len(prepared.Presentation.Preview))
	}
	prepared.Presentation.Preview += "tampered"
	if result := value.CommitMutation(t.Context(), prepared); resultCode(t, result) != CodeInternalError || result.Publication != PublicationUnchanged {
		t.Fatalf("tampered preview commit=%+v", result)
	}
}

func TestWorkspaceMutationAllowsHiddenGitAndCometPaths(t *testing.T) {
	root := t.TempDir()
	for _, directory := range []string{".git", ".comet"} {
		if err := os.Mkdir(filepath.Join(root, directory), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	value, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer value.Close()
	for _, path := range []string{".env", ".git/config", ".comet/state.yaml"} {
		arguments, _ := json.Marshal(map[string]any{"path": path, "mode": "create", "content": "allowed\n"})
		prepared, failure := value.PrepareMutation(t.Context(), ToolWrite, string(arguments))
		if prepared == nil || failure.Value != nil {
			t.Fatalf("path=%q prepared=%+v failure=%+v", path, prepared, failure)
		}
		if result := value.CommitMutation(t.Context(), prepared); result.Publication != PublicationCompleted || resultCode(t, result) != "" {
			t.Fatalf("path=%q result=%+v", path, result)
		}
	}
}

func TestMutationDeniedUsesOriginalStableOutcome(t *testing.T) {
	prepared := &PreparedMutation{Presentation: MutationPresentation{Tool: ToolWrite, Operation: "write_create", Path: "note.txt"}}
	result := MutationDenied(prepared)
	value := resultObject(t, result)
	if result.Publication != PublicationUnchanged || value["code"] != CodeAuthorizationDenied || value["path"] != "note.txt" || value["publication_outcome"] != string(PublicationUnchanged) {
		t.Fatalf("denied=%+v", result)
	}
	if !reflect.DeepEqual(prepared.Presentation, MutationPresentation{Tool: ToolWrite, Operation: "write_create", Path: "note.txt"}) {
		t.Fatalf("denial mutated prepared presentation: %+v", prepared.Presentation)
	}
}
