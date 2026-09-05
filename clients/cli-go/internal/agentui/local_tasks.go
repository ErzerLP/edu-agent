package agentui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/localexec"
)

type localTaskSource interface {
	LocalExecutionAvailable() bool
	LocalTasks() ([]localexec.Snapshot, error)
	ReadLocalTask(string, string, int64, int) (localexec.OutputPage, error)
	StopLocalTask(context.Context, string) (localexec.Snapshot, error)
}

type localTaskPanel struct {
	epoch                 uint64
	tasks                 []localexec.Snapshot
	taskID                string
	stream                string
	offsets               map[string]int64
	page                  localexec.OutputPage
	output                viewport.Model
	notice                string
	stopping              bool
	query                 string
	queryMode, searchBusy bool
	searchNext            int64
	historyError          string
}

type localTaskMsg struct {
	generation, epoch uint64
	tasks             []localexec.Snapshot
	taskID, stream    string
	page              localexec.OutputPage
	err               error
	stopped           bool
	historyError      string
}

type localTaskTick struct{ generation, epoch uint64 }

func newLocalTaskPanel(epoch uint64, width, height int) *localTaskPanel {
	p := &localTaskPanel{epoch: epoch, stream: "stdout", offsets: make(map[string]int64), output: viewport.New(max(20, width-4), max(3, height-12))}
	return p
}

func (p *localTaskPanel) offsetKey() string { return p.taskID + "/" + p.stream }

func (p *localTaskPanel) setPage(page localexec.OutputPage, width, height int) {
	p.page = page
	p.output.Width, p.output.Height = max(20, width-4), max(3, height-12)
	// Presentation is sanitized and wrapped; navigation still uses raw byte
	// positions, independently of the model's read cursors.
	content := strings.Join(wrapDisplayLines(safeTerminalText(string(page.Data)), p.output.Width, 8192), "\n")
	p.output.SetContent(content)
}

func localTaskLoadCmd(ctx context.Context, source localTaskSource, generation, epoch uint64, taskID, stream string, offset int64) tea.Cmd {
	return func() tea.Msg {
		message := localTaskMsg{generation: generation, epoch: epoch, taskID: taskID, stream: stream}
		if ctx.Err() != nil {
			message.err = ctx.Err()
			return message
		}
		if status, ok := source.(interface{ LocalOutputStatus() string }); ok {
			message.historyError = status.LocalOutputStatus()
		}
		message.tasks, message.err = source.LocalTasks()
		if message.err != nil {
			return message
		}
		found := false
		for _, task := range message.tasks {
			if task.TaskID == taskID {
				found = true
				break
			}
		}
		if !found && len(message.tasks) > 0 {
			message.taskID = message.tasks[0].TaskID
			offset = 0
		}
		if len(message.tasks) > 0 {
			message.page, message.err = source.ReadLocalTask(message.taskID, stream, offset, 4096)
		}
		return message
	}
}

func localTaskTickCmd(generation, epoch uint64) tea.Cmd {
	return tea.Tick(300*time.Millisecond, func(time.Time) tea.Msg { return localTaskTick{generation, epoch} })
}

func localTaskStopCmd(ctx context.Context, source localTaskSource, generation, epoch uint64, taskID string) tea.Cmd {
	return func() tea.Msg {
		stopCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		_, err := source.StopLocalTask(stopCtx, taskID)
		return localTaskMsg{generation: generation, epoch: epoch, taskID: taskID, err: err, stopped: true}
	}
}

func (m model) refreshLocalTasks() tea.Cmd {
	source, ok := m.session.(localTaskSource)
	if !ok || m.taskPanel == nil {
		return nil
	}
	p := m.taskPanel
	return localTaskLoadCmd(m.ctx, source, m.generation, p.epoch, p.taskID, p.stream, p.offsets[p.offsetKey()])
}

func (m model) handleLocalTaskMessage(msg localTaskMsg) (tea.Model, tea.Cmd) {
	p := m.taskPanel
	if p == nil || msg.generation != m.generation || msg.epoch != p.epoch {
		return m, nil
	}
	if msg.stopped {
		p.stopping = false
		p.notice = "停止请求已处理；请核对任务实际状态"
		if msg.err != nil {
			p.notice = localTaskError(msg.err)
		}
		return m, m.refreshLocalTasks()
	}
	p.historyError = msg.historyError
	if msg.err != nil {
		p.notice = localTaskError(msg.err)
	} else {
		p.tasks, p.taskID = msg.tasks, msg.taskID
		p.setPage(msg.page, m.width, m.height)
		if p.notice == "正在读取所选输出" {
			p.notice = ""
		}
	}
	return m, localTaskTickCmd(m.generation, p.epoch)
}

func (m model) handleLocalTaskKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	p := m.taskPanel
	if p == nil {
		return m, nil
	}
	source, ok := m.session.(localTaskSource)
	if !ok {
		return m, nil
	}
	key := msg.String()
	if p.queryMode {
		return m.handleLocalTaskQueryKey(msg)
	}
	if key == "esc" || key == "f5" {
		m.taskPanel = nil
		m.restoreInputFocus()
		return m, nil
	}
	if p.stopping || p.searchBusy {
		return m, nil
	}
	index := 0
	for i, task := range p.tasks {
		if task.TaskID == p.taskID {
			index = i
			break
		}
	}
	switch key {
	case "/":
		m.taskEpoch++
		p.epoch = m.taskEpoch
		p.queryMode, p.query, p.searchNext = true, "", 0
		p.notice = "输入字面检索文本；Enter 检索，Esc 取消"
		return m, nil
	case "n":
		return m.searchLocalTask()
	case "up", "down":
		if len(p.tasks) == 0 {
			return m, nil
		}
		if key == "up" {
			index = max(0, index-1)
		} else {
			index = min(len(p.tasks)-1, index+1)
		}
		p.taskID = p.tasks[index].TaskID
		p.searchNext = 0
		p.output.GotoTop()
	case "tab":
		p.searchNext = 0
		if p.stream == "stdout" {
			p.stream = "stderr"
		} else {
			p.stream = "stdout"
		}
		p.output.GotoTop()
	case "pgdown":
		if p.page.NextOffset > p.offsets[p.offsetKey()] {
			p.offsets[p.offsetKey()] = p.page.NextOffset
		}
		p.output.GotoTop()
	case "pgup":
		p.offsets[p.offsetKey()] = max(int64(0), p.offsets[p.offsetKey()]-4096)
		p.output.GotoTop()
	case "home":
		p.offsets[p.offsetKey()] = 0
		p.output.GotoTop()
	case "ctrl+up":
		p.output.LineUp(3)
		return m, nil
	case "ctrl+down":
		p.output.LineDown(3)
		return m, nil
	case "s":
		if p.taskID == "" {
			return m, nil
		}
		m.taskEpoch++
		p.epoch = m.taskEpoch
		p.stopping = true
		p.notice = "正在停止任务及受管子进程"
		return m, localTaskStopCmd(m.ctx, source, m.generation, p.epoch, p.taskID)
	default:
		return m, nil
	}
	m.taskEpoch++
	p.epoch = m.taskEpoch
	// The previous page belongs to another request. Clear it before accepting
	// another navigation key; its cursor must not skip bytes in the new stream.
	offset := p.offsets[p.offsetKey()]
	p.setPage(localexec.OutputPage{Offset: offset, NextOffset: offset}, m.width, m.height)
	p.notice = "正在读取所选输出"
	return m, m.refreshLocalTasks()
}

func localTaskError(err error) string {
	var executionError *localexec.Error
	if errors.As(err, &executionError) {
		return "任务操作未完成：" + executionError.Code
	}
	return "任务当前不可访问；不要据此判断进程已结束或自动重跑"
}

func (p *localTaskPanel) render(width, height int) string {
	width = max(20, width-4)
	lines := []string{titleStyle.Render("本地任务 · Shell 不受文件确认模式限制"), "当前 Session · 执行状态与输出保存分别报告"}
	selected := 0
	for i, task := range p.tasks {
		if task.TaskID == p.taskID {
			selected = i
			break
		}
	}
	start := max(0, selected-1)
	for i := start; i < min(len(p.tasks), start+3); i++ {
		task := p.tasks[i]
		prefix := "  "
		if task.TaskID == p.taskID {
			prefix = "› "
		}
		lines = append(lines, prefix+task.TaskID+" "+task.State)
	}
	if len(p.tasks) == 0 {
		lines = append(lines, "当前进程无可访问任务；历史状态不代表当前状态", "未保存的旧任务控制/输出不可用；不自动重跑")
	}
	if len(p.tasks) > 0 {
		task := p.tasks[selected]
		if task.Persistence == "" || task.Persistence == "memory_only" {
			lines = append(lines, "未保存输出仅内存保留，退出后不可恢复")
		} else {
			lines = append(lines, fmt.Sprintf("输出保存：%s stdout=%d stderr=%d %s", task.Persistence, task.StdoutSaved, task.StderrSaved, task.PersistenceError))
		}
		if task.Restored {
			lines = append(lines, "历史结算/输出；无进程控制，不重跑")
		}
		lines = append(lines, "状态："+task.State+" / "+task.Reason)
		if task.ExitCode != nil {
			lines[len(lines)-1] += fmt.Sprintf(" exit=%d", *task.ExitCode)
		}
		if task.CleanupIncomplete {
			lines[len(lines)-1] += " 清理不完整"
		}
	}
	lines = append(lines, fmt.Sprintf("%s [%d,%d) 已接收=%d", p.stream, p.page.Offset, p.page.NextOffset, p.page.Received), fmt.Sprintf("已保留=%d 更多=%t 缺口=%t 未确认EOF=%t", p.page.Retained, p.page.More, p.page.Truncated, p.page.Incomplete))
	if p.page.Availability != "" {
		lines = append(lines, fmt.Sprintf("本页来源=%s 已加密保存=%d %s", p.page.Availability, p.page.Saved, p.page.PersistenceError))
	}
	if p.historyError != "" {
		lines = append(lines, "历史目录不可完整访问："+p.historyError)
	}
	if p.query != "" || p.queryMode {
		lines = append(lines, "检索："+safeTerminalText(p.query))
	}
	lines = append(lines, p.notice)
	for i, line := range lines {
		lines[i] = truncateDisplayWidth(line, width)
	}
	view := p.output
	view.Height = max(1, height-len(lines)-2)
	lines = append(lines, view.View(), truncateDisplayWidth("↑↓选任务 Tab流 PgUp/Dn页 Ctrl↑↓滚动", width), truncateDisplayWidth("/检索 n继续 s停止 Home从头 Esc/F5返回", width))
	return lipgloss.NewStyle().Width(width).Render(strings.Join(lines, "\n"))
}
