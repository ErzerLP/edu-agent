package agentui

import (
	"context"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/agentloop"
)

type fakeConversation struct {
	result        agentloop.Result
	resolved      agentloop.Result
	sent          string
	approved      bool
	resolveCalls  int
	resolveErrors []error
}

func (c *fakeConversation) Send(_ context.Context, input string) (agentloop.Result, error) {
	c.sent = input
	return c.result, nil
}
func (c *fakeConversation) ResolvePreference(_ context.Context, approved bool) (agentloop.Result, error) {
	c.approved, c.resolveCalls = approved, c.resolveCalls+1
	if len(c.resolveErrors) > 0 {
		err := c.resolveErrors[0]
		c.resolveErrors = c.resolveErrors[1:]
		if err != nil {
			return agentloop.Result{}, err
		}
	}
	return c.resolved, nil
}

func TestAgentUIIsChineseAndSendsInput(t *testing.T) {
	conversation := &fakeConversation{result: agentloop.Result{Text: "这是回答。", Events: []agentloop.Event{{Tool: "search_knowledge", Summary: "检索到 1 条知识片段"}}}}
	value := newModel(t.Context(), conversation, "test-model")
	view := value.View()
	for _, expected := range []string{"AI学习助手", "模型 test-model", "Enter发送", "长期偏好"} {
		if !strings.Contains(view, expected) {
			t.Fatalf("view missing %q: %s", expected, view)
		}
	}
	value.input.SetValue("解释图论")
	updated, command := value.Update(tea.KeyMsg{Type: tea.KeyEnter})
	value = updated.(model)
	if !value.busy || command == nil {
		t.Fatalf("busy=%t command=%v", value.busy, command)
	}
	updated, _ = value.Update(command().(turnMsg))
	value = updated.(model)
	if conversation.sent != "解释图论" || value.busy || !strings.Contains(value.viewport.View(), "这是回答") {
		t.Fatalf("sent=%q busy=%t viewport=%s", conversation.sent, value.busy, value.viewport.View())
	}
}

func TestAgentUIPreferenceRequiresYOrN(t *testing.T) {
	pending := &agentloop.PreferenceConfirmation{
		Content: "先给结论", Reason: "用户明确要求长期保持回答顺序",
		Category: "interaction_preference", Sensitivity: "non_sensitive", Stability: "stable",
	}
	conversation := &fakeConversation{result: agentloop.Result{Pending: pending}, resolved: agentloop.Result{Text: "已处理。"}}
	value := newModel(t.Context(), conversation, "model")
	value.input.SetValue("记住这个偏好")
	updated, command := value.Update(tea.KeyMsg{Type: tea.KeyEnter})
	value = updated.(model)
	updated, _ = value.Update(command().(turnMsg))
	value = updated.(model)
	if value.pending == nil || !strings.Contains(value.View(), "将保存以下长期偏好") || !strings.Contains(value.viewport.View(), "用户明确要求长期保持回答顺序") {
		t.Fatalf("pending view=%s viewport=%s", value.View(), value.viewport.View())
	}
	updated, command = value.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")})
	value = updated.(model)
	if command == nil || !value.busy {
		t.Fatalf("confirmation did not start")
	}
	if _, duplicate := value.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")}); duplicate != nil {
		t.Fatal("busy confirmation accepted a duplicate Y")
	}
	if _, decline := value.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")}); decline != nil {
		t.Fatal("busy confirmation accepted N after Y")
	}
	updated, _ = value.Update(command().(turnMsg))
	value = updated.(model)
	if conversation.resolveCalls != 1 || !conversation.approved || value.pending != nil {
		t.Fatalf("calls=%d approved=%t pending=%v", conversation.resolveCalls, conversation.approved, value.pending)
	}
}

type blockingConversation struct {
	started  chan struct{}
	canceled chan struct{}
}

func (c *blockingConversation) Send(ctx context.Context, _ string) (agentloop.Result, error) {
	close(c.started)
	<-ctx.Done()
	close(c.canceled)
	return agentloop.Result{}, ctx.Err()
}

func (*blockingConversation) ResolvePreference(context.Context, bool) (agentloop.Result, error) {
	return agentloop.Result{}, nil
}

func TestAgentUIAmbiguousPreferenceCanOnlyRetry(t *testing.T) {
	pending := &agentloop.PreferenceConfirmation{
		Content: "先给结论", Reason: "用户明确要求长期保持回答顺序",
		Category: "interaction_preference", Sensitivity: "non_sensitive", Stability: "stable",
	}
	conversation := &fakeConversation{
		result: agentloop.Result{Pending: pending}, resolved: agentloop.Result{Text: "已核对提交。"},
		resolveErrors: []error{agentloop.ErrPreferenceOutcomeUnknown},
	}
	value := newModel(t.Context(), conversation, "model")
	value.input.SetValue("记住这个偏好")
	updated, command := value.Update(tea.KeyMsg{Type: tea.KeyEnter})
	value = updated.(model)
	updated, _ = value.Update(command().(turnMsg))
	value = updated.(model)
	updated, command = value.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")})
	value = updated.(model)
	updated, _ = value.Update(command().(turnMsg))
	value = updated.(model)
	view := value.View()
	if value.pending == nil || !value.pending.RetryOnly || !strings.Contains(view, "提交结果未知") || !strings.Contains(view, "Y 重试核对") || strings.Contains(view, "N 取消") {
		t.Fatalf("ambiguous preference state = pending=%+v view=%s", value.pending, view)
	}
	if _, decline := value.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")}); decline != nil || conversation.resolveCalls != 1 {
		t.Fatalf("ambiguous preference accepted decline: command=%v calls=%d", decline, conversation.resolveCalls)
	}
	updated, command = value.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")})
	value = updated.(model)
	updated, _ = value.Update(command().(turnMsg))
	value = updated.(model)
	if value.pending != nil || conversation.resolveCalls != 2 || !strings.Contains(value.View(), "已核对提交") {
		t.Fatalf("retry state pending=%+v calls=%d view=%s", value.pending, conversation.resolveCalls, value.View())
	}
}

func TestAgentUIExitCancelsInFlightConversation(t *testing.T) {
	conversation := &blockingConversation{started: make(chan struct{}), canceled: make(chan struct{})}
	value := newModel(context.Background(), conversation, "model")
	value.input.SetValue("阻塞请求")
	updated, command := value.Update(tea.KeyMsg{Type: tea.KeyEnter})
	value = updated.(model)
	if command == nil {
		t.Fatal("send command missing")
	}
	result := make(chan tea.Msg, 1)
	go func() { result <- command() }()
	select {
	case <-conversation.started:
	case <-time.After(time.Second):
		t.Fatal("conversation did not start")
	}
	_, quit := value.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	if quit == nil {
		t.Fatal("quit command missing")
	}
	select {
	case <-conversation.canceled:
	case <-time.After(time.Second):
		t.Fatal("in-flight conversation was not canceled")
	}
	select {
	case message := <-result:
		if turn, ok := message.(turnMsg); !ok || turn.err == nil {
			t.Fatalf("canceled turn = %#v", message)
		}
	case <-time.After(time.Second):
		t.Fatal("canceled conversation did not return")
	}
}

func TestAgentUISanitizesUntrustedTerminalControls(t *testing.T) {
	conversation := &fakeConversation{result: agentloop.Result{
		Text: "正常回答\x1b[2J\x1b]0;伪造标题\a结束",
		Pending: &agentloop.PreferenceConfirmation{
			Content: "保留\x1b[H偏好", Reason: "原因\r覆盖", Category: "personal_context",
			Sensitivity: "sensitive", Stability: "transient",
		},
	}}
	value := newModel(t.Context(), conversation, "model\x1b[2J")
	value.input.SetValue("测试")
	updated, command := value.Update(tea.KeyMsg{Type: tea.KeyEnter})
	value = updated.(model)
	updated, _ = value.Update(command().(turnMsg))
	value = updated.(model)
	view := value.View()
	if strings.ContainsAny(view, "\x1b\r\a") || !strings.Contains(view, "�") || !strings.Contains(value.viewport.View(), "原因") {
		t.Fatalf("untrusted controls reached terminal: %q", view)
	}
}

func TestAgentUILongPreferenceKeepsConfirmationControlsVisible(t *testing.T) {
	pending := &agentloop.PreferenceConfirmation{
		Content: strings.Repeat("完整候选内容", 180), Reason: strings.Repeat("保存原因", 40),
		Category: "interaction_preference", Sensitivity: "non_sensitive", Stability: "stable",
	}
	conversation := &fakeConversation{result: agentloop.Result{Pending: pending}}
	value := newModel(t.Context(), conversation, "model")
	updated, _ := value.Update(tea.WindowSizeMsg{Width: 80, Height: minimumHeight})
	value = updated.(model)
	value.input.SetValue("记住")
	updated, command := value.Update(tea.KeyMsg{Type: tea.KeyEnter})
	value = updated.(model)
	updated, _ = value.Update(command().(turnMsg))
	value = updated.(model)
	view := value.View()
	if !strings.Contains(view, "Y 确认保存") || !strings.Contains(view, "Ctrl+↑/↓滚动检查") || value.viewport.TotalLineCount() <= value.viewport.Height {
		t.Fatalf("long confirmation is not inspectable: lines=%d height=%d view=%s", value.viewport.TotalLineCount(), value.viewport.Height, view)
	}
	before := value.viewport.YOffset
	updated, _ = value.Update(tea.KeyMsg{Type: tea.KeyCtrlUp})
	value = updated.(model)
	if value.viewport.YOffset >= before || conversation.resolveCalls != 0 {
		t.Fatalf("scroll failed or resolved unexpectedly: before=%d after=%d calls=%d", before, value.viewport.YOffset, conversation.resolveCalls)
	}
}

func TestAgentUISmallTerminalStaysBounded(t *testing.T) {
	value := newModel(t.Context(), &fakeConversation{}, "model")
	updated, _ := value.Update(tea.WindowSizeMsg{Width: 12, Height: 3})
	view := updated.(model).View()
	lines := strings.Split(view, "\n")
	if len(lines) > 3 {
		t.Fatalf("lines=%d view=%q", len(lines), view)
	}
	for _, line := range lines {
		if len([]rune(line)) > 12 {
			t.Fatalf("line too wide: %q", line)
		}
	}
}
