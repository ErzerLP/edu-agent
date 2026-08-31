package agentui

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/agentloop"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/modelclient"
)

const (
	composerMinRows = 2
	composerMaxRows = 6
)

var (
	selectorPanelStyle  = lipgloss.NewStyle().Border(lipgloss.NormalBorder()).BorderForeground(lipgloss.Color("6")).Padding(0, 1)
	selectorTitleStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("6"))
	selectorOptionStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("7"))
	selectorFocusStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("3"))
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

func (m model) renderControl(width int) string {
	if m.selector != nil {
		return m.renderSelector(width)
	}
	return m.renderComposer(width)
}

func (m model) renderSelector(width int) string {
	selector := m.selector
	if selector == nil {
		return ""
	}
	innerWidth := max(12, width-4)
	bodyRows, detailRows := 2, 2
	lines := []string{selectorTitleStyle.Render(truncateDisplayWidth(selector.title, innerWidth))}
	for _, line := range wrapDisplayLines(selector.body, innerWidth, bodyRows) {
		lines = append(lines, mutedStyle.Render(line))
	}

	maxVisibleOptions, optionLabelRows := 5, 1
	if selector.kind == selectorQuestion {
		maxVisibleOptions, optionLabelRows = 3, 2
		if m.height <= minimumHeight {
			maxVisibleOptions = 1
		}
		if m.height >= 28 {
			optionLabelRows = 3
		}
	} else if m.height >= 22 {
		maxVisibleOptions = 8
	}
	start, end := 0, len(selector.options)
	if end > maxVisibleOptions {
		start = max(0, selector.focus-maxVisibleOptions/2)
		end = min(len(selector.options), start+maxVisibleOptions)
		start = max(0, end-maxVisibleOptions)
	}
	for index := start; index < end; index++ {
		option := selector.options[index]
		cursor := "  "
		if selector.focus == index {
			cursor = "› "
		}
		marker := "○"
		if selector.kind == selectorQuestion && selector.mode == agentloop.QuestionMultiple {
			marker = "[ ]"
			if option.Selected {
				marker = "[x]"
			}
		} else if option.Selected {
			marker = "●"
		}
		labelText := safeSingleLineTerminalText(option.Label)
		prefix := fmt.Sprintf("%s%d. %s ", cursor, index+1, marker)
		continuation := strings.Repeat(" ", lipgloss.Width(prefix))
		labelWidth := max(4, innerWidth-lipgloss.Width(prefix))
		wrappedLabel := wrapDisplayLines(labelText, labelWidth, optionLabelRows)
		style := selectorOptionStyle
		if selector.focus == index {
			style = selectorFocusStyle
		}
		for row, labelLine := range wrappedLabel {
			linePrefix := continuation
			if row == 0 {
				linePrefix = prefix
			}
			lines = append(lines, style.Render(linePrefix+labelLine))
		}
	}
	if selector.focus >= start && selector.focus < end {
		focused := selector.options[selector.focus]
		if description := strings.TrimSpace(focused.Description); description != "" {
			for _, line := range wrapDisplayLines("说明："+description, innerWidth, detailRows) {
				lines = append(lines, mutedStyle.Render(line))
			}
		}
	}
	if start > 0 || end < len(selector.options) {
		lines = append(lines, mutedStyle.Render(fmt.Sprintf("  选项 %d-%d / %d", start+1, end, len(selector.options))))
	}
	if selector.hasCustom {
		cursor := "  "
		if selector.focus == len(selector.options) {
			cursor = "› "
		}
		value := safeComposerText(selector.custom.Value())
		if strings.TrimSpace(value) == "" {
			value = "输入自定义回答"
		}
		customLines := strings.Split(value, "\n")
		if len(customLines) > 2 {
			customLines = customLines[len(customLines)-2:]
		}
		for index, line := range customLines {
			prefix := "    "
			if index == 0 {
				prefix = cursor + "0. 自定义 "
			}
			style := selectorOptionStyle
			if selector.focus == len(selector.options) {
				style = selectorFocusStyle
			}
			lines = append(lines, style.Render(truncateDisplayWidth(prefix+line, innerWidth)))
		}
	}
	lines = append(lines, mutedStyle.Render(truncateDisplayWidth(selector.helpText(), innerWidth)))
	return selectorPanelStyle.Width(max(12, width-2)).Render(strings.Join(lines, "\n"))
}

func wrapDisplayLines(value string, width, maxLines int) []string {
	if width <= 0 || maxLines <= 0 {
		return nil
	}
	value = strings.ReplaceAll(safeTerminalText(value), "\t", " ")
	wrapped := make([]string, 0, maxLines)
	for sourceLine := range strings.SplitSeq(value, "\n") {
		if sourceLine == "" {
			wrapped = append(wrapped, "")
			continue
		}
		line := ""
		for _, current := range sourceLine {
			candidate := line + string(current)
			if line != "" && lipgloss.Width(candidate) > width {
				wrapped = append(wrapped, line)
				line = string(current)
				continue
			}
			line = candidate
		}
		wrapped = append(wrapped, line)
	}
	if len(wrapped) <= maxLines {
		return wrapped
	}
	wrapped = wrapped[:maxLines]
	wrapped[maxLines-1] = truncateDisplayWidth(strings.TrimSuffix(wrapped[maxLines-1], "…")+"…", width)
	return wrapped
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

	effort := m.session.ReasoningEffort()
	if effort == "" {
		effort = modelclient.ReasoningEffortAuto
	}
	reasoningPart := "推理 " + string(effort)
	if m.busy && m.activeEffort != "" && m.activeEffort != effort {
		reasoningPart = fmt.Sprintf("推理 %s → 下一请求 %s", m.activeEffort, effort)
	}
	candidates := [][]string{
		{newPart, m.renderStatus(), mutedStyle.Render(m.contextSummary(false)), mutedStyle.Render(reasoningPart), mutedStyle.Render(modelPart)},
		{newPart, m.renderStatusText(m.compactStatus()), mutedStyle.Render(m.contextPercentSummary()), mutedStyle.Render(reasoningPart), mutedStyle.Render(modelPart)},
		{newPart, m.renderStatusText(m.compactStatus()), mutedStyle.Render(m.contextSummary(true)), mutedStyle.Render(reasoningPart)},
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
	case m.selector != nil:
		return [][]footerHint{
			{{key: "↑/↓", action: "选择"}, {key: "Enter", action: "确认"}, {key: "PgUp/PgDn", action: "检查详情"}, {key: "Ctrl+O", action: "活动详情"}},
			{{key: "↑/↓", action: "选择"}, {key: "Enter", action: "确认"}},
		}
	case m.stopping:
		return [][]footerHint{{{key: "Esc", action: "正在停止"}, {key: "Ctrl+C", action: "退出"}}}
	case m.busy && m.activeCancelable:
		hints := []footerHint{{key: "Esc", action: "停止当前轮次"}, {key: "F3", action: "推理强度"}, {key: "Ctrl+O", action: "活动详情"}}
		if m.isSlowTurn() {
			hints = append(hints, footerHint{key: "提示", action: m.slowTurnDetail()})
		}
		return [][]footerHint{hints, {{key: "Esc", action: "停止"}, {key: "F3", action: "推理"}, {key: "Ctrl+C", action: "退出"}}}
	case m.busy:
		return [][]footerHint{
			{{key: "长期偏好写入", action: "不可中断"}, {key: "F3", action: "下一请求推理强度"}, {key: "Ctrl+O", action: "活动详情"}},
			{{key: "Ctrl+C", action: "退出整个 Agent"}},
		}
	default:
		return [][]footerHint{
			{{key: "Enter", action: "发送"}, {key: "Ctrl+J", action: "换行"}, {key: "F3", action: "推理强度"}, {key: "Ctrl+O", action: "活动详情"}, {key: "Esc", action: "退出"}},
			{{key: "Enter", action: "发送"}, {key: "Ctrl+J", action: "换行"}, {key: "F3", action: "推理"}, {key: "Esc", action: "退出"}},
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
