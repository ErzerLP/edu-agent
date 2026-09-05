package agentui

import (
	"context"
	"fmt"
	"time"
	"unicode"
	"unicode/utf8"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/localexec"
)

type localTerminalSource interface {
	InputLocalTask(context.Context, string, []byte) (localexec.InputResult, error)
	InterruptLocalTask(context.Context, string) (localexec.InputResult, error)
	EOFLocalTask(context.Context, string) (localexec.InputResult, error)
	ResizeLocalTask(string, int, int) (localexec.Snapshot, error)
}

type localTerminalMsg struct {
	generation, epoch, request uint64
	taskID, action             string
	input                      localexec.InputResult
	sent, rows, cols           int
	err                        error
}

func (p *localTaskPanel) selectedTask() localexec.Snapshot {
	for _, task := range p.tasks {
		if task.TaskID == p.taskID {
			return task
		}
	}
	return localexec.Snapshot{}
}

func (m model) enterLocalTerminal() (tea.Model, tea.Cmd) {
	p := m.taskPanel
	if _, ok := m.session.(localTerminalSource); !ok || !p.selectedTask().PTY || !p.selectedTask().Controllable {
		p.notice = "只可进入当前可控制的 PTY；pipe/历史任务不可附着"
		return m, nil
	}
	m.taskEpoch++
	p.epoch, p.terminalMode, p.terminalDraft = m.taskEpoch, true, ""
	p.notice = "行式输入不回显草稿；程序回显可能保存/供模型读取"
	return m, tea.Batch(m.resizeLocalTerminal(), m.refreshLocalTasks())
}

func (m model) resizeLocalTerminal() tea.Cmd {
	p := m.taskPanel
	source, ok := m.session.(localTerminalSource)
	if p == nil || !p.terminalMode || !ok || !p.selectedTask().Controllable {
		return nil
	}
	p.resizeRequest++
	generation, epoch, request, id := m.generation, p.epoch, p.resizeRequest, p.taskID
	rows, cols := min(4096, max(1, m.height-14)), min(4096, max(1, m.width-4))
	return func() tea.Msg {
		_, err := source.ResizeLocalTask(id, rows, cols)
		return localTerminalMsg{generation: generation, epoch: epoch, request: request, taskID: id, action: "resize", rows: rows, cols: cols, err: err}
	}
}

func (m model) handleLocalTerminalKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	p := m.taskPanel
	key := msg.String()
	if key == "esc" {
		p.terminalMode, p.terminalDraft = false, ""
		p.notice = "已离开输入模式；已发出的输入不能撤销或自动重传"
		return m, m.refreshLocalTasks()
	}
	if key == "f5" {
		m.status = "已关闭终端查看；在途输入可能已生效，不自动重传"
		m.taskPanel = nil
		m.restoreInputFocus()
		return m, nil
	}
	if p.terminalBusy {
		return m, nil
	}
	switch msg.Type {
	case tea.KeyEnter:
		content := []byte(p.terminalDraft + "\n")
		p.terminalDraft = ""
		return m.sendLocalTerminal("input", content)
	case tea.KeyCtrlC:
		p.terminalDraft = ""
		return m.sendLocalTerminal("interrupt", nil)
	case tea.KeyCtrlD:
		if p.terminalDraft != "" {
			p.notice = "请先发送当前行，或Esc放弃草稿后再进入并发送EOF"
			return m, nil
		}
		return m.sendLocalTerminal("eof", nil)
	case tea.KeyBackspace, tea.KeyDelete:
		if len(p.terminalDraft) > 0 {
			_, size := utf8.DecodeLastRuneInString(p.terminalDraft)
			p.terminalDraft = p.terminalDraft[:len(p.terminalDraft)-size]
		}
	case tea.KeyRunes:
		for _, r := range msg.Runes {
			if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
				p.notice = "行式输入不接受粘贴中的换行/控制字符；本次粘贴未加入草稿"
				return m, nil
			}
		}
		text := string(msg.Runes)
		if len(p.terminalDraft)+len(text) > 4096 {
			p.notice = "单行最多4096字节，本次输入未加入草稿"
			return m, nil
		}
		p.terminalDraft += text
	}
	return m, nil
}

func (m model) sendLocalTerminal(action string, content []byte) (tea.Model, tea.Cmd) {
	p := m.taskPanel
	source, ok := m.session.(localTerminalSource)
	if !ok {
		p.notice = "终端操作不可用"
		return m, nil
	}
	p.terminalRequest++
	p.terminalBusy = true
	p.notice = "正在发送终端输入；接受不等于程序已处理"
	generation, epoch, request, id, ctx := m.generation, p.epoch, p.terminalRequest, p.taskID, m.ctx
	return m, func() tea.Msg {
		inputCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		message := localTerminalMsg{generation: generation, epoch: epoch, request: request, taskID: id, action: action, sent: len(content)}
		switch action {
		case "input":
			message.input, message.err = source.InputLocalTask(inputCtx, id, content)
		case "interrupt":
			message.input, message.err = source.InterruptLocalTask(inputCtx, id)
		case "eof":
			message.input, message.err = source.EOFLocalTask(inputCtx, id)
		}
		return message
	}
}

func (m model) handleLocalTerminalMessage(msg localTerminalMsg) (tea.Model, tea.Cmd) {
	p := m.taskPanel
	if p == nil || msg.generation != m.generation || msg.epoch != p.epoch || msg.taskID != p.taskID {
		return m, nil
	}
	if msg.action == "resize" {
		if msg.request != p.resizeRequest {
			return m, nil
		}
		if msg.err != nil {
			p.notice = localTaskError(msg.err)
		} else {
			for i := range p.tasks {
				if p.tasks[i].TaskID == msg.taskID {
					p.tasks[i].Rows, p.tasks[i].Cols = msg.rows, msg.cols
				}
			}
		}
		return m, nil
	}
	if msg.request != p.terminalRequest {
		return m, nil
	}
	p.terminalBusy = false
	p.notice = fmt.Sprintf("终端%s：已接受%d字节，%s；不代表程序处理完成，不自动重传", msg.action, msg.input.Written, msg.input.Outcome)
	if msg.action == "input" {
		p.notice += fmt.Sprintf("（请求%d字节）", msg.sent)
	}
	if msg.err != nil {
		p.notice += "；" + localTaskError(msg.err)
	}
	return m, m.refreshLocalTasks()
}
