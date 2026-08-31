package agentui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode"

	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/agentloop"
)

const (
	minimumWidth     = 46
	minimumHeight    = 18
	maximumWidth     = 108
	turnStreamBuffer = 128
)

type Conversation interface {
	Send(context.Context, string) (agentloop.Result, error)
	ResolvePreference(context.Context, bool) (agentloop.Result, error)
}

type contextConversation interface {
	ContextStatus() agentloop.ContextStatus
	ContextUpdates() <-chan agentloop.ContextEvent
}

type Runner struct {
	In        io.Reader
	Out       io.Writer
	Session   Conversation
	ModelName string
}

func (r Runner) Run(ctx context.Context) error {
	if r.Session == nil {
		return fmt.Errorf("agent session is not configured")
	}
	initial := newModel(ctx, r.Session, r.ModelName)
	defer initial.cancel()
	program := tea.NewProgram(initial, tea.WithAltScreen(), tea.WithInput(r.In), tea.WithOutput(r.Out), tea.WithContext(ctx))
	_, err := program.Run()
	return err
}

type turnKind string

const (
	turnSend       turnKind = "send"
	turnPreference turnKind = "preference"
)

type turnMsg struct {
	kind     turnKind
	result   agentloop.Result
	err      error
	activity *agentloop.Activity
	stream   <-chan turnMsg
	done     bool
}

type contextMsg struct {
	event  agentloop.ContextEvent
	stream <-chan agentloop.ContextEvent
}

type learningMsg struct {
	status agentloop.LearningStatus
	err    error
}

type model struct {
	ctx                    context.Context
	cancel                 context.CancelFunc
	session                Conversation
	modelName              string
	width                  int
	height                 int
	contentWidth           int
	sidebarWidth           int
	viewport               viewport.Model
	input                  textarea.Model
	entries                []transcriptEntry
	pending                *agentloop.PreferenceConfirmation
	busy                   bool
	status                 string
	follow                 bool
	hasNewContent          bool
	toolsExpanded          bool
	shownEventKeys         map[string]struct{}
	contextStatus          agentloop.ContextStatus
	contextUpdates         <-chan agentloop.ContextEvent
	learningStatus         agentloop.LearningStatus
	learningLoaded         bool
	learningLoading        bool
	learningRefreshPending bool
	learningFailed         bool
	learningProvider       bool
}

func newModel(ctx context.Context, session Conversation, modelName string) model {
	sessionCtx, cancel := context.WithCancel(ctx)
	input := textarea.New()
	input.Placeholder = "输入学习问题；Agent 会按需读取知识、进度和长期偏好"
	input.Prompt = "› "
	input.ShowLineNumbers = false
	input.CharLimit = 8000
	input.MaxHeight = composerMaxRows
	input.FocusedStyle.Prompt = composerPromptStyle
	input.FocusedStyle.Placeholder = mutedStyle
	input.BlurredStyle.Prompt = mutedStyle
	input.BlurredStyle.Placeholder = mutedStyle
	input.KeyMap.InsertNewline.SetKeys("ctrl+j", "alt+enter")
	input.Focus()
	view := viewport.New(80, 14)
	value := model{
		ctx: sessionCtx, cancel: cancel, session: session, modelName: safeSingleLineTerminalText(modelName), width: 80, height: 24,
		viewport: view, input: input, status: "就绪", follow: true, shownEventKeys: map[string]struct{}{},
		entries: []transcriptEntry{{kind: entryNotice, text: "可以直接提问，也可以让我结合服务端知识库、学习进度和长期偏好帮助你学习。"}},
	}
	if contextSession, ok := session.(contextConversation); ok {
		value.contextStatus = contextSession.ContextStatus()
		value.contextUpdates = contextSession.ContextUpdates()
	}
	if _, ok := session.(learningConversation); ok {
		value.learningProvider = true
		value.learningLoading = true
	}
	value.resize()
	value.refreshTranscript(false)
	return value
}

func (m model) Init() tea.Cmd {
	return tea.Batch(textarea.Blink, waitContextCmd(m.ctx, m.contextUpdates), loadLearningStatusCmd(m.ctx, m.session))
}

func (m model) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := message.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.resize()
		m.refreshTranscript(false)
		return m, nil
	case turnMsg:
		if msg.stream != nil && msg.activity == nil && !msg.done {
			return m, waitTurnCmd(msg.stream)
		}
		if msg.activity != nil {
			m.handleActivity(*msg.activity)
			m.refreshTranscript(true)
			return m, waitTurnCmd(msg.stream)
		}
		m.busy = false
		if msg.err != nil {
			m.handleTurnError(msg.err)
		} else {
			m.handleTurnResult(msg.result)
		}
		if m.pending == nil {
			m.input.Focus()
		} else {
			m.input.Blur()
		}
		m.resize()
		m.refreshTranscript(true)
		learningCmd := m.startLearningRefresh()
		return m, tea.Batch(textarea.Blink, learningCmd)
	case learningMsg:
		m.learningLoading = false
		m.learningLoaded = msg.err == nil
		m.learningFailed = msg.err != nil
		if msg.err == nil {
			m.learningStatus = msg.status
		} else {
			m.learningStatus = agentloop.LearningStatus{}
		}
		if m.learningRefreshPending {
			m.learningRefreshPending = false
			return m, m.startLearningRefresh()
		}
		return m, nil
	case contextMsg:
		m.contextStatus = msg.event.Status
		if msg.event.Kind == agentloop.ContextEventCompacted || msg.event.Kind == agentloop.ContextEventDegraded || msg.event.Kind == agentloop.ContextEventSourceUnavailable {
			m.entries = append(m.entries, transcriptEntry{kind: entryContext, contextEvent: msg.event})
			m.refreshTranscript(true)
		}
		return m, waitContextCmd(m.ctx, msg.stream)
	case tea.KeyMsg:
		key := msg.String()
		if key == "ctrl+c" || key == "ctrl+q" || key == "esc" && !m.busy && m.pending == nil && strings.TrimSpace(m.input.Value()) == "" {
			m.cancel()
			return m, tea.Quit
		}
		if m.terminalTooSmall() {
			return m, nil
		}
		if key == "ctrl+r" && m.sidebarWidth > 0 {
			return m, m.startLearningRefresh()
		}
		if m.handleNavigationKey(key) {
			return m, nil
		}
		if m.pending != nil {
			if m.busy {
				return m, nil
			}
			switch key {
			case "y", "Y":
				m.busy, m.status = true, "正在保存长期偏好"
				return m, resolvePreferenceCmd(m.ctx, m.session, true)
			case "n", "N", "esc":
				if m.pending.RetryOnly {
					return m, nil
				}
				m.busy, m.status = true, "正在取消保存"
				return m, resolvePreferenceCmd(m.ctx, m.session, false)
			}
			return m, nil
		}
		if m.busy {
			return m, nil
		}
		switch key {
		case "enter":
			m.sanitizeComposer()
			input := strings.TrimSpace(m.input.Value())
			if input == "" {
				return m, nil
			}
			m.entries = append(m.entries, transcriptEntry{kind: entryUser, text: input})
			m.input.Reset()
			m.input.Blur()
			m.busy, m.status = true, "Agent 正在思考"
			m.follow, m.hasNewContent = true, false
			m.shownEventKeys = map[string]struct{}{}
			m.resize()
			m.refreshTranscript(true)
			return m, sendCmd(m.ctx, m.session, input)
		}
		previousHeight := m.input.Height()
		var command tea.Cmd
		m.input, command = m.input.Update(msg)
		m.resize()
		if m.input.Height() != previousHeight {
			m.refreshTranscript(false)
		}
		return m, command
	}
	return m, nil
}

func (m *model) handleActivity(activity agentloop.Activity) {
	event := activity.Event
	switch activity.Kind {
	case agentloop.ActivityThinking:
		m.entries = upsertThinkingActivity(m.entries, event)
	case agentloop.ActivityTool:
		m.entries = upsertToolEvent(m.entries, event)
		if event.Status != agentloop.EventRunning {
			m.shownEventKeys[eventKey(event)] = struct{}{}
		}
	default:
		return
	}
	switch event.Status {
	case agentloop.EventRunning:
		m.status = event.Summary
	case agentloop.EventConfirmationRequired:
		m.status = "等待确认"
	case agentloop.EventFailed, agentloop.EventInvalid, agentloop.EventOutcomeUnknown:
		m.status = "Agent 遇到异常"
	default:
		m.status = "Agent 正在继续处理"
	}
}

func (m *model) handleTurnError(err error) {
	if errors.Is(err, agentloop.ErrPreferenceOutcomeUnknown) && m.pending != nil {
		m.pending.RetryOnly = true
		m.entries = updatePreferenceToolStatus(m.entries, agentloop.EventOutcomeUnknown, "长期偏好保存结果未知", "outcome_unknown")
		for index := len(m.entries) - 1; index >= 0; index-- {
			if m.entries[index].kind == entryConfirm {
				m.entries[index].text = preferenceConfirmationText(m.pending)
				break
			}
		}
		m.status = "保存结果待核对"
	} else {
		if m.pending != nil {
			m.entries = updatePreferenceToolStatus(m.entries, agentloop.EventFailed, "长期偏好未保存", "request_rejected")
		}
		m.status = "请求失败"
	}
	m.entries = append(m.entries, transcriptEntry{kind: entryError, text: errorCardText(err, m.pending != nil)})
}

func (m *model) handleTurnResult(result agentloop.Result) {
	newEvents := make([]agentloop.Event, 0, len(result.Events))
	for _, event := range result.Events {
		key := eventKey(event)
		if _, shown := m.shownEventKeys[key]; shown {
			continue
		}
		m.shownEventKeys[key] = struct{}{}
		newEvents = append(newEvents, event)
	}
	m.entries = appendToolEvents(m.entries, newEvents)
	if strings.TrimSpace(result.Text) != "" {
		m.entries = append(m.entries, transcriptEntry{kind: entryAssistant, text: result.Text})
	}
	m.pending = result.Pending
	if m.pending != nil {
		m.entries = append(m.entries, transcriptEntry{kind: entryConfirm, text: preferenceConfirmationText(m.pending)})
		m.status = "等待确认"
		return
	}
	m.status = "就绪"
}

func (m *model) handleNavigationKey(key string) bool {
	switch key {
	case "ctrl+o":
		m.toolsExpanded = !m.toolsExpanded
		m.refreshTranscript(false)
		return true
	case "pgup":
		m.viewport.PageUp()
		m.updateFollowAfterScroll()
		return true
	case "ctrl+up":
		m.viewport.LineUp(3)
		m.updateFollowAfterScroll()
		return true
	case "home", "ctrl+home":
		m.viewport.GotoTop()
		m.follow = false
		return true
	case "pgdown":
		m.viewport.PageDown()
		m.updateFollowAfterScroll()
		return true
	case "ctrl+down":
		m.viewport.LineDown(3)
		m.updateFollowAfterScroll()
		return true
	case "end", "ctrl+g":
		m.viewport.GotoBottom()
		m.follow, m.hasNewContent = true, false
		return true
	default:
		return false
	}
}

func (m *model) updateFollowAfterScroll() {
	m.follow = m.viewport.AtBottom()
	if m.follow {
		m.hasNewContent = false
	}
}

func sendCmd(ctx context.Context, session Conversation, input string) tea.Cmd {
	return startTurnCmd(ctx, turnSend, func(turnCtx context.Context) (agentloop.Result, error) {
		return session.Send(turnCtx, input)
	})
}

func resolvePreferenceCmd(ctx context.Context, session Conversation, approved bool) tea.Cmd {
	return startTurnCmd(ctx, turnPreference, func(turnCtx context.Context) (agentloop.Result, error) {
		return session.ResolvePreference(turnCtx, approved)
	})
}

func startTurnCmd(ctx context.Context, kind turnKind, run func(context.Context) (agentloop.Result, error)) tea.Cmd {
	return func() tea.Msg {
		stream := make(chan turnMsg, turnStreamBuffer)
		go func() {
			turnCtx := agentloop.WithActivityReporter(ctx, func(activity agentloop.Activity) {
				value := activity
				select {
				case stream <- turnMsg{kind: kind, activity: &value, stream: stream}:
				case <-ctx.Done():
				}
			})
			result, err := run(turnCtx)
			stream <- turnMsg{kind: kind, result: result, err: err, stream: stream, done: true}
			close(stream)
		}()
		return turnMsg{kind: kind, stream: stream}
	}
}

func waitTurnCmd(stream <-chan turnMsg) tea.Cmd {
	return func() tea.Msg {
		message, ok := <-stream
		if !ok {
			return turnMsg{done: true, err: errors.New("Agent 状态流意外关闭")}
		}
		return message
	}
}

func waitContextCmd(ctx context.Context, stream <-chan agentloop.ContextEvent) tea.Cmd {
	if stream == nil {
		return nil
	}
	return func() tea.Msg {
		select {
		case event := <-stream:
			return contextMsg{event: event, stream: stream}
		case <-ctx.Done():
			return nil
		}
	}
}

func (m *model) resize() {
	contentWidth := m.width - 6
	if contentWidth > maximumWidth {
		contentWidth = maximumWidth
	}
	if contentWidth < 20 {
		contentWidth = 20
	}
	m.contentWidth = contentWidth
	mainWidth, sidebarWidth := sidebarLayoutWidths(contentWidth)
	m.sidebarWidth = sidebarWidth
	m.sanitizeComposer()
	m.input.SetWidth(composerInnerWidth(mainWidth))
	m.input.SetHeight(m.composerInputRows(mainWidth))
	m.viewport.Width = mainWidth
	m.viewport.Height = m.height - m.input.Height() - 7
	if m.viewport.Height < 5 {
		m.viewport.Height = 5
	}
}

func (m *model) sanitizeComposer() {
	value := safeComposerText(m.input.Value())
	if value != m.input.Value() {
		m.input.SetValue(value)
	}
}

func (m *model) refreshTranscript(newContent bool) {
	width := max(20, m.viewport.Width)
	previousOffset := m.viewport.YOffset
	parts := make([]string, 0, len(m.entries))
	for _, entry := range m.entries {
		parts = append(parts, renderTranscriptEntry(entry, width, m.toolsExpanded))
	}
	m.viewport.SetContent(strings.Join(parts, "\n\n"))
	if m.follow {
		m.viewport.GotoBottom()
		m.follow, m.hasNewContent = true, false
		return
	}
	m.viewport.SetYOffset(previousOffset)
	if newContent {
		m.hasNewContent = true
	}
}

func (m model) View() string {
	if m.terminalTooSmall() {
		return smallTerminalView(m.width, m.height)
	}
	mainWidth := max(20, m.viewport.Width)
	contentWidth := max(mainWidth, m.contentWidth)
	main := lipgloss.JoinVertical(lipgloss.Left,
		m.viewport.View(),
		m.renderComposer(mainWidth),
		m.renderFooter(mainWidth),
	)
	if m.sidebarWidth > 0 {
		main = lipgloss.JoinHorizontal(lipgloss.Top,
			main,
			strings.Repeat(" ", sidebarGap),
			m.renderSidebar(m.sidebarWidth, lipgloss.Height(main)),
		)
	}
	body := lipgloss.JoinVertical(lipgloss.Left,
		m.renderHeader(contentWidth),
		dividerStyle.Render(strings.Repeat("─", contentWidth)),
		main,
	)
	return lipgloss.NewStyle().Width(m.width).Align(lipgloss.Center).Render(body)
}

func (m model) renderStatus() string {
	return m.renderStatusText(m.status)
}

func (m model) renderStatusText(status string) string {
	text := "● " + safeSingleLineTerminalText(status)
	switch {
	case m.busy:
		return statusBusyStyle.Render(text)
	case m.status == "请求失败" || m.status == "提交结果待核对" || m.status == "保存结果待核对" || m.status == "Agent 遇到异常":
		return statusErrorStyle.Render(text)
	default:
		return statusReadyStyle.Render(text)
	}
}

func (m model) compactStatus() string {
	switch {
	case m.busy:
		return "处理中"
	case m.status == "请求失败":
		return "失败"
	case m.status == "提交结果待核对" || m.status == "保存结果待核对":
		return "待核对"
	case m.pending != nil:
		return "待确认"
	default:
		return "就绪"
	}
}

func errorCardText(err error, pending bool) string {
	stage, suggestion := "Agent 请求", "可以调整问题后重试；错误卡片会保留在对话中。"
	if errors.Is(err, agentloop.ErrPreferenceOutcomeUnknown) {
		stage = "长期偏好提交"
		suggestion = "结果未知，不能取消；请按 Y 使用同一操作 ID 重试核对。"
	} else if pending {
		stage = "长期偏好提交"
		suggestion = "服务端未接受本次提交；可按 N 取消，或按 Y 再次尝试。"
	} else {
		var contextErr *agentloop.ContextError
		if errors.As(err, &contextErr) {
			stage = "对话上下文"
			switch contextErr.Code {
			case agentloop.ContextBudgetInvalid:
				suggestion = "固定系统规则、工具定义或完整历史超过安全预算；可恢复自动整理或开启新会话。"
			case agentloop.ContextTurnTooLarge:
				suggestion = "当前这一轮本身过大；请缩短输入或减少单轮工具结果后重试。"
			case agentloop.ContextRecentTurnsTooLarge:
				suggestion = "当前轮次与最近完整轮次无法同时保留；请开启新会话，或避免在连续轮次中返回超大工具结果。"
			default:
				suggestion = "上下文整理未完成；可缩短问题或开启新会话后重试。"
			}
		} else {
			message := strings.ToLower(err.Error())
			switch {
			case strings.Contains(message, "模型"), strings.Contains(message, "provider"), strings.Contains(message, "chat completion"):
				stage = "模型请求"
				suggestion = "检查模型配置或网络后重试。"
			case strings.Contains(message, "上下文"), strings.Contains(message, "context"):
				stage = "对话上下文"
				suggestion = "缩短问题或开启新会话后重试。"
			case strings.Contains(message, "工具"):
				stage = "Agent 工具"
				suggestion = "检查服务端状态后重试；已完成的工具结果仍保留。"
			}
		}
	}
	return fmt.Sprintf("阶段：%s\n原因：%s\n建议：%s", stage, safeTerminalText(err.Error()), suggestion)
}

func preferenceConfirmationText(pending *agentloop.PreferenceConfirmation) string {
	text := fmt.Sprintf(
		"将保存以下长期偏好：\n内容：%s\n原因：%s\n分类：%s\n敏感性：%s\n稳定性：%s",
		pending.Content,
		pending.Reason,
		preferenceValueLabel(pending.Category),
		preferenceValueLabel(pending.Sensitivity),
		preferenceValueLabel(pending.Stability),
	)
	if pending.RetryOnly {
		text += "\n提交结果未知：不能再取消，只能使用原操作ID重试核对。"
	}
	return text
}

func preferenceValueLabel(value string) string {
	labels := map[string]string{
		"interaction_preference": "交互偏好",
		"time_constraint":        "时间约束",
		"personal_context":       "个人学习背景",
		"non_sensitive":          "非敏感",
		"sensitive":              "敏感",
		"stable":                 "长期稳定",
		"transient":              "阶段性",
	}
	if label, ok := labels[value]; ok {
		return label + " (" + value + ")"
	}
	return value
}

func safeSingleLineTerminalText(value string) string {
	return strings.Map(func(current rune) rune {
		if current == '\n' || current == '\t' {
			return ' '
		}
		if unicode.IsControl(current) || current >= '\u202a' && current <= '\u202e' || current >= '\u2066' && current <= '\u2069' {
			return '�'
		}
		return current
	}, value)
}

func safeComposerText(value string) string {
	return strings.Map(func(current rune) rune {
		if current == '\n' {
			return current
		}
		if current == '\t' {
			return ' '
		}
		if unicode.IsControl(current) || current >= '\u202a' && current <= '\u202e' || current >= '\u2066' && current <= '\u2069' {
			return '�'
		}
		return current
	}, value)
}

func safeTerminalText(value string) string {
	return strings.Map(func(current rune) rune {
		if current == '\n' || current == '\t' {
			return current
		}
		if unicode.IsControl(current) || current >= '\u202a' && current <= '\u202e' || current >= '\u2066' && current <= '\u2069' {
			return '�'
		}
		return current
	}, value)
}

func (m model) terminalTooSmall() bool {
	return m.width < minimumWidth || m.height < minimumHeight
}

func smallTerminalView(width, height int) string {
	if width <= 0 || height <= 0 {
		return ""
	}
	lines := []string{"edu-agent", "", "终端尺寸过小", "请调整窗口后继续"}
	if len(lines) > height {
		lines = lines[:height]
	}
	for index, line := range lines {
		for lipgloss.Width(line) > width && len([]rune(line)) > 0 {
			runes := []rune(line)
			line = string(runes[:len(runes)-1])
		}
		lines[index] = line
	}
	return strings.Join(lines, "\n")
}
