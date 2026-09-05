package agentui

import (
	"context"
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/localexec"
)

type taskPanelConversation struct {
	*fakeConversation
	tasks   []localexec.Snapshot
	reads   []taskPanelRead
	stopped string
	stopErr error
}

type taskPanelRead struct {
	id, stream string
	offset     int64
	limit      int
}

func (c *taskPanelConversation) LocalExecutionAvailable() bool { return true }
func (c *taskPanelConversation) LocalTasks() ([]localexec.Snapshot, error) {
	return append([]localexec.Snapshot(nil), c.tasks...), nil
}
func (c *taskPanelConversation) ReadLocalTask(id, stream string, offset int64, limit int) (localexec.OutputPage, error) {
	c.reads = append(c.reads, taskPanelRead{id, stream, offset, limit})
	data := []byte("output\x1b]52;clipboard\a\x1b[2J\u202e\n" + strings.Repeat("x", 9000))
	if stream == "stderr" {
		data = []byte("diagnostic")
	}
	if offset > int64(len(data)) {
		return localexec.OutputPage{}, &localexec.Error{Code: "invalid_offset"}
	}
	end := min(offset+int64(limit), int64(len(data)))
	return localexec.OutputPage{Data: data[offset:end], Offset: offset, NextOffset: end, Received: int64(len(data)), Retained: int64(len(data)), More: end < int64(len(data))}, nil
}
func (c *taskPanelConversation) StopLocalTask(_ context.Context, id string) (localexec.Snapshot, error) {
	c.stopped = id
	return c.tasks[0], c.stopErr
}

func taskPanelTestModel(t *testing.T) (model, *taskPanelConversation) {
	t.Helper()
	conversation := &taskPanelConversation{fakeConversation: &fakeConversation{}, tasks: []localexec.Snapshot{{TaskID: "task_a", State: "running", Controllable: true}, {TaskID: "task_b", State: "running", Controllable: true}}}
	m := newModel(t.Context(), conversation, "model")
	t.Cleanup(m.cancel)
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 96, Height: 30})
	return updated.(model), conversation
}

func taskPanelKey(m model, key tea.KeyType) (model, tea.Cmd) {
	updated, cmd := m.Update(tea.KeyMsg{Type: key})
	return updated.(model), cmd
}

func taskPanelApply(t *testing.T, m model, cmd tea.Cmd) model {
	t.Helper()
	if cmd == nil {
		t.Fatal("missing task command")
	}
	updated, _ := m.Update(cmd())
	return updated.(model)
}

func TestLocalExecutionTaskPanelUnavailableHistoryIsExplicit(t *testing.T) {
	m, conversation := taskPanelTestModel(t)
	conversation.tasks = nil
	m, cmd := taskPanelKey(m, tea.KeyF5)
	m = taskPanelApply(t, m, cmd)
	for _, expected := range []string{"当前进程无可访问任务", "历史状态不代表当前状态", "旧任务控制/输出不可用", "不自动重跑"} {
		if !strings.Contains(m.View(), expected) {
			t.Fatalf("empty task view missing %q", expected)
		}
	}
}

func TestLocalExecutionTaskPanelWorksWhileModelBusy(t *testing.T) {
	m, conversation := taskPanelTestModel(t)
	m.busy, m.activeCancelable = true, true
	m, cmd := taskPanelKey(m, tea.KeyF5)
	m = taskPanelApply(t, m, cmd)
	if m.taskPanel == nil || m.taskPanel.taskID != "task_a" || len(conversation.reads) != 1 || !m.busy || conversation.sent != "" {
		t.Fatalf("panel did not bypass the model: %+v reads=%+v", m.taskPanel, conversation.reads)
	}
	view := m.View()
	for _, expected := range []string{"本地任务", "running", "stdout", "已保留", "退出后不可恢复"} {
		if !strings.Contains(view, expected) {
			t.Fatalf("missing %q in %s", expected, view)
		}
	}
	for _, unsafe := range []string{"\x1b]52", "\x1b[2J", "\u202e"} {
		if strings.Contains(view, unsafe) {
			t.Fatalf("unsafe terminal output %q", unsafe)
		}
	}
	conversation.stopErr = errors.New("raw /private/path command secret")
	updated, stop := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("s")})
	m = taskPanelApply(t, updated.(model), stop)
	if conversation.stopped != "task_a" || !m.busy || m.ctx.Err() != nil || conversation.sent != "" || m.taskPanel.stopping {
		t.Fatal("direct stop changed the model lifetime or failed to settle")
	}
	if strings.Contains(m.View(), "secret") || !strings.Contains(m.View(), "不要据此判断进程已结束") {
		t.Fatal("stop error leaked OS detail or claimed task completion")
	}
	m, _ = taskPanelKey(m, tea.KeyEsc)
	if m.taskPanel != nil || m.ctx.Err() != nil || !m.busy {
		t.Fatal("closing panel exited/canceled the model")
	}
}

func TestLocalExecutionTaskPanelIndependentBytePagesAndStaleReplies(t *testing.T) {
	m, conversation := taskPanelTestModel(t)
	m, cmd := taskPanelKey(m, tea.KeyF5)
	m = taskPanelApply(t, m, cmd)
	m, oldPage := taskPanelKey(m, tea.KeyPgDown)
	if m.taskPanel.offsets["task_a/stdout"] != 4096 {
		t.Fatal("page is not raw-byte based")
	}
	m, stderrPage := taskPanelKey(m, tea.KeyTab)
	if len(m.taskPanel.page.Data) != 0 || m.taskPanel.page.NextOffset != 0 {
		t.Fatal("old stream body/cursor remained visible")
	}
	m = taskPanelApply(t, m, oldPage)
	if len(m.taskPanel.page.Data) != 0 {
		t.Fatal("stale stdout reply contaminated stderr")
	}
	m = taskPanelApply(t, m, stderrPage)
	if string(m.taskPanel.page.Data) != "diagnostic" || m.taskPanel.page.Offset != 0 {
		t.Fatal("stderr skipped content")
	}
	m, cmd = taskPanelKey(m, tea.KeyTab)
	m = taskPanelApply(t, m, cmd)
	if m.taskPanel.page.Offset != 4096 {
		t.Fatal("stream cursor not independent")
	}
	m, cmd = taskPanelKey(m, tea.KeyDown)
	m = taskPanelApply(t, m, cmd)
	last := conversation.reads[len(conversation.reads)-1]
	if last.id != "task_b" || last.offset != 0 || last.limit != 4096 {
		t.Fatalf("task cursor contaminated: %+v", last)
	}
	m, cmd = taskPanelKey(m, tea.KeyUp)
	m = taskPanelApply(t, m, cmd)
	if m.taskPanel.page.Offset != 4096 {
		t.Fatal("task cursor not preserved")
	}
	m, cmd = taskPanelKey(m, tea.KeyHome)
	m = taskPanelApply(t, m, cmd)
	if m.taskPanel.page.Offset != 0 {
		t.Fatal("Home did not reset current cursor")
	}
}

func TestLocalExecutionTaskPanelRejectsClosedPanelAndOldSessionMessages(t *testing.T) {
	m, _ := taskPanelTestModel(t)
	m, old := taskPanelKey(m, tea.KeyF5)
	m, _ = taskPanelKey(m, tea.KeyEsc)
	m, current := taskPanelKey(m, tea.KeyF5)
	m = taskPanelApply(t, m, old)
	if len(m.taskPanel.tasks) != 0 {
		t.Fatal("closed panel result accepted")
	}
	message := current().(localTaskMsg)
	message.generation = m.generation + 1
	updated, _ := m.Update(message)
	m = updated.(model)
	if len(m.taskPanel.tasks) != 0 {
		t.Fatal("different Session result accepted")
	}
	m = taskPanelApply(t, m, current)
	updated, _ = m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = updated.(model)
	if m.taskPanel.output.Width != 76 {
		t.Fatal("panel did not resize")
	}
	m, _ = taskPanelKey(m, tea.KeyCtrlC)
	if m.ctx.Err() == nil {
		t.Fatal("global exit did not cancel UI")
	}
}
