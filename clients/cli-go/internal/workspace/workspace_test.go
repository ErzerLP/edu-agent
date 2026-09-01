package workspace

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestWorkspaceFailuresExposeStableSafeRecovery(t *testing.T) {
	codes := []string{
		CodeReplacementMissing, CodeReplacementNotUnique, CodeReplacementOverlap,
		CodeContentChanged, CodeAuthorizationDenied, CodeCancelled, CodeTimeout, CodeInternalError,
	}
	for _, code := range codes {
		result := failureResult(code, "工作区操作未完成")
		value := resultObject(t, result)
		message, _ := value["message"].(string)
		suggestion, _ := value["suggestion"].(string)
		encoded, _ := json.Marshal(value)
		if value["code"] != code || value["error"] != code || message == "" || suggestion == "" || value["complete"] != false {
			t.Fatalf("code=%s value=%+v", code, value)
		}
		for _, secret := range []string{"/home/private", `C:\\Users\\private`, "permission denied"} {
			if strings.Contains(string(encoded), secret) {
				t.Fatalf("code=%s leaked %q: %s", code, secret, encoded)
			}
		}
	}
	missing := resultObject(t, failureResult(CodeReplacementMissing, "文件编辑目标文本不存在"))
	if !strings.Contains(missing["suggestion"].(string), "old_text") {
		t.Fatalf("missing suggestion=%q", missing["suggestion"])
	}
	changed := resultObject(t, failureResult(CodeContentChanged, "文件内容版本已变化"))
	if !strings.Contains(changed["suggestion"].(string), "content_hash") {
		t.Fatalf("content_changed suggestion=%q", changed["suggestion"])
	}
}

func TestWorkspacePathContractRejectsEscapesControlsAndADS(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "safe.txt"), []byte("safe\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	workspace, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer workspace.Close()

	tests := []struct {
		path string
		code string
	}{
		{path: "../secret", code: CodePathOutsideWorkspace},
		{path: "/etc/passwd", code: CodePathOutsideWorkspace},
		{path: `C:\\secret.txt`, code: CodePathOutsideWorkspace},
		{path: `dir\\file.txt`, code: CodeInvalidPath},
		{path: "safe.txt:stream", code: CodeInvalidPath},
		{path: "CON", code: CodeInvalidPath},
		{path: "bad\x00name", code: CodeInvalidPath},
		{path: "bad\u202ename", code: CodeInvalidPath},
	}
	for _, test := range tests {
		t.Run(test.path, func(t *testing.T) {
			arguments, marshalErr := json.Marshal(map[string]any{"path": test.path})
			if marshalErr != nil {
				t.Fatal(marshalErr)
			}
			result := workspace.Execute(t.Context(), ToolRead, string(arguments))
			if code := resultCode(t, result); code != test.code {
				t.Fatalf("code=%q want=%q value=%+v", code, test.code, result.Value)
			}
			encoded, _ := json.Marshal(result.Value)
			if strings.Contains(string(encoded), root) {
				t.Fatalf("absolute root leaked: %s", encoded)
			}
		})
	}

	for _, raw := range []string{`null`, `{"path":"safe.txt","unknown":true}`, `{"path":"safe.txt"} {}`} {
		result := workspace.Execute(t.Context(), ToolRead, raw)
		if code := resultCode(t, result); code != CodeInvalidArguments {
			t.Fatalf("raw=%q code=%q value=%+v", raw, code, result.Value)
		}
	}
}

func TestWorkspaceListIncludesDotfilesLinksAndStablePagination(t *testing.T) {
	root := t.TempDir()
	for _, directory := range []string{".git", ".comet", "dir"} {
		if err := os.Mkdir(filepath.Join(root, directory), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{".env", "b.txt", "a.txt"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(name), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	linkCreated := os.Symlink(filepath.Join(root, "a.txt"), filepath.Join(root, "linked")) == nil

	limits := DefaultLimits()
	limits.ListEntries = 3
	workspace, err := OpenWithLimits(root, limits)
	if err != nil {
		t.Fatal(err)
	}
	defer workspace.Close()

	first := workspace.Execute(t.Context(), ToolList, `{}`)
	firstValue := resultObject(t, first)
	if firstValue["complete"] != false || intValue(firstValue["next_offset"]) != 3 {
		t.Fatalf("first page=%+v", firstValue)
	}
	pages := []Result{first}
	all := entryPaths(t, firstValue)
	lastValue := firstValue
	for lastValue["complete"] == false {
		nextOffset := intValue(lastValue["next_offset"])
		arguments, marshalErr := json.Marshal(map[string]any{"offset": nextOffset})
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		page := workspace.Execute(t.Context(), ToolList, string(arguments))
		pages = append(pages, page)
		lastValue = resultObject(t, page)
		all = append(all, entryPaths(t, lastValue)...)
	}
	expected := []string{".comet", ".env", ".git", "a.txt", "b.txt", "dir"}
	if linkCreated {
		expected = append(expected, "linked")
	}
	sort.Strings(expected)
	if !reflect.DeepEqual(all, expected) {
		t.Fatalf("paths=%+v want=%+v", all, expected)
	}
	if linkCreated {
		types := entryTypes(t, lastValue)
		if types["linked"] != "link" {
			t.Fatalf("link type missing: %+v", types)
		}
	}
	for _, result := range pages {
		encoded, _ := json.Marshal(result.Value)
		if len(encoded) > limits.ResultBytes || strings.Contains(string(encoded), root) {
			t.Fatalf("unbounded or leaking list result: %s", encoded)
		}
	}
}

func TestWorkspaceReadPaginatesUTF8AndPinsContentHash(t *testing.T) {
	root := t.TempDir()
	content := "第一行\n第二行\n第三行\n第四行\n"
	if err := os.WriteFile(filepath.Join(root, "notes.md"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	limits := DefaultLimits()
	limits.ReadLines = 2
	workspace, err := OpenWithLimits(root, limits)
	if err != nil {
		t.Fatal(err)
	}
	defer workspace.Close()

	first := resultObject(t, workspace.Execute(t.Context(), ToolRead, `{"path":"notes.md"}`))
	if first["content"] != "第一行\n第二行\n" || first["complete"] != false || intValue(first["next_offset"]) != 3 || intValue(first["next_byte_offset"]) != 0 {
		t.Fatalf("first read=%+v", first)
	}
	hash, _ := first["content_hash"].(string)
	secondArgs, _ := json.Marshal(map[string]any{"path": "notes.md", "offset": 3, "limit": 2, "expected_hash": hash})
	second := resultObject(t, workspace.Execute(t.Context(), ToolRead, string(secondArgs)))
	if second["content"] != "第三行\n第四行\n" || second["complete"] != true || second["content_hash"] != hash {
		t.Fatalf("second read=%+v", second)
	}
	if err := os.WriteFile(filepath.Join(root, "notes.md"), []byte("changed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	changed := workspace.Execute(t.Context(), ToolRead, string(secondArgs))
	if code := resultCode(t, changed); code != CodeContentChanged || changed.Reference == nil || !changed.Reference.InvalidateObserved || changed.Reference.ContentHash != hash {
		t.Fatalf("changed code=%q reference=%+v value=%+v", code, changed.Reference, changed.Value)
	}
}

func TestWorkspaceReadLongLineUsesRuneSafeByteContinuation(t *testing.T) {
	root := t.TempDir()
	content := strings.Repeat("界", 2400) + "\nend\n"
	if err := os.WriteFile(filepath.Join(root, "long.txt"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	limits := DefaultLimits()
	limits.ResultBytes = 1400
	workspace, err := OpenWithLimits(root, limits)
	if err != nil {
		t.Fatal(err)
	}
	defer workspace.Close()

	first := resultObject(t, workspace.Execute(t.Context(), ToolRead, `{"path":"long.txt"}`))
	chunk, _ := first["content"].(string)
	nextByte := intValue(first["next_byte_offset"])
	if chunk == "" || !utf8.ValidString(chunk) || first["truncation_reason"] != "result_bytes" || nextByte != len(chunk) {
		t.Fatalf("first long read=%+v", first)
	}
	arguments, _ := json.Marshal(map[string]any{
		"path": "long.txt", "offset": 1, "byte_offset": nextByte,
		"expected_hash": first["content_hash"],
	})
	second := resultObject(t, workspace.Execute(t.Context(), ToolRead, string(arguments)))
	if second["content"] == "" || !utf8.ValidString(second["content"].(string)) {
		t.Fatalf("second long read=%+v", second)
	}
}

func TestWorkspaceReadRejectsBinaryInvalidUTF8OversizeAndLink(t *testing.T) {
	root := t.TempDir()
	files := map[string][]byte{
		"binary.dat":  {'a', 0, 'b'},
		"invalid.txt": {0xff, 0xfe},
		"large.txt":   []byte(strings.Repeat("x", 64)),
	}
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(root, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	linkCreated := os.Symlink(filepath.Join(root, "binary.dat"), filepath.Join(root, "link.txt")) == nil
	limits := DefaultLimits()
	limits.FileBytes = 32
	limits.SearchBytes = 64
	workspace, err := OpenWithLimits(root, limits)
	if err != nil {
		t.Fatal(err)
	}
	defer workspace.Close()

	for name, code := range map[string]string{"binary.dat": CodeBinaryFile, "invalid.txt": CodeInvalidUTF8, "large.txt": CodeFileTooLarge} {
		args, _ := json.Marshal(map[string]any{"path": name})
		if got := resultCode(t, workspace.Execute(t.Context(), ToolRead, string(args))); got != code {
			t.Fatalf("%s code=%q want=%q", name, got, code)
		}
	}
	if linkCreated {
		if got := resultCode(t, workspace.Execute(t.Context(), ToolRead, `{"path":"link.txt"}`)); got != CodeLinkNotAllowed {
			t.Fatalf("link code=%q", got)
		}
	}
}

func TestWorkspaceSearchIsBoundedStableAndIncludesHiddenPaths(t *testing.T) {
	root := t.TempDir()
	for _, directory := range []string{".git", ".comet", "src"} {
		if err := os.Mkdir(filepath.Join(root, directory), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	files := map[string]string{
		".env":            "Token marker\n",
		".git/config":     "marker git\n",
		".comet/state.md": "MARKER comet\n",
		"src/a.go":        "marker one\nMARKER two\n",
		"src/b.txt":       "marker text\n",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(name)), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	outside := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(outside, []byte("marker outside\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_ = os.Symlink(outside, filepath.Join(root, "outside-link"))

	limits := DefaultLimits()
	limits.SearchMatches = 4
	workspace, err := OpenWithLimits(root, limits)
	if err != nil {
		t.Fatal(err)
	}
	defer workspace.Close()

	result := resultObject(t, workspace.Execute(t.Context(), ToolSearch, `{"query":"marker","case":"insensitive"}`))
	paths := matchPaths(t, result)
	if !reflect.DeepEqual(paths, []string{".comet/state.md", ".env", ".git/config", "src/a.go"}) || result["complete"] != false || result["truncation_reason"] != "match_limit" {
		t.Fatalf("search=%+v paths=%+v", result, paths)
	}
	if strings.Contains(strings.Join(paths, ","), "outside") {
		t.Fatalf("search followed external link: %+v", paths)
	}

	goOnly := resultObject(t, workspace.Execute(t.Context(), ToolSearch, `{"query":"marker (one|two)","mode":"regex","case":"insensitive","include":["*.go"]}`))
	if got := matchPaths(t, goOnly); !reflect.DeepEqual(got, []string{"src/a.go", "src/a.go"}) {
		t.Fatalf("regex/include paths=%+v value=%+v", got, goOnly)
	}
	invalid := workspace.Execute(t.Context(), ToolSearch, `{"query":"(","mode":"regex"}`)
	if code := resultCode(t, invalid); code != CodeRegexInvalid {
		t.Fatalf("invalid regex code=%q value=%+v", code, invalid.Value)
	}
}

func TestWorkspaceRootRemainsFixedAfterCurrentDirectoryChanges(t *testing.T) {
	root := t.TempDir()
	other := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "root.txt"), []byte("root value\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(other, "root.txt"), []byte("other value\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	workspace, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer workspace.Close()
	t.Chdir(other)
	value := resultObject(t, workspace.Execute(t.Context(), ToolRead, `{"path":"root.txt"}`))
	if value["content"] != "root value\n" {
		t.Fatalf("workspace followed cwd: %+v", value)
	}
}

func TestWorkspaceCancelledAndTimedOutBeforeExecutionReturnStableCodes(t *testing.T) {
	workspace, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer workspace.Close()
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	if code := resultCode(t, workspace.Execute(cancelled, ToolSearch, `{"query":"x"}`)); code != CodeCancelled {
		t.Fatalf("cancel code=%q", code)
	}
	timedOut, stop := context.WithTimeout(t.Context(), time.Nanosecond)
	defer stop()
	<-timedOut.Done()
	if code := resultCode(t, workspace.Execute(timedOut, ToolSearch, `{"query":"x"}`)); code != CodeTimeout {
		t.Fatalf("timeout code=%q", code)
	}
}

func resultObject(t *testing.T, result Result) map[string]any {
	t.Helper()
	value, ok := result.Value.(map[string]any)
	if !ok {
		t.Fatalf("result is not an object: %#v", result.Value)
	}
	return value
}

func resultCode(t *testing.T, result Result) string {
	t.Helper()
	value := resultObject(t, result)
	code, _ := value["code"].(string)
	return code
}

func intValue(value any) int {
	switch current := value.(type) {
	case int:
		return current
	case float64:
		return int(current)
	default:
		return 0
	}
}

func entryPaths(t *testing.T, value map[string]any) []string {
	t.Helper()
	entries, ok := value["entries"].([]map[string]any)
	if !ok {
		t.Fatalf("entries=%T %+v", value["entries"], value["entries"])
	}
	paths := make([]string, 0, len(entries))
	for _, entry := range entries {
		paths = append(paths, entry["path"].(string))
	}
	return paths
}

func entryTypes(t *testing.T, value map[string]any) map[string]string {
	t.Helper()
	entries := value["entries"].([]map[string]any)
	result := make(map[string]string, len(entries))
	for _, entry := range entries {
		result[entry["path"].(string)] = entry["type"].(string)
	}
	return result
}

func matchPaths(t *testing.T, value map[string]any) []string {
	t.Helper()
	matches, ok := value["matches"].([]map[string]any)
	if !ok {
		t.Fatalf("matches=%T %+v", value["matches"], value["matches"])
	}
	paths := make([]string, 0, len(matches))
	for _, match := range matches {
		paths = append(paths, match["path"].(string))
	}
	return paths
}

func TestWorkspaceDefinitionsExposeFileToolsWithoutShell(t *testing.T) {
	names := make([]string, 0, len(Definitions()))
	for _, definition := range Definitions() {
		names = append(names, definition.Function.Name)
	}
	if !reflect.DeepEqual(names, []string{ToolList, ToolRead, ToolSearch, ToolWrite, ToolEdit}) {
		t.Fatalf("definitions=%+v", names)
	}
	for _, forbidden := range []string{"delete", "move", "copy", "patch", "shell"} {
		if strings.Contains(strings.Join(names, ","), forbidden) {
			t.Fatalf("forbidden tool exposed: %+v", names)
		}
	}
	if runtime.GOOS == "windows" {
		t.Log("native Windows path safety is exercised by this test on Windows")
	}
}
