package dashboard

import (
	"reflect"
	"testing"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/workbench"
)

func TestWorkbenchAgentEntryDoesNotStartTeaching(t *testing.T) {
	next, cmd := newModel(Snapshot{LocalState: LocalStatePaired}).Update(workbench.AgentMsg{Space: "space-A", Goal: "goal-A", Session: "teaching-A"})
	want := []string{"agent", "--space", "space-A", "--goal", "goal-A", "--session", "teaching-A"}
	if cmd == nil || !reflect.DeepEqual(next.(model).command, want) {
		t.Fatal("工作台没有启动明确绑定的独立聊天")
	}
}
