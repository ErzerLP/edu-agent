package agentui

import (
	"context"
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/agentloop"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/api"
)

const (
	sidebarGap          = 2
	sidebarBreakpoint   = 86
	sidebarMinMainWidth = 56
	sidebarMinWidth     = 26
	sidebarMaxWidth     = 30
)

var (
	sidebarBorderStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	sidebarTitleStyle    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("6"))
	sidebarSectionStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("6"))
	sidebarLabelStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	sidebarValueStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("7"))
	sidebarProgressStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("2"))
	sidebarTrackStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	sidebarWarningStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("3"))
)

type learningConversation interface {
	LearningStatus(context.Context) (agentloop.LearningStatus, error)
}

func loadLearningStatusCmd(ctx context.Context, session Conversation) tea.Cmd {
	provider, ok := session.(learningConversation)
	if !ok {
		return nil
	}
	return func() tea.Msg {
		status, err := provider.LearningStatus(ctx)
		return learningMsg{status: status, err: err}
	}
}

func (m *model) startLearningRefresh() tea.Cmd {
	if !m.learningProvider {
		return nil
	}
	if m.learningLoading {
		m.learningRefreshPending = true
		return nil
	}
	m.learningLoading = true
	m.learningLoaded = false
	m.learningFailed = false
	m.learningStatus = agentloop.LearningStatus{}
	return loadLearningStatusCmd(m.ctx, m.session)
}

func sidebarLayoutWidths(contentWidth int) (int, int) {
	if contentWidth < sidebarBreakpoint {
		return contentWidth, 0
	}
	sidebarWidth := min(max(contentWidth/4, sidebarMinWidth), sidebarMaxWidth)
	mainWidth := contentWidth - sidebarGap - sidebarWidth
	if mainWidth < sidebarMinMainWidth {
		return contentWidth, 0
	}
	return mainWidth, sidebarWidth
}

func (m model) renderSidebar(width, height int) string {
	if width < 8 || height < 3 {
		return ""
	}
	innerWidth := width - 4
	left := sidebarBorderStyle.Render("╭─ ") + sidebarTitleStyle.Render("学习概览") + sidebarBorderStyle.Render(" ")
	fill := max(1, width-lipgloss.Width(left)-1)
	rows := []string{left + sidebarBorderStyle.Render(strings.Repeat("─", fill)+"╮")}

	capacity := height - 2
	contentBudget := max(0, capacity-1)
	content := m.sidebarContent(innerWidth, contentBudget)
	hint := keyHintStyle.Render("Ctrl+R") + mutedStyle.Render(" 刷新")
	if capacity > 0 {
		if len(content) > capacity-1 {
			content = content[:max(0, capacity-1)]
		}
		for len(content) < capacity-1 {
			content = append(content, "")
		}
		content = append(content, hint)
	}
	for _, line := range content {
		rows = append(rows, sidebarFrameLine(line, innerWidth))
	}
	rows = append(rows, sidebarBorderStyle.Render("╰"+strings.Repeat("─", width-2)+"╯"))
	return strings.Join(rows, "\n")
}

func (m model) sidebarContent(width, budget int) []string {
	compact := budget <= 15
	lines := []string{
		sidebarSectionStyle.Render("AGENT"),
		m.renderStatus(),
		sidebarKV("上下文", m.contextTokenSummary()),
		sidebarKV("缓存命中", m.cacheHitSummary()),
	}
	if !compact {
		lines = append(lines,
			sidebarKV("最近", fmt.Sprintf("%d 轮", m.contextStatus.RecentCompleteTurns)),
			sidebarKV("记忆", fmt.Sprintf("%d 条", m.contextStatus.MemoryItemCount)),
			sidebarKV("模型", safeSingleLineTerminalText(m.modelName)),
		)
	}
	lines = append(lines, "", sidebarSectionStyle.Render("当前学习"))
	return append(lines, m.learningSidebarLines(width, compact)...)
}

func (m model) contextTokenSummary() string {
	if m.contextStatus.ContextWindow <= 0 {
		return m.contextSummary(true)
	}
	prefix := ""
	if m.contextStatus.Estimated && m.contextStatus.CurrentTokens > 0 {
		prefix = "约"
	}
	return prefix + formatCompactTokens(m.contextStatus.CurrentTokens) + "/" + formatCompactTokens(m.contextStatus.ContextWindow)
}

func (m model) cacheHitSummary() string {
	if !m.contextStatus.CacheHitRateAvailable {
		return "—"
	}
	rate := min(max(m.contextStatus.CacheHitRate, 0), 100)
	return fmt.Sprintf("%.1f%%", rate)
}

func formatCompactTokens(value int) string {
	value = max(0, value)
	switch {
	case value < 1000:
		return fmt.Sprintf("%d", value)
	case value < 1_000_000:
		if value%1000 == 0 {
			return fmt.Sprintf("%dk", value/1000)
		}
		return fmt.Sprintf("%.1fk", float64(value)/1000)
	default:
		if value%1_000_000 == 0 {
			return fmt.Sprintf("%dM", value/1_000_000)
		}
		return fmt.Sprintf("%.1fM", float64(value)/1_000_000)
	}
}

func (m model) learningSidebarLines(width int, compact bool) []string {
	switch {
	case !m.learningProvider:
		return []string{mutedStyle.Render("学习状态源不可用")}
	case m.learningLoading:
		return []string{sidebarWarningStyle.Render("◌ 正在读取服务端状态")}
	case m.learningFailed:
		return []string{
			sidebarWarningStyle.Render("! 当前状态暂不可用"),
			mutedStyle.Render("按 Ctrl+R 重试"),
		}
	case !m.learningLoaded:
		return []string{mutedStyle.Render("等待读取学习状态")}
	case !m.learningStatus.Active:
		return []string{
			sidebarValueStyle.Render("尚无进行中的学习会话"),
			mutedStyle.Render("可以请 Agent 制定目标"),
		}
	}

	view := m.learningStatus.View
	lines := make([]string, 0, 10)
	goalLines := 2
	if compact {
		goalLines = 1
	}
	if view.WorkItem != nil && view.WorkItem.GoalRevision != nil {
		lines = append(lines, sidebarWrappedKV("目标", view.WorkItem.GoalRevision.Text, width, goalLines)...)
	} else {
		lines = append(lines, sidebarKV("目标", "尚未设置"))
	}
	lines = append(lines, sidebarKV("会话", learningSessionState(view.Session.State)))
	lines = append(lines, routeProgressLine(view.Session.Focus.RouteStepID, view.Session.CompletedRoute, view.WorkItem, width))

	if !compact && view.WorkItem != nil && view.WorkItem.RouteRevision != nil {
		if intent := currentTeachingIntent(view.Session.Focus.RouteStepID, view.WorkItem.RouteRevision.Steps); intent != "" {
			lines = append(lines, sidebarWrappedKV("当前", intent, width, 2)...)
		}
	}
	if view.WorkItem != nil && view.WorkItem.Activity != nil {
		activity := learningActivityType(view.WorkItem.Activity.Type)
		if view.WorkItem.Activity.Review {
			activity += " · 复习"
		}
		if view.WorkItem.Activity.Difficulty > 0 {
			activity += fmt.Sprintf(" · 难度%d", view.WorkItem.Activity.Difficulty)
		}
		lines = append(lines, sidebarKV("活动", activity))
	}
	if view.EstimatedActiveTime.DurationSeconds > 0 {
		lines = append(lines, sidebarKV("活跃", formatActiveTime(view.EstimatedActiveTime.DurationSeconds, view.EstimatedActiveTime.Estimated)))
	}
	return lines
}

func sidebarFrameLine(line string, width int) string {
	line = ansi.Truncate(line, width, "")
	padding := max(0, width-lipgloss.Width(line))
	return sidebarBorderStyle.Render("│") + " " + line + strings.Repeat(" ", padding) + " " + sidebarBorderStyle.Render("│")
}

func sidebarKV(label, value string) string {
	return sidebarLabelStyle.Render(label+" ") + sidebarValueStyle.Render(safeSingleLineTerminalText(value))
}

func sidebarWrappedKV(label, value string, width, maxLines int) []string {
	labelText := label + " "
	labelWidth := lipgloss.Width(labelText)
	wrapped := wrapSidebarText(safeSingleLineTerminalText(value), max(1, width-labelWidth), maxLines)
	if len(wrapped) == 0 {
		return []string{sidebarKV(label, "—")}
	}
	lines := make([]string, 0, len(wrapped))
	for index, line := range wrapped {
		prefix := strings.Repeat(" ", labelWidth)
		if index == 0 {
			prefix = sidebarLabelStyle.Render(labelText)
		}
		lines = append(lines, prefix+sidebarValueStyle.Render(line))
	}
	return lines
}

func wrapSidebarText(value string, width, maxLines int) []string {
	value = strings.Join(strings.Fields(value), " ")
	if value == "" || width <= 0 || maxLines <= 0 {
		return nil
	}
	lines := make([]string, 0, maxLines)
	var current strings.Builder
	currentWidth := 0
	truncated := false
	for _, char := range value {
		charWidth := lipgloss.Width(string(char))
		if currentWidth > 0 && currentWidth+charWidth > width {
			lines = append(lines, current.String())
			current.Reset()
			currentWidth = 0
			if len(lines) == maxLines {
				truncated = true
				break
			}
		}
		current.WriteRune(char)
		currentWidth += charWidth
	}
	if !truncated && current.Len() > 0 && len(lines) < maxLines {
		lines = append(lines, current.String())
	}
	if truncated && len(lines) > 0 {
		lines[len(lines)-1] = ansi.Truncate(lines[len(lines)-1], max(1, width-1), "") + "…"
	}
	return lines
}

func routeProgressLine(routeStepID string, completed bool, workItem *api.SessionWorkItem, width int) string {
	if workItem == nil || workItem.RouteRevision == nil || len(workItem.RouteRevision.Steps) == 0 {
		return sidebarKV("路线", "尚未生成")
	}
	steps := workItem.RouteRevision.Steps
	position := 0
	for index, step := range steps {
		if step.RouteStepID == routeStepID {
			position = index + 1
			break
		}
	}
	if completed {
		position = len(steps)
	}
	barWidth := min(8, max(4, width-12))
	filled := 0
	if len(steps) > 0 {
		filled = min(barWidth, max(0, (position*barWidth+len(steps)-1)/len(steps)))
	}
	bar := sidebarProgressStyle.Render(strings.Repeat("█", filled)) + sidebarTrackStyle.Render(strings.Repeat("░", barWidth-filled))
	return sidebarLabelStyle.Render("路线 ") + bar + sidebarValueStyle.Render(fmt.Sprintf(" %d/%d", position, len(steps)))
}

func currentTeachingIntent(routeStepID string, steps []api.RouteStep) string {
	for _, step := range steps {
		if step.RouteStepID == routeStepID {
			return safeSingleLineTerminalText(step.TeachingIntent)
		}
	}
	return ""
}

func learningSessionState(value string) string {
	states := map[string]string{
		"GoalSet":            "目标已设置",
		"Diagnostic":         "诊断中",
		"RouteProposed":      "路线待确认",
		"RouteActive":        "路线学习中",
		"ActivityIssued":     "活动已布置",
		"AwaitingResponse":   "等待回答",
		"AwaitingAssessment": "等待评估",
		"FocusSuspended":     "自由问答中",
		"FreeQuestion":       "自由提问中",
		"FreeAnswer":         "查看自由回答",
		"FocusResumed":       "已恢复学习",
		"Completed":          "已完成",
	}
	if label := states[value]; label != "" {
		return label
	}
	if strings.TrimSpace(value) == "" {
		return "状态未知"
	}
	return safeSingleLineTerminalText(value)
}

func learningActivityType(value string) string {
	types := map[string]string{
		"explanation": "讲解",
		"example":     "示例",
		"practice":    "练习",
		"quiz":        "测验",
		"assessment":  "评估",
		"review":      "复习",
	}
	if label := types[strings.ToLower(value)]; label != "" {
		return label
	}
	if strings.TrimSpace(value) == "" {
		return "学习活动"
	}
	return safeSingleLineTerminalText(value)
}

func formatActiveTime(seconds int64, estimated bool) string {
	minutes := max(int64(1), (seconds+59)/60)
	value := fmt.Sprintf("%d 分钟", minutes)
	if minutes >= 60 {
		value = fmt.Sprintf("%d小时%d分", minutes/60, minutes%60)
	}
	if estimated {
		return "约 " + value
	}
	return value
}
