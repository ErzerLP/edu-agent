package workbench

import (
	"context"
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func TestInitialFailureKeepsNavigationAndRefreshRecovery(t *testing.T) {
	fail := true
	m := New(t.Context(), serviceFunc(func(_ context.Context, req Request) (Page, error) {
		if req.Page == "help" {
			return Page{Content: "帮助内容"}, nil
		}
		if fail {
			return Page{SpaceName: "真实学习区", Content: "未完成数据"}, errors.New("protocol_error decode=unknown_field")
		}
		return Page{SpaceName: "真实学习区", Content: "真实进度"}, nil
	}), "00000000-0000-4000-8000-000000000001")
	finish(m, m.Init())
	_, _ = m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	for _, text := range []string{"加载失败", "protocol_error", "尚未加载", "真实学习区", "未加载"} {
		if !strings.Contains(m.View(), text) {
			t.Fatalf("失败页缺少 %s", text)
		}
	}
	if strings.Contains(m.View(), "未完成数据") {
		t.Fatal("将部分结果冒充成成功页面")
	}
	finish(m, m.Open(t.Context(), "help"))
	if !strings.Contains(m.view.View(), "帮助内容") {
		t.Fatal("失败后不能切页")
	}
	finish(m, key(m, tea.KeyEsc))
	if m.page != "overview" || m.loadFailure == "" {
		t.Fatal("返回没有重读失败页")
	}
	fail = false
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("r")})
	finish(m, cmd)
	if !strings.Contains(m.view.View(), "真实进度") || m.loadFailure != "" {
		t.Fatal("连接恢复后刷新未恢复真实内容")
	}
	if cmd := key(m, tea.KeyEsc); cmd == nil {
		t.Fatal("无法返回主菜单")
	}
}
