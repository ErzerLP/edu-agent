package agentloop

import (
	"encoding/json"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/modelclient"
)

// Legacy checkpoint references used kind=file for both observations and writes.
// Recognize explicit publication facts without changing the checkpoint schema.
func fileOperationFact(text string) bool {
	var value struct {
		Operation string `json:"operation"`
		Outcome   string `json:"publication_outcome"`
	}
	if json.Unmarshal([]byte(text), &value) != nil || (value.Outcome != "completed" && value.Outcome != "unknown") {
		return false
	}
	switch value.Operation {
	case "write_create", "write_replace", "edit", "archive", "mkdir", "copy", "move":
		return true
	}
	return false
}

// Side-effect rounds outlive raw-history pressure. Their compact tool results
// remain the authoritative local record; a prose excerpt must not erase them.
func groupHasSideEffects(group []modelclient.Message) bool {
	for _, message := range group {
		for _, call := range message.ToolCalls {
			switch call.Function.Name {
			case "write", "edit", "archive", "mkdir", "copy", "move", "remember_preference":
				return true
			}
		}
	}
	return false
}
