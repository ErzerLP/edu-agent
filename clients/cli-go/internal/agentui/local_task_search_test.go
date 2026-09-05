package agentui

import (
	"context"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/localexec"
)

type taskSearchRequest struct {
	taskID, stream, needle string
	offset                 int64
	limit                  int
}
type taskSearchConversation struct {
	*taskPanelConversation
	queries []taskSearchRequest
}

func (c *taskSearchConversation) SearchLocalTask(_ context.Context, id, stream, needle string, offset int64, limit int) (localexec.SearchPage, error) {
	c.queries = append(c.queries, taskSearchRequest{id, stream, needle, offset, limit})
	match := int64(17)
	if offset > 0 {
		match = 31
	}
	return localexec.SearchPage{Offset: offset, Offsets: []int64{match}, NextOffset: match + 1, More: true, Received: 9040, Retained: 9040}, nil
}
func (c *taskSearchConversation) ReadLocalTask(id, stream string, offset int64, limit int) (localexec.OutputPage, error) {
	page, err := c.taskPanelConversation.ReadLocalTask(id, stream, offset, limit)
	page.Availability, page.Historical, page.Saved = "saved", true, page.Retained
	return page, err
}
func (c *taskSearchConversation) LocalOutputStatus() string { return "output_corrupt" }

func taskSearchRune(m model, text string) (model, tea.Cmd) {
	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(text)})
	return updated.(model), cmd
}
func TestLocalOutputTaskPanelSearchIsIndependentOfReadAndModel(t *testing.T) {
	m, base := taskPanelTestModel(t)
	conversation := &taskSearchConversation{taskPanelConversation: base}
	m.session, m.busy = conversation, true
	base.tasks[0].Persistence, base.tasks[0].Restored, base.tasks[0].State, base.tasks[0].Controllable = "saved", true, "exited", false
	base.tasks[0].StdoutSaved = 9040
	m, cmd := taskPanelKey(m, tea.KeyF5)
	m = taskPanelApply(t, m, cmd)
	m, cmd = taskPanelKey(m, tea.KeyPgDown)
	m = taskPanelApply(t, m, cmd)
	m, _ = taskSearchRune(m, "/")
	m, _ = taskSearchRune(m, "output")
	m, cmd = taskPanelKey(m, tea.KeyEnter)
	m = taskPanelApply(t, m, cmd)
	if len(conversation.queries) != 1 || conversation.queries[0].offset != 0 || conversation.queries[0].needle != "output" || conversation.queries[0].limit != 1 || m.taskPanel.offsets["task_a/stdout"] != 17 {
		t.Fatalf("query/page cursor mixed: %+v %+v", conversation.queries, m.taskPanel.offsets)
	}
	m = taskPanelApply(t, m, m.refreshLocalTasks())
	for _, text := range []string{"历史结算/输出", "输出保存", "已加密保存", "命中字节 17", "历史目录不可完整访问"} {
		if !strings.Contains(m.View(), text) {
			t.Fatalf("missing %q in %s", text, m.View())
		}
	}
	m, cmd = taskSearchRune(m, "n")
	m = taskPanelApply(t, m, cmd)
	if conversation.queries[1].offset != 18 || m.taskPanel.offsets["task_a/stdout"] != 31 || !m.busy || conversation.sent != "" {
		t.Fatal("next match used page cursor or entered the model")
	}
}

func TestLocalOutputTaskPanelRejectsLateSearchAndBoundsQuery(t *testing.T) {
	m, base := taskPanelTestModel(t)
	conversation := &taskSearchConversation{taskPanelConversation: base}
	m.session = conversation
	m, cmd := taskPanelKey(m, tea.KeyF5)
	m = taskPanelApply(t, m, cmd)
	m, _ = taskSearchRune(m, "/")
	m, _ = taskSearchRune(m, "\x1b\n\u202e"+strings.Repeat("查", 400))
	if len(m.taskPanel.query) > 512 || strings.ContainsAny(m.taskPanel.query, "\x1b\n\u202e") {
		t.Fatal("unsafe or unbounded query")
	}
	m, stale := taskPanelKey(m, tea.KeyEnter)
	m, _ = taskPanelKey(m, tea.KeyEsc)
	m, cmd = taskPanelKey(m, tea.KeyF5)
	m = taskPanelApply(t, m, cmd)
	m = taskPanelApply(t, m, stale)
	if m.taskPanel.searchNext != 0 || m.taskPanel.offsets["task_a/stdout"] != 0 {
		t.Fatal("old query jumped new panel")
	}
	m, _ = taskSearchRune(m, "/")
	m, _ = taskPanelKey(m, tea.KeyF5)
	if m.taskPanel != nil || m.ctx.Err() != nil {
		t.Fatal("F5 did not close query without closing client")
	}
}
