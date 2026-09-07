package workspace

import (
	"encoding/json"
	"fmt"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/agentlimits"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/modelclient"
)

func Definitions() []modelclient.Tool {
	definitions := []modelclient.Tool{
		workspaceTool(ToolFind, "Find workspace paths (*, ?, **); no content or links.", `{"type":"object","properties":{"path":{"type":"string"},"pattern":{"type":"string","minLength":1,"maxLength":256},"type":{"type":"string","enum":["file","directory","any"]},"limit":{"type":"integer","minimum":1,"maximum":200},"respect_gitignore":{"type":"boolean","default":false}},"required":["pattern"],"additionalProperties":false}`),
		workspaceTool(ToolStat, "Inspect metadata; hash=true reads at most 1MiB, no links.", `{"type":"object","properties":{"path":{"type":"string","minLength":1,"maxLength":4096},"hash":{"type":"boolean"}},"required":["path"],"additionalProperties":false}`),
		workspaceTool(ToolList, "List one workspace directory; no links.", `{"type":"object","properties":{"path":{"type":"string"},"offset":{"type":"integer","minimum":0,"maximum":2000}},"additionalProperties":false}`),
		readDefinition(DefaultLimits()),
		workspaceTool(ToolSearch, "Search bounded workspace UTF-8 text; no links.", `{"type":"object","properties":{"query":{"type":"string","minLength":1,"maxLength":1000},"path":{"type":"string"},"mode":{"type":"string","enum":["literal","regex"]},"case":{"type":"string","enum":["smart","sensitive","insensitive"]},"glob":{"type":"string","minLength":1,"maxLength":256},"respect_gitignore":{"type":"boolean","default":false},"output":{"type":"string","enum":["content","files","count"],"default":"content"},"context":{"type":"integer","minimum":0,"maximum":3,"default":0},"include":{"type":"array","maxItems":16,"items":{"type":"string","minLength":1,"maxLength":256}},"exclude":{"type":"array","maxItems":16,"items":{"type":"string","minLength":1,"maxLength":256}}},"required":["query"],"additionalProperties":false,"anyOf":[{"properties":{"output":{"const":"content"}}},{"properties":{"context":{"const":0}}}]}`),
		workspaceTool(ToolWrite, "Create absent or hash-replace workspace UTF-8 text.", fmt.Sprintf(`{"type":"object","properties":{"path":{"type":"string","minLength":1,"maxLength":4096},"mode":{"type":"string","enum":["create","replace"]},"content":{"type":"string","maxLength":%d},"expected_hash":{"type":"string","pattern":"^sha256:[0-9a-f]{64}$"}},"required":["path","mode","content"],"additionalProperties":false}`, agentlimits.MaxFileMutationArgumentsBytes)),
		workspaceTool(ToolEdit, editDescription(DefaultLimits()), fmt.Sprintf(`{"type":"object","properties":{"path":{"type":"string","minLength":1,"maxLength":4096},"expected_hash":{"type":"string","pattern":"^sha256:[0-9a-f]{64}$"},"edits":{"type":"array","minItems":1,"maxItems":32,"items":{"type":"object","properties":{"old_text":{"type":"string","minLength":1,"maxLength":%d},"new_text":{"type":"string","maxLength":%d}},"required":["old_text","new_text"],"additionalProperties":false}}},"required":["path","expected_hash","edits"],"additionalProperties":false}`, agentlimits.MaxFileMutationArgumentsBytes, agentlimits.MaxFileMutationArgumentsBytes)),
		patchDefinition(DefaultLimits()),
		workspaceTool(ToolMkdir, "Create a workspace directory; parents requires explicit true; no archive or links.", `{"type":"object","properties":{"path":{"type":"string","minLength":1,"maxLength":4096},"parents":{"type":"boolean","default":false}},"required":["path"],"additionalProperties":false}`),
		workspaceTool(ToolCopy, copyDescription(DefaultLimits()), `{"type":"object","properties":{"source":{"type":"string","minLength":1,"maxLength":4096},"destination":{"type":"string","minLength":1,"maxLength":4096},"expected_version":{"type":"string","pattern":"^entry-v1:[0-9a-f]{64}$"}},"required":["source","destination","expected_version"],"additionalProperties":false}`),
		workspaceTool(ToolMove, "Move a stat-versioned file or directory; same-filesystem no-replace, existing parent; no root/archive/links/self-descendants or copy-delete fallback.", `{"type":"object","properties":{"source":{"type":"string","minLength":1,"maxLength":4096},"destination":{"type":"string","minLength":1,"maxLength":4096},"expected_version":{"type":"string","pattern":"^entry-v1:[0-9a-f]{64}$"}},"required":["source","destination","expected_version"],"additionalProperties":false}`),
		workspaceTool(ToolArchive, "Archive a file or directory; never permanently delete.", `{"type":"object","properties":{"path":{"type":"string","minLength":1,"maxLength":4096}},"required":["path"],"additionalProperties":false}`),
	}
	for i := range definitions {
		if isQueryTool(definitions[i].Function.Name) {
			definitions[i] = queryDefinition(definitions[i], DefaultLimits())
		}
	}
	return definitions
}

func (w *Workspace) Definitions() []modelclient.Tool {
	definitions := Definitions()
	if w != nil {
		for index := range definitions {
			switch definitions[index].Function.Name {
			case ToolFind, ToolList, ToolSearch:
				definitions[index] = queryDefinition(definitions[index], w.limits)
			case ToolRead:
				definitions[index] = readDefinition(w.limits)
			case ToolEdit:
				definitions[index].Function.Description = editDescription(w.limits)
			case ToolCopy:
				definitions[index].Function.Description = copyDescription(w.limits)
			case ToolPatch:
				definitions[index] = patchDefinition(w.limits)
			}
		}
	}
	return definitions
}

func patchDefinition(limits Limits) modelclient.Tool {
	return workspaceTool(ToolPatch,
		fmt.Sprintf("Strict Begin/End Patch: Add File (+lines), Update File (bare @@, exact context/-/+), Delete File (archive); optional End of File; no move/no-newline markers. Hashes cover every update/delete only. Max 16 files, original/candidate totals %d bytes each; preflight all, authorize once, publish sequentially without rollback.", limits.PatchBytes),
		fmt.Sprintf(`{"type":"object","properties":{"patch":{"type":"string","minLength":1,"maxLength":%d},"expected_hashes":{"type":"object","maxProperties":16,"additionalProperties":{"type":"string","pattern":"^sha256:[0-9a-f]{64}$"}}},"required":["patch","expected_hashes"],"additionalProperties":false}`, agentlimits.MaxFileMutationArgumentsBytes))
}

func copyDescription(limits Limits) string {
	return fmt.Sprintf("Copy a stat-versioned file (including binary) or recursive directory, up to %d total file bytes/%d entries. Keep source; absent destination, existing parent; no overwrite/merge/archive/links. Authorize frozen plan once; journal each item, stop on failure, retain completed prefix; artifact reads full plan/append-only receipt, never replay.", limits.CopyBytes, limits.CopyEntries)
}

func editDescription(limits Limits) string {
	return fmt.Sprintf("Exact unique non-overlapping edits to one hash; original/candidate up to %d bytes.", limits.EditFileBytes)
}

func readDefinition(limits Limits) modelclient.Tool {
	return workspaceTool(ToolRead,
		fmt.Sprintf("Read UTF-8 up to %d bytes; whole-file hash; line/byte continuation; no links.", limits.ReadFileBytes),
		fmt.Sprintf(`{"type":"object","properties":{"path":{"type":"string","minLength":1,"maxLength":4096},"offset":{"type":"integer","minimum":1,"maximum":%d},"limit":{"type":"integer","minimum":1,"maximum":%d},"byte_offset":{"type":"integer","minimum":0,"maximum":%d},"expected_hash":{"type":"string","pattern":"^sha256:[0-9a-f]{64}$"}},"required":["path"],"additionalProperties":false}`, readOffsetLimit(limits), limits.ReadLines, limits.ReadFileBytes))
}

func workspaceTool(name, description, schema string) modelclient.Tool {
	return modelclient.Tool{Type: "function", Function: modelclient.ToolDefinition{
		Name: name, Description: description, Parameters: json.RawMessage(schema),
	}}
}

func IsReadTool(name string) bool {
	return name == ToolFind || name == ToolStat || name == ToolList || name == ToolRead || name == ToolSearch
}

func IsMutationTool(name string) bool {
	return name == ToolWrite || name == ToolEdit || name == ToolArchive || name == ToolMkdir || name == ToolCopy || name == ToolMove || name == ToolPatch
}
