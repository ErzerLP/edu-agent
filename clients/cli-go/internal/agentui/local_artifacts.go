package agentui

import (
	"context"
	"errors"
	"fmt"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/localartifact"
)

// Browsing is a controller-only capability, not a model tool or task stream.
type localArtifactSource interface {
	LocalArtifacts() ([]localartifact.Info, error)
	ReadLocalArtifact(context.Context, string, int64, int) (localartifact.Page, error)
	SearchLocalArtifact(context.Context, string, string, int64, int) (localartifact.SearchPage, error)
}

type localArtifactCursor struct {
	offset, searchNext int64
	query              string
	searched, more     bool
}

type localArtifactPanel struct {
	epoch, request uint64
	items          []localartifact.Info
	id             string
	cursors        map[string]*localArtifactCursor
	page           localartifact.Page
	pageReady      bool
	output         viewport.Model
	catalogReady   bool
	catalogError   string
	notice         string
	loading        bool
	searching      bool
	queryMode      bool
	queryDraft     string
	listRows       int
	cancel         context.CancelFunc
}

type localArtifactMsg struct {
	generation, epoch, request uint64
	listed                     bool
	items                      []localartifact.Info
	id                         string
	page                       localartifact.Page
	catalogErr, err            error
}

type localArtifactSearchMsg struct {
	generation, epoch, request uint64
	id                         string
	page                       localartifact.SearchPage
	err                        error
}

func newLocalArtifactPanel(epoch uint64) *localArtifactPanel {
	return &localArtifactPanel{epoch: epoch, cursors: make(map[string]*localArtifactCursor), output: viewport.New(1, 1)}
}

func (p *localArtifactPanel) cursor() *localArtifactCursor {
	if p.cursors[p.id] == nil {
		p.cursors[p.id] = &localArtifactCursor{}
	}
	return p.cursors[p.id]
}

func (p *localArtifactPanel) invalidate(clearPage bool) {
	if p.cancel != nil {
		p.cancel()
		p.cancel = nil
	}
	p.request++
	p.loading, p.searching = false, false
	if clearPage {
		// Never navigate using the last artifact/request's NextOffset.
		offset := p.cursor().offset
		p.page = localartifact.Page{Offset: offset, NextOffset: offset}
		p.pageReady = false
		p.output.SetContent("")
		p.output.GotoTop()
	}
}

func (m model) openLocalArtifacts() (tea.Model, tea.Cmd) {
	if _, ok := m.session.(localArtifactSource); !ok {
		m.status = "完整差异/逐项回执浏览不可用"
		return m, nil
	}
	m.artifactEpoch++
	m.artifactPanel = newLocalArtifactPanel(m.artifactEpoch)
	if m.pendingFileMutation != nil {
		m.artifactPanel.id = m.pendingFileMutation.DiffID
		if m.pendingFileMutation.PlanID != "" {
			m.artifactPanel.id = m.pendingFileMutation.PlanID
		}
	}
	m.input.Blur()
	return m, m.loadLocalArtifacts(true)
}

func (m model) closeLocalArtifacts() (tea.Model, tea.Cmd) {
	m.artifactPanel.invalidate(true)
	m.artifactPanel = nil
	m.artifactEpoch++
	// The underlying selector (including focus/review state) was never replaced.
	m.restoreInputFocus()
	return m, nil
}

func localArtifactError(err error) string {
	var stable *localartifact.Error
	if errors.As(err, &stable) && stable != nil {
		return safeSingleLineTerminalText(stable.Code)
	}
	return "artifact_unavailable"
}

func (m model) loadLocalArtifacts(list bool) tea.Cmd {
	p := m.artifactPanel
	source, ok := m.session.(localArtifactSource)
	if p == nil || !ok {
		return nil
	}
	p.invalidate(true)
	p.loading = true
	if list {
		p.catalogReady, p.catalogError = false, ""
		p.notice = "正在读取产物目录"
	}
	p.resize(m.width, m.height)
	ctx, cancel := context.WithCancel(m.ctx)
	p.cancel = cancel
	generation, epoch, request, id := m.generation, p.epoch, p.request, p.id
	offsets := make(map[string]int64, len(p.cursors))
	for key, cursor := range p.cursors {
		offsets[key] = cursor.offset
	}
	return func() tea.Msg {
		defer cancel()
		msg := localArtifactMsg{generation: generation, epoch: epoch, request: request, listed: list, id: id}
		if list {
			if ctx.Err() != nil {
				msg.catalogErr = ctx.Err()
				return msg
			}
			msg.items, msg.catalogErr = source.LocalArtifacts()
			if msg.catalogErr != nil {
				return msg
			}
			found := false
			for _, item := range msg.items {
				if item.ID == id {
					found = true
					break
				}
			}
			if !found {
				msg.id = ""
				if len(msg.items) > 0 {
					msg.id = msg.items[0].ID
				}
			}
		}
		if msg.id != "" {
			msg.page, msg.err = source.ReadLocalArtifact(ctx, msg.id, offsets[msg.id], 4096)
		}
		return msg
	}
}

func (m model) handleLocalArtifactMessage(msg localArtifactMsg) (tea.Model, tea.Cmd) {
	p := m.artifactPanel
	if p == nil || msg.generation != m.generation || msg.epoch != p.epoch || msg.request != p.request {
		return m, nil
	}
	defer p.resize(m.width, m.height)
	p.loading = false
	if msg.listed {
		p.items = nil
		if msg.catalogErr != nil {
			p.catalogReady = false
			p.catalogError = localArtifactError(msg.catalogErr)
			p.notice = ""
			return m, nil
		}
		p.items, p.id = msg.items, msg.id
		p.catalogReady, p.catalogError, p.notice = true, "", ""
	} else if msg.id != p.id {
		return m, nil
	}
	if msg.err != nil {
		p.notice = "读取失败：" + localArtifactError(msg.err)
		return m, nil
	}
	if p.id != "" {
		p.page, p.pageReady = msg.page, true
		p.cursor().offset = msg.page.Offset
		p.output.GotoTop()
	}
	return m, nil
}

func (m model) handleLocalArtifactKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	p := m.artifactPanel
	defer p.resize(m.width, m.height)
	key := msg.String()
	if key == "esc" || key == "f6" {
		return m.closeLocalArtifacts()
	}
	if p.queryMode {
		return m.handleLocalArtifactQueryKey(msg)
	}
	switch key {
	case "r":
		return m, m.loadLocalArtifacts(true)
	case "/":
		p.invalidate(false)
		p.queryMode, p.queryDraft = true, ""
		p.notice = "字面检索≤512字节；Enter检索，Esc关闭"
		return m, nil
	case "n":
		return m.searchLocalArtifact()
	case "ctrl+up":
		p.output.LineUp(3)
		return m, nil
	case "ctrl+down":
		p.output.LineDown(3)
		return m, nil
	case "up", "down":
		if len(p.items) == 0 || !p.catalogReady {
			return m, nil
		}
		index := 0
		for i, item := range p.items {
			if item.ID == p.id {
				index = i
				break
			}
		}
		if key == "up" {
			index = max(0, index-1)
		} else {
			index = min(len(p.items)-1, index+1)
		}
		p.id = p.items[index].ID
	case "pgdown":
		if !p.pageReady || !p.page.More || p.page.NextOffset <= p.cursor().offset {
			return m, nil
		}
		p.cursor().offset = p.page.NextOffset
	case "pgup":
		p.cursor().offset = max(int64(0), p.cursor().offset-4096)
	case "home":
		p.cursor().offset = 0
	default:
		// Enter, F4 and selector shortcuts cannot authorize from this browser.
		return m, nil
	}
	if p.id == "" || !p.catalogReady {
		return m, nil
	}
	p.notice = ""
	return m, m.loadLocalArtifacts(false)
}

func (m model) handleLocalArtifactSearchMessage(msg localArtifactSearchMsg) (tea.Model, tea.Cmd) {
	p := m.artifactPanel
	if p == nil || msg.generation != m.generation || msg.epoch != p.epoch || msg.request != p.request || msg.id != p.id {
		return m, nil
	}
	defer p.resize(m.width, m.height)
	p.searching = false
	if msg.err != nil {
		p.notice = "检索失败：" + localArtifactError(msg.err)
		return m, nil
	}
	cursor := p.cursor()
	cursor.searchNext, cursor.searched, cursor.more = msg.page.NextOffset, true, msg.page.More
	if len(msg.page.Offsets) > 0 {
		cursor.offset = msg.page.Offsets[0]
		p.notice = fmt.Sprintf("命中字节 %d；n继续", cursor.offset)
	} else if msg.page.More {
		p.notice = "本窗口无匹配；n继续，尚未扫描完"
	} else {
		p.notice = "已扫描完，未找到更多匹配"
	}
	return m, m.loadLocalArtifacts(false)
}
