package mentorrun

import (
	"github.com/edu-agent/edu-agent/packages/agentcore/modelclient"
	"testing"
)

func TestWebCallsCannotReplayIDsOrAcquireOtherTools(t *testing.T) {
	call := modelclient.ToolCall{ID: "search-1", Type: "function", Function: modelclient.ToolFunction{Name: "search_web", Arguments: `{"query":"公开资料"}`}}
	history := []modelclient.Message{{Role: "assistant", ToolCalls: []modelclient.ToolCall{call}}, {Role: "tool", ToolCallID: call.ID, Content: `{"items":[]}`}}
	if validateWebCalls(history, []modelclient.ToolCall{call}) == nil {
		t.Fatal("replayed call ID accepted")
	}
	call.ID = "new-call"
	call.Function.Name = "write_knowledge"
	if validateWebCalls(history, []modelclient.ToolCall{call}) == nil {
		t.Fatal("page acquired knowledge-writing authority")
	}
	call.Function.Name = "read_web"
	if err := validateWebCalls(history, []modelclient.ToolCall{call}); err != nil {
		t.Fatal(err)
	}
	if validateWebCalls(nil, []modelclient.ToolCall{call, call}) == nil {
		t.Fatal("duplicate IDs within one response accepted")
	}
}
