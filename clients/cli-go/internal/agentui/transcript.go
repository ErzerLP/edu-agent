package agentui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/agentloop"
)

type entryKind string

const (
	entryNotice    entryKind = "notice"
	entryUser      entryKind = "user"
	entryAssistant entryKind = "assistant"
	entryThinking  entryKind = "thinking"
	entryTools     entryKind = "tools"
	entryConfirm   entryKind = "confirm"
	entryError     entryKind = "error"
	entryContext   entryKind = "context"
)

type transcriptEntry struct {
	kind         entryKind
	text         string
	event        agentloop.Event
	events       []agentloop.Event
	contextEvent agentloop.ContextEvent
}

func renderTranscriptEntry(entry transcriptEntry, width int, toolsExpanded bool) string {
	if width < 20 {
		width = 20
	}
	switch entry.kind {
	case entryUser:
		return userStyle.Width(width).Render(userLabelStyle.Render("你") + "\n" + renderMarkdown(entry.text, width-3))
	case entryAssistant:
		return assistantStyle.Width(width).Render(assistantLabelStyle.Render("Agent") + "\n" + renderMarkdown(entry.text, width-3))
	case entryThinking:
		return renderThinkingActivity(entry.event, width)
	case entryTools:
		return renderToolGroup(entry.events, width, toolsExpanded)
	case entryConfirm:
		return confirmStyle.Width(max(20, width-4)).Render(confirmLabelStyle.Render("长期偏好确认") + "\n" + safeTerminalText(entry.text))
	case entryError:
		return errorCardStyle.Width(max(20, width-4)).Render(errorLabelStyle.Render("请求失败") + "\n" + safeTerminalText(entry.text))
	case entryContext:
		return renderContextEvent(entry.contextEvent, width)
	default:
		return noticeStyle.Width(width).Render("提示  " + safeTerminalText(entry.text))
	}
}

func renderContextEvent(event agentloop.ContextEvent, width int) string {
	label := "上下文已整理"
	detail := fmt.Sprintf("已整理较早 %d 轮 · 保留最近 %d 轮 · %d 条观察 · %d 条反思", event.DroppedTurns, event.RecentTurns, event.ObservationCount, event.ReflectionCount)
	style := contextInfoStyle
	switch event.Kind {
	case agentloop.ContextEventDegraded:
		label = "上下文整理降级"
		detail = fmt.Sprintf("仅保留最近完整轮次 · 已丢弃 %d 轮 · 代码：%s", event.DroppedTurns, safeTerminalText(event.Code))
		style = contextDangerStyle
	case agentloop.ContextEventSourceUnavailable:
		label = "会话证据来源不可用"
		detail = "指定来源正文已回收或失效 · 代码：" + safeTerminalText(event.Code)
		style = contextDangerStyle
	}
	return style.Width(max(20, width-4)).Render(label + "\n" + detail)
}

func renderThinkingActivity(event agentloop.Event, width int) string {
	status := normalizedEventStatus(event.Status)
	icon, label, style := "◇", "已思考", thinkingDoneStyle
	switch status {
	case agentloop.EventRunning:
		icon, label, style = "◐", "思考中", thinkingActiveStyle
	case agentloop.EventFailed:
		icon, label, style = "!", "思考中断", toolDangerStyle
	}
	line := fmt.Sprintf("%s %s  %s", icon, label, safeTerminalText(event.Summary))
	return thinkingStyle.Width(width).Render(style.Render(line))
}

func renderToolGroup(events []agentloop.Event, width int, expanded bool) string {
	if len(events) == 0 {
		return ""
	}
	succeeded, failed, waiting, running := 0, 0, 0, 0
	for _, event := range events {
		switch normalizedEventStatus(event.Status) {
		case agentloop.EventRunning:
			running++
		case agentloop.EventSucceeded:
			succeeded++
		case agentloop.EventConfirmationRequired:
			waiting++
		default:
			failed++
		}
	}
	summary := fmt.Sprintf("工具调用 · %d 项", len(events))
	if running > 0 {
		summary += fmt.Sprintf(" · %d 进行中", running)
	}
	if succeeded > 0 {
		summary += fmt.Sprintf(" · %d 完成", succeeded)
	}
	if failed > 0 {
		summary += fmt.Sprintf(" · %d 异常", failed)
	}
	if waiting > 0 {
		summary += fmt.Sprintf(" · %d 待确认", waiting)
	}
	if expanded {
		summary += "  [详情已展开]"
	} else {
		summary += "  [Ctrl+O 展开]"
	}

	lines := []string{toolHeaderStyle.Render(summary)}
	for _, event := range events {
		status := normalizedEventStatus(event.Status)
		icon, statusText, style := toolEventAppearance(status)
		name := toolDisplayName(event.Tool)
		line := fmt.Sprintf("%s %-8s %s", icon, name, safeTerminalText(event.Summary))
		lines = append(lines, style.Width(max(20, width-2)).Render(line))
		if expanded {
			detail := fmt.Sprintf("    状态：%s · 工具：%s", statusText, safeTerminalText(event.Tool))
			if strings.TrimSpace(event.Detail) != "" {
				detail += " · 代码：" + safeTerminalText(event.Detail)
			}
			lines = append(lines, toolDetailStyle.Width(max(20, width-4)).Render(detail))
		}
	}
	return toolGroupStyle.Width(width).Render(strings.Join(lines, "\n"))
}

func normalizedEventStatus(status agentloop.EventStatus) agentloop.EventStatus {
	if status == "" {
		return agentloop.EventSucceeded
	}
	return status
}

func toolEventAppearance(status agentloop.EventStatus) (icon, text string, style lipgloss.Style) {
	switch status {
	case agentloop.EventRunning:
		return "◐", "进行中", thinkingActiveStyle
	case agentloop.EventSucceeded:
		return "✓", "完成", toolSuccessStyle
	case agentloop.EventInvalid:
		return "!", "参数无效", toolWarningStyle
	case agentloop.EventConfirmationRequired:
		return "?", "等待确认", toolWarningStyle
	case agentloop.EventOutcomeUnknown:
		return "?", "结果未知", toolDangerStyle
	default:
		return "×", "失败", toolDangerStyle
	}
}

func toolDisplayName(name string) string {
	labels := map[string]string{
		"search_knowledge":           "检索知识库",
		"get_learning_progress":      "读取学习进度",
		"get_learning_route":         "读取学习路线",
		"get_due_reviews":            "读取到期复习",
		"list_long_term_preferences": "读取长期偏好",
		"recall_session_memory":      "回查会话证据",
		"remember_preference":        "保存长期偏好",
	}
	if label, ok := labels[name]; ok {
		return label
	}
	return "未知工具"
}

func eventKey(event agentloop.Event) string {
	return strings.Join([]string{event.ID, event.Tool, string(normalizedEventStatus(event.Status)), event.Summary, event.Detail}, "\x00")
}

func upsertThinkingActivity(entries []transcriptEntry, event agentloop.Event) []transcriptEntry {
	if event.ID != "" {
		for index := len(entries) - 1; index >= 0; index-- {
			if entries[index].kind == entryUser {
				break
			}
			if entries[index].kind == entryThinking && entries[index].event.ID == event.ID {
				entries[index].event = event
				return entries
			}
		}
	}
	return append(entries, transcriptEntry{kind: entryThinking, event: event})
}

func upsertToolEvent(entries []transcriptEntry, event agentloop.Event) []transcriptEntry {
	if event.ID != "" {
		for entryIndex := len(entries) - 1; entryIndex >= 0; entryIndex-- {
			if entries[entryIndex].kind == entryUser {
				break
			}
			if entries[entryIndex].kind != entryTools {
				continue
			}
			for eventIndex := len(entries[entryIndex].events) - 1; eventIndex >= 0; eventIndex-- {
				if entries[entryIndex].events[eventIndex].ID == event.ID {
					entries[entryIndex].events[eventIndex] = event
					return entries
				}
			}
		}
	}
	if len(entries) > 0 && entries[len(entries)-1].kind == entryTools {
		entries[len(entries)-1].events = append(entries[len(entries)-1].events, event)
		return entries
	}
	return append(entries, transcriptEntry{kind: entryTools, events: []agentloop.Event{event}})
}

func appendToolEvents(entries []transcriptEntry, events []agentloop.Event) []transcriptEntry {
	for _, event := range events {
		entries = upsertToolEvent(entries, event)
	}
	return entries
}

func updatePreferenceToolStatus(entries []transcriptEntry, status agentloop.EventStatus, summary, detail string) []transcriptEntry {
	for entryIndex := len(entries) - 1; entryIndex >= 0; entryIndex-- {
		if entries[entryIndex].kind == entryUser {
			break
		}
		if entries[entryIndex].kind != entryTools {
			continue
		}
		for eventIndex := len(entries[entryIndex].events) - 1; eventIndex >= 0; eventIndex-- {
			if entries[entryIndex].events[eventIndex].Tool == "remember_preference" {
				entries[entryIndex].events[eventIndex].Status = status
				entries[entryIndex].events[eventIndex].Summary = summary
				entries[entryIndex].events[eventIndex].Detail = detail
				return entries
			}
		}
	}
	return entries
}

func truncateDisplayWidth(value string, width int) string {
	value = safeTerminalText(value)
	if width <= 0 {
		return ""
	}
	if lipgloss.Width(value) <= width {
		return value
	}
	runes := []rune(value)
	for len(runes) > 0 && lipgloss.Width(string(runes)+"…") > width {
		runes = runes[:len(runes)-1]
	}
	return string(runes) + "…"
}

var (
	titleStyle                = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("6"))
	headerSubtitleStyle       = lipgloss.NewStyle().Foreground(lipgloss.Color("7"))
	headerSeparatorStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	statusReadyStyle          = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("2"))
	statusBusyStyle           = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("3"))
	statusErrorStyle          = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("1"))
	userLabelStyle            = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("2"))
	assistantLabelStyle       = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("6"))
	confirmLabelStyle         = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("3"))
	errorLabelStyle           = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("1"))
	userStyle                 = lipgloss.NewStyle().BorderLeft(true).BorderStyle(lipgloss.ThickBorder()).BorderForeground(lipgloss.Color("2")).PaddingLeft(1)
	assistantStyle            = lipgloss.NewStyle().BorderLeft(true).BorderStyle(lipgloss.ThickBorder()).BorderForeground(lipgloss.Color("6")).PaddingLeft(1)
	thinkingStyle             = lipgloss.NewStyle().BorderLeft(true).BorderStyle(lipgloss.ThickBorder()).BorderForeground(lipgloss.Color("8")).PaddingLeft(1)
	thinkingActiveStyle       = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("3"))
	thinkingDoneStyle         = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	toolGroupStyle            = lipgloss.NewStyle().BorderLeft(true).BorderStyle(lipgloss.ThickBorder()).BorderForeground(lipgloss.Color("8")).PaddingLeft(1)
	toolHeaderStyle           = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("7"))
	toolSuccessStyle          = lipgloss.NewStyle().Foreground(lipgloss.Color("2"))
	toolWarningStyle          = lipgloss.NewStyle().Foreground(lipgloss.Color("3"))
	toolDangerStyle           = lipgloss.NewStyle().Foreground(lipgloss.Color("1"))
	toolDetailStyle           = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	noticeStyle               = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	mutedStyle                = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	confirmStyle              = lipgloss.NewStyle().Border(lipgloss.NormalBorder()).BorderForeground(lipgloss.Color("3")).Padding(0, 1)
	errorCardStyle            = lipgloss.NewStyle().Border(lipgloss.NormalBorder()).BorderForeground(lipgloss.Color("1")).Padding(0, 1)
	contextInfoStyle          = lipgloss.NewStyle().Border(lipgloss.NormalBorder()).BorderForeground(lipgloss.Color("6")).Padding(0, 1)
	contextDangerStyle        = lipgloss.NewStyle().Border(lipgloss.NormalBorder()).BorderForeground(lipgloss.Color("1")).Padding(0, 1)
	composerPromptStyle       = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("6"))
	composerLabelStyle        = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("7"))
	composerCountStyle        = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	composerReadyBorderStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("6"))
	composerBusyBorderStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("3"))
	composerDangerBorderStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("1"))
	newMessageStyle           = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("3"))
	keyHintStyle              = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("6"))
	footerSeparatorStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	dividerStyle              = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
)
