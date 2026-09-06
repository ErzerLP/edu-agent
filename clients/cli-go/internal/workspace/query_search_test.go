package workspace

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/securefile"
)

func newQuerySearchTest(t *testing.T, text, raw string, limits Limits) (*workspaceQuery, string) {
	t.Helper()
	directory := t.TempDir()
	name := filepath.Join(directory, "note.txt")
	if err := os.WriteFile(name, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := securefile.OpenRoot(directory)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := root.Close(); err != nil {
			t.Error(err)
		}
	})
	args, err := decodeSearchArguments(raw)
	if err != nil {
		t.Fatal(err)
	}
	if args.Mode == "" {
		args.Mode = "literal"
	}
	if args.Case == "" {
		args.Case = "smart"
	}
	matcher, err := compileSearchMatcher(args)
	if err != nil {
		t.Fatal(err)
	}
	glob, err := compilePathGlob(*args.Glob)
	if err != nil {
		t.Fatal(err)
	}
	q := &workspaceQuery{
		w:        &Workspace{root: root, limits: limits},
		args:     queryArguments{tool: ToolSearch, path: "note.txt", search: args},
		observed: map[string]queryObservation{}, matcher: matcher, searchGlob: glob,
	}
	if *args.RespectGitignore {
		q.ignore = q.w.newGitignoreState(func(reason string, _ bool) { q.note(reason) })
	}
	return q, name
}

func openQuerySearchTest(t *testing.T, q *workspaceQuery) *queryStep {
	t.Helper()
	step := &queryStep{}
	paused, err := q.openSearchFile(t.Context(), queryNode{path: "note.txt", kind: securefile.EntryFile}, step)
	if err != nil || paused || q.done {
		t.Fatalf("open: paused=%v err=%v done=%v reason=%s", paused, err, q.done, q.reason)
	}
	return step
}

func drainQuerySearchTest(t *testing.T, q *workspaceQuery, step *queryStep) int {
	t.Helper()
	pages := 1
	for attempts := 0; attempts < 256 && q.searchFile != nil && !q.done; attempts++ {
		rows, lines, matches := len(q.rows), q.matchedLines, step.matches
		file := q.searchFile
		paused, err := q.advanceSearchFile(t.Context(), step)
		if err != nil {
			t.Fatal(err)
		}
		if len(q.rows)-rows > 1 || q.matchedLines-lines > 1 || step.matches-matches > 1 {
			t.Fatalf("more than one primary item advanced: rows=%d lines=%d matches=%d", len(q.rows)-rows, q.matchedLines-lines, step.matches-matches)
		}
		if q.searchFile != nil && q.searchFile != file {
			t.Fatal("continuation replaced the retained file")
		}
		if step.matches > q.w.limits.SearchMatches {
			t.Fatalf("match budget overrun: %+v", step)
		}
		if paused {
			if step.pause != "match_limit" || step.matches != q.w.limits.SearchMatches {
				t.Fatalf("unexpected pause: %+v", step)
			}
			step = &queryStep{}
			if q.ignore != nil {
				q.ignore.usedFiles, q.ignore.usedBytes = 0, 0
			}
			pages++
		}
	}
	if q.searchFile != nil {
		t.Fatalf("file did not reach EOF or release on capacity: done=%v reason=%s", q.done, q.reason)
	}
	if q.bytes != q.w.queryBytes || q.bytes < 0 || q.bytes > q.w.limits.QueryMemoryBytes {
		t.Fatalf("invalid retained accounting: query=%d workspace=%d", q.bytes, q.w.queryBytes)
	}
	return pages
}

func TestQuerySearchContentResumesWithinFile(t *testing.T) {
	limits := DefaultLimits()
	limits.SearchMatches = 2
	text := "\ufeff界 x x x\r\nno match\r\néx x\r\nx\r\n"
	q, _ := newQuerySearchTest(t, text, `{"query":"x"}`, limits)
	step := openQuerySearchTest(t, q)
	if step.files != 1 || step.bytes != int64(len(text)) {
		t.Fatalf("open accounting=%+v", step)
	}
	pages := drainQuerySearchTest(t, q, step)
	var positions [][2]int
	for _, row := range q.rows {
		positions = append(positions, [2]int{row.value["line"].(int), row.value["column"].(int)})
		preview := row.value["preview"].(string)
		if !utf8.ValidString(preview) || strings.ContainsAny(preview, "\r\n\ufeff") || len(row.context) != 0 {
			t.Fatalf("invalid preview/context: %+v", row)
		}
	}
	want := [][2]int{{1, 3}, {1, 5}, {1, 7}, {3, 2}, {3, 4}, {4, 1}}
	if !reflect.DeepEqual(positions, want) || pages != 3 || q.matchedLines != 3 || q.matchedFiles != 1 {
		t.Fatalf("positions=%v pages=%d lines=%d files=%d", positions, pages, q.matchedLines, q.matchedFiles)
	}
	if q.scannedFiles != 1 || q.scannedBytes != int64(len(text)) || q.reason != "" || q.done {
		t.Fatalf("file was reread or incompletely scanned: %+v", q)
	}
}

func TestQuerySearchRegexIndicesPersistAcrossPages(t *testing.T) {
	for _, pattern := range []string{"^|x", ".*?", "x*", "^|$"} {
		t.Run(pattern, func(t *testing.T) {
			limits := DefaultLimits()
			limits.SearchMatches = 2
			text := "界xxéx\r\n\r\nx界x\n"
			raw := `{"query":"` + pattern + `","mode":"regex","case":"sensitive"}`
			q, _ := newQuerySearchTest(t, text, raw, limits)
			var want [][2]int
			for i, rawLine := range splitTextLines(text) {
				line := trimLineEnding(rawLine)
				for _, match := range q.matcher.FindAllStringIndex(line, -1) {
					want = append(want, [2]int{i + 1, utf8.RuneCountInString(line[:match[0]]) + 1})
				}
			}
			step := openQuerySearchTest(t, q)
			paused, err := q.advanceSearchFile(t.Context(), step)
			if err != nil || paused || q.searchFile == nil || !q.searchFile.lineReady || q.searchFile.index != 1 {
				t.Fatalf("first advance: paused=%v err=%v file=%+v", paused, err, q.searchFile)
			}
			firstPair := &q.searchFile.indices[0][0]
			if paused, err := q.advanceSearchFile(t.Context(), step); err != nil || paused {
				t.Fatalf("second advance: paused=%v err=%v", paused, err)
			}
			if q.searchFile.line == 0 && q.searchFile.lineReady && &q.searchFile.indices[0][0] != firstPair {
				t.Fatal("indices were recomputed")
			}
			drainQuerySearchTest(t, q, step)
			var got [][2]int
			for _, row := range q.rows {
				got = append(got, [2]int{row.value["line"].(int), row.value["column"].(int)})
			}
			if !reflect.DeepEqual(got, want) || q.done || q.reason != "" {
				t.Fatalf("got=%v want=%v done=%v reason=%s", got, want, q.done, q.reason)
			}
		})
	}
}

func TestQuerySearchFilesAndCount(t *testing.T) {
	for _, output := range []string{"files", "count"} {
		t.Run(output, func(t *testing.T) {
			limits := DefaultLimits()
			limits.SearchMatches = 2
			q, _ := newQuerySearchTest(t, "xxx\nx\nnone\nxx\n", `{"query":"x","output":"`+output+`"}`, limits)
			pages := drainQuerySearchTest(t, q, openQuerySearchTest(t, q))
			if q.matchedFiles != 1 || q.scannedFiles != 1 || q.reason != "" || q.done {
				t.Fatalf("counters: %+v", q)
			}
			if output == "files" {
				if len(q.rows) != 1 || !reflect.DeepEqual(q.rows[0].value, map[string]any{"path": "note.txt"}) || len(q.rows[0].context) != 0 || pages != 1 {
					t.Fatalf("files leaked content or repeated: rows=%+v pages=%d", q.rows, pages)
				}
			} else if q.matchedLines != 3 || len(q.rows) != 0 || pages != 2 || q.units != 4 {
				t.Fatalf("count is not matching lines: lines=%d rows=%d pages=%d units=%d", q.matchedLines, len(q.rows), pages, q.units)
			}
		})
	}
}

func TestQuerySearchNoMatchAndSkippedText(t *testing.T) {
	for _, test := range []struct {
		name, text, raw string
		binary          int
	}{
		{name: "no match", text: "none\r\nno match\n", raw: `{"query":"x"}`},
		{name: "empty file even with empty regex", raw: `{"query":".*?","mode":"regex"}`},
		{name: "binary", text: "x\x00x", raw: `{"query":"x"}`, binary: 1},
		{name: "invalid UTF8", text: "x\xffx", raw: `{"query":"x"}`, binary: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			q, _ := newQuerySearchTest(t, test.text, test.raw, DefaultLimits())
			drainQuerySearchTest(t, q, openQuerySearchTest(t, q))
			if len(q.rows) != 0 || q.matchedFiles != 0 || q.matchedLines != 0 || q.binary != test.binary || q.reason != "" {
				t.Fatalf("unexpected match/skip: %+v", q)
			}
			if q.bytes != int64(2*len("note.txt")+384+128) || len(q.hashes) != 1 || q.scannedFiles != 1 || q.scannedBytes != int64(len(test.text)) {
				t.Fatalf("temporary memory or IO charged incorrectly: %+v", q)
			}
		})
	}
}

func TestQuerySearchContextAndPreviewAreBounded(t *testing.T) {
	limits := DefaultLimits()
	limits.SearchMatches, limits.SearchPreviewBytes = 2, 32
	long := strings.Repeat("界", 40)
	q, _ := newQuerySearchTest(t, "before\n"+long+"\nabove\nx x\nbelow\n"+long+"\nafter\nextra\n", `{"query":"x","context":3}`, limits)
	drainQuerySearchTest(t, q, openQuerySearchTest(t, q))
	if len(q.rows) != 3 {
		t.Fatalf("rows=%d", len(q.rows))
	}
	for _, row := range q.rows {
		line := row.value["line"].(int)
		if len(row.context) > 7 || len(row.value["preview"].(string)) > limits.SearchPreviewBytes {
			t.Fatalf("unbounded row=%+v", row)
		}
		for _, neighbor := range row.context {
			text := neighbor["content"].(string)
			n := neighbor["line"].(int)
			if n < line-3 || n > line+3 || len(text) > limits.SearchPreviewBytes || !utf8.ValidString(text) {
				t.Fatalf("invalid neighbor=%+v", neighbor)
			}
			if (n == 2 || n == 6) && neighbor["truncated"] != true {
				t.Fatalf("missing truncation marker: %+v", neighbor)
			}
		}
	}
}

func TestQuerySearchOpenBudgetsPauseBeforeReading(t *testing.T) {
	for _, respect := range []bool{false, true} {
		for _, budget := range []string{"files", "bytes"} {
			t.Run(budget+"/ignore="+map[bool]string{false: "false", true: "true"}[respect], func(t *testing.T) {
				limits := DefaultLimits()
				limits.SearchFiles, limits.SearchBytes, limits.FileBytes = 1, 8, 7
				raw := `{"query":"x","respect_gitignore":` + map[bool]string{false: "false", true: "true"}[respect] + `}`
				q, _ := newQuerySearchTest(t, "xxxxxxx", raw, limits)
				step := &queryStep{}
				wantPause := "scan_bytes"
				if budget == "files" {
					wantPause = "file_limit"
					if respect {
						q.ignore.usedFiles = 1
					} else {
						step.files = 1
					}
				} else if respect {
					q.ignore.usedBytes = 1
				} else {
					step.bytes = 1
				}
				before := *step
				paused, err := q.openSearchFile(t.Context(), queryNode{path: "note.txt"}, step)
				if err != nil || !paused || step.pause != wantPause || q.searchFile != nil || q.scannedFiles != 0 || q.scannedBytes != 0 || step.files != before.files || step.bytes != before.bytes {
					t.Fatalf("open did work on pause: paused=%v err=%v step=%+v query=%+v", paused, err, step, q)
				}
				if q.ignore != nil {
					q.ignore.usedFiles, q.ignore.usedBytes = 0, 0
				}
				step = openQuerySearchTest(t, q)
				if step.files != 1 || step.bytes != 7 || q.ignore != nil && (q.ignore.usedFiles != 1 || q.ignore.usedBytes != 7) {
					t.Fatalf("resumed accounting: step=%+v ignore=%+v", step, q.ignore)
				}
				drainQuerySearchTest(t, q, step)
			})
		}
	}
}

func TestQuerySearchImpossibleAtomicReadAndFileLimit(t *testing.T) {
	for _, test := range []struct {
		name, reason string
		fileBytes    int64
		large        int
	}{
		{name: "growth probe cannot fit", reason: "scan_bytes", fileBytes: 7},
		{name: "file budget unchanged", reason: "file_too_large", fileBytes: 6, large: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			limits := DefaultLimits()
			limits.SearchBytes, limits.FileBytes = 7, test.fileBytes
			q, _ := newQuerySearchTest(t, "xxxxxxx", `{"query":"x"}`, limits)
			step := &queryStep{}
			paused, err := q.openSearchFile(t.Context(), queryNode{path: "note.txt"}, step)
			if err != nil || paused || q.done || q.reason != test.reason || q.large != test.large || q.searchFile != nil || step.files != 0 || step.bytes != 0 {
				t.Fatalf("impossible atom must skip without reading/retrying: paused=%v err=%v step=%+v query=%+v", paused, err, step, q)
			}
		})
	}
}

func TestQuerySearchCapacityBoundsBodyLinesAndIndices(t *testing.T) {
	t.Run("body reservation", func(t *testing.T) {
		limits := DefaultLimits()
		limits.QueryMemoryBytes = 600
		q, _ := newQuerySearchTest(t, strings.Repeat("x", 100), `{"query":"x"}`, limits)
		step := &queryStep{}
		paused, err := q.openSearchFile(t.Context(), queryNode{path: "note.txt"}, step)
		if err != nil || paused || !q.done || q.reason != CodeQueryCapacity || step.files != 0 || q.searchFile != nil {
			t.Fatalf("body overallocated: paused=%v err=%v step=%+v query=%+v", paused, err, step, q)
		}
	})
	t.Run("line reservation", func(t *testing.T) {
		limits := DefaultLimits()
		limits.QueryMemoryBytes = 6000
		q, _ := newQuerySearchTest(t, strings.Repeat("\n", 1000), `{"query":"x"}`, limits)
		step := &queryStep{}
		paused, err := q.openSearchFile(t.Context(), queryNode{path: "note.txt"}, step)
		if err != nil || paused || !q.done || q.reason != CodeQueryCapacity || step.files != 1 || q.searchFile != nil || q.bytes != int64(2*len("note.txt")+384+128) || len(q.hashes) != 1 {
			t.Fatalf("line table overallocated or body leaked: paused=%v err=%v step=%+v query=%+v", paused, err, step, q)
		}
	})
	t.Run("bounded empty regex lookahead", func(t *testing.T) {
		limits := DefaultLimits()
		limits.QueryEntries, limits.SearchMatches = 4, 2
		q, _ := newQuerySearchTest(t, strings.Repeat("x", 1<<20), `{"query":".*?","mode":"regex"}`, limits)
		step := openQuerySearchTest(t, q)
		if paused, err := q.advanceSearchFile(t.Context(), step); err != nil || paused {
			t.Fatalf("advance: paused=%v err=%v", paused, err)
		}
		if q.searchFile == nil || len(q.searchFile.indices) != 3 || !q.searchFile.indicesPartial || q.searchFile.indexBytes > 4*querySearchIndexBytes+querySearchIndexOverhead {
			t.Fatalf("empty regex allocated unbounded indices: %+v", q.searchFile)
		}
		drainQuerySearchTest(t, q, step)
		if !q.done || q.reason != CodeQueryCapacity || len(q.rows) != 3 || q.units != 4 || q.scannedFiles != 1 {
			t.Fatalf("bounded prefix claimed EOF or dropped a retained match: %+v", q)
		}
		for i, row := range q.rows {
			if row.value["column"] != i+1 {
				t.Fatalf("prefix lost order: %+v", q.rows)
			}
		}
	})
	t.Run("index memory", func(t *testing.T) {
		q, _ := newQuerySearchTest(t, "xxx", `{"query":"x"}`, DefaultLimits())
		step := openQuerySearchTest(t, q)
		q.w.limits.QueryMemoryBytes = q.w.queryBytes + querySearchIndexOverhead + querySearchIndexBytes
		if paused, err := q.advanceSearchFile(t.Context(), step); err != nil || paused || !q.done || q.reason != CodeQueryCapacity || len(q.rows) != 0 || q.searchFile != nil {
			t.Fatalf("index memory overrun: paused=%v err=%v query=%+v", paused, err, q)
		}
	})
	t.Run("no match with no index capacity", func(t *testing.T) {
		q, _ := newQuerySearchTest(t, "none\n", `{"query":"x"}`, DefaultLimits())
		step := openQuerySearchTest(t, q)
		q.w.limits.QueryEntries = q.units
		q.w.limits.QueryMemoryBytes = q.w.queryBytes
		drainQuerySearchTest(t, q, step)
		if q.done || q.reason != "" || len(q.rows) != 0 {
			t.Fatalf("capacity manufactured a match or gap: %+v", q)
		}
	})
	t.Run("count cumulative units", func(t *testing.T) {
		limits := DefaultLimits()
		limits.QueryEntries = 3
		q, _ := newQuerySearchTest(t, "xx\nx\nx\n", `{"query":"x","output":"count"}`, limits)
		drainQuerySearchTest(t, q, openQuerySearchTest(t, q))
		if !q.done || q.reason != CodeQueryCapacity || q.matchedLines != 2 || q.matchedFiles != 1 || len(q.rows) != 0 {
			t.Fatalf("count escaped cumulative capacity: %+v", q)
		}
	})
}

// Run a synchronous hook at a context boundary, without timing races or
// changing the secure reader's production hooks. ReadSnapshot itself accepts
// no context, so the first Err after scannedFiles advances is post-read.
type querySearchHookContext struct {
	context.Context
	hook func()
}

func (c querySearchHookContext) Err() error {
	c.hook()
	return c.Context.Err()
}

func TestQuerySearchPostReadRevalidationAndCancellation(t *testing.T) {
	for _, action := range []string{"replace", "cancel"} {
		t.Run(action, func(t *testing.T) {
			q, name := newQuerySearchTest(t, "xxx", `{"query":"x"}`, DefaultLimits())
			base, cancel := context.WithCancel(t.Context())
			defer cancel()
			triggered := false
			ctx := querySearchHookContext{Context: base, hook: func() {
				if triggered || q.scannedFiles == 0 {
					return
				}
				triggered = true
				if action == "cancel" {
					cancel()
				} else if err := os.WriteFile(name, []byte("replacement"), 0o600); err != nil {
					t.Fatal(err)
				}
			}}
			step := &queryStep{}
			paused, err := q.openSearchFile(ctx, queryNode{path: "note.txt"}, step)
			want := securefile.ErrChanged
			if action == "cancel" {
				want = context.Canceled
			}
			if !triggered || !errors.Is(err, want) || paused || step.files != 1 || step.bytes != 3 || q.searchFile != nil || len(q.rows) != 0 || q.bytes != int64(len("note.txt")+384) {
				t.Fatalf("post-read change/cancel was hidden or leaked memory: paused=%v err=%v step=%+v query=%+v", paused, err, step, q)
			}
		})
	}
}

func TestQuerySearchCancellationAndChangedVersion(t *testing.T) {
	t.Run("cancel before open", func(t *testing.T) {
		q, _ := newQuerySearchTest(t, "x", `{"query":"x"}`, DefaultLimits())
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		step := &queryStep{}
		paused, err := q.openSearchFile(ctx, queryNode{path: "note.txt"}, step)
		if !errors.Is(err, context.Canceled) || paused || q.scannedFiles != 0 || q.bytes != 0 {
			t.Fatalf("cancelled open advanced: paused=%v err=%v query=%+v", paused, err, q)
		}
	})
	t.Run("cancel retained file", func(t *testing.T) {
		q, _ := newQuerySearchTest(t, "xxx", `{"query":"x"}`, DefaultLimits())
		step := openQuerySearchTest(t, q)
		if _, err := q.advanceSearchFile(t.Context(), step); err != nil {
			t.Fatal(err)
		}
		before := q.bytes - q.searchFile.retained - q.searchFile.indexBytes
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		if paused, err := q.advanceSearchFile(ctx, step); !errors.Is(err, context.Canceled) || paused || len(q.rows) != 1 || q.searchFile != nil || q.bytes != before {
			t.Fatalf("cancelled continuation advanced or retained body: paused=%v err=%v query=%+v", paused, err, q)
		}
	})
	for _, stage := range []string{"before open", "retained file"} {
		t.Run("version/"+stage, func(t *testing.T) {
			q, name := newQuerySearchTest(t, "xxx", `{"query":"x"}`, DefaultLimits())
			step := &queryStep{}
			if stage == "before open" {
				if _, err := q.observe(t.Context(), "note.txt"); err != nil {
					t.Fatal(err)
				}
			} else {
				step = openQuerySearchTest(t, q)
			}
			if err := os.WriteFile(name, []byte("different size"), 0o600); err != nil {
				t.Fatal(err)
			}
			var err error
			if stage == "before open" {
				_, err = q.openSearchFile(t.Context(), queryNode{path: "note.txt"}, step)
			} else {
				_, err = q.advanceSearchFile(t.Context(), step)
			}
			if !errors.Is(err, securefile.ErrChanged) || len(q.rows) != 0 || q.searchFile != nil {
				t.Fatalf("version change not rejected: err=%v query=%+v", err, q)
			}
		})
	}
}

func TestQuerySearchPathFiltersAndCaseRemainCompatible(t *testing.T) {
	for _, raw := range []string{
		`{"query":"x","glob":"*.md"}`,
		`{"query":"x","include":["*.md"]}`,
		`{"query":"x","include":["*.txt"],"exclude":["note.txt"]}`,
	} {
		q, _ := newQuerySearchTest(t, "x", raw, DefaultLimits())
		step := &queryStep{}
		if paused, err := q.openSearchFile(t.Context(), queryNode{path: "note.txt"}, step); err != nil || paused || q.scannedFiles != 0 || len(q.observed) != 0 || q.searchFile != nil {
			t.Fatalf("filter performed IO: raw=%s paused=%v err=%v query=%+v", raw, paused, err, q)
		}
	}
	for _, test := range []struct {
		raw  string
		want int
	}{
		{raw: `{"query":"x"}`, want: 3},
		{raw: `{"query":"X"}`, want: 2},
		{raw: `{"query":"X","case":"insensitive"}`, want: 3},
		{raw: `{"query":"x","case":"sensitive"}`, want: 1},
		{raw: `{"query":"."}`, want: 1},
	} {
		q, _ := newQuerySearchTest(t, "xXX.", test.raw, DefaultLimits())
		drainQuerySearchTest(t, q, openQuerySearchTest(t, q))
		if len(q.rows) != test.want {
			t.Fatalf("raw=%s rows=%d want=%d", test.raw, len(q.rows), test.want)
		}
	}
}

func TestQuerySearchUnsafeEntriesAreNotRead(t *testing.T) {
	q, name := newQuerySearchTest(t, "x", `{"query":"x"}`, DefaultLimits())
	if err := os.Mkdir(filepath.Join(filepath.Dir(name), "directory"), 0o700); err != nil {
		t.Fatal(err)
	}
	step := &queryStep{}
	if paused, err := q.openSearchFile(t.Context(), queryNode{path: "directory"}, step); err != nil || paused || q.other != 1 || step.files != 0 || q.searchFile != nil {
		t.Fatalf("directory body read: paused=%v err=%v query=%+v", paused, err, q)
	}
	if err := os.Symlink(name, filepath.Join(filepath.Dir(name), "link.txt")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if paused, err := q.openSearchFile(t.Context(), queryNode{path: "link.txt"}, step); err != nil || paused || q.links != 1 || step.files != 0 || q.searchFile != nil {
		t.Fatalf("link body read: paused=%v err=%v query=%+v", paused, err, q)
	}
	if err := q.searchReadError(t.Context(), securefile.ErrOutsideRoot); !errors.Is(err, securefile.ErrOutsideRoot) {
		t.Fatalf("outside-root error swallowed: %v", err)
	}
}
