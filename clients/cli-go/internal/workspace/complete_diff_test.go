package workspace

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestCompleteDiffGolden(t *testing.T) {
	for _, test := range []struct {
		name, old, next, want string
		create                bool
	}{
		{"empty create", "", "", "--- /dev/null\n+++ b/file.txt\n", true},
		{"create", "", "new\n", "--- /dev/null\n+++ b/file.txt\n@@ -0,0 +1,1 @@\n+new\n", true},
		{"create without LF", "", "new", "--- /dev/null\n+++ b/file.txt\n@@ -0,0 +1,1 @@\n+new\n\\ No newline at end of file\n", true},
		{"replace without LF", "one\ntwo", "one\nTWO", "--- a/file.txt\n+++ b/file.txt\n@@ -1,2 +1,2 @@\n one\n-two\n\\ No newline at end of file\n+TWO\n\\ No newline at end of file\n", false},
		{"remove final LF", "line\n", "line", "--- a/file.txt\n+++ b/file.txt\n@@ -1,1 +1,1 @@\n-line\n+line\n\\ No newline at end of file\n", false},
		{"add final LF", "line", "line\n", "--- a/file.txt\n+++ b/file.txt\n@@ -1,1 +1,1 @@\n-line\n\\ No newline at end of file\n+line\n", false},
		{"remove all", "line\n", "", "--- a/file.txt\n+++ b/file.txt\n@@ -1,1 +0,0 @@\n-line\n", false},
		{"BOM and CRLF", "\ufeffold\r\n", "\ufeffnew\r\n", "--- a/file.txt\n+++ b/file.txt\n@@ -1,1 +1,1 @@\n-\ufeffold\r\n+\ufeffnew\r\n", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := buildCompleteDiff(t.Context(), "file.txt", []byte(test.old), []byte(test.next), test.create, nil, DefaultDiffBytes)
			if err != nil || got != test.want {
				t.Fatalf("diff=%q want=%q err=%v", got, test.want, err)
			}
			if applied := applyCompleteDiffForTest(t, test.old, got); applied != test.next {
				t.Fatalf("applied=%q want=%q", applied, test.next)
			}
		})
	}
}

func TestCompleteDiffPrepareWrite(t *testing.T) {
	for _, test := range []struct {
		name, before, content, want string
		create                      bool
	}{
		{"empty create", "", "", "", true},
		{"raw create", "", "\ufeffα\r\nβ\n末", "\ufeffα\r\nβ\n末", true},
		{"replace empty file", "", "new", "new", false},
		{"replace empty candidate", "old\n", "", "", false},
		{"replace normalization", "\ufeffold\r\nkeep\r\n", "NEW\nkeep\n末", "\ufeffNEW\r\nkeep\r\n末", false},
		{"replace mixed endings", "old\r\nkeep\nlast\n", "NEW\r\nkeep\nlast\n", "NEW\nkeep\nlast\n", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			if !test.create {
				if err := os.WriteFile(filepath.Join(root, "file.txt"), []byte(test.before), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			w, err := Open(root)
			if err != nil {
				t.Fatal(err)
			}
			defer w.Close()
			args := writeArguments{Path: "file.txt", Mode: "create", Content: test.content}
			if !test.create {
				args.Mode, args.ExpectedHash = "replace", contentHash([]byte(test.before))
			}
			prepared, failure := w.PrepareMutation(t.Context(), ToolWrite, completeDiffJSON(t, args))
			if prepared == nil || failure.Value != nil {
				t.Fatalf("prepare failed: %+v", failure)
			}
			completeDiffCheckPrepared(t, prepared, test.before, test.want)
			completeDiffDiskUnchanged(t, root, test.before, test.create)
			if result := w.CommitMutation(t.Context(), prepared); result.Publication != PublicationCompleted {
				t.Fatalf("commit failed: %+v", result)
			}
			data, err := os.ReadFile(filepath.Join(root, "file.txt"))
			if err != nil || string(data) != test.want {
				t.Fatalf("committed=%q want=%q err=%v", data, test.want, err)
			}
		})
	}
}

func TestCompleteDiffPrepareEditRanges(t *testing.T) {
	var distant strings.Builder
	for i := 0; i < 35; i++ {
		fmt.Fprintf(&distant, "line%02d α\n", i)
	}
	before := distant.String()
	want := strings.ReplaceAll(before, "line02", "two\ninsert")
	want = strings.ReplaceAll(want, "line18 α\n", "")
	want = strings.ReplaceAll(want, "line34 α\n", "末")
	for _, test := range []struct {
		name, before, want string
		edits              []editReplacement
		hunks              int
	}{
		{"distant growth shrink and EOF", before, want, []editReplacement{{"line34 α\n", "末"}, {"line18 α\n", ""}, {"line02", "two\ninsert"}}, 3},
		{"touching context windows", before, strings.NewReplacer("line02", "TWO", "line09", "NINE").Replace(before), []editReplacement{{"line02", "TWO"}, {"line09", "NINE"}}, 1},
		{"separated context windows", before, strings.NewReplacer("line02", "TWO", "line10", "TEN").Replace(before), []editReplacement{{"line02", "TWO"}, {"line10", "TEN"}}, 2},
		{"same line", "a b c\n", "AA B\ninsert \n", []editReplacement{{"c", ""}, {"a", "AA"}, {"b", "B\ninsert"}}, 1},
		{"adjacent", "one\ntwo\nthree\n", "TWO\nextra\nthree\n", []editReplacement{{"two", "TWO\nextra"}, {"one\n", ""}}, 1},
		{"join lines", "head\nbody\nend", "headxend", []editReplacement{{"\nbody\n", "x"}}, 1},
		{"leave BOM", "\ufeffα", "\ufeff", []editReplacement{{"α", ""}}, 1},
		{"empty candidate", "α\n", "", []editReplacement{{"α\n", ""}}, 1},
		{"BOM mixed endings", "\ufeffα\r\nbravo\ncharlie\r\ndelta\n終", "\ufeffα\r\nB\nextra\ndelta\n末\n", []editReplacement{{"終", "末\n"}, {"bravo", "B\r\nextra"}, {"charlie\r\n", ""}}, 1},
		{"trailing CR byte", "first\nlast\r", "FIRST\nlast\r", []editReplacement{{"first", "FIRST"}}, 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			w, root := largeEditWorkspace(t, DefaultLimits(), "file.txt", test.before)
			prepared := largeEditPrepare(t, w, "file.txt", test.before, test.edits)
			completeDiffCheckPrepared(t, prepared, test.before, test.want)
			if got := strings.Count(prepared.FullDiff(), "\n@@ "); got != test.hunks {
				t.Fatalf("hunks=%d want=%d: %s", got, test.hunks, prepared.FullDiff())
			}
			completeDiffDiskUnchanged(t, root, test.before, false)
			if result := w.CommitMutation(t.Context(), prepared); result.Publication != PublicationCompleted {
				t.Fatalf("commit failed: %+v", result)
			}
			largeEditAssertDisk(t, root, "file.txt", test.want)
		})
	}
}

func TestCompleteDiffRangeInsertionAndByteBoundaries(t *testing.T) {
	// Exhaust byte boundaries independently of the editor's non-empty old_text
	// contract, so future internal patch callers can also supply insertions.
	for _, before := range []string{"", "abc", "a\nb\nc\n", "a\r\nb\nc", "\ufeffα\nβ"} {
		for start := 0; start <= len(before); start++ {
			for end := start; end <= len(before); end++ {
				if !utf8.ValidString(before[:start]) || !utf8.ValidString(before[end:]) {
					continue
				}
				for _, text := range []string{"", "X", "X\nY", "\n", "\r\n"} {
					want := before[:start] + text + before[end:]
					ranges := []replacementRange{{start: start, end: end, text: text}}
					for _, r := range [][]replacementRange{ranges, nil} {
						diff, err := buildCompleteDiff(t.Context(), "file.txt", []byte(before), []byte(want), false, r, DefaultDiffBytes)
						if err != nil {
							t.Fatalf("before=%q range=%+v: %v", before, ranges, err)
						}
						if got := applyCompleteDiffForTest(t, before, diff); got != want {
							t.Fatalf("before=%q range=%+v applied=%q want=%q", before, ranges, got, want)
						}
					}
				}
			}
		}
	}
	before := "start\n" + strings.Repeat("middle\n", 25) + "end"
	want := "intro\nstart\n" + strings.Repeat("middle\n", 25) + "end\nlast"
	ranges := []replacementRange{{0, 0, "intro\n"}, {len(before), len(before), "\nlast"}}
	diff, err := buildCompleteDiff(t.Context(), "file.txt", []byte(before), []byte(want), false, ranges, DefaultDiffBytes)
	if err != nil || strings.Count(diff, "\n@@ ") != 2 || applyCompleteDiffForTest(t, before, diff) != want ||
		!strings.Contains(diff, "@@ -24,4 +25,5 @@\n") {
		t.Fatalf("distant edge insertions: diff=%q err=%v", diff, err)
	}
}

func TestCompleteDiffBudgetAndFrozenPreparation(t *testing.T) {
	before := strings.Repeat("old", 4000) + "\nend"
	for _, mode := range []string{"create", "replace", "edit"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			create := mode == "create"
			original := before
			if create {
				original = ""
			} else if err := os.WriteFile(filepath.Join(root, "file.txt"), []byte(original), 0o600); err != nil {
				t.Fatal(err)
			}
			w, err := Open(root)
			if err != nil {
				t.Fatal(err)
			}
			defer w.Close()
			tool, args := ToolWrite, completeDiffJSON(t, writeArguments{Path: "file.txt", Mode: mode, Content: before + "NEW"})
			if mode == "replace" {
				args = completeDiffJSON(t, writeArguments{Path: "file.txt", Mode: mode, Content: "NEW", ExpectedHash: contentHash([]byte(original))})
			} else if mode == "edit" {
				tool = ToolEdit
				args = completeDiffJSON(t, editArguments{Path: "file.txt", ExpectedHash: contentHash([]byte(original)), Edits: []editReplacement{{original, "NEW"}}})
			}
			prepared, failed := w.PrepareMutation(t.Context(), tool, args)
			if prepared == nil || failed.Value != nil {
				t.Fatalf("initial prepare: %+v", failed)
			}
			full := prepared.FullDiff()
			w.limits.DiffBytes = int64(len(full))
			exact, failed := w.PrepareMutation(t.Context(), tool, args)
			if exact == nil || failed.Value != nil || exact.FullDiff() != full {
				t.Fatalf("exact byte boundary refused: %+v", failed)
			}
			for _, limit := range []int64{int64(len(full) - 1), 1} {
				w.limits.DiffBytes = limit
				rejected, failure := w.PrepareMutation(t.Context(), tool, args)
				if rejected != nil || resultCode(t, failure) != CodeDiffTooLarge || failure.Publication != PublicationUnchanged {
					t.Fatalf("oversized diff accepted: %+v", failure)
				}
				value := resultObject(t, failure)
				if value["diff_byte_limit"] != limit || !strings.Contains(value["message"].(string), fmt.Sprint(limit)) ||
					!strings.Contains(value["suggestion"].(string), "分次") || !strings.Contains(value["suggestion"].(string), "Shell") ||
					strings.Contains(completeDiffJSON(t, value), "oldold") {
					t.Fatalf("unsafe/incomplete diff failure: %+v", value)
				}
				completeDiffDiskUnchanged(t, root, original, create)
			}
			if exact.FullDiff() != full || w.CommitMutation(t.Context(), exact).Publication != PublicationCompleted {
				t.Fatal("a later diff budget changed the frozen complete candidate or commit")
			}
		})
	}
	// Account for UTF-8 paths, file markers, final-LF markers and both headers.
	path := strings.Repeat("界", 1365) + "x"
	full, err := buildCompleteDiff(t.Context(), path, nil, []byte("尾"), true, nil, DefaultDiffBytes)
	if err != nil {
		t.Fatal(err)
	}
	if exact, err := buildCompleteDiff(t.Context(), path, nil, []byte("尾"), true, nil, int64(len(full))); err != nil || exact != full {
		t.Fatal("full header/marker byte boundary refused")
	}
	if partial, err := buildCompleteDiff(t.Context(), path, nil, []byte("尾"), true, nil, int64(len(full)-1)); partial != "" || !errors.Is(err, errCompleteDiffTooLarge) {
		t.Fatal("returned partial complete diff at one byte over budget")
	}
}

func TestCompleteDiffLimitsAndDataContract(t *testing.T) {
	defaults := DefaultLimits()
	if DefaultDiffBytes != 128<<20 || defaults.DiffBytes != DefaultDiffBytes {
		t.Fatal("complete diff default budget changed")
	}
	maxLimit := int64(^uint(0)>>1) - 1
	for _, limit := range []int64{0, 1, 4096, maxLimit} {
		limits := defaults
		limits.DiffBytes = limit
		w, err := OpenWithLimits(t.TempDir(), limits)
		if err != nil {
			t.Fatal(err)
		}
		want := limit
		if limit == 0 {
			want = DefaultDiffBytes
		}
		if w.limits.DiffBytes != want || !reflect.DeepEqual(w.Definitions(), Definitions()) {
			t.Fatal("diff limit fallback or unchanged tool schema violated")
		}
		w.Close()
	}
	for _, limit := range []int64{-1, maxLimit + 1} {
		limits := defaults
		limits.DiffBytes = limit
		if w, err := OpenWithLimits(t.TempDir(), limits); err == nil {
			w.Close()
			t.Fatalf("invalid diff limit accepted: %d", limit)
		}
	}
	w, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	content := strings.Repeat("body\n", 3000) + "PRIVATE_DIFF_TAIL"
	prepared, failure := w.PrepareMutation(t.Context(), ToolWrite, completeDiffJSON(t, writeArguments{Path: "file.txt", Mode: "create", Content: content}))
	if prepared == nil || failure.Value != nil {
		t.Fatalf("prepare failed: %+v", failure)
	}
	completeDiffCheckPrepared(t, prepared, "", content)
	full := prepared.FullDiff()
	copyBytes := []byte(full)
	copyBytes[0] = '!'
	prepared.Presentation.Path = "display-only.txt"
	if prepared.FullDiff() != full || !strings.Contains(full, "PRIVATE_DIFF_TAIL") || !prepared.Presentation.Truncated {
		t.Fatal("complete diff is not a frozen independent string")
	}
	for _, value := range []any{prepared, mutationSuccess(prepared).Value, MutationDenied(prepared).Value} {
		encoded := completeDiffJSON(t, value)
		if strings.Contains(encoded, "PRIVATE_DIFF_TAIL") || strings.Contains(encoded, "full_diff") {
			t.Fatal("full diff leaked into a serializable presentation/result")
		}
	}
	if (*PreparedMutation)(nil).FullDiff() != "" {
		t.Fatal("nil mutation has a full diff")
	}
	for _, tool := range []string{ToolMove, ToolCopy, ToolMkdir, ToolArchive} {
		if p := (&PreparedMutation{Presentation: MutationPresentation{Tool: tool}}); p.FullDiff() != "" {
			t.Fatalf("non-text %s has a diff", tool)
		}
	}
}

func TestCompleteDiffCancellation(t *testing.T) {
	for _, before := range []string{strings.Repeat("x", 128<<10), strings.Repeat("same\n", 2000)} {
		next := before + "NEW"
		probe := newCompleteDiffCancelContext(t.Context(), 0)
		if _, err := buildCompleteDiff(probe, "file.txt", []byte(before), []byte(next), false, nil, DefaultDiffBytes); err != nil {
			t.Fatal(err)
		}
		for _, stop := range []int{1, 8, probe.checks / 2, probe.checks - 1, probe.checks} {
			ctx := newCompleteDiffCancelContext(t.Context(), stop)
			diff, err := buildCompleteDiff(ctx, "file.txt", []byte(before), []byte(next), false, nil, DefaultDiffBytes)
			ctx.cancel()
			if !errors.Is(err, context.Canceled) || diff != "" {
				t.Fatalf("cancel at check %d/%d returned a diff: bytes=%d err=%v", stop, probe.checks, len(diff), err)
			}
		}
		probe.cancel()
	}
	w, root := largeEditWorkspace(t, DefaultLimits(), "file.txt", "old\n")
	for _, tool := range []string{ToolWrite, ToolEdit} {
		args := completeDiffJSON(t, writeArguments{Path: "file.txt", Mode: "replace", ExpectedHash: contentHash([]byte("old\n")), Content: "new"})
		if tool == ToolEdit {
			args = completeDiffJSON(t, editArguments{Path: "file.txt", ExpectedHash: contentHash([]byte("old\n")), Edits: []editReplacement{{"old", "new"}}})
		}
		probe := newCompleteDiffCancelContext(t.Context(), 0)
		if prepared, failed := w.PrepareMutation(probe, tool, args); prepared == nil || failed.Value != nil {
			t.Fatalf("uncancelled prepare failed: %+v", failed)
		}
		// Exercise the generator's final check and both new post-generation
		// prepare checks, without depending on unrelated archive I/O internals.
		for _, stop := range []int{1, probe.checks - 2, probe.checks - 1, probe.checks} {
			ctx := newCompleteDiffCancelContext(t.Context(), stop)
			prepared, failure := w.PrepareMutation(ctx, tool, args)
			ctx.cancel()
			if prepared != nil || resultCode(t, failure) != CodeCancelled || failure.Publication != PublicationUnchanged {
				t.Fatalf("cancelled prepare returned success at check %d: %+v", stop, failure)
			}
			completeDiffDiskUnchanged(t, root, "old\n", false)
		}
		probe.cancel()
	}
}

type completeDiffCancelContext struct {
	context.Context
	cancel       context.CancelFunc
	checks, stop int
}

func newCompleteDiffCancelContext(parent context.Context, stop int) *completeDiffCancelContext {
	ctx, cancel := context.WithCancel(parent)
	return &completeDiffCancelContext{Context: ctx, cancel: cancel, stop: stop}
}

func (c *completeDiffCancelContext) Err() error {
	c.checks++
	if c.checks == c.stop {
		c.cancel()
	}
	return c.Context.Err()
}

func completeDiffJSON(t *testing.T, value any) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func completeDiffCheckPrepared(t *testing.T, prepared *PreparedMutation, before, want string) {
	t.Helper()
	full := prepared.FullDiff()
	if full == "" || full != prepared.FullDiff() || string(prepared.candidate) != want || applyCompleteDiffForTest(t, before, full) != want {
		t.Fatalf("prepared candidate/diff disagreement: candidate=%q want=%q", prepared.candidate, want)
	}
	preview, truncated, firstLine := buildMutationPreview(prepared.path, strings.TrimPrefix(before, "\ufeff"), strings.TrimPrefix(want, "\ufeff"), prepared.Presentation.PreviewKind, DefaultLimits().MutationPreviewBytes)
	if prepared.Presentation.Preview != preview || prepared.Presentation.Truncated != truncated || prepared.firstChangeLine != firstLine {
		t.Fatal("full diff changed the legacy short preview")
	}
}

func completeDiffDiskUnchanged(t *testing.T, root, before string, create bool) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, "file.txt"))
	if create {
		if !os.IsNotExist(err) {
			t.Fatal("create prepare touched disk")
		}
	} else if err != nil || !bytes.Equal(data, []byte(before)) {
		t.Fatal("prepare changed the original bytes")
	}
}

// Independent, small unified-diff applier: no production indexing, matching,
// range mapping or rendering helpers are used. It checks coordinates, exact
// old/context bytes and both hunk counts before reconstructing the candidate.
func applyCompleteDiffForTest(t *testing.T, before, diff string) string {
	t.Helper()
	split := func(text string) []string {
		parts := strings.SplitAfter(text, "\n")
		if parts[len(parts)-1] == "" {
			parts = parts[:len(parts)-1]
		}
		return parts
	}
	old, records := split(before), split(diff)
	if len(records) < 2 || !strings.HasPrefix(records[0], "--- ") || !strings.HasPrefix(records[1], "+++ ") {
		t.Fatalf("missing unified headers: %q", diff)
	}
	var output []string
	cursor := 0
	for at := 2; at < len(records); {
		var oldStart, oldCount, newStart, newCount int
		if n, err := fmt.Sscanf(records[at], "@@ -%d,%d +%d,%d @@\n", &oldStart, &oldCount, &newStart, &newCount); err != nil || n != 4 {
			t.Fatalf("invalid hunk header %q: %v", records[at], err)
		}
		oldOffset, newOffset := oldStart, newStart
		if oldCount > 0 {
			oldOffset--
		}
		if newCount > 0 {
			newOffset--
		}
		if oldOffset < cursor || oldOffset > len(old) {
			t.Fatalf("overlapping/outside old hunk at %d, cursor %d", oldOffset, cursor)
		}
		output = append(output, old[cursor:oldOffset]...)
		cursor = oldOffset
		if newOffset != len(output) {
			t.Fatalf("new coordinate=%d reconstructed=%d", newOffset, len(output))
		}
		at++
		usedOld, usedNew := 0, 0
		for at < len(records) && !strings.HasPrefix(records[at], "@@ ") {
			record := records[at]
			if len(record) < 2 || !strings.ContainsRune(" +-", rune(record[0])) || !strings.HasSuffix(record, "\n") {
				t.Fatalf("invalid body record %q", record)
			}
			marker, text := record[0], record[1:]
			at++
			if at < len(records) && records[at] == "\\ No newline at end of file\n" {
				text = strings.TrimSuffix(text, "\n")
				at++
			}
			if marker != '+' {
				if cursor >= len(old) || old[cursor] != text {
					t.Fatalf("old/context bytes do not match at line %d: %q", cursor+1, text)
				}
				cursor++
				usedOld++
			}
			if marker != '-' {
				output = append(output, text)
				usedNew++
			}
		}
		if usedOld != oldCount || usedNew != newCount {
			t.Fatalf("hunk counts got -%d +%d want -%d +%d", usedOld, usedNew, oldCount, newCount)
		}
	}
	output = append(output, old[cursor:]...)
	return strings.Join(output, "")
}
