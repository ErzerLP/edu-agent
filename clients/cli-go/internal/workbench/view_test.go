package workbench

import (
	"context"
	"fmt"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

func assertFits(t *testing.T, view string, width, height int) {
	t.Helper()
	if lipgloss.Height(view) > height {
		t.Fatalf("画面高 %d，终端高 %d", lipgloss.Height(view), height)
	}
	for _, line := range strings.Split(view, "\n") {
		if lipgloss.Width(line) > width {
			t.Fatalf("行宽 %d，终端宽 %d：%q", lipgloss.Width(line), width, line)
		}
	}
}

func TestPagesShowAvailableActionsWithinTerminal(t *testing.T) {
	for _, page := range []string{"overview", "materials", "goals", "learn", "reviews", "spaces", "help", "goal/中文"} {
		t.Run(page, func(t *testing.T) {
			m := New(t.Context(), serviceFunc(func(context.Context, Request) (Page, error) {
				return Page{Title: "中文页面", SpaceName: "测试学习区", Content: strings.Repeat("很长的中文正文与English🙂\n", 40),
					Entries: []Entry{{ID: "page:detail", Label: "查看详情"}}, Actions: []Action{{ID: "save", Label: "保存草稿", Input: "正文"}, {ID: "end", Label: "结束学习", Confirmation: "结束？"}}}, nil
			}), "A")
			finish(m, m.Open(t.Context(), page))
			for _, height := range []int{24, 16} {
				_, _ = m.Update(tea.WindowSizeMsg{Width: 80, Height: height})
				view := m.View()
				assertFits(t, view, 80, height)
				for _, label := range []string{"查看详情", "保存草稿", "结束学习", "Esc", "Ctrl+C"} {
					if !strings.Contains(view, label) {
						t.Fatalf("画面缺少操作/提示 %s：\n%s", label, view)
					}
				}
			}
		})
	}
}

func TestEnterSelectsPageActionWithoutTraversingNavigation(t *testing.T) {
	m := New(t.Context(), serviceFunc(func(context.Context, Request) (Page, error) {
		return Page{Actions: []Action{{ID: "draft", Label: "创建目标", Input: "目标内容"}}}, nil
	}), "A")
	finish(m, m.Init())
	key(m, tea.KeyEnter)
	if !m.editing {
		t.Fatal("Enter 没有直接打开当前页的首个操作")
	}
}

func TestRefreshPreservesCurrentScrollAndSelection(t *testing.T) {
	m := New(t.Context(), serviceFunc(func(context.Context, Request) (Page, error) {
		return Page{Content: strings.Repeat("一行正文\n", 100), Entries: []Entry{{ID: "page:one", Label: "一"}, {ID: "page:two", Label: "二"}}}, nil
	}), "A")
	finish(m, m.Init())
	key(m, tea.KeyDown)
	key(m, tea.KeyPgDown)
	offset, cursor := m.view.YOffset, m.cursor
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("r")})
	finish(m, cmd)
	if m.view.YOffset != offset || m.cursor != cursor {
		t.Fatalf("刷新丢失当前位置：offset %d→%d，cursor %d→%d", offset, m.view.YOffset, cursor, m.cursor)
	}
}

func TestLongConfirmationRemainsReadableAndScrollable(t *testing.T) {
	m := New(t.Context(), nil, "A")
	_, _ = m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m.begin(Action{ID: "end", Label: "结束教学", Confirmation: strings.Repeat("长确认内容🙂", 100) + "确认文案末尾"})
	assertFits(t, m.View(), 80, 24)
	if !strings.Contains(m.View(), "y 确认") {
		t.Fatal("确认提示被裁掉")
	}
	key(m, tea.KeyEnd)
	if !strings.Contains(m.View(), "确认文案末尾") {
		t.Fatal("无法滚动查看完整确认文案")
	}
}

func TestLongActionListFollowsSelection(t *testing.T) {
	m := New(t.Context(), nil, "A")
	for i := 0; i < 50; i++ {
		m.data.Entries = append(m.data.Entries, Entry{ID: fmt.Sprintf("page:%d", i), Label: fmt.Sprintf("中文操作%02d", i)})
	}
	for i := 0; i < 40; i++ {
		key(m, tea.KeyDown)
	}
	view := m.View()
	assertFits(t, view, 80, 24)
	if !strings.Contains(view, "中文操作40") || !strings.Contains(view, "中文操作41") {
		t.Fatalf("当前操作及相邻操作不可见：\n%s", view)
	}
}

func TestFormKeepsFieldsAndSubmissionVisible(t *testing.T) {
	m := New(t.Context(), nil, "A")
	for _, height := range []int{24, 16} {
		_, _ = m.Update(tea.WindowSizeMsg{Width: 80, Height: height})
		m.begin(Action{ID: "save", Label: "编辑目标", Fields: []Field{{ID: "name", Label: "目标名称"}, {ID: "priority", Label: "优先级", Choices: []string{"low", "normal", "high"}, Value: "normal"}}})
		m.input.SetValue("中文草稿\n第二行\x1b[2J\u202e")
		original := m.input.Value()
		view := m.View()
		assertFits(t, view, 80, height)
		for _, text := range []string{"编辑目标", "目标名称", "优先级", "Ctrl+S", "Esc"} {
			if !strings.Contains(view, text) {
				t.Fatalf("表单缺少 %s", text)
			}
		}
		if strings.Contains(view, "\x1b[2J") || strings.Contains(view, "\u202e") {
			t.Fatal("输入框输出了终端控制字符")
		}
		if m.input.Value() != original {
			t.Fatal("渲染改变了草稿原文")
		}
		key(m, tea.KeyTab)
		if !strings.Contains(m.View(), "←/→") {
			t.Fatal("选项字段未提示方向键切换")
		}
		key(m, tea.KeyEsc)
	}
}

func TestColorModesAndNoColor(t *testing.T) {
	for _, tc := range []struct {
		mode, noColor string
		wantColor     bool
	}{{"always", "", true}, {"never", "", false}, {"always", "1", false}, {"auto", "1", false}} {
		t.Run(tc.mode+tc.noColor, func(t *testing.T) {
			t.Setenv("TERM", "xterm-256color")
			t.Setenv("EDU_AGENT_COLOR", tc.mode)
			t.Setenv("NO_COLOR", tc.noColor)
			m := New(t.Context(), nil, "A")
			m.begin(Action{ID: "save", Input: "输入"})
			view := m.View()
			if got := strings.Contains(view, "\x1b["); got != tc.wantColor {
				t.Fatalf("颜色转义=%v，期望 %v", got, tc.wantColor)
			}
		})
	}
}

func TestConfiguredColorMode(t *testing.T) {
	t.Setenv("EDU_AGENT_COLOR", "")
	t.Setenv("NO_COLOR", "")
	t.Setenv("TERM", "xterm-256color")
	m := New(t.Context(), nil, "A")
	for _, mode := range []string{"always", "never", "auto"} {
		var output strings.Builder
		m.SetColorMode(mode, &output)
		view := m.View()
		if got, want := strings.Contains(view, "\x1b["), mode == "always"; got != want {
			t.Fatalf("配置 %s：颜色转义=%v，期望 %v", mode, got, want)
		}
	}
}

func TestEmbeddedLeafFitsAndCanScrollOverflow(t *testing.T) {
	l := &testLeaf{text: strings.Repeat("长导入路径中文🙂", 100) + "\n叶子页末尾"}
	m := New(t.Context(), serviceFunc(func(context.Context, Request) (Page, error) { return Page{Leaf: l}, nil }), "A")
	finish(m, m.Open(t.Context(), "import"))
	assertFits(t, m.View(), 80, 24)
	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyPgDown, Alt: true})
	if !strings.Contains(m.View(), "叶子页末尾") || !strings.Contains(m.View(), "F10") {
		t.Fatal("无法查看子页溢出内容，或丢失返回提示")
	}
}
