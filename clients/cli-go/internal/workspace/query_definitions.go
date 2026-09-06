package workspace

import (
	"encoding/json"
	"fmt"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/modelclient"
)

func isQueryTool(tool string) bool {
	return tool == ToolList || tool == ToolFind || tool == ToolSearch
}

func queryDefinition(tool modelclient.Tool, limits Limits) modelclient.Tool {
	// These schemas are generated locally from constants, never provider JSON.
	var schema map[string]any
	_ = json.Unmarshal(tool.Function.Parameters, &schema)
	properties := schema["properties"].(map[string]any)
	properties["cursor"] = map[string]any{"type": "string", "minLength": 1, "maxLength": 96}
	switch tool.Function.Name {
	case ToolList:
		properties["offset"].(map[string]any)["maximum"] = limits.QueryEntries
		tool.Function.Description = "List one safe directory; repeat original parameters with next_cursor for further scan/results."
	case ToolFind:
		tool.Function.Description = "Find workspace paths (*, ?, **), no body/links; repeat original parameters with next_cursor."
	case ToolSearch:
		tool.Function.Description = "Search bounded UTF-8 text; repeat original parameters with next_cursor, including in-file matches."
	}
	tool.Function.Description += fmt.Sprintf(" Query retention: %d bytes/%d units; cursors expire on change, restart or cache reclamation.", limits.QueryMemoryBytes, limits.QueryEntries)
	tool.Function.Parameters, _ = json.Marshal(schema)
	return tool
}
