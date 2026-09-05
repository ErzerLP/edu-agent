package workspace

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestWorkspaceDefinitionsExposeStrictSchemas(t *testing.T) {
	expectedSchemas := map[string]string{
		ToolArchive: `{"type":"object","properties":{"path":{"type":"string","minLength":1,"maxLength":4096}},"required":["path"],"additionalProperties":false}`,
		ToolList:    `{"type":"object","properties":{"path":{"type":"string"},"offset":{"type":"integer","minimum":0,"maximum":2000}},"additionalProperties":false}`,
		ToolRead:    `{"type":"object","properties":{"path":{"type":"string","minLength":1,"maxLength":4096},"offset":{"type":"integer","minimum":1,"maximum":1000000},"limit":{"type":"integer","minimum":1,"maximum":200},"byte_offset":{"type":"integer","minimum":0,"maximum":1048576},"expected_hash":{"type":"string","pattern":"^sha256:[0-9a-f]{64}$"}},"required":["path"],"additionalProperties":false}`,
		ToolSearch:  `{"type":"object","properties":{"query":{"type":"string","minLength":1,"maxLength":1000},"path":{"type":"string"},"mode":{"type":"string","enum":["literal","regex"]},"case":{"type":"string","enum":["smart","sensitive","insensitive"]},"include":{"type":"array","maxItems":16,"items":{"type":"string","minLength":1,"maxLength":256}},"exclude":{"type":"array","maxItems":16,"items":{"type":"string","minLength":1,"maxLength":256}}},"required":["query"],"additionalProperties":false}`,
		ToolWrite:   `{"type":"object","properties":{"path":{"type":"string","minLength":1,"maxLength":4096},"mode":{"type":"string","enum":["create","replace"]},"content":{"type":"string","maxLength":8192},"expected_hash":{"type":"string","pattern":"^sha256:[0-9a-f]{64}$"}},"required":["path","mode","content"],"additionalProperties":false}`,
		ToolEdit:    `{"type":"object","properties":{"path":{"type":"string","minLength":1,"maxLength":4096},"expected_hash":{"type":"string","pattern":"^sha256:[0-9a-f]{64}$"},"edits":{"type":"array","minItems":1,"maxItems":32,"items":{"type":"object","properties":{"old_text":{"type":"string","minLength":1,"maxLength":8192},"new_text":{"type":"string","maxLength":8192}},"required":["old_text","new_text"],"additionalProperties":false}}},"required":["path","expected_hash","edits"],"additionalProperties":false}`,
	}
	expectedDescriptions := map[string]string{
		ToolArchive: "Archive a file or directory; never permanently delete.",
		ToolList:    "List one workspace directory; no links.",
		ToolRead:    "Read bounded workspace UTF-8 text; no links.",
		ToolSearch:  "Search bounded workspace UTF-8 text; no links.",
		ToolWrite:   "Create absent or hash-replace workspace UTF-8 text.",
		ToolEdit:    "Apply exact unique non-overlapping edits to one hash.",
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
		ToolArchive: `{"path":"missing.txt"}`,
		ToolList:    `{}`,
		ToolRead:    `{"path":"missing.txt"}`,
		ToolSearch:  `{"query":"needle"}`,
		ToolWrite:   `{"path":"new.txt","mode":"create","content":"new"}`,
		ToolEdit:    `{"path":"missing.txt","expected_hash":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","edits":[{"old_text":"old","new_text":"new"}]}`,
	}
	withUnknown := map[string]string{
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
		ToolArchive: {`"path":{"type":"string","minLength":1,"maxLength":4096}`, `"required":["path"]`, `"additionalProperties":false`},
		ToolList:    {`"offset":{"type":"integer","minimum":0,"maximum":2000}`},
		ToolRead:    {`"path":{"type":"string","minLength":1,"maxLength":4096}`, `"offset":{"type":"integer","minimum":1,"maximum":1000000}`, `"limit":{"type":"integer","minimum":1,"maximum":200}`, `"byte_offset":{"type":"integer","minimum":0,"maximum":1048576}`, `"expected_hash":{"type":"string","pattern":"^sha256:[0-9a-f]{64}$"}`},
		ToolSearch:  {`"query":{"type":"string","minLength":1,"maxLength":1000}`, `"mode":{"type":"string","enum":["literal","regex"]}`, `"case":{"type":"string","enum":["smart","sensitive","insensitive"]}`, `"include":{"type":"array","maxItems":16`, `"exclude":{"type":"array","maxItems":16`, `"maxLength":256`},
		ToolWrite:   {`"mode":{"type":"string","enum":["create","replace"]}`, `"content":{"type":"string","maxLength":8192}`, `"expected_hash":{"type":"string","pattern":"^sha256:[0-9a-f]{64}$"}`},
		ToolEdit:    {`"edits":{"type":"array","minItems":1,"maxItems":32`, `"old_text":{"type":"string","minLength":1,"maxLength":8192}`, `"new_text":{"type":"string","maxLength":8192}`, `"additionalProperties":false`},
	}
	for tool, fragments := range checks {
		for _, fragment := range fragments {
			if !strings.Contains(encoded[tool], fragment) {
				t.Fatalf("%s missing contract %s in %s", tool, fragment, encoded[tool])
			}
		}
	}
}
