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

func ignorePaths(t *testing.T, w *Workspace, tool, scope string, respect *bool) ([]string, map[string]any) {
	t.Helper()
	args := map[string]any{"path": scope}
	if tool == ToolFind {
		args["pattern"], args["type"] = "*.txt", "file"
	} else {
		args["query"], args["output"] = "needle", "files"
	}
	if respect != nil {
		args["respect_gitignore"] = *respect
	}
	raw, _ := json.Marshal(args)
	result := w.Execute(t.Context(), tool, string(raw))
	value := resultObject(t, result)
	if code := resultCode(t, result); code != "" {
		t.Fatalf("%s %s: %+v", tool, raw, result)
	}
	if tool == ToolFind {
		return findPaths(t, result), value
	}
	return value["files"].([]string), value
}

func TestGitignoreDiscoveryLayersNegationAndExplicitScopes(t *testing.T) {
	w, root := openSearchOutputFixture(t, map[string]string{
		".gitignore": "# comment\r\n*.log\n*.tmp\n/root-only.txt\nblocked/\n!blocked/keep.txt\n*.txt\n!keep.txt\n!nested.txt\n!hidden.txt\n!open.txt\n!large.txt\n!locked.txt\n!.hidden.txt\n!sub/\n",
		"keep.txt":   "needle", "drop.txt": "needle", "root-only.txt": "needle", ".hidden.txt": "needle",
		"blocked/keep.txt": "needle", "blocked/.gitignore": "!keep.txt\n",
		"sub/.gitignore": "!nested.txt\n/root-only.txt\n!root-only.txt\nlocal/\n",
		"sub/nested.txt": "needle", "sub/drop.txt": "needle", "sub/root-only.txt": "needle", "sub/local/open.txt": "needle",
		"sub/deep/open.txt": "needle", "sub/deep/root-only.txt": "needle",
		ArchiveDirectory + "/keep.txt": "needle",
	}, DefaultLimits())
	// Restrict the root anchor with a later rule, while the child can override it.
	f, err := os.OpenFile(filepath.Join(root, ".gitignore"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.WriteString("!root-only.txt\n/root-only.txt\n"); err != nil {
		t.Fatal(err)
	}
	if err = f.Close(); err != nil {
		t.Fatal(err)
	}
	on, off := true, false
	for _, tool := range []string{ToolFind, ToolSearch} {
		t.Run(tool, func(t *testing.T) {
			legacy, legacyValue := ignorePaths(t, w, tool, ".", nil)
			explicit, explicitValue := ignorePaths(t, w, tool, ".", &off)
			if !reflect.DeepEqual(legacyValue, explicitValue) || !reflect.DeepEqual(legacy, explicit) || len(legacy) != 11 {
				t.Fatalf("default changed %v %v", legacy, explicit)
			}
			paths, value := ignorePaths(t, w, tool, ".", &on)
			want := []string{".hidden.txt", "keep.txt", "sub/deep/open.txt", "sub/deep/root-only.txt", "sub/nested.txt", "sub/root-only.txt"}
			if !reflect.DeepEqual(paths, want) || value["complete"] != true || value["ignore_files"] != 2 || value["ignored_entries"] != 5 {
				t.Fatalf("layers: paths=%v value=%+v", paths, value)
			}
			for scope, want := range map[string][]string{
				"sub":      {"sub/deep/open.txt", "sub/deep/root-only.txt", "sub/nested.txt", "sub/root-only.txt"},
				"sub/deep": {"sub/deep/open.txt", "sub/deep/root-only.txt"},
				"blocked":  {}, "blocked/keep.txt": {"blocked/keep.txt"},
				"sub/local": {}, "drop.txt": {"drop.txt"},
				ArchiveDirectory: {ArchiveDirectory + "/keep.txt"},
			} {
				paths, value := ignorePaths(t, w, tool, scope, &on)
				if !reflect.DeepEqual(paths, want) || value["complete"] != true {
					t.Fatalf("%s paths=%v want=%v value=%+v", scope, paths, want, value)
				}
			}
		})
	}
}

func TestGitignoreGlobDirectoryRulesAndEscapes(t *testing.T) {
	for _, test := range []struct {
		pattern, path   string
		directory, want bool
	}{
		{"*.txt", "a/.hidden.txt", false, true}, {"/root.txt", "sub/root.txt", false, false},
		{"/root.txt", "root.txt", false, true}, {"docs/*.txt", "docs/deep/a.txt", false, false},
		{"docs/**/a?.txt", "docs/a1.txt", false, true}, {"docs/**/a?.txt", "docs/deep/a2.txt", false, true},
		{"**/cache/", "deep/cache", true, true}, {"cache/", "cache", false, false},
		{"cache/", "deep/cache", true, true}, {"a/**", "a", true, false}, {"a/**", "a/b", true, true},
		{"a/**", "a/b/c", false, true}, {"**", "a/b", false, true},
		{"file[!0-9].txt", "filea.txt", false, true}, {"file[!0-9].txt", "file1.txt", false, false},
		{"file[^0-9].txt", "filea.txt", false, true}, {"file[]a].txt", "file].txt", false, true},
		{`file[\n].txt`, "filen.txt", false, true}, {`file[a\-z].txt`, "file-.txt", false, true},
		{`\#file.txt`, "#file.txt", false, true}, {`\!file.txt`, "!file.txt", false, true},
		{`with\ space.txt`, "with space.txt", false, true}, {`file\[a\].txt`, "file[a].txt", false, true},
		{`file\*.txt`, "file*.txt", false, true}, {`file\?.txt`, "file?.txt", false, true},
		{"a.txt   ", "a.txt", false, true}, {`a.txt\ `, "a.txt ", false, true},
		{" leading.txt", " leading.txt", false, true}, {"a**b", "axb", false, true},
	} {
		rule, exists, err := parseIgnoreRule(test.pattern)
		got := exists && (!rule.directory || test.directory) && rule.match(test.path)
		if err != nil || got != test.want {
			t.Fatalf("%q %q: got=%v err=%v", test.pattern, test.path, got, err)
		}
	}
	for _, invalid := range []string{"!", "[", `a\`, "[[:digit:]]", "a//b", strings.Repeat("x", maxIgnorePatternBytes+1)} {
		if _, _, err := parseIgnoreRule(invalid); err == nil {
			t.Fatalf("accepted unsupported %q", invalid)
		}
	}
	for _, comment := range []string{"", " ", "# comment"} {
		if _, exists, err := parseIgnoreRule(comment); exists || err != nil {
			t.Fatalf("comment %q not ignored", comment)
		}
	}
	w, _ := openSearchOutputFixture(t, map[string]string{
		".gitignore": "a/**\n!a/keep.txt\ncache/\nfile[!0-9].txt\n\\#file.txt\n\\!file.txt\nwith\\ space.txt\n",
		"a/keep.txt": "needle", "a/no.txt": "needle", "a/deep/keep.txt": "needle",
		"a/deep/.gitignore": "!keep.txt", "cache/no.txt": "needle", "filea.txt": "needle", "file1.txt": "needle",
		"#file.txt": "needle", "!file.txt": "needle", "with space.txt": "needle",
	}, DefaultLimits())
	on := true
	for _, tool := range []string{ToolFind, ToolSearch} {
		paths, value := ignorePaths(t, w, tool, ".", &on)
		if !reflect.DeepEqual(paths, []string{"a/keep.txt", "file1.txt"}) || value["complete"] != true || value["ignore_files"] != 1 {
			t.Fatalf("%s trailing **/negation: %v %+v", tool, paths, value)
		}
	}
}

func TestGitignoreFailureClosesOnlyAffectedSubtreeAndPartialCount(t *testing.T) {
	for _, test := range []struct{ name, text, reason string }{
		{"syntax", "# private-rule-text\n[\n", "gitignore_pattern_unsupported"},
		{"utf8", "\xff private-rule-text", "gitignore_invalid_text"},
		{"nul", "a\x00b", "gitignore_invalid_text"},
		{"large", strings.Repeat("#", maxIgnoreFileBytes+1), "gitignore_file_too_large"},
		{"rules", strings.Repeat("*.secret\n", maxIgnoreRules+1), "gitignore_rule_limit"},
		{"pattern", strings.Repeat("x", maxIgnorePatternBytes+1), "gitignore_pattern_unsupported"},
	} {
		t.Run(test.name, func(t *testing.T) {
			w, root := openSearchOutputFixture(t, map[string]string{
				"a/good.txt": "needle", "b/.gitignore": test.text, "b/must-not-scan.txt": "needle",
				"c/good.txt": "needle", "c/deep/good.txt": "needle",
			}, DefaultLimits())
			on := true
			for _, tool := range []string{ToolFind, ToolSearch} {
				paths, value := ignorePaths(t, w, tool, ".", &on)
				if !reflect.DeepEqual(paths, []string{"a/good.txt", "c/deep/good.txt", "c/good.txt"}) || value["complete"] != false || value["truncation_reason"] != test.reason {
					t.Fatalf("%s %+v", tool, value)
				}
				encoded, _ := json.Marshal(value)
				if strings.Contains(string(encoded), "private-rule-text") || strings.Contains(string(encoded), root) {
					t.Fatal("ignore body/root leaked")
				}
				paths, value = ignorePaths(t, w, tool, "b/must-not-scan.txt", &on)
				if len(paths) != 1 || value["complete"] != true || value["ignore_files"] != 0 {
					t.Fatalf("explicit file hidden: %+v", value)
				}
			}
			value := resultObject(t, w.Execute(t.Context(), ToolSearch, `{"query":"needle","output":"count","respect_gitignore":true}`))
			if value["matched_lines"] != 3 || value["matched_files"] != 3 || value["counts_partial"] != true || value["complete"] != false {
				t.Fatalf("dishonest count: %+v", value)
			}
		})
	}
}

func TestGitignoreStrictArgumentsAndCallerFilters(t *testing.T) {
	w, _ := openSearchOutputFixture(t, map[string]string{".gitignore": "*.txt\n", "a.txt": "needle"}, DefaultLimits())
	for _, tool := range []string{ToolFind, ToolSearch} {
		base := `"pattern":"*"`
		if tool == ToolSearch {
			base = `"query":"needle"`
		}
		for _, invalid := range []string{"null", "0", "1", `"true"`, "[]", "{}"} {
			raw := "{" + base + `,"respect_gitignore":` + invalid + "}"
			if resultCode(t, w.Execute(t.Context(), tool, raw)) != CodeInvalidArguments {
				t.Fatalf("accepted %s", raw)
			}
			if _, ok := InitialProgress(tool, raw); ok {
				t.Fatalf("invalid progress %s", raw)
			}
		}
	}
	for _, raw := range []string{
		`{"path":"a.txt","query":"needle","respect_gitignore":true,"glob":"*.go"}`,
		`{"path":"a.txt","query":"needle","respect_gitignore":true,"include":["*.go"]}`,
		`{"path":"a.txt","query":"needle","respect_gitignore":true,"exclude":["*.txt"]}`,
	} {
		value := resultObject(t, w.Execute(t.Context(), ToolSearch, raw))
		if value["returned"] != 0 || value["scanned_files"] != 0 || value["complete"] != true {
			t.Fatalf("explicit file bypassed filters %+v", value)
		}
	}
	if paths := findPaths(t, w.Execute(t.Context(), ToolFind, `{"path":"a.txt","pattern":"*.go","respect_gitignore":true}`)); len(paths) != 0 {
		t.Fatal("explicit find bypassed glob")
	}
}

func TestGitignoreOutsideArchiveAndUnsafeFiles(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "root")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(parent, ".gitignore"), []byte("*.txt\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("needle"), 0o600); err != nil {
		t.Fatal(err)
	}
	w, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	on := true
	for _, tool := range []string{ToolFind, ToolSearch} {
		paths, value := ignorePaths(t, w, tool, ".", &on)
		if !reflect.DeepEqual(paths, []string{"a.txt"}) || value["ignore_files"] != 0 || value["complete"] != true {
			t.Fatalf("read outside rules %+v", value)
		}
	}
	for _, kind := range []string{"directory", "link"} {
		t.Run(kind, func(t *testing.T) {
			w, root := openSearchOutputFixture(t, map[string]string{
				"a.txt": "needle", "bad/hidden.txt": "needle", ArchiveDirectory + "/secret.txt": "needle",
			}, DefaultLimits())
			path := filepath.Join(root, "bad", ".gitignore")
			if kind == "directory" {
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.Symlink(filepath.Join(parent, ".gitignore"), path); err != nil {
					t.Skipf("symlink unavailable: %v", err)
				}
			}
			for _, tool := range []string{ToolFind, ToolSearch} {
				paths, value := ignorePaths(t, w, tool, ".", &on)
				if !reflect.DeepEqual(paths, []string{"a.txt"}) || value["complete"] != false || value["truncation_reason"] != "gitignore_unsafe_type" {
					t.Fatalf("unsafe rules broadened scope %+v", value)
				}
				paths, value = ignorePaths(t, w, tool, ArchiveDirectory, &on)
				if len(paths) != 1 || value["complete"] != true {
					t.Fatalf("explicit archive denied %+v", value)
				}
			}
			if err := os.WriteFile(filepath.Join(root, ".gitignore"), []byte(ArchiveDirectory+"/\n!"+ArchiveDirectory+"/secret.txt\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			for _, tool := range []string{ToolFind, ToolSearch} {
				paths, value := ignorePaths(t, w, tool, ArchiveDirectory, &on)
				if len(paths) != 0 || value["complete"] != true {
					t.Fatalf("archive directory bypassed ignored ancestor %+v", value)
				}
				paths, value = ignorePaths(t, w, tool, ArchiveDirectory+"/secret.txt", &on)
				if len(paths) != 1 || value["complete"] != true {
					t.Fatalf("explicit archive file hidden %+v", value)
				}
			}
		})
	}
}

func TestGitignoreBoundedTotalsAndSharedScanBudgets(t *testing.T) {
	for _, kind := range []string{"ignore_files", "ignore_bytes", "file_budget", "byte_budget"} {
		t.Run(kind, func(t *testing.T) {
			files := map[string]string{}
			limits := DefaultLimits()
			limits.SearchMatches = 1000
			wantReason := "gitignore_file_limit"
			switch kind {
			case "ignore_files":
				for i := 0; i <= maxIgnoreFiles; i++ {
					files[fmt.Sprintf("d%03d/.gitignore", i)] = ""
					files[fmt.Sprintf("d%03d/a.txt", i)] = "needle"
				}
			case "ignore_bytes":
				for i := 0; i < 5; i++ {
					files[fmt.Sprintf("d%d/.gitignore", i)] = strings.Repeat("#", maxIgnoreFileBytes-1) + "\n"
					files[fmt.Sprintf("d%d/a.txt", i)] = "needle"
				}
				wantReason = "gitignore_bytes"
			case "file_budget":
				limits.SearchFiles = 1
				files[".gitignore"], files["d/.gitignore"], files["d/a.txt"] = "#comment", "", "needle"
				wantReason = "file_limit"
			case "byte_budget":
				limits.FileBytes, limits.SearchBytes = 16, 16
				files[".gitignore"], files["d/.gitignore"], files["d/a.txt"] = "# 12345678", "# 12345678", "needle"
				wantReason = "scan_bytes"
			}
			w, _ := openSearchOutputFixture(t, files, limits)
			// Use a nonmatching find glob so output limits cannot mask loading limits.
			for tool, raw := range map[string]string{
				ToolFind:   `{"pattern":"no-match","respect_gitignore":true}`,
				ToolSearch: `{"query":"needle","output":"count","glob":"*.txt","respect_gitignore":true}`,
			} {
				value := resultObject(t, w.Execute(t.Context(), tool, raw))
				if value["complete"] != false || value["truncation_reason"] != wantReason {
					t.Fatalf("%s %+v", tool, value)
				}
				if value["ignore_files"].(int) > min(maxIgnoreFiles, limits.SearchFiles) || value["ignore_bytes"].(int64) > min(int64(maxIgnoreBytes), limits.SearchBytes) {
					t.Fatalf("unbounded ignore stats %+v", value)
				}
				if tool == ToolSearch && (value["counts_partial"] != true || int64(value["scanned_bytes"].(int64))+value["ignore_bytes"].(int64) > limits.SearchBytes || value["scanned_files"].(int)+value["ignore_files"].(int) > limits.SearchFiles) {
					t.Fatalf("scan budget widened %+v", value)
				}
			}
		})
	}
}

func TestGitignoreFindDoesNotReadCandidatesAndCancellation(t *testing.T) {
	w, root := openSearchOutputFixture(t, map[string]string{".gitignore": "*.other\n", "huge.txt": strings.Repeat("x", 2<<20), "locked.txt": "private body"}, DefaultLimits())
	if err := os.Chmod(filepath.Join(root, "locked.txt"), 0); err != nil {
		t.Fatal(err)
	}
	on := true
	paths, value := ignorePaths(t, w, ToolFind, ".", &on)
	if !reflect.DeepEqual(paths, []string{"huge.txt", "locked.txt"}) || value["complete"] != true || value["ignore_bytes"] != int64(8) {
		t.Fatalf("find read candidate body %+v", value)
	}
	ctx, cancel := context.WithCancel(t.Context())
	ctx = WithProgressReporter(ctx, func(Progress) { cancel() })
	result := w.Execute(ctx, ToolFind, `{"pattern":"*","respect_gitignore":true}`)
	if resultCode(t, result) != CodeCancelled {
		t.Fatalf("cancel ignored %+v", result)
	}
}
