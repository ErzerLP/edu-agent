package workspace

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

func openSearchOutputFixture(t *testing.T, files map[string]string, limits Limits) (*Workspace, string) {
	t.Helper()
	root := t.TempDir()
	for path, content := range files {
		full := filepath.Join(root, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	w, err := OpenWithLimits(root, limits)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = w.Close() })
	return w, root
}

func TestSearchOutputDefaultCompatibilityAndModes(t *testing.T) {
	w, _ := openSearchOutputFixture(t, map[string]string{
		"b.txt": "needle body-secret needle\nneedle body-secret\n",
		"a.txt": "界needle needle body-secret\nNEEDLE body-secret\n",
		"c.txt": "unmatched\n",
	}, DefaultLimits())
	legacy := w.Execute(t.Context(), ToolSearch, `{"query":"needle"}`)
	explicit := w.Execute(t.Context(), ToolSearch, `{"query":"needle","output":"content","context":0}`)
	if !reflect.DeepEqual(legacy, explicit) {
		t.Fatalf("default changed: %+v explicit=%+v", legacy, explicit)
	}
	value := resultObject(t, legacy)
	matches := value["matches"].([]map[string]any)
	if value["output"] != "content" || value["complete"] != true || len(matches) != 6 || value["context_lines"] != nil ||
		matches[0]["path"] != "a.txt" || matches[0]["line"] != 1 || matches[0]["column"] != 2 || matches[1]["column"] != 9 || matches[2]["line"] != 2 {
		t.Fatalf("legacy occurrences/columns/sort changed: %+v", value)
	}
	for _, output := range []string{"files", "count"} {
		t.Run(output, func(t *testing.T) {
			result := w.Execute(t.Context(), ToolSearch, fmt.Sprintf(`{"query":"needle","output":%q}`, output))
			value := resultObject(t, result)
			encoded, _ := json.Marshal(value)
			if value["complete"] != true || value["counts_partial"] != false || value["matched_files"] != 2 || value["output"] != output {
				t.Fatalf("output=%s value=%+v", output, value)
			}
			for _, forbidden := range []string{"body-secret", "preview", "content", "context_lines", `"matches"`} {
				if strings.Contains(string(encoded), forbidden) || strings.Contains(result.Summary, forbidden) {
					t.Fatalf("body leaked: %s %s", encoded, result.Summary)
				}
			}
			if output == "files" {
				if !reflect.DeepEqual(value["files"], []string{"a.txt", "b.txt"}) || value["returned"] != 2 {
					t.Fatalf("files not unique/sorted: %+v", value)
				}
			} else if value["matched_lines"] != 4 || value["returned"] != 4 || value["files"] != nil {
				t.Fatalf("counted occurrences rather than lines: %+v", value)
			}
		})
	}
	for _, output := range []string{"content", "files", "count"} {
		value := resultObject(t, w.Execute(t.Context(), ToolSearch, fmt.Sprintf(`{"query":"missing","output":%q}`, output)))
		if value["returned"] != 0 || value["complete"] != true {
			t.Fatalf("empty %s: %+v", output, value)
		}
	}
}

func TestSearchOutputStrictArguments(t *testing.T) {
	w, _ := openSearchOutputFixture(t, nil, DefaultLimits())
	for _, fields := range []string{
		`"output":""`, `"output":"bad"`, `"output":null`, `"output":3`, `"output":[]`,
		`"context":-1`, `"context":4`, `"context":1.5`, `"context":null`, `"context":"1"`, `"context":true`,
		`"output":"files","context":1`, `"output":"count","context":3`,
		`"output":"files","unknown":true`, `"future_glob":"**"`, `"future_respect_gitignore":true`,
	} {
		raw := `{"query":"needle",` + fields + `}`
		if result := w.Execute(t.Context(), ToolSearch, raw); resultCode(t, result) != CodeInvalidArguments {
			t.Fatalf("accepted %s: %+v", raw, result.Value)
		}
		if _, ok := InitialProgress(ToolSearch, raw); ok {
			t.Fatalf("invalid presentation arguments reported progress: %s", raw)
		}
	}
	for _, fields := range []string{`"context":0`, `"context":3`, `"output":"files","context":0`, `"output":"count","context":0`} {
		raw := `{"query":"needle",` + fields + `}`
		if result := w.Execute(t.Context(), ToolSearch, raw); resultCode(t, result) != "" {
			t.Fatalf("rejected %s: %+v", raw, result.Value)
		}
	}
}

func TestSearchContextWindowsMergeByPathAndLine(t *testing.T) {
	text := "一\r\n二\r\nneedle needle\r\n四\r\nneedle\r\n六\r\n七\r\n八\r\n"
	w, _ := openSearchOutputFixture(t, map[string]string{"b.txt": text, "a.txt": text}, DefaultLimits())
	for contextLines := 0; contextLines <= 3; contextLines++ {
		value := resultObject(t, w.Execute(t.Context(), ToolSearch, fmt.Sprintf(`{"query":"needle","context":%d}`, contextLines)))
		if value["complete"] != true || value["returned"] != 6 {
			t.Fatalf("context=%d result=%+v", contextLines, value)
		}
		if contextLines == 0 {
			if value["context_lines"] != nil {
				t.Fatal("context=0 changed the content shape")
			}
			continue
		}
		lines := value["context_lines"].([]map[string]any)
		first, last := max(1, 3-contextLines), min(8, 5+contextLines)
		if len(lines) != 2*(last-first+1) {
			t.Fatalf("overlapping windows inflated: %+v", lines)
		}
		for i, line := range lines {
			path := "a.txt"
			if i >= last-first+1 {
				path = "b.txt"
			}
			if line["path"] != path || line["line"] != first+i%(last-first+1) || strings.ContainsAny(line["content"].(string), "\r\n") {
				t.Fatalf("context ordering/line=%+v index=%d", line, i)
			}
		}
	}
}

func TestSearchContextUTF8LongLinesAndSharedResultBudget(t *testing.T) {
	limits := DefaultLimits()
	w, _ := openSearchOutputFixture(t, map[string]string{
		"long.txt": strings.Repeat("界", 2000) + "\nneedle\n" + strings.Repeat("語", 2000),
		"many.txt": strings.Repeat("needle "+strings.Repeat(`"界`, 180)+"\n", 80),
	}, limits)
	value := resultObject(t, w.Execute(t.Context(), ToolSearch, `{"query":"needle","path":"long.txt","context":3}`))
	lines := value["context_lines"].([]map[string]any)
	if len(lines) != 3 || lines[0]["truncated"] != true || lines[2]["truncated"] != true {
		t.Fatalf("long context did not mark clipped lines: %+v", value)
	}
	for _, line := range lines {
		text := line["content"].(string)
		if !utf8.ValidString(text) || len(text) > limits.SearchPreviewBytes {
			t.Fatalf("unbounded/invalid context: %+v", line)
		}
	}
	value = resultObject(t, w.Execute(t.Context(), ToolSearch, `{"query":"needle","path":"many.txt","context":3}`))
	if safeResultJSONSize(value) > limits.ResultBytes || value["complete"] != false || value["truncation_reason"] != "result_bytes" || value["returned"] == 0 {
		t.Fatalf("context escaped shared result budget: %+v", value)
	}
}

func TestSearchOutputBoundedOccurrenceAllocationAndEmptyRegex(t *testing.T) {
	limits := DefaultLimits()
	limits.SearchMatches = 3
	w, _ := openSearchOutputFixture(t, map[string]string{"many.txt": strings.Repeat("界", 200000)}, limits)
	for _, query := range []string{"界", "^|界", ".*?"} {
		raw, _ := json.Marshal(map[string]any{"query": query, "mode": "regex"})
		value := resultObject(t, w.Execute(t.Context(), ToolSearch, string(raw)))
		if value["returned"] != 3 || value["complete"] != false || value["truncation_reason"] != "match_limit" {
			t.Fatalf("query=%s not bounded: %+v", query, value)
		}
	}
	value := resultObject(t, w.Execute(t.Context(), ToolSearch, `{"query":"界","output":"count"}`))
	if value["matched_lines"] != 1 || value["matched_files"] != 1 || value["counts_partial"] != false {
		t.Fatalf("count treats repeated occurrences as lines: %+v", value)
	}
}

func TestSearchCountPartialResourceLimits(t *testing.T) {
	for _, test := range []struct {
		name, reason string
		files        map[string]string
		limit        func(*Limits)
	}{
		{"matches", "match_limit", map[string]string{"a": "needle needle\nneedle\nneedle\n"}, func(l *Limits) { l.SearchMatches = 2 }},
		{"files", "file_limit", map[string]string{"a": "needle\n", "b": "needle\n"}, func(l *Limits) { l.SearchFiles = 1 }},
		{"entries", "entry_limit", map[string]string{"a/one": "needle\n", "b/two": "needle\n"}, func(l *Limits) { l.SearchEntries, l.SearchFiles = 2, 2 }},
		{"directory", "directory_entry_limit", map[string]string{"a": "needle\n", "b": "needle\n"}, func(l *Limits) { l.DirectoryScanEntries, l.ListEntries = 1, 1 }},
		{"bytes", "scan_bytes", map[string]string{"a": "needle\n", "b": "needle\n"}, func(l *Limits) { l.FileBytes, l.SearchBytes = 10, 10 }},
		{"depth", "depth_limit", map[string]string{"a/b/c": "needle\n"}, func(l *Limits) { l.SearchDepth = 1 }},
		{"large", "file_too_large", map[string]string{"a": "needle too large\n", "b": "needle\n"}, func(l *Limits) { l.FileBytes = 10 }},
	} {
		t.Run(test.name, func(t *testing.T) {
			limits := DefaultLimits()
			test.limit(&limits)
			w, _ := openSearchOutputFixture(t, test.files, limits)
			value := resultObject(t, w.Execute(t.Context(), ToolSearch, `{"query":"needle","output":"count"}`))
			if value["complete"] != false || value["counts_partial"] != true || value["truncation_reason"] != test.reason || safeResultJSONSize(value) > limits.ResultBytes {
				t.Fatalf("dishonest count: %+v", value)
			}
			if test.name == "matches" && (value["matched_lines"] != 2 || value["matched_files"] != 1) {
				t.Fatalf("partial count not line based: %+v", value)
			}
		})
	}
}

func TestSearchFilesPartialResultBytesAndFileLimit(t *testing.T) {
	files := map[string]string{}
	for i := range 80 {
		files[fmt.Sprintf("%02d-%s.txt", i, strings.Repeat("x", 100))] = "needle body-secret needle\nneedle body-secret\n"
	}
	limits := DefaultLimits()
	w, _ := openSearchOutputFixture(t, files, limits)
	value := resultObject(t, w.Execute(t.Context(), ToolSearch, `{"query":"needle","output":"files"}`))
	if value["counts_partial"] != true || value["complete"] != false || value["truncation_reason"] != "result_bytes" || safeResultJSONSize(value) > limits.ResultBytes || value["returned"] == 0 {
		t.Fatalf("files escaped result budget: %+v", value)
	}
	limits.SearchMatches = 2
	w, _ = openSearchOutputFixture(t, map[string]string{"a": "needle\n", "b": "needle\n", "c": "needle\n"}, limits)
	value = resultObject(t, w.Execute(t.Context(), ToolSearch, `{"query":"needle","output":"files"}`))
	if value["counts_partial"] != true || value["returned"] != 2 || value["truncation_reason"] != "match_limit" {
		t.Fatalf("unbounded files: %+v", value)
	}
}

func TestSearchCountUnreadableEntriesRemainPartial(t *testing.T) {
	for _, directory := range []bool{false, true} {
		t.Run(fmt.Sprint(directory), func(t *testing.T) {
			missing, reason := "b", "file_unavailable"
			if directory {
				missing, reason = "b/file", "directory_unavailable"
			}
			w, root := openSearchOutputFixture(t, map[string]string{"a": "needle\n", missing: "needle\n", "c": "needle\n"}, DefaultLimits())
			removed := false
			ctx := WithProgressReporter(t.Context(), func(p Progress) {
				if p.ScannedFiles == 1 && !removed {
					removed = true
					if err := os.RemoveAll(filepath.Join(root, "b")); err != nil {
						t.Fatal(err)
					}
				}
			})
			value := resultObject(t, w.Execute(ctx, ToolSearch, `{"query":"needle","output":"count"}`))
			if value["complete"] != false || value["counts_partial"] != true || value["truncation_reason"] != reason || value["matched_lines"] != 2 {
				t.Fatalf("missing entry counted as complete: %+v", value)
			}
		})
	}
}

func TestSearchOutputArchiveLinksLegacyFiltersAndCancellation(t *testing.T) {
	w, root := openSearchOutputFixture(t, map[string]string{
		".edu-agent-archive/saved.txt": "needle archived\n", "src/a.go": "needle needle\n", "src/b.txt": "needle\n",
	}, DefaultLimits())
	outside := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(outside, []byte("needle outside-body"), 0o600); err != nil {
		t.Fatal(err)
	}
	linked := os.Symlink(outside, filepath.Join(root, "linked.txt")) == nil
	for _, output := range []string{"content", "files", "count"} {
		value := resultObject(t, w.Execute(t.Context(), ToolSearch, fmt.Sprintf(`{"query":"needle","output":%q,"include":["*.go"],"exclude":["*.txt"]}`, output)))
		encoded, _ := json.Marshal(value)
		if strings.Contains(string(encoded), "archived") || strings.Contains(string(encoded), "outside-body") || value["scanned_files"] != 1 || value["complete"] != true {
			t.Fatalf("filters or confinement changed: %s", encoded)
		}
		value = resultObject(t, w.Execute(t.Context(), ToolSearch, fmt.Sprintf(`{"query":"needle","output":%q,"path":".edu-agent-archive"}`, output)))
		if value["returned"] != 1 || value["complete"] != true {
			t.Fatalf("explicit archive read denied: %+v", value)
		}
		if linked {
			result := w.Execute(t.Context(), ToolSearch, fmt.Sprintf(`{"query":"needle","output":%q,"path":"linked.txt"}`, output))
			if resultCode(t, result) != CodeLinkNotAllowed {
				// Some securefile backends classify a leaf symlink as a
				// non-directory first; legacy search then reports it skipped.
				value := resultObject(t, result)
				skipped, _ := value["skipped"].(map[string]any)
				if resultCode(t, result) != "" || value["returned"] != 0 || value["scanned_files"] != 0 || skipped["links"] != 1 {
					t.Fatalf("explicit link followed: %+v", result)
				}
			}
		}
		cancelled, cancel := context.WithCancel(t.Context())
		reports := 0
		cancelled = WithProgressReporter(cancelled, func(p Progress) {
			reports++
			if p.ScannedFiles == 1 {
				cancel()
			}
		})
		result := w.Execute(cancelled, ToolSearch, fmt.Sprintf(`{"query":"needle","output":%q}`, output))
		cancel()
		if resultCode(t, result) != CodeCancelled || reports != 2 {
			t.Fatalf("cancellation/progress mismatch: %+v reports=%d", result, reports)
		}
	}
}

func TestSearchOutputProgressUsesModeUnits(t *testing.T) {
	w, _ := openSearchOutputFixture(t, map[string]string{"a": "needle needle\nneedle\n"}, DefaultLimits())
	for output, want := range map[string]int{"content": 3, "count": 2, "files": 1} {
		var final Progress
		ctx := WithProgressReporter(t.Context(), func(p Progress) { final = p })
		result := w.Execute(ctx, ToolSearch, fmt.Sprintf(`{"query":"needle","output":%q}`, output))
		if resultCode(t, result) != "" || final.Matches != want || final.Returned != want || final.ScannedFiles != 1 {
			t.Fatalf("%s progress=%+v result=%+v", output, final, result)
		}
	}
}
