package workbench

import (
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

func (m *Model) leafSize() tea.WindowSizeMsg {
	return tea.WindowSizeMsg{Width: max(1, m.width-4), Height: m.bodyRows()}
}

func (m *Model) leafPanel() string {
	// 子页继续拥有自己的按键与草稿；外壳为长路径、长提示提供整页滚动。
	m.leafView.SetContent(ansi.Hardwrap(m.leaf.View(), m.leafView.Width, true))
	return m.panel("资料导入 · F10 返回", m.leafView.View(), m.bodyRows(), false)
}

func (m *Model) wrapLeaf(cmd tea.Cmd) tea.Cmd {
	if cmd == nil {
		return nil
	}
	generation := m.generation
	return func() tea.Msg { return leafMessage{generation, cmd()} }
}
