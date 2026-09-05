package agentui

import (
	"context"
	"fmt"
	"unicode"
	"unicode/utf8"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/localexec"
)

type localTaskSearcher interface {
	SearchLocalTask(context.Context, string, string, string, int64, int) (localexec.SearchPage, error)
}
type localTaskSearchMsg struct {
	generation, epoch uint64
	page              localexec.SearchPage
	err               error
}

func (m model) handleLocalTaskQueryKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	p := m.taskPanel
	if msg.String() == "f5" {
		m.taskPanel = nil
		m.restoreInputFocus()
		return m, nil
	}
	switch msg.Type {
	case tea.KeyEsc:
		p.queryMode, p.notice = false, ""
		return m, m.refreshLocalTasks()
	case tea.KeyEnter:
		p.queryMode = false
		return m.searchLocalTask()
	case tea.KeyBackspace, tea.KeyDelete:
		if len(p.query) > 0 {
			_, size := utf8.DecodeLastRuneInString(p.query)
			p.query = p.query[:len(p.query)-size]
		}
	case tea.KeyRunes:
		for _, r := range msg.Runes {
			if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
				continue
			}
			if len(p.query)+utf8.RuneLen(r) > 512 {
				break
			}
			p.query += string(r)
		}
	}
	return m, nil
}

func (m model) searchLocalTask() (tea.Model, tea.Cmd) {
	p := m.taskPanel
	source, ok := m.session.(localTaskSearcher)
	if !ok || p.taskID == "" || p.query == "" {
		p.notice = "请先选择可访问任务，并用 / 输入检索文本"
		return m, m.refreshLocalTasks()
	}
	m.taskEpoch++
	p.epoch, p.searchBusy = m.taskEpoch, true
	p.notice = "正在检索当前输出范围"
	generation, epoch, ctx := m.generation, p.epoch, m.ctx
	taskID, stream, query, offset := p.taskID, p.stream, p.query, p.searchNext
	return m, func() tea.Msg {
		page, err := source.SearchLocalTask(ctx, taskID, stream, query, offset, 1)
		return localTaskSearchMsg{generation: generation, epoch: epoch, page: page, err: err}
	}
}

func (m model) handleLocalTaskSearchMessage(msg localTaskSearchMsg) (tea.Model, tea.Cmd) {
	p := m.taskPanel
	if p == nil || msg.generation != m.generation || msg.epoch != p.epoch {
		return m, nil
	}
	p.searchBusy = false
	if msg.err != nil {
		p.notice = localTaskError(msg.err)
		return m, m.refreshLocalTasks()
	}
	p.searchNext = msg.page.NextOffset
	if len(msg.page.Offsets) > 0 {
		offset := msg.page.Offsets[0]
		p.offsets[p.offsetKey()] = offset
		p.setPage(localexec.OutputPage{Offset: offset, NextOffset: offset}, m.width, m.height)
		p.output.GotoTop()
		p.notice = fmt.Sprintf("命中字节 %d；n 继续，/ 新检索", offset)
	} else if msg.page.More {
		p.notice = "当前扫描窗口无匹配；n 继续扫描，尚未扫描完"
	} else {
		p.notice = "当前保留范围未找到更多匹配；运行任务的后续输出需再查"
	}
	if msg.page.Truncated || msg.page.Incomplete {
		p.notice += "；存在缺口或未确认完整性"
	}
	return m, m.refreshLocalTasks()
}
