package workspace

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/agentlimits"
)

func TestLargeFileEditDistantRangesAndFrozenPublishBudget(t *testing.T) {
	const boundary = 1 << 20
	before := "first\n" + strings.Repeat("p", boundary-len("first\n")-4) + "cross-boundary\n" + strings.Repeat("q", boundary) + "\nlast\n"
	want := "FIRST\n" + strings.Repeat("p", boundary-len("first\n")-4) + "CROSS\n" + strings.Repeat("q", boundary) + "\nLAST\n"
	w, root := largeEditWorkspace(t, DefaultLimits(), "large.txt", before)
	edits := []editReplacement{{"last", "LAST"}, {"cross-boundary", "CROSS"}, {"first", "FIRST"}}
	prepared := largeEditPrepare(t, w, "large.txt", before, edits)
	largeEditAssertDisk(t, root, "large.txt", before)
	if prepared.fileBytes != DefaultEditFileBytes || prepared.baseVersion != largeEditHash(before) || prepared.replacements != 3 || prepared.firstChangeLine != 1 {
		t.Fatal("prepare did not freeze the edit budget, original hash, or replacement metadata")
	}
	if !prepared.Presentation.Truncated || !strings.Contains(prepared.Presentation.Preview, "preview truncated") || len(prepared.Presentation.Preview) > w.limits.MutationPreviewBytes {
		t.Fatal("distant changes did not produce a bounded, explicitly truncated preview")
	}
	// A prepared operation must not consult a later workspace budget, including
	// Publish's final securefile version recheck of this >1MiB original.
	w.limits.EditFileBytes, w.limits.FileBytes = 1, 1
	result := w.CommitMutation(t.Context(), prepared)
	largeEditAssertSuccess(t, result, want)
	largeEditAssertDisk(t, root, "large.txt", want)
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 1 {
		t.Fatalf("publication left temporary entries: count=%d err=%v", len(entries), err)
	}
	if repeated := w.CommitMutation(t.Context(), prepared); resultCode(t, repeated) != CodeInvalidArguments || repeated.Publication != PublicationUnchanged {
		t.Fatal("a processed large edit was accepted a second time")
	}
}

func TestLargeFileEditAll32RangesUseOriginalOffsets(t *testing.T) {
	var original, expected strings.Builder
	var edits []editReplacement
	for index := 0; index < 32; index++ {
		oldText, newText := fmt.Sprintf("<old-%02d>", index), fmt.Sprintf("<replacement-%02d>\n", index)
		padding := strings.Repeat("x", 40<<10) + "\n"
		original.WriteString(padding + oldText)
		expected.WriteString(padding + newText)
		edits = append(edits, editReplacement{oldText, newText})
	}
	for left, right := 0, len(edits)-1; left < right; left, right = left+1, right-1 {
		edits[left], edits[right] = edits[right], edits[left]
	}
	before, want := original.String(), expected.String()
	w, root := largeEditWorkspace(t, DefaultLimits(), "ranges.txt", before)
	prepared := largeEditPrepare(t, w, "ranges.txt", before, edits)
	largeEditAssertSuccess(t, w.CommitMutation(t.Context(), prepared), want)
	largeEditAssertDisk(t, root, "ranges.txt", want)
}

func TestLargeFileEditRejectsInvalidChangesWithoutPublication(t *testing.T) {
	base := strings.Repeat("padding\n", 160000) + "abcdef\nsame same\n"
	for _, test := range []struct {
		name, suffix, hash, code string
		edits                    []editReplacement
	}{
		{name: "missing", edits: []editReplacement{{"missing", "new"}}, code: CodeReplacementMissing},
		{name: "nonunique", edits: []editReplacement{{"same", "new"}}, code: CodeReplacementNotUnique},
		{name: "overlapping occurrences", suffix: "aaa", edits: []editReplacement{{"aa", "new"}}, code: CodeReplacementNotUnique},
		{name: "overlapping ranges", edits: []editReplacement{{"abc", "new"}, {"bcd", "NEW"}}, code: CodeReplacementOverlap},
		{name: "unchanged", edits: []editReplacement{{"abc", "abc"}}, code: CodeNoChanges},
		{name: "empty old text", edits: []editReplacement{{"", "new"}}, code: CodeInvalidArguments},
		{name: "invalid fragment", edits: []editReplacement{{"abc", "new\x00"}}, code: CodeInvalidArguments},
		{name: "invalid UTF8 tail", suffix: "\xff", edits: []editReplacement{{"abc", "new"}}, code: CodeInvalidUTF8},
		{name: "binary tail", suffix: "\x00", edits: []editReplacement{{"abc", "new"}}, code: CodeBinaryFile},
		{name: "binary controls", suffix: "\x01\x02\x03\x04\x05", edits: []editReplacement{{"abc", "new"}}, code: CodeBinaryFile},
		{name: "old hash", hash: largeEditHash("stale"), edits: []editReplacement{{"abc", "new"}}, code: CodeContentChanged},
		{name: "too many replacements", edits: make([]editReplacement, 33), code: CodeInvalidArguments},
	} {
		t.Run(test.name, func(t *testing.T) {
			before := base + test.suffix
			w, root := largeEditWorkspace(t, DefaultLimits(), "large.txt", before)
			hash := test.hash
			if hash == "" {
				hash = largeEditHash(before)
			}
			prepared, result := w.PrepareMutation(t.Context(), ToolEdit, largeEditArguments(t, "large.txt", hash, test.edits))
			if prepared != nil || resultCode(t, result) != test.code || result.Publication != PublicationUnchanged {
				t.Fatalf("prepare code=%q want=%q publication=%q", resultCode(t, result), test.code, result.Publication)
			}
			if test.hash != "" && (result.Reference == nil || !result.Reference.InvalidateObserved || result.Reference.ContentHash != hash) {
				t.Fatal("stale full-file hash was not invalidated")
			}
			largeEditAssertDisk(t, root, "large.txt", before)
		})
	}
}

func TestLargeFileEditBOMNewlinesAndPermissions(t *testing.T) {
	for _, newline := range []string{"\n", "\r\n"} {
		for _, bom := range []string{"", "\ufeff"} {
			for _, trailing := range []bool{false, true} {
				t.Run(fmt.Sprintf("newline=%q/bom=%t/trailing=%t", newline, bom != "", trailing), func(t *testing.T) {
					padding := strings.Repeat("padding"+newline, 160000)
					ending := ""
					if trailing {
						ending = newline
					}
					before := bom + "alpha" + newline + padding + "omega" + ending
					want := bom + "ALPHA" + newline + "extra" + newline + padding + "OMEGA" + ending
					w, root := largeEditWorkspace(t, DefaultLimits(), "encoded.txt", before)
					prepared := largeEditPrepare(t, w, "encoded.txt", before, []editReplacement{{"alpha" + newline, "ALPHA\nextra\r\n"}, {"omega", "OMEGA"}})
					path := filepath.Join(root, "encoded.txt")
					if runtime.GOOS != "windows" {
						if err := os.Chmod(path, 0o640); err != nil {
							t.Fatal(err)
						}
					}
					largeEditAssertSuccess(t, w.CommitMutation(t.Context(), prepared), want)
					largeEditAssertDisk(t, root, "encoded.txt", want)
					if runtime.GOOS != "windows" {
						info, err := os.Stat(path)
						if err != nil {
							t.Fatal(err)
						}
						if info.Mode().Perm() != 0o640 {
							t.Fatalf("commit-time permissions changed to %o", info.Mode().Perm())
						}
					}
				})
			}
		}
	}
}

func TestLargeFileEditCandidateBudgetUsesFinalRawBytes(t *testing.T) {
	large := "target\n" + strings.Repeat("x", 1<<20)
	for _, test := range []struct {
		name, before, want, code string
		limit                    int64
		edits                    []editReplacement
	}{
		{name: "large original exceeds limit", before: large, limit: 1 << 20, edits: []editReplacement{{"target", "new"}}, code: CodeFileTooLarge},
		{name: "large candidate exceeds limit", before: large, limit: int64(len(large)), edits: []editReplacement{{"target", "larger-target"}}, code: CodeFileTooLarge},
		{name: "original and candidate at exact limit", before: large, want: "TARGET\n" + strings.Repeat("x", 1<<20), limit: int64(len(large)), edits: []editReplacement{{"target", "TARGET"}}},
		{name: "BOM counts against budget", before: "\ufeffAB", limit: 6, edits: []editReplacement{{"A", "CCC"}}, code: CodeFileTooLarge},
		{name: "normalized CRLF counts against budget", before: "A\r\nB\r\n", limit: 7, edits: []editReplacement{{"A", "A\n"}}, code: CodeFileTooLarge},
		{name: "normalized multibyte fits exactly", before: "\ufeffA\r\nB\r\n", want: "\ufeff界\r\nB\r\n", limit: 11, edits: []editReplacement{{"A\r\n", "界\n"}}},
		{name: "early growth offset by later shrink", before: "A\nBBBB\n", want: "LONG\nB\n", limit: 7, edits: []editReplacement{{"A", "LONG"}, {"BBBB", "B"}}},
		{name: "later growth offset by early shrink", before: "BBBB\nA\n", want: "B\nLONG\n", limit: 7, edits: []editReplacement{{"A", "LONG"}, {"BBBB", "B"}}},
		{name: "normalized no change", before: "A\r\nB\r\n", limit: 6, edits: []editReplacement{{"A\r\n", "A\n"}}, code: CodeNoChanges},
		{name: "delete text leaving BOM", before: "\ufeffA", want: "\ufeff", limit: 4, edits: []editReplacement{{"A", ""}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			limits := DefaultLimits()
			limits.EditFileBytes = test.limit
			w, root := largeEditWorkspace(t, limits, "budget.txt", test.before)
			prepared, result := w.PrepareMutation(t.Context(), ToolEdit, largeEditArguments(t, "budget.txt", largeEditHash(test.before), test.edits))
			largeEditAssertDisk(t, root, "budget.txt", test.before)
			if test.code != "" {
				if prepared != nil || resultCode(t, result) != test.code || result.Publication != PublicationUnchanged {
					t.Fatalf("budget failure code=%q want=%q", resultCode(t, result), test.code)
				}
				if test.code == CodeFileTooLarge {
					value := resultObject(t, result)
					if value["edit_byte_limit"] != test.limit || !strings.Contains(value["message"].(string), fmt.Sprint(test.limit)) ||
						!strings.Contains(value["suggestion"].(string), "--file-edit-limit") || !strings.Contains(value["suggestion"].(string), "Shell") {
						t.Fatal("oversize edit omitted its explicit limit or recovery hint")
					}
				}
				return
			}
			if prepared == nil || result.Value != nil || string(prepared.candidate) != test.want {
				t.Fatal("legal final candidate was refused or has incorrect bytes")
			}
			largeEditAssertSuccess(t, w.CommitMutation(t.Context(), prepared), test.want)
			largeEditAssertDisk(t, root, "budget.txt", test.want)
		})
	}
}

func TestLargeFileEditIndependentLimitsAndSchema(t *testing.T) {
	defaults := DefaultLimits()
	if defaults.EditFileBytes != 64<<20 || defaults.ReadFileBytes != 64<<20 || defaults.FileBytes != 1<<20 {
		t.Fatal("independent default processing budgets changed")
	}
	for _, test := range []struct {
		name       string
		edit, want int64
	}{{"explicit", 64, 64}, {"legacy zero", 0, 32}, {"minimum", 1, 1}, {"maximum", int64(^uint(0)>>1) - 1, int64(^uint(0)>>1) - 1}} {
		t.Run(test.name, func(t *testing.T) {
			limits := defaults
			limits.FileBytes, limits.EditFileBytes = 32, test.edit
			w, err := OpenWithLimits(t.TempDir(), limits)
			if err != nil {
				t.Fatal(err)
			}
			defer w.Close()
			if w.limits.EditFileBytes != test.want || w.limits.ReadFileBytes != defaults.ReadFileBytes || w.limits.FileBytes != 32 {
				t.Fatal("independent or legacy budget was not preserved")
			}
			base, instance := Definitions(), w.Definitions()
			for index, definition := range instance {
				if definition.Function.Name == ToolEdit {
					if !strings.Contains(definition.Function.Description, fmt.Sprint(test.want)) || !bytes.Equal(definition.Function.Parameters, base[index].Function.Parameters) {
						t.Fatal("edit description omitted the effective budget or changed the arguments schema")
					}
				} else if !reflect.DeepEqual(definition, base[index]) {
					t.Fatalf("edit budget changed %s definition", definition.Function.Name)
				}
			}
			if !reflect.DeepEqual(base, Definitions()) {
				t.Fatal("instance definitions mutated package defaults")
			}
		})
	}
	for _, limit := range []int64{-1, int64(^uint(0) >> 1)} {
		limits := defaults
		limits.EditFileBytes = limit
		if w, err := OpenWithLimits(t.TempDir(), limits); err == nil {
			w.Close()
			t.Fatalf("unsafe edit limit accepted: %d", limit)
		}
	}

	limits := defaults
	limits.ReadFileBytes = 16
	before := "target\n" + strings.Repeat("p", 1<<20)
	w, root := largeEditWorkspace(t, limits, "large.txt", before)
	if code := resultCode(t, w.Execute(t.Context(), ToolRead, `{"path":"large.txt"}`)); code != CodeFileTooLarge {
		t.Fatal("edit widened the explicitly small read budget")
	}
	if code := resultCode(t, w.Execute(t.Context(), ToolStat, `{"path":"large.txt","hash":true}`)); code != CodeFileTooLarge {
		t.Fatal("edit widened stat.hash")
	}
	search := resultObject(t, w.Execute(t.Context(), ToolSearch, `{"path":"large.txt","query":"target"}`))
	if search["complete"] != false || search["truncation_reason"] != "file_too_large" {
		t.Fatal("edit widened search")
	}
	write, err := json.Marshal(writeArguments{Path: "large.txt", Mode: "replace", Content: "small", ExpectedHash: largeEditHash(before)})
	if err != nil {
		t.Fatal(err)
	}
	if prepared, result := w.PrepareMutation(t.Context(), ToolWrite, string(write)); prepared != nil || resultCode(t, result) != CodeFileTooLarge {
		t.Fatal("edit widened write.replace")
	}
	prepared := largeEditPrepare(t, w, "large.txt", before, []editReplacement{{"target", "TARGET"}})
	want := "TARGET\n" + strings.Repeat("p", 1<<20)
	largeEditAssertSuccess(t, w.CommitMutation(t.Context(), prepared), want)
	largeEditAssertDisk(t, root, "large.txt", want)

	// The shared commit path must also freeze FileBytes for ordinary writes.
	small, smallRoot := largeEditWorkspace(t, defaults, "small.txt", "old\n")
	write, err = json.Marshal(writeArguments{Path: "small.txt", Mode: "replace", Content: "new\n", ExpectedHash: largeEditHash("old\n")})
	if err != nil {
		t.Fatal(err)
	}
	written, failure := small.PrepareMutation(t.Context(), ToolWrite, string(write))
	if written == nil || failure.Value != nil || written.fileBytes != defaults.FileBytes {
		t.Fatal("ordinary write did not freeze its own processing budget")
	}
	small.limits.FileBytes = 1
	largeEditAssertSuccess(t, small.CommitMutation(t.Context(), written), "new\n")
	largeEditAssertDisk(t, smallRoot, "small.txt", "new\n")
}

func TestLargeFileEditCommitConflictCancellationAndIntegrity(t *testing.T) {
	before := "target\n" + strings.Repeat("x", 1<<20)
	for _, scenario := range []string{"external change", "cancel prepare", "cancel commit", "deny", "candidate tamper", "preview tamper"} {
		t.Run(scenario, func(t *testing.T) {
			w, root := largeEditWorkspace(t, DefaultLimits(), "large.txt", before)
			args := largeEditArguments(t, "large.txt", largeEditHash(before), []editReplacement{{"target", "TARGET"}})
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if scenario == "cancel prepare" {
				cancel()
				prepared, result := w.PrepareMutation(ctx, ToolEdit, args)
				if prepared != nil || resultCode(t, result) != CodeCancelled || result.Publication != PublicationUnchanged {
					t.Fatal("cancelled prepare produced a candidate")
				}
				largeEditAssertDisk(t, root, "large.txt", before)
				return
			}
			prepared := largeEditPrepare(t, w, "large.txt", before, []editReplacement{{"target", "TARGET"}})
			want, code := before, CodeInternalError
			var result Result
			switch scenario {
			case "external change":
				want, code = before[:len(before)-1]+"z", CodeContentChanged
				if err := os.WriteFile(filepath.Join(root, "large.txt"), []byte(want), 0o600); err != nil {
					t.Fatal(err)
				}
			case "cancel commit":
				cancel()
				code = CodeCancelled
			case "deny":
				code = CodeAuthorizationDenied
			case "candidate tamper":
				prepared.candidate[len(prepared.candidate)-1] = 'z'
			case "preview tamper":
				prepared.Presentation.Preview += "tampered"
			}
			if scenario == "deny" {
				result = MutationDenied(prepared)
			} else {
				result = w.CommitMutation(ctx, prepared)
			}
			if resultCode(t, result) != code || result.Publication != PublicationUnchanged {
				t.Fatalf("commit code=%q want=%q publication=%q", resultCode(t, result), code, result.Publication)
			}
			largeEditAssertDisk(t, root, "large.txt", want)
		})
	}
}

func TestLargeFileEditRejectsLinksArchiveAndUnsafePaths(t *testing.T) {
	before := "target\n" + strings.Repeat("x", 1<<20)
	w, root := largeEditWorkspace(t, DefaultLimits(), "large.txt", before)
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte(before), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "directory"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, ArchiveDirectory), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ArchiveDirectory, "saved.txt"), []byte(before), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, link := range [][2]string{{filepath.Join(outside, "secret.txt"), "linked.txt"}, {outside, "linked-parent"}} {
		if err := os.Symlink(link[0], filepath.Join(root, link[1])); err != nil {
			t.Fatal(err)
		}
	}
	for path, code := range map[string]string{
		"linked.txt":                    CodeLinkNotAllowed,
		"linked-parent/secret.txt":      CodeLinkNotAllowed,
		ArchiveDirectory + "/saved.txt": CodeArchiveProtected,
		"directory":                     CodeUnsupportedType,
		".":                             CodeInvalidPath,
		"../secret.txt":                 CodePathOutsideWorkspace,
	} {
		prepared, result := w.PrepareMutation(t.Context(), ToolEdit, largeEditArguments(t, path, largeEditHash(before), []editReplacement{{"target", "TARGET"}}))
		actual := resultCode(t, result)
		// Linux may report ENOTDIR for O_DIRECTORY|O_NOFOLLOW on a parent link.
		validCode := actual == code || path == "linked-parent/secret.txt" && actual == CodeNotDirectory
		if prepared != nil || !validCode || result.Publication != PublicationUnchanged {
			t.Fatalf("unsafe path %q code=%q want=%q", path, actual, code)
		}
	}
	prepared := largeEditPrepare(t, w, "large.txt", before, []editReplacement{{"target", "TARGET"}})
	if err := os.Rename(filepath.Join(root, "large.txt"), filepath.Join(root, "original.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(root, "large.txt")); err != nil {
		t.Fatal(err)
	}
	result := w.CommitMutation(t.Context(), prepared)
	if resultCode(t, result) != CodeLinkNotAllowed || result.Publication != PublicationUnchanged {
		t.Fatal("commit followed a target swapped to a symlink after prepare")
	}
	largeEditAssertDisk(t, root, "original.txt", before)
	largeEditAssertDisk(t, root, ArchiveDirectory+"/saved.txt", before)
	largeEditAssertDisk(t, outside, "secret.txt", before)
}

func TestLargeFileEditPreviewCompatibilityAndBoundedPrefix(t *testing.T) {
	for _, test := range []struct{ name, old, new, full string }{
		{"small", "one\ntwo\nthree\n", "one\nTWO\nthree\n", "--- file.txt\n+++ file.txt\n@@ -1,3 +1,3 @@\n one\n-two\n+TWO\n three\n"},
		{"no final LF", "one\ntwo", "one\nTWO", "--- file.txt\n+++ file.txt\n@@ -1,2 +1,2 @@\n one\n-two+TWO"},
		{"empty candidate", "old\n", "", "--- file.txt\n+++ file.txt\n@@ -1,1 +1,0 @@\n-old\n"},
		{"new candidate", "", "new\n", "--- file.txt\n+++ file.txt\n@@ -1,0 +1,1 @@\n+new\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			for _, limit := range []int{len(test.full) - 1, len(test.full), len(test.full) + 1, int(^uint(0) >> 1)} {
				preview, truncated, _ := buildMutationPreview("file.txt", test.old, test.new, "diff", limit)
				want, wantTruncated := boundedPreview(test.full, limit)
				if preview != want || truncated != wantTruncated {
					t.Fatalf("preview changed at limit=%d: %q", limit, preview)
				}
			}
		})
	}
	old := strings.Repeat("界", 400000) + "\n"
	newText := "NEW\n"
	preview, truncated, firstLine := buildMutationPreview("file.txt", old, newText, "diff", 1024)
	want, _ := boundedPreview("--- file.txt\n+++ file.txt\n@@ -1,1 +1,1 @@\n-"+old[:2048], 1024)
	if !truncated || firstLine != 1 || len(preview) > 1024 || preview != want || !utf8.ValidString(preview) {
		t.Fatal("large UTF8 preview did not keep the original bounded diff prefix")
	}
}

func largeEditWorkspace(t *testing.T, limits Limits, path, before string) (*Workspace, string) {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, path), []byte(before), 0o600); err != nil {
		t.Fatal(err)
	}
	w, err := OpenWithLimits(root, limits)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { w.Close() })
	return w, root
}

func largeEditHash(raw string) string {
	return fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(raw)))
}

func largeEditArguments(t *testing.T, path, hash string, edits []editReplacement) string {
	t.Helper()
	raw, err := json.Marshal(editArguments{Path: path, ExpectedHash: hash, Edits: edits})
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) > agentlimits.MaxFileMutationArgumentsBytes {
		t.Fatal("test accidentally widened the model arguments budget")
	}
	return string(raw)
}

func largeEditPrepare(t *testing.T, w *Workspace, path, before string, edits []editReplacement) *PreparedMutation {
	t.Helper()
	prepared, failure := w.PrepareMutation(t.Context(), ToolEdit, largeEditArguments(t, path, largeEditHash(before), edits))
	if prepared == nil || failure.Value != nil {
		t.Fatalf("prepare failed: %+v", failure)
	}
	return prepared
}

func largeEditAssertDisk(t *testing.T, root, path, want string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(path)))
	if err != nil || string(data) != want {
		t.Fatalf("disk bytes differ for %q: got=%d want=%d err=%v", path, len(data), len(want), err)
	}
}

func largeEditAssertSuccess(t *testing.T, result Result, want string) {
	t.Helper()
	value := resultObject(t, result)
	hash := largeEditHash(want)
	if result.Publication != PublicationCompleted || resultCode(t, result) != "" || value["complete"] != true ||
		value["content_hash"] != hash || intValue(value["bytes"]) != len(want) || result.Reference == nil || result.Reference.ContentHash != hash ||
		result.Effect == nil || result.Effect.Target.Version != hash {
		t.Fatalf("publication, full raw hash, byte count, or file effect is incorrect: code=%q", resultCode(t, result))
	}
}
