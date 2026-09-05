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
)

func TestFindGlobSemantics(t *testing.T) {
	for _, test := range []struct {
		pattern, path string
		want          bool
	}{
		{"*.go", "src/deep/a.go", true}, {"src/*.go", "src/deep/a.go", false},
		{"src/**/*.go", "src/a.go", true}, {"src/**/*.go", "src/deep/a.go", true},
		{"src/**/test?.[ch]", "src/a/test1.c", true}, {"*.go", ".hidden.go", true},
		{"*.GO", "a.go", false}, {"a/**/b/**/c", "a/b/c", true},
		{strings.Repeat("**/", 60) + "a.go", "a/b/a.go", true},
	} {
		glob, err := compilePathGlob(test.pattern)
		if err != nil || glob.Match(test.path) != test.want {
			t.Fatalf("%q %q: err=%v", test.pattern, test.path, err)
		}
	}
	for _, bad := range []string{"", "[", "a/[", "../*", "/x/*", "a//b", "a\\b", "a\nb", strings.Repeat("x", 257)} {
		if _, err := compilePathGlob(bad); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
}

func findPaths(t *testing.T, result Result) []string {
	t.Helper()
	value := resultObject(t, result)
	if _, failed := value["error"]; failed {
		t.Fatalf("find failed: %+v", value)
	}
	paths := []string{}
	for _, entry := range value["entries"].([]map[string]any) {
		paths = append(paths, entry["path"].(string))
	}
	return paths
}

func TestFindPathsTypesArchiveAndNoBodyReads(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"src/main.go", "src/deep/test.go", "src/deep/data.bin", ".hidden.go", ".gitignore", ArchiveDirectory + "/old/a.go"} {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("src/\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "src", "locked.go"), []byte("private body"), 0); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "large.bin"), make([]byte, 1200000), 0o600); err != nil {
		t.Fatal(err)
	}
	w, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	for _, test := range []struct {
		args  string
		paths []string
	}{
		{`{"pattern":"*.go","type":"file"}`, []string{".hidden.go", "src/deep/test.go", "src/locked.go", "src/main.go"}},
		{`{"path":"src","pattern":"src/**/*.go","type":"file"}`, []string{"src/deep/test.go", "src/locked.go", "src/main.go"}},
		{`{"path":"src/main.go","pattern":"*.go"}`, []string{"src/main.go"}},
		{`{"pattern":"src/**","type":"directory"}`, []string{"src", "src/deep"}},
		{`{"path":".edu-agent-archive","pattern":"*.go"}`, []string{ArchiveDirectory + "/old/a.go"}},
		{`{"pattern":"large.bin"}`, []string{"large.bin"}},
		{`{"pattern":"missing"}`, []string{}},
	} {
		result := w.Execute(t.Context(), ToolFind, test.args)
		if got := findPaths(t, result); !reflect.DeepEqual(got, test.paths) {
			t.Fatalf("%s: got=%v want=%v", test.args, got, test.paths)
		}
		value := resultObject(t, result)
		if value["complete"] != true || result.Reference.Kind != "find_result" || result.Publication != "" {
			t.Fatalf("result=%+v", result)
		}
		encoded, _ := json.Marshal(value)
		if strings.Contains(string(encoded), "private body") || strings.Contains(string(encoded), root) {
			t.Fatal("find read/exposed body or root")
		}
	}
	t.Run("links", func(t *testing.T) {
		if err := os.Symlink(filepath.Join(root, "src"), filepath.Join(root, "alias")); err != nil {
			t.Skipf("symlink unavailable: %v", err)
		}
		result := w.Execute(t.Context(), ToolFind, `{"pattern":"alias/**"}`)
		if len(findPaths(t, result)) != 0 || resultObject(t, result)["skipped"].(map[string]int)["links"] != 1 {
			t.Fatalf("followed link: %+v", result)
		}
		if resultCode(t, w.Execute(t.Context(), ToolFind, `{"path":"alias","pattern":"*"}`)) != CodeLinkNotAllowed {
			t.Fatal("accepted scope link")
		}
	})
}

func TestFindBoundsCancellationAndInvalidArguments(t *testing.T) {
	root := t.TempDir()
	for index := 0; index < 8; index++ {
		if err := os.WriteFile(filepath.Join(root, strings.Repeat("a", 180)+fmt.Sprint(index)+".go"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, kind := range []string{"result", "count", "scan", "depth"} {
		t.Run(kind, func(t *testing.T) {
			limits := DefaultLimits()
			args := `{"pattern":"*.go"}`
			switch kind {
			case "result":
				limits.ResultBytes = 1024
			case "count":
				args = `{"pattern":"*.go","limit":1}`
			case "scan":
				limits.SearchEntries = 2
				limits.SearchFiles = 1
			case "depth":
				limits.SearchDepth = 1
				if err := os.MkdirAll(filepath.Join(root, "d", "e", "f"), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			w, err := OpenWithLimits(root, limits)
			if err != nil {
				t.Fatal(err)
			}
			defer w.Close()
			result := w.Execute(t.Context(), ToolFind, args)
			findPaths(t, result)
			value := resultObject(t, result)
			if value["complete"] != false || value["truncation_reason"] == "" || safeResultJSONSize(value) > limits.ResultBytes {
				t.Fatalf("unbounded/incomplete result: %+v", value)
			}
			if _, ok := value["next_offset"]; ok {
				t.Fatal("invented continuation")
			}
			if value["visited_entries"].(int) > limits.SearchEntries {
				t.Fatal("entry budget exceeded")
			}
		})
	}
	w, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	for _, raw := range []string{`{}`, `{"pattern":"*","type":"link"}`, `{"pattern":"*","limit":0}`, `{"pattern":"*","limit":201}`, `{"pattern":"["}`, `{"pattern":"*","unknown":1}`} {
		if resultCode(t, w.Execute(t.Context(), ToolFind, raw)) != CodeInvalidArguments {
			t.Fatalf("accepted bad arguments %s", raw)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if resultCode(t, w.Execute(ctx, ToolFind, `{"pattern":"*"}`)) != CodeCancelled {
		t.Fatal("cancel ignored")
	}
	archive := &Reference{Path: "src", Kind: "archive_directory", InvalidateObserved: true}
	for _, scope := range []string{".", "src", "src/deep", ArchiveDirectory} {
		if !archive.Supersedes(&Reference{Path: scope, Kind: "find_result", ContentHash: "old"}) {
			t.Fatalf("archive did not expire %s", scope)
		}
	}
	if archive.Supersedes(&Reference{Path: "unrelated", Kind: "find_result", ContentHash: "old"}) {
		t.Fatal("unrelated find invalidated")
	}
}
