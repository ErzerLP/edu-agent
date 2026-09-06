package agentui

import (
	"context"
	"unicode"
	"unicode/utf8"

	tea "github.com/charmbracelet/bubbletea"
)

func (m model) handleLocalArtifactQueryKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	p := m.artifactPanel
	switch msg.Type {
	case tea.KeyEnter:
		if p.queryDraft == "" {
			p.notice = "请输入非空字面检索文本"
			return m, nil
		}
		cursor := p.cursor()
		cursor.query, cursor.searchNext = p.queryDraft, 0
		cursor.searched, cursor.more = false, false
		p.queryMode, p.queryDraft = false, ""
		return m.searchLocalArtifact()
	case tea.KeyBackspace, tea.KeyDelete:
		if p.queryDraft != "" {
			_, size := utf8.DecodeLastRuneInString(p.queryDraft)
			p.queryDraft = p.queryDraft[:len(p.queryDraft)-size]
		}
	case tea.KeyRunes:
		// A paste is accepted atomically. Never silently search a filtered or
		// truncated string which is different from what the user supplied.
		for _, r := range msg.Runes {
			if !utf8.ValidRune(r) || unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
				p.notice = "检索输入已拒绝：不允许控制字符"
				return m, nil
			}
		}
		text := string(msg.Runes)
		if len(p.queryDraft)+len(text) > 512 {
			p.notice = "检索输入已拒绝：最多512字节"
			return m, nil
		}
		p.queryDraft += text
	}
	return m, nil
}

func (m model) searchLocalArtifact() (tea.Model, tea.Cmd) {
	p := m.artifactPanel
	if p.searching {
		return m, nil
	}
	source, ok := m.session.(localArtifactSource)
	cursor := p.cursor()
	if !ok || !p.catalogReady || p.id == "" || cursor.query == "" {
		p.notice = "请先选择可访问产物，并用/输入检索文本"
		return m, nil
	}
	if cursor.searched && !cursor.more {
		p.notice = "检索已到末尾；/开始新检索"
		return m, nil
	}
	p.invalidate(true)
	p.searching, p.notice = true, "正在字面检索，不调用模型"
	p.resize(m.width, m.height)
	ctx, cancel := context.WithCancel(m.ctx)
	p.cancel = cancel
	generation, epoch, request := m.generation, p.epoch, p.request
	id, query, offset := p.id, cursor.query, cursor.searchNext
	return m, func() tea.Msg {
		defer cancel()
		page, err := source.SearchLocalArtifact(ctx, id, query, offset, 1)
		return localArtifactSearchMsg{generation: generation, epoch: epoch, request: request, id: id, page: page, err: err}
	}
}
