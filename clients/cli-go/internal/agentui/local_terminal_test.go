package agentui

import (
	"context"
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/localexec"
)

type terminalConversation struct {
	*taskPanelConversation
	content                      []byte
	interrupts, eofs, rows, cols int
	inputErr                     error
}

func (c *terminalConversation) InputLocalTask(_ context.Context, _ string, content []byte) (localexec.InputResult, error) {
	c.content = append([]byte(nil), content...)
	if c.inputErr != nil {
		return localexec.InputResult{Written: 2, Outcome: "partial"}, c.inputErr
	}
	return localexec.InputResult{Written: len(content), Outcome: "written"}, nil
}
func (c *terminalConversation) InterruptLocalTask(context.Context, string) (localexec.InputResult, error) {
	c.interrupts++
	return localexec.InputResult{Written: 1, Outcome: "written"}, nil
}
func (c *terminalConversation) EOFLocalTask(context.Context, string) (localexec.InputResult, error) {
	c.eofs++
	return localexec.InputResult{Written: 1, Outcome: "written"}, nil
}
func (c *terminalConversation) ResizeLocalTask(_ string, rows, cols int) (localexec.Snapshot, error) {
	c.rows, c.cols = rows, cols
	return c.tasks[0], nil
}

func applyTerminalCommand(t *testing.T, m model, cmd tea.Cmd) model {
	t.Helper()
	if cmd == nil {
		t.Fatal("missing terminal command")
	}
	msg := cmd()
	if batch, ok := msg.(tea.BatchMsg); ok {
		for _, child := range batch {
			if child != nil {
				m = applyTerminalCommand(t, m, child)
			}
		}
		return m
	}
	updated, _ := m.Update(msg)
	return updated.(model)
}
func terminalTestModel(t *testing.T) (model, *terminalConversation) {
	m, base := taskPanelTestModel(t)
	base.tasks[0].PTY, base.tasks[0].Rows, base.tasks[0].Cols = true, 24, 80
	c := &terminalConversation{taskPanelConversation: base}
	m.session, m.busy = c, true
	m, cmd := taskPanelKey(m, tea.KeyF5)
	m = applyTerminalCommand(t, m, cmd)
	return m, c
}
func TestPTYTaskPanelDirectInputControlsAndResize(t *testing.T) {
	m, c := terminalTestModel(t)
	m, _ = taskPanelKey(m, tea.KeyTab)
	if m.taskPanel.stream != "stdout" || !strings.Contains(m.taskPanel.notice, "已合并") {
		t.Fatal("PTY advertised independent stderr")
	}
	m, cmd := taskSearchRune(m, "i")
	m = applyTerminalCommand(t, m, cmd)
	if !m.taskPanel.terminalMode || c.cols != 92 || c.rows != 16 {
		t.Fatal("input mode/initial resize missing")
	}
	m, _ = taskSearchRune(m, "DIRECT_SECRET_901")
	if strings.Contains(m.View(), "DIRECT_SECRET_901") {
		t.Fatal("terminal draft echoed into view")
	}
	m, cmd = taskPanelKey(m, tea.KeyEnter)
	m = applyTerminalCommand(t, m, cmd)
	if string(c.content) != "DIRECT_SECRET_901\n" || c.sent != "" || !m.busy || m.taskPanel.terminalDraft != "" {
		t.Fatal("input was lost or routed through model")
	}
	m, cmd = taskPanelKey(m, tea.KeyCtrlC)
	m = applyTerminalCommand(t, m, cmd)
	if c.interrupts != 1 || m.ctx.Err() != nil {
		t.Fatal("PTY Ctrl+C exited client")
	}
	m, cmd = taskPanelKey(m, tea.KeyCtrlD)
	m = applyTerminalCommand(t, m, cmd)
	if c.eofs != 1 {
		t.Fatal("missing terminal EOF")
	}
	updated, cmd := m.Update(tea.WindowSizeMsg{Width: 110, Height: 40})
	m = applyTerminalCommand(t, updated.(model), cmd)
	if c.rows != 26 || c.cols != 106 {
		t.Fatal("window resize not delivered")
	}
	m, cmd = taskPanelKey(m, tea.KeyCtrlQ)
	if m.ctx.Err() == nil || cmd == nil {
		t.Fatal("Ctrl+Q no longer exits")
	}
}
func TestPTYTaskPanelPartialInputAndLateReplyAreNotReplayed(t *testing.T) {
	m, c := terminalTestModel(t)
	m, cmd := taskSearchRune(m, "i")
	m = applyTerminalCommand(t, m, cmd)
	c.inputErr = errors.New("private raw failure")
	m, _ = taskSearchRune(m, "abc")
	m, cmd = taskPanelKey(m, tea.KeyEnter)
	m = applyTerminalCommand(t, m, cmd)
	if !strings.Contains(m.taskPanel.notice, "接受2字节") || !strings.Contains(m.taskPanel.notice, "partial") || strings.Contains(m.View(), "private raw") || m.taskPanel.terminalDraft != "" {
		t.Fatal("partial outcome hidden or queued for replay")
	}
	m, _ = taskSearchRune(m, "safe")
	m, _ = taskSearchRune(m, "\necho unsafe")
	if m.taskPanel.terminalDraft != "safe" {
		t.Fatal("control paste silently rewritten")
	}
	m, late := taskPanelKey(m, tea.KeyEnter)
	m, _ = taskPanelKey(m, tea.KeyF5)
	m, cmd = taskPanelKey(m, tea.KeyF5)
	m = applyTerminalCommand(t, m, cmd)
	m = applyTerminalCommand(t, m, late)
	if m.taskPanel.terminalMode || m.taskPanel.terminalBusy || strings.Contains(m.taskPanel.notice, "partial") {
		t.Fatal("old terminal reply entered new panel")
	}
}
func TestPTYTaskPanelDoesNotAttachHistoricalOrPipe(t *testing.T) {
	m, c := terminalTestModel(t)
	c.tasks[0].Controllable = false
	m = applyTerminalCommand(t, m, m.refreshLocalTasks())
	m, cmd := taskSearchRune(m, "i")
	if cmd != nil || m.taskPanel.terminalMode {
		t.Fatal("attached retired task")
	}
	m, cmd = taskPanelKey(m, tea.KeyDown)
	m = applyTerminalCommand(t, m, cmd)
	m, cmd = taskSearchRune(m, "i")
	if cmd != nil || m.taskPanel.terminalMode {
		t.Fatal("attached pipe task")
	}
}
