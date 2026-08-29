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
	minimumWidth  = 46
	minimumHeight = 18
)

type Conversation interface {
	Send(context.Context, string) (agentloop.Result, error)
	ResolvePreference(context.Context, bool) (agentloop.Result, error)
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

type turnMsg struct {
	result agentloop.Result
	err    error
}

type chatEntry struct {
	role string
	text string
}

type model struct {
	ctx       context.Context
	cancel    context.CancelFunc
	session   Conversation
	modelName string
	width     int
	height    int
	viewport  viewport.Model
	input     textarea.Model
	entries   []chatEntry
	pending   *agentloop.PreferenceConfirmation
	busy      bool
	status    string
}

func newModel(ctx context.Context, session Conversation, modelName string) model {
	sessionCtx, cancel := context.WithCancel(ctx)
	input := textarea.New()
	input.Placeholder = "输入学习问题，Agent会按需读取知识、进度和长期偏好"
	input.Prompt = ""
	input.ShowLineNumbers = false
	input.CharLimit = 8000
	input.MaxHeight = 5
	input.KeyMap.InsertNewline.SetKeys("ctrl+j", "alt+enter")
	input.Focus()
	view := viewport.New(80, 14)
	value := model{
		ctx: sessionCtx, cancel: cancel, session: session, modelName: safeTerminalText(modelName), width: 80, height: 24,
		viewport: view, input: input, status: "就绪",
		entries: []chatEntry{{role: "system", text: "可以直接提问，也可以让我结合服务端知识库、学习进度和长期偏好帮助你学习。"}},
	}
	value.resize()
	value.refreshTranscript()
	return value
}

func (m model) Init() tea.Cmd { return textarea.Blink }

func (m model) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := message.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.resize()
		m.refreshTranscript()
		return m, nil
	case turnMsg:
		m.busy = false
		if msg.err != nil {
			m.entries = append(m.entries, chatEntry{role: "error", text: msg.err.Error()})
			if errors.Is(msg.err, agentloop.ErrPreferenceOutcomeUnknown) && m.pending != nil {
				m.pending.RetryOnly = true
				for index := len(m.entries) - 1; index >= 0; index-- {
					if m.entries[index].role == "confirm" {
						m.entries[index].text = preferenceConfirmationText(m.pending)
						break
					}
				}
				m.status = "提交结果待核对"
			} else {
				m.status = "请求失败"
			}
		} else {
			for _, event := range msg.result.Events {
				m.entries = append(m.entries, chatEntry{role: "tool", text: event.Summary})
			}
			if msg.result.Text != "" {
				m.entries = append(m.entries, chatEntry{role: "assistant", text: msg.result.Text})
			}
			m.pending = msg.result.Pending
			if m.pending != nil {
				m.entries = append(m.entries, chatEntry{role: "confirm", text: preferenceConfirmationText(m.pending)})
				m.status = "等待确认"
			} else {
				m.status = "就绪"
			}
		}
		m.input.Focus()
		m.refreshTranscript()
		return m, textarea.Blink
	case tea.KeyMsg:
		key := msg.String()
		if key == "ctrl+c" || key == "ctrl+q" || key == "esc" && !m.busy && m.pending == nil && strings.TrimSpace(m.input.Value()) == "" {
			m.cancel()
			return m, tea.Quit
		}
		if m.terminalTooSmall() {
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
			case "ctrl+up":
				m.viewport.LineUp(3)
				return m, nil
			case "ctrl+down":
				m.viewport.LineDown(3)
				return m, nil
			}
			return m, nil
		}
		if m.busy {
			return m, nil
		}
		switch key {
		case "enter":
			input := strings.TrimSpace(m.input.Value())
			if input == "" {
				return m, nil
			}
			m.entries = append(m.entries, chatEntry{role: "user", text: input})
			m.input.Reset()
			m.busy, m.status = true, "Agent正在思考"
			m.refreshTranscript()
			return m, sendCmd(m.ctx, m.session, input)
		case "ctrl+up":
			m.viewport.LineUp(3)
			return m, nil
		case "ctrl+down":
			m.viewport.LineDown(3)
			return m, nil
		}
		var command tea.Cmd
		m.input, command = m.input.Update(msg)
		return m, command
	}
	return m, nil
}

func sendCmd(ctx context.Context, session Conversation, input string) tea.Cmd {
	return func() tea.Msg {
		result, err := session.Send(ctx, input)
		return turnMsg{result: result, err: err}
	}
}

func resolvePreferenceCmd(ctx context.Context, session Conversation, approved bool) tea.Cmd {
	return func() tea.Msg {
		result, err := session.ResolvePreference(ctx, approved)
		return turnMsg{result: result, err: err}
	}
}

func (m *model) resize() {
	contentWidth := m.width - 6
	if contentWidth < 20 {
		contentWidth = 20
	}
	inputHeight := 4
	if m.height < 24 {
		inputHeight = 3
	}
	m.input.SetWidth(contentWidth)
	m.input.SetHeight(inputHeight)
	m.viewport.Width = contentWidth
	m.viewport.Height = m.height - inputHeight - 8
	if m.viewport.Height < 5 {
		m.viewport.Height = 5
	}
}

func (m *model) refreshTranscript() {
	width := m.viewport.Width
	if width < 20 {
		width = 20
	}
	parts := make([]string, 0, len(m.entries))
	for _, entry := range m.entries {
		var label string
		style := assistantStyle
		switch entry.role {
		case "user":
			label, style = "你", userStyle
		case "assistant":
			label = "Agent"
		case "tool":
			label, style = "工具", toolStyle
		case "confirm":
			label, style = "待确认偏好", confirmStyle
		case "error":
			label, style = "错误", errorStyle
		default:
			label, style = "提示", mutedStyle
		}
		entryWidth := width
		if entry.role == "confirm" {
			entryWidth -= 4
			if entryWidth < 20 {
				entryWidth = 20
			}
		}
		parts = append(parts, style.Width(entryWidth).Render(safeTerminalText(label)+"\n"+safeTerminalText(entry.text)))
	}
	m.viewport.SetContent(strings.Join(parts, "\n\n"))
	m.viewport.GotoBottom()
}

func (m model) View() string {
	if m.terminalTooSmall() {
		return smallTerminalView(m.width, m.height)
	}
	width := m.width - 2
	header := titleStyle.Render("edu-agent · AI学习助手")
	modelName := safeTerminalText(strings.TrimSpace(m.modelName))
	if modelName == "" {
		modelName = "未命名模型"
	}
	header += "  " + mutedStyle.Render("模型 "+modelName+" · "+m.status)

	footer := mutedStyle.Render("Enter发送  Ctrl+J换行  Ctrl+↑/↓滚动  Esc退出")
	if m.pending != nil {
		footerWidth := width - 8
		if footerWidth < 20 {
			footerWidth = 20
		}
		if m.pending.RetryOnly {
			footer = confirmStyle.Width(footerWidth).Render("上次提交结果未知；候选全文在上方，只能用同一操作ID重试核对。\nY 重试核对")
		} else {
			footer = confirmStyle.Width(footerWidth).Render("候选全文已显示在上方，可用 Ctrl+↑/↓滚动检查。\nY 确认保存   N 取消")
		}
	}
	body := lipgloss.JoinVertical(lipgloss.Left,
		lipgloss.NewStyle().Width(width).Padding(0, 2).Render(header),
		lipgloss.NewStyle().Width(width).Padding(1, 2).Render(m.viewport.View()),
		lipgloss.NewStyle().Width(width).Padding(0, 2).Render(m.input.View()),
		lipgloss.NewStyle().Width(width).Padding(0, 2).Render(footer),
	)
	return body
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

var (
	titleStyle     = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("6"))
	userStyle      = lipgloss.NewStyle().BorderLeft(true).BorderStyle(lipgloss.ThickBorder()).BorderForeground(lipgloss.Color("2")).PaddingLeft(1)
	assistantStyle = lipgloss.NewStyle().BorderLeft(true).BorderStyle(lipgloss.ThickBorder()).BorderForeground(lipgloss.Color("6")).PaddingLeft(1)
	toolStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("3"))
	errorStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("1"))
	mutedStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	confirmStyle   = lipgloss.NewStyle().Border(lipgloss.NormalBorder()).BorderForeground(lipgloss.Color("3")).Padding(0, 1)
)
