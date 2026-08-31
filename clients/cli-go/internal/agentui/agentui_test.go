package agentui

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/agentloop"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/api"
)

type fakeConversation struct {
	result             agentloop.Result
	resolved           agentloop.Result
	activities         []agentloop.Activity
	resolvedActivities []agentloop.Activity
	sendErr            error
	sent               string
	approved           bool
	resolveCalls       int
	resolveErrors      []error
}

func (c *fakeConversation) Send(ctx context.Context, input string) (agentloop.Result, error) {
	c.sent = input
	for _, activity := range c.activities {
		agentloop.PublishActivity(ctx, activity)
	}
	return c.result, c.sendErr
}
func (c *fakeConversation) ResolvePreference(ctx context.Context, approved bool) (agentloop.Result, error) {
	c.approved, c.resolveCalls = approved, c.resolveCalls+1
	for _, activity := range c.resolvedActivities {
		agentloop.PublishActivity(ctx, activity)
	}
	if len(c.resolveErrors) > 0 {
		err := c.resolveErrors[0]
		c.resolveErrors = c.resolveErrors[1:]
		if err != nil {
			return agentloop.Result{}, err
		}
	}
	return c.resolved, nil
}

func runTurn(t *testing.T, value model, command tea.Cmd) model {
	t.Helper()
	for value.busy {
		if command == nil {
			t.Fatal("turn command ended while the model was still busy")
		}
		message := command()
		updated, next := value.Update(message)
		value = updated.(model)
		command = next
	}
	return value
}

func TestAgentUIIsChineseAndSendsInput(t *testing.T) {
	conversation := &fakeConversation{result: agentloop.Result{Text: "这是回答。", Events: []agentloop.Event{{Tool: "search_knowledge", Summary: "检索到 1 条知识片段"}}}}
	value := newModel(t.Context(), conversation, "test-model")
	view := value.View()
	for _, expected := range []string{"AI 学习助手", "模型 test-model", "Enter 发送", "长期偏好"} {
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
	value = runTurn(t, value, command)
	if conversation.sent != "解释图论" || value.busy || !strings.Contains(value.viewport.View(), "这是回答") {
		t.Fatalf("sent=%q busy=%t viewport=%s", conversation.sent, value.busy, value.viewport.View())
	}
}

type learningStatusConversation struct {
	fakeConversation
	status agentloop.LearningStatus
	err    error
	calls  int
}

func (c *learningStatusConversation) LearningStatus(context.Context) (agentloop.LearningStatus, error) {
	c.calls++
	return c.status, c.err
}

func TestAgentUIRightSidebarShowsAuthoritativeLearningStatus(t *testing.T) {
	conversation := &learningStatusConversation{status: agentloop.LearningStatus{
		Active: true,
		View: api.SessionView{
			Session: api.TutoringSession{
				State: "RouteActive", Focus: api.FocusContext{RouteStepID: "step-2"},
			},
			EstimatedActiveTime: api.ActiveTimeEstimate{DurationSeconds: 1920, Estimated: true},
			WorkItem: &api.SessionWorkItem{
				GoalRevision: &api.GoalRevision{Text: "掌握图论基础\x1b[2J并完成练习"},
				RouteRevision: &api.RouteRevision{Steps: []api.RouteStep{
					{RouteStepID: "step-1", TeachingIntent: "理解顶点与边"},
					{RouteStepID: "step-2", TeachingIntent: "掌握图的遍历"},
					{RouteStepID: "step-3", TeachingIntent: "应用最短路径"},
				}},
				Activity: &api.Activity{Type: "practice", Difficulty: 2},
			},
		},
	}}
	value := newModel(t.Context(), conversation, "test-model")
	updated, _ := value.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	value = updated.(model)
	command := loadLearningStatusCmd(value.ctx, value.session)
	if command == nil {
		t.Fatal("learning status command is nil")
	}
	updated, _ = value.Update(command())
	value = updated.(model)
	view := value.View()
	for _, expected := range []string{"学习概览", "AGENT", "当前学习", "掌握图论基础", "路线学习中", "2/3", "掌握图的遍历", "练习 · 难度2", "约 32 分钟", "Ctrl+R 刷新"} {
		if !strings.Contains(view, expected) {
			t.Fatalf("sidebar missing %q: %s", expected, view)
		}
	}
	if strings.ContainsAny(view, "\x1b\r\a") || strings.Contains(view, "step-2") || value.sidebarWidth == 0 || value.viewport.Width < sidebarMinMainWidth {
		t.Fatalf("unsafe or invalid sidebar layout: sidebar=%d main=%d view=%q", value.sidebarWidth, value.viewport.Width, view)
	}
	for _, line := range strings.Split(view, "\n") {
		if lipgloss.Width(line) > 120 {
			t.Fatalf("line width=%d limit=120 line=%q", lipgloss.Width(line), line)
		}
	}

	updated, refresh := value.Update(tea.KeyMsg{Type: tea.KeyCtrlR})
	value = updated.(model)
	if refresh == nil || !value.learningLoading {
		t.Fatalf("Ctrl+R did not start refresh: loading=%t", value.learningLoading)
	}
	updated, _ = value.Update(refresh())
	value = updated.(model)
	if conversation.calls != 2 || !value.learningLoaded {
		t.Fatalf("refresh calls=%d loaded=%t", conversation.calls, value.learningLoaded)
	}

	conversation.err = errors.New("server unavailable")
	updated, refresh = value.Update(tea.KeyMsg{Type: tea.KeyCtrlR})
	value = updated.(model)
	updated, _ = value.Update(refresh())
	value = updated.(model)
	if !value.learningFailed || strings.Contains(value.View(), "掌握图论基础") || !strings.Contains(value.View(), "当前状态暂不可用") {
		t.Fatalf("failed refresh retained stale learning data: %s", value.View())
	}
}

func TestAgentUISidebarCollapsesBeforeMainTranscriptGetsTooNarrow(t *testing.T) {
	conversation := &learningStatusConversation{status: agentloop.LearningStatus{
		Active: true,
		View: api.SessionView{
			Session:             api.TutoringSession{State: "RouteActive", Focus: api.FocusContext{RouteStepID: "step-2"}},
			EstimatedActiveTime: api.ActiveTimeEstimate{DurationSeconds: 1920, Estimated: true},
			WorkItem: &api.SessionWorkItem{
				GoalRevision:  &api.GoalRevision{Text: "掌握图论基础并完成一组综合练习"},
				RouteRevision: &api.RouteRevision{Steps: []api.RouteStep{{RouteStepID: "step-1"}, {RouteStepID: "step-2"}, {RouteStepID: "step-3"}}},
				Activity:      &api.Activity{Type: "practice", Difficulty: 2},
			},
		},
	}}
	value := newModel(t.Context(), conversation, "model")
	updated, _ := value.Update(loadLearningStatusCmd(value.ctx, value.session)())
	value = updated.(model)
	updated, _ = value.Update(tea.WindowSizeMsg{Width: 91, Height: 24})
	value = updated.(model)
	if value.sidebarWidth != 0 || strings.Contains(value.View(), "学习概览") {
		t.Fatalf("sidebar did not collapse: width=%d view=%s", value.sidebarWidth, value.View())
	}
	updated, _ = value.Update(tea.WindowSizeMsg{Width: 92, Height: minimumHeight})
	value = updated.(model)
	view := value.View()
	if value.sidebarWidth < sidebarMinWidth || value.viewport.Width < sidebarMinMainWidth || !strings.Contains(view, "学习概览") ||
		!strings.Contains(view, "练习 · 难度2") || !strings.Contains(view, "约 32 分钟") {
		t.Fatalf("compact sidebar omitted core status: sidebar=%d main=%d view=%s", value.sidebarWidth, value.viewport.Width, view)
	}
	lines := strings.Split(view, "\n")
	if len(lines) > minimumHeight {
		t.Fatalf("sidebar minimum-height view has %d lines: %s", len(lines), view)
	}
	for _, line := range lines {
		if lipgloss.Width(line) > 92 {
			t.Fatalf("sidebar minimum-height line width=%d: %q", lipgloss.Width(line), line)
		}
	}
}

func TestAgentUIPlacesMetadataBelowDynamicComposer(t *testing.T) {
	conversation := &contextAwareConversation{status: agentloop.ContextStatus{
		Estimated: true, WindowPercent: 54, RecentCompleteTurns: 6, MemoryItemCount: 18,
	}}
	value := newModel(t.Context(), conversation, "test-model")
	updated, _ := value.Update(tea.WindowSizeMsg{Width: 96, Height: 30})
	value = updated.(model)
	value.input.SetValue("解释图论\n给练习")
	value.resize()
	view := value.View()

	header := strings.Split(view, "\n")[0]
	for _, forbidden := range []string{"模型 test-model", "上下文", "就绪"} {
		if strings.Contains(header, forbidden) {
			t.Fatalf("header still contains %q: %s", forbidden, header)
		}
	}
	composerEnd := strings.Index(view, "╰")
	metadata := strings.Index(view, "上下文约 54%")
	if composerEnd < 0 || metadata <= composerEnd || !strings.Contains(view, "╭─ 消息") ||
		!strings.Contains(view, "› 解释图论") || !strings.Contains(view, "8/8000") {
		t.Fatalf("composer/footer hierarchy is incomplete: %s", view)
	}
}

func TestAgentUIComposerGrowsAndShrinksWithContent(t *testing.T) {
	value := newModel(t.Context(), &fakeConversation{}, "model")
	baseRows, baseViewport := value.input.Height(), value.viewport.Height
	value.input.SetValue(strings.Repeat("多行内容\n", 10))
	value.resize()
	if value.input.Height() != composerMaxRows || value.viewport.Height >= baseViewport {
		t.Fatalf("expanded rows=%d viewport=%d baseRows=%d baseViewport=%d", value.input.Height(), value.viewport.Height, baseRows, baseViewport)
	}
	value.input.Reset()
	value.resize()
	if value.input.Height() != composerMinRows || value.viewport.Height != baseViewport {
		t.Fatalf("collapsed rows=%d viewport=%d baseViewport=%d", value.input.Height(), value.viewport.Height, baseViewport)
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
	value = runTurn(t, value, command)
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
	value = runTurn(t, value, command)
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
	value = runTurn(t, value, command)
	updated, command = value.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")})
	value = updated.(model)
	value = runTurn(t, value, command)
	view := value.View()
	if value.pending == nil || !value.pending.RetryOnly || !strings.Contains(view, "保存结果未知") || !strings.Contains(view, "Y 重试核对") || strings.Contains(view, "N 取消") {
		t.Fatalf("ambiguous preference state = pending=%+v view=%s", value.pending, view)
	}
	if _, decline := value.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")}); decline != nil || conversation.resolveCalls != 1 {
		t.Fatalf("ambiguous preference accepted decline: command=%v calls=%d", decline, conversation.resolveCalls)
	}
	updated, command = value.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")})
	value = updated.(model)
	value = runTurn(t, value, command)
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
	start, ok := command().(turnMsg)
	if !ok || start.stream == nil {
		t.Fatalf("turn stream did not start: %#v", start)
	}
	updated, wait := value.Update(start)
	value = updated.(model)
	if wait == nil {
		t.Fatal("turn stream wait command missing")
	}
	result := make(chan tea.Msg, 1)
	go func() { result <- wait() }()
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
		if turn, ok := message.(turnMsg); !ok || !turn.done || turn.err == nil {
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
	value = runTurn(t, value, command)
	view := value.View()
	if strings.ContainsAny(view, "\x1b\r\a") || !strings.Contains(view, "�") || !strings.Contains(value.viewport.View(), "原因") {
		t.Fatalf("untrusted controls reached terminal: %q", view)
	}
}

func TestAgentUIChromeSanitizesComposerAndSingleLineMetadata(t *testing.T) {
	value := newModel(t.Context(), &fakeConversation{}, "model\n伪造状态\tname")
	value.status = "就绪\n伪造状态"
	value.input.SetValue("正常输入\x1b[2J\t文本\u202e")
	updated, _ := value.Update(tea.WindowSizeMsg{Width: minimumWidth, Height: minimumHeight})
	value = updated.(model)
	view := value.View()
	if strings.ContainsAny(view, "\x1b\r\a\t") || strings.ContainsRune(view, '\u202e') ||
		strings.ContainsAny(value.input.Value(), "\x1b\r\a\t") || strings.ContainsRune(value.input.Value(), '\u202e') {
		t.Fatalf("unsafe chrome/composer content reached terminal: %q", view)
	}
	lines := strings.Split(view, "\n")
	if len(lines) > minimumHeight {
		t.Fatalf("line count=%d limit=%d view=%q", len(lines), minimumHeight, view)
	}
	for _, line := range lines {
		if lipgloss.Width(line) > minimumWidth {
			t.Fatalf("line width=%d limit=%d line=%q", lipgloss.Width(line), minimumWidth, line)
		}
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
	value = runTurn(t, value, command)
	view := value.View()
	if !strings.Contains(view, "Y 确认保存") || !strings.Contains(view, "PgUp/PgDn 滚动检查") || value.viewport.TotalLineCount() <= value.viewport.Height {
		t.Fatalf("long confirmation is not inspectable: lines=%d height=%d view=%s", value.viewport.TotalLineCount(), value.viewport.Height, view)
	}
	before := value.viewport.YOffset
	updated, _ = value.Update(tea.KeyMsg{Type: tea.KeyCtrlUp})
	value = updated.(model)
	if value.viewport.YOffset >= before || conversation.resolveCalls != 0 {
		t.Fatalf("scroll failed or resolved unexpectedly: before=%d after=%d calls=%d", before, value.viewport.YOffset, conversation.resolveCalls)
	}
}

func TestAgentUIStreamsThinkingAndToolActivityIntoTranscript(t *testing.T) {
	toolDone := agentloop.Event{ID: "call-1", Tool: "search_knowledge", Summary: "检索到 2 条知识片段", Status: agentloop.EventSucceeded}
	conversation := &fakeConversation{
		activities: []agentloop.Activity{
			{Kind: agentloop.ActivityThinking, Event: agentloop.Event{ID: "thinking-1", Summary: "正在分析问题", Status: agentloop.EventRunning}},
			{Kind: agentloop.ActivityThinking, Event: agentloop.Event{ID: "thinking-1", Summary: "已确定下一步工具操作", Status: agentloop.EventSucceeded}},
			{Kind: agentloop.ActivityTool, Event: agentloop.Event{ID: "call-1", Tool: "search_knowledge", Summary: "正在检索服务端知识库", Status: agentloop.EventRunning}},
			{Kind: agentloop.ActivityTool, Event: toolDone},
			{Kind: agentloop.ActivityThinking, Event: agentloop.Event{ID: "thinking-2", Summary: "正在结合工具结果继续分析", Status: agentloop.EventRunning}},
			{Kind: agentloop.ActivityThinking, Event: agentloop.Event{ID: "thinking-2", Summary: "已完成回答组织", Status: agentloop.EventSucceeded}},
		},
		result: agentloop.Result{Text: "这是结合知识库后的回答。", Events: []agentloop.Event{toolDone}},
	}
	value := newModel(t.Context(), conversation, "model")
	updated, _ := value.Update(tea.WindowSizeMsg{Width: 88, Height: 42})
	value = updated.(model)
	value.input.SetValue("解释图论")
	updated, command := value.Update(tea.KeyMsg{Type: tea.KeyEnter})
	value = updated.(model)
	start := command().(turnMsg)
	updated, wait := value.Update(start)
	value = updated.(model)
	updated, next := value.Update(wait())
	value = updated.(model)
	if !value.busy || !strings.Contains(value.viewport.View(), "思考中") || !strings.Contains(value.viewport.View(), "正在分析问题") {
		t.Fatalf("live thinking state missing: %s", value.viewport.View())
	}
	value = runTurn(t, value, next)
	transcript := value.viewport.View()
	for _, expected := range []string{"已思考", "工具调用 · 1 项", "检索知识库", "检索到 2 条知识片段", "这是结合知识库后的回答"} {
		if !strings.Contains(transcript, expected) {
			t.Fatalf("transcript missing %q: %s", expected, transcript)
		}
	}
	if strings.Count(transcript, "检索到 2 条知识片段") != 1 || strings.Contains(transcript, `{"query"`) {
		t.Fatalf("tool lifecycle duplicated or leaked arguments: %s", transcript)
	}
}

func TestAgentUIToolTimelineShowsStatusAndDetails(t *testing.T) {
	conversation := &fakeConversation{result: agentloop.Result{
		Text: "已完成检查。",
		Events: []agentloop.Event{
			{Tool: "search_knowledge", Summary: "检索到 2 条知识片段", Status: agentloop.EventSucceeded},
			{Tool: "get_due_reviews", Summary: "复习任务不可用", Status: agentloop.EventFailed, Detail: "protocol_error"},
		},
	}}
	value := newModel(t.Context(), conversation, "model")
	value.input.SetValue("检查工具")
	updated, command := value.Update(tea.KeyMsg{Type: tea.KeyEnter})
	value = updated.(model)
	value = runTurn(t, value, command)
	collapsed := value.viewport.View()
	if !strings.Contains(collapsed, "工具调用 · 2 项") || !strings.Contains(collapsed, "检索知识库") || !strings.Contains(collapsed, "复习任务不可用") || strings.Contains(collapsed, "protocol_error") {
		t.Fatalf("collapsed tool timeline=%s", collapsed)
	}
	updated, _ = value.Update(tea.KeyMsg{Type: tea.KeyCtrlO})
	value = updated.(model)
	expanded := value.viewport.View()
	if !strings.Contains(expanded, "search_knowledge") || !strings.Contains(expanded, "protocol_error") || !strings.Contains(expanded, "状态：失败") {
		t.Fatalf("expanded tool timeline=%s", expanded)
	}
}

func TestAgentUIScrollPauseShowsNewContentIndicator(t *testing.T) {
	value := newModel(t.Context(), &fakeConversation{}, "model")
	value.entries = append(value.entries, transcriptEntry{kind: entryAssistant, text: strings.Repeat("一行内容\n", 80)})
	value.refreshTranscript(true)
	updated, _ := value.Update(tea.KeyMsg{Type: tea.KeyCtrlUp})
	value = updated.(model)
	if value.follow {
		t.Fatal("scrolling up did not pause follow mode")
	}
	before := value.viewport.YOffset
	updated, _ = value.Update(turnMsg{kind: turnSend, result: agentloop.Result{Text: "这是新回复"}, done: true})
	value = updated.(model)
	if !value.hasNewContent || value.viewport.YOffset != before || !strings.Contains(value.View(), "有新消息") {
		t.Fatalf("follow=%t new=%t before=%d after=%d view=%s", value.follow, value.hasNewContent, before, value.viewport.YOffset, value.View())
	}
	updated, _ = value.Update(tea.KeyMsg{Type: tea.KeyCtrlG})
	value = updated.(model)
	if value.hasNewContent || !value.follow || !value.viewport.AtBottom() {
		t.Fatalf("Ctrl+G did not restore follow mode: follow=%t new=%t bottom=%t", value.follow, value.hasNewContent, value.viewport.AtBottom())
	}
}

func TestAgentUIKeepsStructuredErrorCard(t *testing.T) {
	conversation := &fakeConversation{sendErr: errors.New("provider unavailable")}
	value := newModel(t.Context(), conversation, "model")
	value.input.SetValue("测试错误")
	updated, command := value.Update(tea.KeyMsg{Type: tea.KeyEnter})
	value = updated.(model)
	value = runTurn(t, value, command)
	transcript := value.viewport.View()
	if value.busy || value.status != "请求失败" || !strings.Contains(transcript, "请求失败") || !strings.Contains(transcript, "阶段：模型请求") || !strings.Contains(transcript, "provider unavailable") {
		t.Fatalf("error state busy=%t status=%q transcript=%s", value.busy, value.status, transcript)
	}
}

type contextAwareConversation struct {
	fakeConversation
	status  agentloop.ContextStatus
	updates chan agentloop.ContextEvent
}

func (c *contextAwareConversation) ContextStatus() agentloop.ContextStatus        { return c.status }
func (c *contextAwareConversation) ContextUpdates() <-chan agentloop.ContextEvent { return c.updates }

func TestAgentUIContextHeaderUpdatesAndStructuredCards(t *testing.T) {
	conversation := &contextAwareConversation{
		status:  agentloop.ContextStatus{Estimated: true, WindowPercent: 54, RecentCompleteTurns: 6, MemoryItemCount: 18, Mode: agentloop.ContextCompactionAuto},
		updates: make(chan agentloop.ContextEvent, 4),
	}
	value := newModel(t.Context(), conversation, "model")
	updatedModel, _ := value.Update(tea.WindowSizeMsg{Width: 96, Height: 48})
	value = updatedModel.(model)
	if view := value.View(); !strings.Contains(view, "上下文 约54%") || !strings.Contains(view, "最近 6 轮") || !strings.Contains(view, "记忆 18 条") {
		t.Fatalf("initial context status missing: %s", view)
	}
	conversation.updates <- agentloop.ContextEvent{
		Kind:   agentloop.ContextEventStatus,
		Status: agentloop.ContextStatus{Estimated: true, WindowPercent: 63, RecentCompleteTurns: 5, MemoryItemCount: 20},
	}
	message := waitContextCmd(value.ctx, conversation.updates)().(contextMsg)
	updated, next := value.Update(message)
	value = updated.(model)
	view := value.View()
	if next == nil || !strings.Contains(view, "上下文 约63%") || !strings.Contains(view, "最近 5 轮") || !strings.Contains(view, "记忆 20 条") || strings.Contains(value.viewport.View(), "上下文已整理") {
		t.Fatalf("routine status should update chrome only: %s", view)
	}

	for _, event := range []agentloop.ContextEvent{
		{Kind: agentloop.ContextEventCompacted, Code: "context_compacted", DroppedTurns: 9, RecentTurns: 6, ObservationCount: 14, ReflectionCount: 3},
		{Kind: agentloop.ContextEventDegraded, Code: agentloop.ContextCompactionDegraded, DroppedTurns: 4},
		{Kind: agentloop.ContextEventSourceUnavailable, Code: agentloop.ContextSourceUnavailable},
	} {
		event.Status = value.contextStatus
		updated, _ = value.Update(contextMsg{event: event, stream: conversation.updates})
		value = updated.(model)
	}
	transcript := value.viewport.View()
	for _, expected := range []string{"上下文已整理", "已整理较早 9 轮", "上下文整理降级", agentloop.ContextCompactionDegraded, "会话证据来源不可用", agentloop.ContextSourceUnavailable} {
		if !strings.Contains(transcript, expected) {
			t.Fatalf("context transcript missing %q: %s", expected, transcript)
		}
	}
	for _, forbidden := range []string{"SECRET", `{"memory_id"`, "Observer prompt", "raw arguments"} {
		if strings.Contains(transcript, forbidden) {
			t.Fatalf("context card leaked %q: %s", forbidden, transcript)
		}
	}
}

func TestAgentUIContextCardsRespectScrollPause(t *testing.T) {
	conversation := &contextAwareConversation{updates: make(chan agentloop.ContextEvent, 1)}
	value := newModel(t.Context(), conversation, "model")
	value.entries = append(value.entries, transcriptEntry{kind: entryAssistant, text: strings.Repeat("历史内容\n", 80)})
	value.refreshTranscript(true)
	updated, _ := value.Update(tea.KeyMsg{Type: tea.KeyCtrlUp})
	value = updated.(model)
	before := value.viewport.YOffset
	updated, _ = value.Update(contextMsg{event: agentloop.ContextEvent{
		Kind: agentloop.ContextEventCompacted, DroppedTurns: 5, RecentTurns: 3,
	}, stream: conversation.updates})
	value = updated.(model)
	if !value.hasNewContent || value.viewport.YOffset != before || !strings.Contains(value.View(), "有新消息") {
		t.Fatalf("context card ignored paused scroll: before=%d after=%d view=%s", before, value.viewport.YOffset, value.View())
	}
}

func TestAgentUITypedContextErrorsHaveDistinctGuidance(t *testing.T) {
	budget := errorCardText(&agentloop.ContextError{Code: agentloop.ContextBudgetInvalid, Err: errors.New("fixed budget")}, false)
	turn := errorCardText(&agentloop.ContextError{Code: agentloop.ContextTurnTooLarge, Err: errors.New("large turn")}, false)
	recent := errorCardText(&agentloop.ContextError{Code: agentloop.ContextRecentTurnsTooLarge, Err: errors.New("large recent turns")}, false)
	if !strings.Contains(budget, "恢复自动整理") || !strings.Contains(turn, "当前这一轮本身过大") ||
		!strings.Contains(recent, "最近完整轮次无法同时保留") || budget == turn || turn == recent {
		t.Fatalf("typed guidance budget=%q turn=%q recent=%q", budget, turn, recent)
	}
	legacy := errorCardText(errors.New("context overflow"), false)
	if !strings.Contains(legacy, "对话上下文") || !strings.Contains(legacy, "开启新会话") {
		t.Fatalf("legacy fallback=%q", legacy)
	}
}

func TestAgentUIRecallDisplayName(t *testing.T) {
	if got := toolDisplayName("recall_session_memory"); got != "回查会话证据" {
		t.Fatalf("display name=%q", got)
	}
}
func TestRenderMarkdownSupportsLearningAnswerStructure(t *testing.T) {
	rendered := renderMarkdown("## 结论\n\n- 第一项\n- 第二项\n\n> 关键提示\n\n```go\nfmt.Println(\"ok\")\n```", 60)
	for _, expected := range []string{"结论", "• 第一项", "• 第二项", "关键提示", "go", "fmt.Println"} {
		if !strings.Contains(rendered, expected) {
			t.Fatalf("rendered markdown missing %q: %s", expected, rendered)
		}
	}
}

func TestAgentUIMinimumTerminalWidthStaysBounded(t *testing.T) {
	value := newModel(t.Context(), &fakeConversation{}, "a-very-long-model-name-that-must-be-truncated")
	updated, _ := value.Update(tea.WindowSizeMsg{Width: minimumWidth, Height: minimumHeight})
	value = updated.(model)
	value.busy = true
	value.status = "正在保存长期偏好"
	view := value.View()
	for _, line := range strings.Split(view, "\n") {
		if lipgloss.Width(line) > minimumWidth {
			t.Fatalf("line width=%d limit=%d line=%q", lipgloss.Width(line), minimumWidth, line)
		}
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
