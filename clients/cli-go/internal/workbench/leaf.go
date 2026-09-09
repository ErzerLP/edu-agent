package workbench

import tea "github.com/charmbracelet/bubbletea"

func (m *Model) wrapLeaf(cmd tea.Cmd) tea.Cmd {
	if cmd == nil {
		return nil
	}
	generation := m.generation
	return func() tea.Msg { return leafMessage{generation, cmd()} }
}
