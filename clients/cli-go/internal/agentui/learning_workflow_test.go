package agentui

import (
	"testing"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/agentcontext"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/agentloop"
)

func TestLateLearningWorkflowCannotNavigateAnotherSession(t *testing.T) {
	m := newModel(t.Context(), &fakeConversation{}, "fixture")
	defer m.cancel()
	m.generation = 2
	m.pendingWorkflow = &agentloop.LearningWorkflow{CallID: "new-call", Kind: "goal"}
	old := agentloop.WorkflowOutcome{Status: "selected", Navigate: &agentcontext.Binding{SpaceID: "10000000-0000-4000-8000-000000000001"}}
	for _, message := range []workflowMessage{{generation: 1, id: "new-call", outcome: old}, {generation: 2, id: "old-call", outcome: old}} {
		next, cmd := m.Update(message)
		m = next.(model)
		if cmd != nil || m.navigation != nil || m.pendingWorkflow.CallID != "new-call" {
			t.Fatal("旧流程污染了新聊天")
		}
	}
	next, _ := m.Update(learningMsg{generation: 1, description: "旧区资料"})
	if next.(model).learningLabel != "" {
		t.Fatal("旧侧栏覆盖了新学习区")
	}
}
