package workspace

import (
	"encoding/json"
	"fmt"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/agentlimits"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/modelclient"
)

func Definitions() []modelclient.Tool {
	return []modelclient.Tool{
		workspaceTool(ToolFind, "Find workspace paths (*, ?, **); no content or links.", `{"type":"object","properties":{"path":{"type":"string"},"pattern":{"type":"string","minLength":1,"maxLength":256},"type":{"type":"string","enum":["file","directory","any"]},"limit":{"type":"integer","minimum":1,"maximum":200},"respect_gitignore":{"type":"boolean","default":false}},"required":["pattern"],"additionalProperties":false}`),
		workspaceTool(ToolStat, "Inspect metadata; hash=true reads at most 1MiB, no links.", `{"type":"object","properties":{"path":{"type":"string","minLength":1,"maxLength":4096},"hash":{"type":"boolean"}},"required":["path"],"additionalProperties":false}`),
		workspaceTool(ToolList, "List one workspace directory; no links.", `{"type":"object","properties":{"path":{"type":"string"},"offset":{"type":"integer","minimum":0,"maximum":2000}},"additionalProperties":false}`),
		workspaceTool(ToolRead, "Read bounded workspace UTF-8 text; no links.", `{"type":"object","properties":{"path":{"type":"string","minLength":1,"maxLength":4096},"offset":{"type":"integer","minimum":1,"maximum":1000000},"limit":{"type":"integer","minimum":1,"maximum":200},"byte_offset":{"type":"integer","minimum":0,"maximum":1048576},"expected_hash":{"type":"string","pattern":"^sha256:[0-9a-f]{64}$"}},"required":["path"],"additionalProperties":false}`),
		workspaceTool(ToolSearch, "Search bounded workspace UTF-8 text; no links.", `{"type":"object","properties":{"query":{"type":"string","minLength":1,"maxLength":1000},"path":{"type":"string"},"mode":{"type":"string","enum":["literal","regex"]},"case":{"type":"string","enum":["smart","sensitive","insensitive"]},"glob":{"type":"string","minLength":1,"maxLength":256},"respect_gitignore":{"type":"boolean","default":false},"output":{"type":"string","enum":["content","files","count"],"default":"content"},"context":{"type":"integer","minimum":0,"maximum":3,"default":0},"include":{"type":"array","maxItems":16,"items":{"type":"string","minLength":1,"maxLength":256}},"exclude":{"type":"array","maxItems":16,"items":{"type":"string","minLength":1,"maxLength":256}}},"required":["query"],"additionalProperties":false,"anyOf":[{"properties":{"output":{"const":"content"}}},{"properties":{"context":{"const":0}}}]}`),
		workspaceTool(ToolWrite, "Create absent or hash-replace workspace UTF-8 text.", fmt.Sprintf(`{"type":"object","properties":{"path":{"type":"string","minLength":1,"maxLength":4096},"mode":{"type":"string","enum":["create","replace"]},"content":{"type":"string","maxLength":%d},"expected_hash":{"type":"string","pattern":"^sha256:[0-9a-f]{64}$"}},"required":["path","mode","content"],"additionalProperties":false}`, agentlimits.MaxFileMutationArgumentsBytes)),
		workspaceTool(ToolEdit, "Apply exact unique non-overlapping edits to one hash.", fmt.Sprintf(`{"type":"object","properties":{"path":{"type":"string","minLength":1,"maxLength":4096},"expected_hash":{"type":"string","pattern":"^sha256:[0-9a-f]{64}$"},"edits":{"type":"array","minItems":1,"maxItems":32,"items":{"type":"object","properties":{"old_text":{"type":"string","minLength":1,"maxLength":%d},"new_text":{"type":"string","maxLength":%d}},"required":["old_text","new_text"],"additionalProperties":false}}},"required":["path","expected_hash","edits"],"additionalProperties":false}`, agentlimits.MaxFileMutationArgumentsBytes, agentlimits.MaxFileMutationArgumentsBytes)),
		workspaceTool(ToolMkdir, "Create a workspace directory; parents requires explicit true; no archive or links.", `{"type":"object","properties":{"path":{"type":"string","minLength":1,"maxLength":4096},"parents":{"type":"boolean","default":false}},"required":["path"],"additionalProperties":false}`),
		workspaceTool(ToolCopy, "Stream-copy a stat-versioned regular file up to 32MiB, including binary; keep source; absent destination, existing parent; no archive or links.", `{"type":"object","properties":{"source":{"type":"string","minLength":1,"maxLength":4096},"destination":{"type":"string","minLength":1,"maxLength":4096},"expected_version":{"type":"string","pattern":"^entry-v1:[0-9a-f]{64}$"}},"required":["source","destination","expected_version"],"additionalProperties":false}`),
		workspaceTool(ToolMove, "Move a stat-versioned file or directory; same-filesystem no-replace, existing parent; no root/archive/links/self-descendants or copy-delete fallback.", `{"type":"object","properties":{"source":{"type":"string","minLength":1,"maxLength":4096},"destination":{"type":"string","minLength":1,"maxLength":4096},"expected_version":{"type":"string","pattern":"^entry-v1:[0-9a-f]{64}$"}},"required":["source","destination","expected_version"],"additionalProperties":false}`),
		workspaceTool(ToolArchive, "Archive a file or directory; never permanently delete.", `{"type":"object","properties":{"path":{"type":"string","minLength":1,"maxLength":4096}},"required":["path"],"additionalProperties":false}`),
	}
}

func (w *Workspace) Definitions() []modelclient.Tool { return Definitions() }

func workspaceTool(name, description, schema string) modelclient.Tool {
	return modelclient.Tool{Type: "function", Function: modelclient.ToolDefinition{
		Name: name, Description: description, Parameters: json.RawMessage(schema),
	}}
}

func IsReadTool(name string) bool {
	return name == ToolFind || name == ToolStat || name == ToolList || name == ToolRead || name == ToolSearch
}

func IsMutationTool(name string) bool {
	return name == ToolWrite || name == ToolEdit || name == ToolArchive || name == ToolMkdir || name == ToolCopy || name == ToolMove
}
