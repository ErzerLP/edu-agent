package dashboard

import (
	"context"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/workbench"
)

type workbenchStub struct{}

func (workbenchStub) Load(_ context.Context, req workbench.Request) (workbench.Page, error) {
	return workbench.Page{Title: req.Page, Content: req.Text, Actions: []workbench.Action{{ID: "goal", Label: "创建目标", Input: "目标草稿"}}}, nil
}

func TestLearningEntriesStayInDashboard(t *testing.T) {
	for _, shortcut := range []string{"g", "i", "l", "z", "w"} {
		m := newModel(Snapshot{LocalState: LocalStatePaired})
		m.ctx = t.Context()
		m.workspace = workbench.New(m.ctx, workbenchStub{}, "默认")
		updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(shortcut)})
		m = updated.(model)
		if !m.workspaceActive || len(m.command) != 0 || m.quit {
			t.Fatalf("%s 仍是退出式命令启动器", shortcut)
		}
		if _, quit := cmd().(tea.QuitMsg); quit {
			t.Fatal("学习入口退出全屏")
		}
		updated, _ = m.Update(workbench.ExitMsg{})
		if updated.(model).workspaceActive {
			t.Fatal("无法返回原菜单")
		}
	}
}
