package workspace

import (
	"encoding/json"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/modelclient"
)

func Definitions() []modelclient.Tool {
	return []modelclient.Tool{
		workspaceTool(ToolList, "List one workspace directory; no links.", `{"type":"object","properties":{"path":{"type":"string"},"offset":{"type":"integer","minimum":0,"maximum":2000}},"additionalProperties":false}`),
		workspaceTool(ToolRead, "Read bounded workspace UTF-8 text; no links.", `{"type":"object","properties":{"path":{"type":"string","minLength":1,"maxLength":4096},"offset":{"type":"integer","minimum":1,"maximum":1000000},"limit":{"type":"integer","minimum":1,"maximum":200},"byte_offset":{"type":"integer","minimum":0,"maximum":1048576},"expected_hash":{"type":"string","pattern":"^sha256:[0-9a-f]{64}$"}},"required":["path"],"additionalProperties":false}`),
		workspaceTool(ToolSearch, "Search bounded workspace UTF-8 text; no links.", `{"type":"object","properties":{"query":{"type":"string","minLength":1,"maxLength":1000},"path":{"type":"string"},"mode":{"type":"string","enum":["literal","regex"]},"case":{"type":"string","enum":["smart","sensitive","insensitive"]},"include":{"type":"array","maxItems":16,"items":{"type":"string","minLength":1,"maxLength":256}},"exclude":{"type":"array","maxItems":16,"items":{"type":"string","minLength":1,"maxLength":256}}},"required":["query"],"additionalProperties":false}`),
		workspaceTool(ToolWrite, "Create absent or hash-replace workspace UTF-8 text.", `{"type":"object","properties":{"path":{"type":"string","minLength":1,"maxLength":4096},"mode":{"type":"string","enum":["create","replace"]},"content":{"type":"string","maxLength":8192},"expected_hash":{"type":"string","pattern":"^sha256:[0-9a-f]{64}$"}},"required":["path","mode","content"],"additionalProperties":false}`),
		workspaceTool(ToolEdit, "Apply exact unique non-overlapping edits to one hash.", `{"type":"object","properties":{"path":{"type":"string","minLength":1,"maxLength":4096},"expected_hash":{"type":"string","pattern":"^sha256:[0-9a-f]{64}$"},"edits":{"type":"array","minItems":1,"maxItems":32,"items":{"type":"object","properties":{"old_text":{"type":"string","minLength":1,"maxLength":8192},"new_text":{"type":"string","maxLength":8192}},"required":["old_text","new_text"],"additionalProperties":false}}},"required":["path","expected_hash","edits"],"additionalProperties":false}`),
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
	return name == ToolList || name == ToolRead || name == ToolSearch
}

func IsMutationTool(name string) bool {
	return name == ToolWrite || name == ToolEdit || name == ToolArchive
}
