package workbench

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"
)

// 与 dashboard 使用相同的终端色号；使用独立 renderer，避免改变 Agent 的主题。
type styles struct {
	title, selected, muted, border, warning lipgloss.Style
}

// SetColorMode 由宿主传入已有配置；环境覆盖规则与 config.Resolve 保持一致。
func (m *Model) SetColorMode(mode string, out io.Writer) {
	if out == nil {
		out = os.Stdout
	}
	if override := strings.TrimSpace(os.Getenv("EDU_AGENT_COLOR")); override != "" {
		mode = override
	}
	r := lipgloss.NewRenderer(out)
	profile := r.ColorProfile()
	switch {
	case mode == "never" || os.Getenv("NO_COLOR") != "" || os.Getenv("TERM") == "dumb":
		profile = termenv.Ascii
	case mode == "always":
		profile = termenv.ANSI256
	}
	r.SetColorProfile(profile)
	r.SetHasDarkBackground(true)
	m.colorEnabled = profile != termenv.Ascii
	m.styles = styles{
		title:    r.NewStyle().Bold(true).Foreground(lipgloss.Color("6")),
		selected: r.NewStyle().Bold(true).Foreground(lipgloss.Color("2")),
		muted:    r.NewStyle().Foreground(lipgloss.Color("8")),
		border:   r.NewStyle().Foreground(lipgloss.Color("6")),
		warning:  r.NewStyle().Bold(true).Foreground(lipgloss.Color("3")),
	}
}

func cleanLines(value string) string {
	lines := strings.Split(value, "\n")
	for i := range lines {
		lines[i] = safeLine(lines[i])
	}
	return strings.Join(lines, "\n")
}

// 先转义每行中的外部控制字符，再按显示单元折行，保留正文自身的换行。
func (m *Model) renderContent() {
	content := m.data.Content
	if m.loadFailure != "" {
		content = "加载失败\n" + m.loadFailure + "\n\n当前页数据尚未加载，不能判断是否为空。\n按 r 重新读取；1–7 切页；Esc 返回。"
	} else if m.loading {
		content = "正在读取页面…"
	}
	m.view.SetContent(ansi.Hardwrap(cleanLines(content), m.view.Width, true))
}

// 3 行标题、1 行标签、3 行底栏始终保留；剩余空间分配给正文和操作。
func (m *Model) actionRows() int {
	return min(len(m.options()), max(3, (m.height-11)/2))
}

func (m *Model) bodyRows() int { return max(1, m.height-9) }

func (m *Model) layout() {
	width := max(1, m.width-4)
	contentRows := max(1, m.height-11-m.actionRows())
	m.view.Width, m.view.Height = width, contentRows
	m.dialog.Width, m.dialog.Height = width, m.bodyRows()
	m.leafView.Width, m.leafView.Height = width, m.bodyRows()
	if m.input.Width() != width {
		m.input.SetWidth(width)
	}
	inputRows := max(1, m.bodyRows()-2)
	if m.input.Height() != inputRows {
		m.input.SetHeight(inputRows)
	}
	m.renderContent()
	if m.confirm {
		m.dialog.SetContent(ansi.Hardwrap(cleanLines(m.action.Confirmation), width, true))
	}
}

func fit(value string, width int) string { return ansi.Truncate(value, max(1, width), "…") }

func (m *Model) panel(title, body string, rows int, warning bool) string {
	width := max(1, m.width-4)
	border, heading := m.styles.border, m.styles.title
	if warning {
		border, heading = m.styles.warning, m.styles.warning
	}
	label := fit(title, width)
	top := border.Render("╭─ ") + heading.Render(label) + border.Render(strings.Repeat("─", max(0, m.width-4-lipgloss.Width(label)))+"╮")
	lines := strings.Split(body, "\n")
	result := []string{top}
	for i := 0; i < rows; i++ {
		line := ""
		if i < len(lines) {
			line = fit(lines[i], width)
		}
		result = append(result, border.Render("│")+" "+line+strings.Repeat(" ", max(0, width-lipgloss.Width(line)))+" "+border.Render("│"))
	}
	return strings.Join(append(result, border.Render("╰"+strings.Repeat("─", max(0, m.width-2))+"╯")), "\n")
}

func (m *Model) activeTab() string {
	page, _, _ := strings.Cut(m.page, "/")
	switch page {
	case "collection", "shared", "import":
		return "materials"
	case "goal", "new-goal", "history":
		return "goals"
	case "session", "sessions":
		return "learn"
	case "global-overview", "goal-progress":
		return "overview"
	case "global-reviews":
		return "reviews"
	}
	if strings.HasPrefix(page, "study-") {
		return "learn"
	}
	return page
}

func (m *Model) tabs() string {
	labels := []string{"概览", "资料", "目标", "学习", "复习", "切区", "帮助"}
	parts := make([]string, len(labels))
	for i, label := range labels {
		text := fmt.Sprintf(" %d%s ", i+1, label)
		if m.activeTab() == navigation[i].ID {
			parts[i] = m.styles.selected.Render("[" + strings.TrimSpace(text) + "]")
		} else {
			parts[i] = m.styles.muted.Render(text)
		}
	}
	// 窄屏标签换为两行会挤掉内容；缩写后仍保留全部 1–7 入口。
	if lipgloss.Width(strings.Join(parts, "")) > m.width {
		for i := range parts {
			label := fmt.Sprintf(" %d ", i+1)
			if m.activeTab() == navigation[i].ID {
				parts[i] = m.styles.selected.Render("[" + fmt.Sprint(i+1) + "]")
			} else {
				parts[i] = m.styles.muted.Render(label)
			}
		}
	}
	return strings.Join(parts, "")
}

func (m *Model) actionPanel() string {
	items, rows := m.options(), m.actionRows()
	start := min(max(0, m.cursor-rows/2), max(0, len(items)-rows))
	end := min(len(items), start+rows)
	lines := make([]string, 0, rows)
	for i := start; i < end; i++ {
		label := fit(safeLine(items[i].Label), max(1, m.width-6))
		if i == m.cursor {
			lines = append(lines, m.styles.selected.Render("› "+label))
		} else {
			lines = append(lines, "  "+label)
		}
	}
	return m.panel(fmt.Sprintf("操作 %d–%d/%d", start+1, end, len(items)), strings.Join(lines, "\n"), rows, false)
}

func (m *Model) formPanel() string {
	label := m.action.Label
	if label == "" {
		label = m.action.Input
	}
	fields, choices := "正文", "Enter 换行 · 草稿在本进程保留"
	if len(m.action.Fields) > 0 {
		f := m.action.Fields[m.field]
		fields = fmt.Sprintf("字段 %d/%d · %s", m.field+1, len(m.action.Fields), safeLine(f.Label))
		if len(m.action.Fields) > 1 {
			fields += " · 下一项：" + safeLine(m.action.Fields[(m.field+1)%len(m.action.Fields)].Label)
		}
		if len(f.Choices) > 0 {
			choices = "←/→ 选择 · " + safeLine(strings.Join(f.Choices, " / "))
		}
	}
	input := m.input
	if value := cleanLines(input.Value()); value != input.Value() {
		input.SetValue(value)
	}
	input.Placeholder = safeLine(input.Placeholder)
	body := m.styles.title.Render(fit(fields, m.width-4)) + "\n" + m.styles.muted.Render(fit(choices, m.width-4)) + "\n" + input.View()
	return m.panel("编辑 · "+safeLine(label), body, m.bodyRows(), false)
}

func (m *Model) View() string {
	m.layout()
	if m.width < 28 || m.height < 16 {
		if m.leaf != nil {
			return fit("终端过小 · F10 返回", m.width)
		}
		return fit("终端过小，请放大 · Esc 返回", m.width)
	}
	name, goal, stage := m.spaceName, m.goal, m.stage
	if name == "" {
		name = m.space
	}
	if goal == "" {
		goal = "未选择教学目标"
	}
	if stage == "" {
		stage = "未选择教学会话"
	}
	if m.loading || m.loadFailure != "" {
		goal, stage = "未加载", "未加载"
	}
	header := m.styles.title.Render(fit("◇ 学习工作台 · 当前区："+safeLine(name), m.width)) + "\n" +
		fit("目标："+safeLine(goal), m.width) + "\n" + m.styles.muted.Render(fit("阶段："+safeLine(stage), m.width)) + "\n" + m.tabs()
	status := fmt.Sprintf("就绪 · 正文 %d%%", int(m.view.ScrollPercent()*100))
	hint := "↑↓/Tab 选择 · Enter 执行 · PgUp/PgDn 滚动正文"
	help := "Esc 返回/取消 · r 刷新 · Ctrl+C 退出"
	var body string
	switch {
	case m.leaf != nil:
		body = m.leafPanel()
		status = fmt.Sprintf("导入草稿 · 页面 %d%%", int(m.leafView.ScrollPercent()*100))
		hint = "按导入页提示操作 · Alt+PgUp/PgDn 滚动整页"
		help = "F10 返回工作台（保留草稿） · Ctrl+C 退出"
	case m.editing:
		body = m.formPanel()
		status = "正在编辑 · 草稿在本进程保留"
		hint = "Ctrl+S 提交 · Enter 换行 · Esc 保留草稿返回"
		help = "Tab/Shift+Tab 切换字段 · Ctrl+C 退出"
	case m.confirm:
		body = m.panel("请确认 · "+safeLine(m.action.Label), m.dialog.View(), m.bodyRows(), true)
		status = fmt.Sprintf("等待确认 · 文案 %d%%", int(m.dialog.ScrollPercent()*100))
		hint = "y 确认 · n/Esc 取消 · Ctrl+C 退出"
		help = "PgUp/PgDn 滚动确认内容 · Home/End 首尾"
	default:
		title := safeLine(m.data.Title)
		if title == "" {
			title = "页面内容"
		}
		body = m.panel(title, m.view.View(), m.view.Height, m.loadFailure != "") + "\n" + m.actionPanel()
	}
	if m.status != "" && m.leaf == nil {
		status = safeLine(m.status)
	}
	result := header + "\n" + body + "\n" + m.styles.muted.Render(fit(status, m.width)) + "\n" + m.styles.muted.Render(fit(hint, m.width)) + "\n" + m.styles.muted.Render(fit(help, m.width))
	if !m.colorEnabled {
		return ansi.Strip(result)
	}
	return result
}
