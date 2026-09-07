package workspace

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestWorkspaceDefinitionsExposeStrictSchemas(t *testing.T) {
	expectedSchemas := map[string]string{
		ToolRestoreArchive: `{"type":"object","properties":{"source":{"type":"string","minLength":1,"maxLength":4096},"destination":{"type":"string","minLength":1,"maxLength":4096},"expected_version":{"type":"string","pattern":"^entry-v1:[0-9a-f]{64}$"}},"required":["source","destination","expected_version"],"additionalProperties":false}`,
		ToolPatch:          `{"type":"object","properties":{"patch":{"type":"string","minLength":1,"maxLength":65536},"expected_hashes":{"type":"object","maxProperties":16,"additionalProperties":{"type":"string","pattern":"^sha256:[0-9a-f]{64}$"}}},"required":["patch","expected_hashes"],"additionalProperties":false}`,
		ToolMove:           `{"type":"object","properties":{"source":{"type":"string","minLength":1,"maxLength":4096},"destination":{"type":"string","minLength":1,"maxLength":4096},"expected_version":{"type":"string","pattern":"^entry-v1:[0-9a-f]{64}$"}},"required":["source","destination","expected_version"],"additionalProperties":false}`,
		ToolCopy:           `{"type":"object","properties":{"source":{"type":"string","minLength":1,"maxLength":4096},"destination":{"type":"string","minLength":1,"maxLength":4096},"expected_version":{"type":"string","pattern":"^entry-v1:[0-9a-f]{64}$"}},"required":["source","destination","expected_version"],"additionalProperties":false}`,
		ToolMkdir:          `{"type":"object","properties":{"path":{"type":"string","minLength":1,"maxLength":4096},"parents":{"type":"boolean","default":false}},"required":["path"],"additionalProperties":false}`,
		ToolFind:           `{"type":"object","properties":{"path":{"type":"string"},"pattern":{"type":"string","minLength":1,"maxLength":256},"type":{"type":"string","enum":["file","directory","any"]},"limit":{"type":"integer","minimum":1,"maximum":200},"respect_gitignore":{"type":"boolean","default":false}},"required":["pattern"],"additionalProperties":false}`,
		ToolStat:           `{"type":"object","properties":{"path":{"type":"string","minLength":1,"maxLength":4096},"hash":{"type":"boolean"}},"required":["path"],"additionalProperties":false}`,
		ToolArchive:        `{"type":"object","properties":{"path":{"type":"string","minLength":1,"maxLength":4096}},"required":["path"],"additionalProperties":false}`,
		ToolList:           `{"type":"object","properties":{"path":{"type":"string"},"offset":{"type":"integer","minimum":0,"maximum":2000}},"additionalProperties":false}`,
		ToolRead:           `{"type":"object","properties":{"path":{"type":"string","minLength":1,"maxLength":4096},"offset":{"type":"integer","minimum":1,"maximum":67108865},"limit":{"type":"integer","minimum":1,"maximum":200},"byte_offset":{"type":"integer","minimum":0,"maximum":67108864},"expected_hash":{"type":"string","pattern":"^sha256:[0-9a-f]{64}$"}},"required":["path"],"additionalProperties":false}`,
		ToolSearch:         `{"type":"object","properties":{"query":{"type":"string","minLength":1,"maxLength":1000},"path":{"type":"string"},"mode":{"type":"string","enum":["literal","regex"]},"case":{"type":"string","enum":["smart","sensitive","insensitive"]},"glob":{"type":"string","minLength":1,"maxLength":256},"respect_gitignore":{"type":"boolean","default":false},"output":{"type":"string","enum":["content","files","count"],"default":"content"},"context":{"type":"integer","minimum":0,"maximum":3,"default":0},"include":{"type":"array","maxItems":16,"items":{"type":"string","minLength":1,"maxLength":256}},"exclude":{"type":"array","maxItems":16,"items":{"type":"string","minLength":1,"maxLength":256}}},"required":["query"],"additionalProperties":false,"anyOf":[{"properties":{"output":{"const":"content"}}},{"properties":{"context":{"const":0}}}]}`,
		ToolWrite:          `{"type":"object","properties":{"path":{"type":"string","minLength":1,"maxLength":4096},"mode":{"type":"string","enum":["create","replace"]},"content":{"type":"string","maxLength":65536},"expected_hash":{"type":"string","pattern":"^sha256:[0-9a-f]{64}$"}},"required":["path","mode","content"],"additionalProperties":false}`,
		ToolEdit:           `{"type":"object","properties":{"path":{"type":"string","minLength":1,"maxLength":4096},"expected_hash":{"type":"string","pattern":"^sha256:[0-9a-f]{64}$"},"edits":{"type":"array","minItems":1,"maxItems":32,"items":{"type":"object","properties":{"old_text":{"type":"string","minLength":1,"maxLength":65536},"new_text":{"type":"string","maxLength":65536}},"required":["old_text","new_text"],"additionalProperties":false}}},"required":["path","expected_hash","edits"],"additionalProperties":false}`,
	}
	expectedDescriptions := map[string]string{
		ToolRestoreArchive: "Restore an exact archive entry using current stat version and explicit absent destination; existing parent, same-filesystem no-replace. Locate with list/find/read/stat. Never infer original path, copy-delete, clean containers or replay.",
		ToolPatch:          "Strict Begin/End Patch: Add File (+lines), Update File (bare @@, exact context/-/+), Delete File (archive); optional End of File; no move/no-newline markers. Hashes cover every update/delete only. Max 16 files, original/candidate totals 67108864 bytes each; preflight all, authorize once, publish sequentially without rollback.",
		ToolMove:           "Move a stat-versioned file or directory; same-filesystem no-replace, existing parent; no root/archive/links/self-descendants or copy-delete fallback.",
		ToolCopy:           "Copy a stat-versioned file (including binary) or recursive directory, up to 1073741824 total file bytes/100000 entries. Keep source; absent destination, existing parent; no overwrite/merge/archive/links. Authorize frozen plan once; journal each item, stop on failure, retain completed prefix; artifact reads full plan/append-only receipt, never replay.",
		ToolMkdir:          "Create a workspace directory; parents requires explicit true; no archive or links.",
		ToolFind:           "Find workspace paths (*, ?, **), no body/links; repeat original parameters with next_cursor. Query retention: 67108864 bytes/100000 units; cursors expire on change, restart or cache reclamation.",
		ToolStat:           "Inspect metadata; hash=true reads at most 1MiB, no links.",
		ToolArchive:        "Archive a file or directory; never permanently delete.",
		ToolList:           "List one safe directory; repeat original parameters with next_cursor for further scan/results. Query retention: 67108864 bytes/100000 units; cursors expire on change, restart or cache reclamation.",
		ToolRead:           "Read UTF-8 up to 67108864 bytes; whole-file hash; line/byte continuation; no links.",
		ToolSearch:         "Search bounded UTF-8 text; repeat original parameters with next_cursor, including in-file matches. Query retention: 67108864 bytes/100000 units; cursors expire on change, restart or cache reclamation.",
		ToolWrite:          "Create absent or hash-replace workspace UTF-8 text.",
		ToolEdit:           "Exact unique non-overlapping edits to one hash; original/candidate up to 67108864 bytes.",
	}

	definitions := Definitions()
	if len(definitions) != len(expectedSchemas) {
		t.Fatalf("definitions=%d want=%d", len(definitions), len(expectedSchemas))
	}
	seen := map[string]bool{}
	for _, definition := range definitions {
		name := definition.Function.Name
		expected, ok := expectedSchemas[name]
		if !ok || seen[name] {
			t.Fatalf("unexpected or duplicate tool %q", name)
		}
		seen[name] = true
		if definition.Type != "function" || definition.Function.Description != expectedDescriptions[name] {
			t.Fatalf("%s definition type=%q description=%q", name, definition.Type, definition.Function.Description)
		}
		var actualSchema, expectedSchema any
		if err := json.Unmarshal(definition.Function.Parameters, &actualSchema); err != nil {
			t.Fatalf("%s actual schema: %v", name, err)
		}
		if err := json.Unmarshal([]byte(expected), &expectedSchema); err != nil {
			t.Fatalf("%s expected schema: %v", name, err)
		}
		if isQueryTool(name) {
			properties := expectedSchema.(map[string]any)["properties"].(map[string]any)
			properties["cursor"] = map[string]any{"type": "string", "minLength": float64(1), "maxLength": float64(96)}
			if name == ToolList {
				properties["offset"].(map[string]any)["maximum"] = float64(100000)
			}
		}
		if !reflect.DeepEqual(actualSchema, expectedSchema) {
			t.Fatalf("%s schema mismatch\nactual:   %s\nexpected: %s", name, definition.Function.Parameters, expected)
		}
	}
	for name := range expectedSchemas {
		if !seen[name] {
			t.Fatalf("missing tool %q", name)
		}
	}
}

func TestWorkspaceAllToolParsersRejectUnknownTrailingAndNonObjectJSON(t *testing.T) {
	workspace, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer workspace.Close()

	valid := map[string]string{
		ToolPatch:   `{"patch":"*** Begin Patch\n*** Add File: new.txt\n+new\n*** End Patch","expected_hashes":{}}`,
		ToolMove:    `{"source":"missing","destination":"moved","expected_version":"entry-v1:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`,
		ToolCopy:    `{"source":"missing","destination":"copy","expected_version":"entry-v1:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`,
		ToolMkdir:   `{"path":"new-dir"}`,
		ToolFind:    `{"pattern":"*.go"}`,
		ToolStat:    `{"path":"missing.txt"}`,
		ToolArchive: `{"path":"missing.txt"}`,
		ToolList:    `{}`,
		ToolRead:    `{"path":"missing.txt"}`,
		ToolSearch:  `{"query":"needle"}`,
		ToolWrite:   `{"path":"new.txt","mode":"create","content":"new"}`,
		ToolEdit:    `{"path":"missing.txt","expected_hash":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","edits":[{"old_text":"old","new_text":"new"}]}`,
	}
	withUnknown := map[string]string{
		ToolPatch:   `{"patch":"*** Begin Patch\n*** Add File: new.txt\n+new\n*** End Patch","expected_hashes":{},"force":true}`,
		ToolMove:    `{"source":"missing","destination":"moved","expected_version":"entry-v1:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","force":true}`,
		ToolCopy:    `{"source":"missing","destination":"copy","expected_version":"entry-v1:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","force":true}`,
		ToolMkdir:   `{"path":"new-dir","unknown":true}`,
		ToolFind:    `{"pattern":"*.go","unknown":true}`,
		ToolStat:    `{"path":"missing.txt","unknown":true}`,
		ToolArchive: `{"path":"missing.txt","unknown":true}`,
		ToolList:    `{"unknown":true}`,
		ToolRead:    `{"path":"missing.txt","unknown":true}`,
		ToolSearch:  `{"query":"needle","unknown":true}`,
		ToolWrite:   `{"path":"new.txt","mode":"create","content":"new","unknown":true}`,
		ToolEdit:    `{"path":"missing.txt","expected_hash":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","edits":[{"old_text":"old","new_text":"new"}],"unknown":true}`,
	}
	for tool, base := range valid {
		t.Run(tool, func(t *testing.T) {
			for _, raw := range []string{"null", withUnknown[tool], base + ` {}`} {
				var result Result
				if IsMutationTool(tool) {
					_, result = workspace.PrepareMutation(t.Context(), tool, raw)
				} else {
					result = workspace.Execute(t.Context(), tool, raw)
				}
				if code := resultCode(t, result); code != CodeInvalidArguments {
					t.Fatalf("raw=%q code=%q value=%+v", raw, code, result.Value)
				}
			}
		})
	}
}

func TestWorkspaceToolSchemaBoundaryContracts(t *testing.T) {
	definitions := Definitions()
	encoded := map[string]string{}
	for _, definition := range definitions {
		encoded[definition.Function.Name] = string(definition.Function.Parameters)
	}
	checks := map[string][]string{
		ToolMove:    {`"required":["source","destination","expected_version"]`, `"pattern":"^entry-v1:[0-9a-f]{64}$"`, `"additionalProperties":false`},
		ToolCopy:    {`"required":["source","destination","expected_version"]`, `"pattern":"^entry-v1:[0-9a-f]{64}$"`, `"additionalProperties":false`},
		ToolMkdir:   {`"parents":{"type":"boolean","default":false}`, `"required":["path"]`, `"additionalProperties":false`},
		ToolFind:    {`"respect_gitignore":{"default":false,"type":"boolean"}`, `"required":["pattern"]`, `"maximum":200`, `"additionalProperties":false`},
		ToolStat:    {`"hash":{"type":"boolean"}`, `"required":["path"]`, `"additionalProperties":false`},
		ToolArchive: {`"path":{"type":"string","minLength":1,"maxLength":4096}`, `"required":["path"]`, `"additionalProperties":false`},
		ToolList:    {`"offset":{"maximum":100000,"minimum":0,"type":"integer"}`, `"cursor":{"maxLength":96,"minLength":1,"type":"string"}`},
		ToolRead:    {`"path":{"type":"string","minLength":1,"maxLength":4096}`, `"offset":{"type":"integer","minimum":1,"maximum":67108865}`, `"limit":{"type":"integer","minimum":1,"maximum":200}`, `"byte_offset":{"type":"integer","minimum":0,"maximum":67108864}`, `"expected_hash":{"type":"string","pattern":"^sha256:[0-9a-f]{64}$"}`},
		ToolSearch:  {`"respect_gitignore":{"default":false,"type":"boolean"}`, `"output":{"default":"content","enum":["content","files","count"],"type":"string"}`, `"context":{"default":0,"maximum":3,"minimum":0,"type":"integer"}`, `"anyOf":[{"properties":{"output":{"const":"content"}}},{"properties":{"context":{"const":0}}}]`, `"query":{"maxLength":1000,"minLength":1,"type":"string"}`, `"mode":{"enum":["literal","regex"],"type":"string"}`, `"case":{"enum":["smart","sensitive","insensitive"],"type":"string"}`, `"include":{"items":`, `"exclude":{"items":`, `"maxItems":16`, `"maxLength":256`},
		ToolWrite:   {`"mode":{"type":"string","enum":["create","replace"]}`, `"content":{"type":"string","maxLength":65536}`, `"expected_hash":{"type":"string","pattern":"^sha256:[0-9a-f]{64}$"}`},
		ToolEdit:    {`"edits":{"type":"array","minItems":1,"maxItems":32`, `"old_text":{"type":"string","minLength":1,"maxLength":65536}`, `"new_text":{"type":"string","maxLength":65536}`, `"additionalProperties":false`},
	}
	for tool, fragments := range checks {
		for _, fragment := range fragments {
			if !strings.Contains(encoded[tool], fragment) {
				t.Fatalf("%s missing contract %s in %s", tool, fragment, encoded[tool])
			}
		}
	}
}
