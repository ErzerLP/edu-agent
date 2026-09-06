package workspace

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func queryPageTest(t *testing.T, w *Workspace, tool string, args map[string]any) map[string]any {
	t.Helper()
	raw, _ := json.Marshal(args)
	result := w.Execute(t.Context(), tool, string(raw))
	if code := resultCode(t, result); code != "" {
		t.Fatalf("%s: code=%s result=%+v", tool, code, result)
	}
	value := resultObject(t, result)
	if safeResultJSONSize(value) > w.limits.ResultBytes {
		t.Fatal("query result exceeded its JSON budget")
	}
	return value
}

func drainQueryTest(t *testing.T, w *Workspace, tool string, args map[string]any) ([]string, map[string]any, int) {
	t.Helper()
	var paths []string
	for page := 0; page < 150; page++ {
		value := queryPageTest(t, w, tool, args)
		if entries, ok := value["entries"].([]map[string]any); ok {
			for _, entry := range entries {
				paths = append(paths, entry["path"].(string))
			}
		}
		if files, ok := value["files"].([]string); ok {
			paths = append(paths, files...)
		}
		if matches, ok := value["matches"].([]map[string]any); ok {
			for _, match := range matches {
				paths = append(paths, fmt.Sprintf("%s:%v:%v", match["path"], match["line"], match["column"]))
			}
		}
		if value["more"] == false {
			return paths, value, page + 1
		}
		cursor, ok := value["next_cursor"].(string)
		if !ok || cursor == "" {
			t.Fatalf("unfinished page has no continuation: %+v", value)
		}
		args["cursor"] = cursor
	}
	t.Fatal("query did not terminate within the bounded fixture")
	return nil, nil, 0
}

func TestQueryPaginationLargeDirectory(t *testing.T) {
	root := t.TempDir()
	const size = 2501
	want := make([]string, size)
	for i := range size {
		want[i] = fmt.Sprintf("f%04d.txt", i)
		if err := os.WriteFile(filepath.Join(root, want[i]), []byte("needle\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	for _, tool := range []string{ToolList, ToolFind, ToolSearch} {
		t.Run(tool, func(t *testing.T) {
			w, err := Open(root)
			if err != nil {
				t.Fatal(err)
			}
			defer w.Close()
			args := map[string]any{}
			if tool == ToolFind {
				args["pattern"] = "*.txt"
			}
			if tool == ToolSearch {
				args["query"], args["output"] = "needle", "files"
			}
			paths, final, pages := drainQueryTest(t, w, tool, args)
			if !slices.Equal(paths, want) || final["complete"] != true || final["scan_complete"] != true || pages < 3 {
				t.Fatalf("lost/repeated/unsorted large directory: returned=%d pages=%d final=%+v", len(paths), pages, final)
			}
			if tool == ToolSearch && final["matched_files"] != size {
				t.Fatal("file count is not cumulative")
			}
		})
	}
}

func TestQueryPaginationSortedFrontierAndSingleFile(t *testing.T) {
	files := map[string]string{"a/z.txt": "x\n", "a.go": "x\n", "a-/m.txt": "x\n", "b.txt": strings.Repeat("x x\n", 15)}
	limits := DefaultLimits()
	limits.ListEntries, limits.DirectoryScanEntries, limits.SearchMatches = 2, 2, 2
	w, _ := openSearchOutputFixture(t, files, limits)
	paths, final, _ := drainQueryTest(t, w, ToolFind, map[string]any{"pattern": "*", "type": "file"})
	want := []string{"a-/m.txt", "a.go", "a/z.txt", "b.txt"}
	if !slices.Equal(paths, want) || final["complete"] != true {
		t.Fatalf("frontier order: %v %+v", paths, final)
	}
	matches, final, _ := drainQueryTest(t, w, ToolSearch, map[string]any{"query": "x", "path": "b.txt"})
	if len(matches) != 30 || final["complete"] != true {
		t.Fatalf("in-file continuation: %v %+v", matches, final)
	}
	for i, match := range matches {
		if match != fmt.Sprintf("b.txt:%d:%d", i/2+1, 1+2*(i%2)) {
			t.Fatalf("match %d=%s", i, match)
		}
	}
	_, final, _ = drainQueryTest(t, w, ToolSearch, map[string]any{"query": "x", "path": "b.txt", "output": "count"})
	if final["matched_lines"] != 15 || final["matched_files"] != 1 || final["counts_partial"] != false {
		t.Fatalf("count=%+v", final)
	}
}

func TestQueryPaginationStaleAfterEnumerationAndContentChanges(t *testing.T) {
	for _, mutation := range []string{"add", "delete", "body", "ignore"} {
		t.Run(mutation, func(t *testing.T) {
			limits := DefaultLimits()
			limits.SearchMatches = 1
			w, root := openSearchOutputFixture(t, map[string]string{"a": "x\nx\n", "b": "x\n", ".gitignore": "#yes\n"}, limits)
			args := map[string]any{"query": "x", "respect_gitignore": true}
			first := queryPageTest(t, w, ToolSearch, args)
			args["cursor"] = first["next_cursor"]
			var err error
			switch mutation {
			case "add":
				err = os.WriteFile(filepath.Join(root, "new"), nil, 0600)
			case "delete":
				err = os.Remove(filepath.Join(root, "b"))
			case "body":
				err = os.WriteFile(filepath.Join(root, "a"), []byte("y\ny\n"), 0600)
			case "ignore":
				err = os.WriteFile(filepath.Join(root, ".gitignore"), []byte("a\n#x\n"), 0600)
			}
			if err != nil {
				t.Fatal(err)
			}
			raw, _ := json.Marshal(args)
			if code := resultCode(t, w.Execute(t.Context(), ToolSearch, string(raw))); code != CodeCursorStale {
				t.Fatalf("%s not stale: %s", mutation, code)
			}
		})
	}
}

func TestQueryPaginationIdentityExpiryAndClose(t *testing.T) {
	limits := DefaultLimits()
	limits.ListEntries, limits.QueryRecords = 1, 1
	w, root := openSearchOutputFixture(t, map[string]string{"a": "x\n", "b": "x\n"}, limits)
	first := queryPageTest(t, w, ToolList, map[string]any{})
	cursor := first["next_cursor"].(string)
	qID, _, _ := parseQueryCursor(cursor)
	q := w.queries[qID]
	guard := q.guards["."]
	wrong, _ := json.Marshal(map[string]any{"pattern": "*", "cursor": cursor})
	if code := resultCode(t, w.Execute(t.Context(), ToolFind, string(wrong))); code != CodeCursorMismatch {
		t.Fatal(code)
	}
	q.lastUsed = time.Now().Add(-queryIdleTTL - time.Second)
	raw, _ := json.Marshal(map[string]any{"cursor": cursor})
	if code := resultCode(t, w.Execute(t.Context(), ToolList, string(raw))); code != CodeCursorExpired {
		t.Fatal(code)
	}
	if guard.Check() == nil {
		t.Fatal("expired query retained its guard")
	}
	other, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if code := resultCode(t, other.Execute(t.Context(), ToolList, string(raw))); code != CodeCursorExpired {
		t.Fatal(code)
	}
	queryPageTest(t, w, ToolList, map[string]any{})
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if w.queryBytes != 0 || len(w.queries) != 0 {
		t.Fatal("Close retained query state")
	}
}
