package agentui

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

const (
	composerMinRows = 2
	composerMaxRows = 6
)

type footerHint struct {
	key    string
	action string
}

func (m model) renderHeader(width int) string {
	brand := titleStyle.Render("◇ edu-agent")
	subtitle := headerSubtitleStyle.Render("AI 学习助手")
	line := brand + headerSeparatorStyle.Render(" · ") + subtitle
	if lipgloss.Width(line) <= width {
		return line
	}
	return titleStyle.Render(truncateDisplayWidth("◇ edu-agent", width))
}

func (m model) renderComposer(width int) string {
	innerWidth := composerInnerWidth(width)
	border := m.composerBorderStyle()
	label := "消息"
	switch {
	case m.busy:
		label = "处理中"
	case m.pending != nil:
		label = "等待偏好确认"
	}

	safeInput := safeComposerText(m.input.Value())
	left := border.Render("╭─ ") + composerLabelStyle.Render(label) + border.Render(" ")
	right := ""
	if count := utf8.RuneCountInString(safeInput); count > 0 {
		right = composerCountStyle.Render(fmt.Sprintf(" %d/%d ", count, m.input.CharLimit))
	}
	fill := max(1, width-lipgloss.Width(left)-lipgloss.Width(right)-1)
	top := left + border.Render(strings.Repeat("─", fill)) + right + border.Render("╮")

	input := m.input
	if safeInput != input.Value() {
		input.SetValue(safeInput)
	}
	lines := strings.Split(input.View(), "\n")
	for len(lines) < input.Height() {
		lines = append(lines, "")
	}
	body := make([]string, 0, len(lines)+2)
	body = append(body, top)
	for _, line := range lines {
		line = ansi.Truncate(line, innerWidth, "")
		padding := max(0, innerWidth-lipgloss.Width(line))
		body = append(body, border.Render("│")+" "+line+strings.Repeat(" ", padding)+" "+border.Render("│"))
	}
	body = append(body, border.Render("╰"+strings.Repeat("─", max(0, width-2))+"╯"))
	return strings.Join(body, "\n")
}

func (m model) composerBorderStyle() lipgloss.Style {
	switch {
	case m.status == "请求失败" || m.status == "提交结果待核对" || m.status == "保存结果待核对":
		return composerDangerBorderStyle
	case m.busy || m.pending != nil:
		return composerBusyBorderStyle
	default:
		return composerReadyBorderStyle
	}
}

func composerInnerWidth(width int) int {
	return max(1, width-4)
}

func (m model) composerInputRows(width int) int {
	contentWidth := max(1, composerInnerWidth(width)-2)
	rows := 0
	for line := range strings.SplitSeq(safeComposerText(m.input.Value()), "\n") {
		lineWidth := lipgloss.Width(line)
		rows += max(1, (lineWidth+contentWidth-1)/contentWidth)
	}
	maxRows := composerMaxRows
	if m.height < 24 {
		maxRows = 3
	}
	return min(max(rows, composerMinRows), maxRows)
}

func (m model) renderFooter(width int) string {
	return lipgloss.JoinVertical(lipgloss.Left, m.renderFooterStatus(width), m.renderFooterHints(width))
}

func (m model) renderFooterStatus(width int) string {
	modelName := strings.TrimSpace(safeSingleLineTerminalText(m.modelName))
	if modelName == "" {
		modelName = "未命名模型"
	}
	modelPart := "模型 " + modelName
	newPart := ""
	if m.hasNewContent {
		newPart = newMessageStyle.Render("↓ 有新消息")
	}

	candidates := [][]string{
		{newPart, m.renderStatus(), mutedStyle.Render(m.contextSummary(false)), mutedStyle.Render(modelPart)},
		{newPart, m.renderStatusText(m.compactStatus()), mutedStyle.Render(m.contextPercentSummary()), mutedStyle.Render(modelPart)},
		{newPart, m.renderStatusText(m.compactStatus()), mutedStyle.Render(m.contextSummary(true) + fmt.Sprintf(" · %d轮 · %d条记忆", m.contextStatus.RecentCompleteTurns, m.contextStatus.MemoryItemCount)), mutedStyle.Render(modelPart)},
		{newPart, m.renderStatusText(m.compactStatus()), mutedStyle.Render(m.contextSummary(true)), mutedStyle.Render(modelPart)},
	}
	for _, parts := range candidates {
		line := joinFooterParts(parts)
		if lipgloss.Width(line) <= width {
			return line
		}
	}

	parts := compactNonEmpty([]string{newPart, m.renderStatusText(m.compactStatus()), mutedStyle.Render(m.contextSummary(true))})
	prefix := joinFooterParts(parts)
	modelPrefix := mutedStyle.Render("模型 ")
	separator := footerSeparatorStyle.Render(" · ")
	available := width - lipgloss.Width(prefix) - lipgloss.Width(separator) - lipgloss.Width(modelPrefix)
	if available <= 0 {
		return prefix
	}
	return prefix + separator + modelPrefix + mutedStyle.Render(truncateDisplayWidth(modelName, available))
}

func (m model) contextSummary(compact bool) string {
	percent := min(max(m.contextStatus.WindowPercent, 0), 100)
	if compact {
		if m.contextStatus.Estimated {
			return fmt.Sprintf("约%d%%", percent)
		}
		return fmt.Sprintf("%d%%", percent)
	}
	return fmt.Sprintf("%s · 最近 %d 轮 · 会话记忆 %d 条", m.contextPercentSummary(), m.contextStatus.RecentCompleteTurns, m.contextStatus.MemoryItemCount)
}

func (m model) contextPercentSummary() string {
	prefix := "上下文"
	if m.contextStatus.Estimated {
		prefix += "约"
	}
	return fmt.Sprintf("%s %d%%", prefix, min(max(m.contextStatus.WindowPercent, 0), 100))
}

func (m model) renderFooterHints(width int) string {
	variants := m.footerHintVariants()
	for _, hints := range variants {
		line := renderHints(hints)
		if lipgloss.Width(line) <= width {
			return line
		}
	}
	return renderHints([]footerHint{{key: "Esc", action: "退出"}})
}

func (m model) footerHintVariants() [][]footerHint {
	switch {
	case m.busy && m.pending != nil:
		return [][]footerHint{
			{{key: "PgUp/PgDn", action: "滚动"}, {key: "Ctrl+G", action: "到底部"}, {key: "Ctrl+C", action: "取消退出"}},
			{{key: "PgUp/PgDn", action: "滚动"}, {key: "Ctrl+C", action: "取消退出"}},
		}
	case m.pending != nil && m.pending.RetryOnly:
		return [][]footerHint{
			{{key: "Y", action: "重试核对"}, {key: "PgUp/PgDn", action: "检查偏好"}, {key: "Ctrl+O", action: "工具详情"}, {key: "Ctrl+C", action: "退出"}},
			{{key: "Y", action: "重试核对"}, {key: "PgUp/PgDn", action: "滚动"}, {key: "Ctrl+C", action: "退出"}},
		}
	case m.pending != nil:
		return [][]footerHint{
			{{key: "Y", action: "确认保存"}, {key: "N", action: "取消"}, {key: "PgUp/PgDn", action: "滚动检查"}, {key: "Ctrl+O", action: "工具详情"}},
			{{key: "Y", action: "保存"}, {key: "N", action: "取消"}, {key: "PgUp/PgDn", action: "滚动"}},
		}
	case m.busy:
		return [][]footerHint{
			{{key: "PgUp/PgDn", action: "滚动"}, {key: "Ctrl+G", action: "到底部"}, {key: "Ctrl+O", action: "工具详情"}, {key: "Ctrl+C", action: "取消退出"}},
			{{key: "PgUp/PgDn", action: "滚动"}, {key: "Ctrl+G", action: "到底部"}, {key: "Ctrl+C", action: "退出"}},
		}
	default:
		return [][]footerHint{
			{{key: "Enter", action: "发送"}, {key: "Ctrl+J", action: "换行"}, {key: "PgUp/PgDn", action: "滚动"}, {key: "Ctrl+G", action: "到底部"}, {key: "Ctrl+O", action: "工具详情"}, {key: "Esc", action: "退出"}},
			{{key: "Enter", action: "发送"}, {key: "Ctrl+J", action: "换行"}, {key: "PgUp/PgDn", action: "滚动"}, {key: "Ctrl+G", action: "到底部"}, {key: "Esc", action: "退出"}},
			{{key: "Enter", action: "发送"}, {key: "Ctrl+J", action: "换行"}, {key: "Esc", action: "退出"}},
		}
	}
}

func renderHints(hints []footerHint) string {
	parts := make([]string, 0, len(hints))
	for _, hint := range hints {
		parts = append(parts, keyHintStyle.Render(hint.key)+mutedStyle.Render(" "+hint.action))
	}
	return strings.Join(parts, footerSeparatorStyle.Render(" · "))
}

func joinFooterParts(parts []string) string {
	return strings.Join(compactNonEmpty(parts), footerSeparatorStyle.Render(" · "))
}

func compactNonEmpty(parts []string) []string {
	result := make([]string, 0, len(parts))
	for _, part := range parts {
		if strings.TrimSpace(part) != "" {
			result = append(result, part)
		}
	}
	return result
}
