package workbench

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

type serviceFunc func(context.Context, Request) (Page, error)

func (f serviceFunc) Load(ctx context.Context, req Request) (Page, error) { return f(ctx, req) }
func key(m *Model, k tea.KeyType) tea.Cmd                                 { _, cmd := m.Update(tea.KeyMsg{Type: k}); return cmd }
func finish(m *Model, cmd tea.Cmd) {
	if cmd != nil {
		_, _ = m.Update(cmd())
	}
}

func TestDraftNavigationAndChineseMultiline(t *testing.T) {
	var submitted Request
	m := New(t.Context(), serviceFunc(func(_ context.Context, req Request) (Page, error) {
		submitted = req
		return Page{Title: req.Page, Content: strings.Repeat("很长的内容\n", 100), Actions: []Action{{ID: "goal", Label: "创建目标", Input: "输入目标"}}}, nil
	}), "A")
	finish(m, m.Open(t.Context(), "goals"))
	finish(m, m.begin(m.data.Actions[0]))
	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("中文qjr123")})
	key(m, tea.KeyEnter)
	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("第二行")})
	if m.page != "goals" || !m.editing || m.input.Value() != "中文qjr123\n第二行" {
		t.Fatalf("输入误触导航：%q", m.input.Value())
	}
	key(m, tea.KeyEsc)
	m.view.SetYOffset(9)
	finish(m, m.Open(t.Context(), "reviews"))
	finish(m, m.Open(t.Context(), "goals"))
	if m.view.YOffset != 9 {
		t.Fatal("返回丢失滚动位置")
	}
	finish(m, m.begin(m.data.Actions[0]))
	if m.input.Value() != "中文qjr123\n第二行" {
		t.Fatal("返回丢失草稿")
	}
	finish(m, key(m, tea.KeyCtrlS))
	if submitted.Text != "中文qjr123\n第二行" || submitted.Space != "A" {
		t.Fatalf("提交错误：%+v", submitted)
	}
	finish(m, m.choose("select:B"))
	finish(m, m.Open(t.Context(), "goals"))
	finish(m, m.begin(m.data.Actions[0]))
	if m.input.Value() != "" {
		t.Fatal("跨区泄漏草稿")
	}
}

func TestCancellationAndLateResponse(t *testing.T) {
	started, canceled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	m := New(t.Context(), serviceFunc(func(ctx context.Context, req Request) (Page, error) {
		if req.Page == "overview" {
			close(started)
			<-ctx.Done()
			close(canceled)
			<-release
			return Page{Content: "旧区秘密"}, nil
		}
		return Page{Content: "新页面"}, nil
	}), "A")
	cmd := m.Init()
	result := make(chan tea.Msg, 1)
	go func() { result <- cmd() }()
	<-started
	finish(m, m.Open(t.Context(), "reviews"))
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("退出页面未取消请求")
	}
	close(release)
	_, _ = m.Update(<-result)
	if m.data.Content != "新页面" {
		t.Fatal("迟到响应覆盖新页面")
	}
}

func TestFailureClearsContentAndCanRetry(t *testing.T) {
	fail := false
	m := New(t.Context(), serviceFunc(func(context.Context, Request) (Page, error) {
		if fail {
			return Page{}, errors.New("service_unavailable\x1b[2J")
		}
		return Page{Content: "旧内容\n下一行\x1b[2J"}, nil
	}), "A")
	finish(m, m.Init())
	if !strings.Contains(m.view.View(), "下一行") || strings.Contains(m.view.View(), "\x1b[2J") {
		t.Fatal("未安全呈现多行内容")
	}
	fail = true
	finish(m, m.refresh(""))
	if m.data.Content != "" || !strings.Contains(m.status, "失败") || strings.Contains(m.status, "\x1b") {
		t.Fatal("错误冒充合法内容或未清理控制字符")
	}
	fail = false
	finish(m, m.refresh(""))
	if m.status != "" {
		t.Fatal("错误后无法继续")
	}
}

func TestConfirmationAndSmallTerminal(t *testing.T) {
	m := New(t.Context(), serviceFunc(func(context.Context, Request) (Page, error) { return Page{}, nil }), "A")
	if cmd := m.begin(Action{ID: "end", Confirmation: "结束？"}); cmd != nil {
		t.Fatal("确认前执行写操作")
	}
	key(m, tea.KeyEsc)
	if m.confirm {
		t.Fatal("无法取消确认")
	}
	_, _ = m.Update(tea.WindowSizeMsg{Width: 12, Height: 4})
	cmd := key(m, tea.KeyEsc)
	if _, ok := cmd().(ExitMsg); !ok {
		t.Fatal("过小终端无法返回")
	}
}

func TestLoadingClampsSelectionAndPrivacyClearsDrafts(t *testing.T) {
	m := New(t.Context(), serviceFunc(func(context.Context, Request) (Page, error) {
		return Page{}, &Failure{Message: "内容已清除", ClearDrafts: true}
	}), "A")
	m.data.Actions = []Action{{ID: "one"}, {ID: "two"}, {ID: "three"}}
	m.cursor = len(m.options()) - 1
	m.state().drafts["secret"] = "敏感草稿"
	cmd := m.refresh("")
	_ = m.View()
	finish(m, cmd)
	if len(m.state().drafts) != 0 {
		t.Fatal("隐私清除后仍保留草稿")
	}
}

func TestResourceNavigationFieldsAndStableRetry(t *testing.T) {
	var requests []Request
	service := serviceFunc(func(_ context.Context, r Request) (Page, error) {
		requests = append(requests, r)
		if r.Action == "save" {
			return Page{}, errors.New("暂时不可用")
		}
		return Page{Version: 2, Content: "目标正文", Actions: []Action{{ID: "save", Fields: []Field{{ID: "name", Label: "名称", Value: "目标"}, {ID: "text", Label: "意图", Value: "旧意图"}}}}}, nil
	})
	m := New(t.Context(), service, "A")
	finish(m, m.Open(t.Context(), "goals"))
	finish(m, m.choose("select:page:goal/目标ID"))
	finish(m, m.begin(m.data.Actions[0]))
	m.input.SetValue("中文目标")
	key(m, tea.KeyTab)
	m.input.SetValue("第一行\n第二行")
	finish(m, key(m, tea.KeyCtrlS))
	first := requests[len(requests)-1]
	if first.Resource != "目标ID" || first.Values["name"] != "中文目标" || first.Text != "第一行\n第二行" || first.Operation == "" {
		t.Fatalf("多字段请求错误：%+v", first)
	}
	finish(m, m.refresh(""))
	finish(m, m.begin(m.data.Actions[0]))
	finish(m, key(m, tea.KeyCtrlS))
	second := requests[len(requests)-1]
	if first.Operation != second.Operation || first.Entity != second.Entity {
		t.Fatal("同内容失败重试丢失操作身份")
	}
	finish(m, key(m, tea.KeyEsc))
	if m.page != "goals" {
		t.Fatalf("返回路径错误：%s", m.page)
	}
}

func TestTeachingDraftsStayWithSessionAndActivity(t *testing.T) {
	m := New(t.Context(), serviceFunc(func(context.Context, Request) (Page, error) { return Page{}, nil }), "A")
	a := Action{ID: "answer", Input: "答案", DraftKey: "/会话A/题目1"}
	m.begin(a)
	m.input.SetValue("A的中文答案")
	key(m, tea.KeyEsc)
	b := a
	b.DraftKey = "/会话B/题目1"
	m.begin(b)
	if m.input.Value() != "" {
		t.Fatal("答案跨会话污染")
	}
	key(m, tea.KeyEsc)
	m.begin(a)
	if m.input.Value() != "A的中文答案" {
		t.Fatal("返回会话丢失答案")
	}
}

type testLeaf struct {
	suspended int
	text      string
}

func (l *testLeaf) Init() tea.Cmd { return nil }
func (l *testLeaf) View() string  { return l.text }
func (l *testLeaf) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if text, ok := msg.(string); ok {
		l.text = text
	}
	return l, nil
}
func (l *testLeaf) Suspend()                       { l.suspended++ }
func (l *testLeaf) Resume(context.Context) tea.Cmd { return nil }
func (l *testLeaf) Destination(context.Context) func() (string, error) {
	return func() (string, error) { return "materials", nil }
}

func TestEmbeddedLeafLateMessagesAndSmallResize(t *testing.T) {
	l := &testLeaf{text: "导入草稿"}
	m := New(t.Context(), serviceFunc(func(_ context.Context, r Request) (Page, error) {
		if r.Page == "import" {
			return Page{Leaf: l}, nil
		}
		return Page{Content: "资料页"}, nil
	}), "A")
	finish(m, m.Open(t.Context(), "import"))
	old := m.wrapLeaf(func() tea.Msg { return "旧响应" })
	_, _ = m.Update(tea.WindowSizeMsg{Width: 20, Height: 8})
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyF10})
	finish(m, cmd)
	finish(m, old)
	if l.text != "导入草稿" || l.suspended == 0 || m.leaf != nil {
		t.Fatal("退出叶子后仍接受旧消息")
	}
	finish(m, m.Open(t.Context(), "import"))
	if !strings.Contains(m.View(), "F10") || l.text != "导入草稿" {
		t.Fatal("嵌入叶子不能保留草稿并返回")
	}
}
