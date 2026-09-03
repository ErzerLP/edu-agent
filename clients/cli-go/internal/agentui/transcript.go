package agentui

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/charmbracelet/lipgloss"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/agentloop"
)

type entryKind string

const (
	entryNotice      entryKind = "notice"
	entryUser        entryKind = "user"
	entryAssistant   entryKind = "assistant"
	entryThinking    entryKind = "thinking"
	entryTools       entryKind = "tools"
	entryConfirm     entryKind = "confirm"
	entryFileConfirm entryKind = "file_confirm"
	entryQuestion    entryKind = "question"
	entryError       entryKind = "error"
	entryContext     entryKind = "context"
)

type transcriptEntry struct {
	kind         entryKind
	text         string
	activity     agentloop.Activity
	activities   []agentloop.Activity
	contextEvent agentloop.ContextEvent
	turnID       uint64
	streaming    bool
	stopped      bool
	failed       bool
	pending      *agentloop.PreferenceConfirmation
	fileMutation *agentloop.PendingFileMutation
	question     *agentloop.PendingQuestion
}

func renderTranscriptEntry(entry transcriptEntry, width int, toolsExpanded bool) string {
	if width < 20 {
		width = 20
	}
	switch entry.kind {
	case entryUser:
		return userStyle.Width(width).Render(userLabelStyle.Render("你") + "\n" + renderMarkdown(entry.text, width-3))
	case entryAssistant:
		label := "Agent"
		switch {
		case entry.streaming:
			label += " · 正在生成"
		case entry.stopped:
			label += " · 已停止"
		case entry.failed:
			label += " · 未完成"
		}
		return assistantStyle.Width(width).Render(assistantLabelStyle.Render(label) + "\n" + renderMarkdown(entry.text, width-3))
	case entryThinking:
		return renderThinkingActivity(entry.activity, width, toolsExpanded)
	case entryTools:
		return renderToolGroup(entry.activities, width, toolsExpanded)
	case entryConfirm:
		return confirmStyle.Width(max(20, width-4)).Render(confirmLabelStyle.Render("长期偏好确认") + "\n" + safeTerminalText(entry.text))
	case entryFileConfirm:
		return confirmStyle.Width(max(20, width-4)).Render(confirmLabelStyle.Render("文件修改授权") + "\n" + safeTerminalText(entry.text))
	case entryQuestion:
		return questionStyle.Width(max(20, width-4)).Render(questionLabelStyle.Render("需要你的选择") + "\n" + safeTerminalText(entry.text))
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

func renderThinkingActivity(activity agentloop.Activity, width int, expanded bool) string {
	event := activity.Event
	status := normalizedEventStatus(event.Status)
	icon, label, style := "◇", "已思考", thinkingDoneStyle
	switch status {
	case agentloop.EventRunning:
		icon, label, style = "◐", "思考中", thinkingActiveStyle
	case agentloop.EventFailed:
		icon, label, style = "!", "思考中断", toolDangerStyle
	}
	line := fmt.Sprintf("%s %s  %s", icon, label, safeTerminalText(event.Summary))
	lines := []string{style.Render(line)}
	if expanded {
		detail := fmt.Sprintf("  阶段：%s", phaseLabel(activity.Phase))
		if elapsed := activityElapsed(activity); elapsed != "" {
			detail += " · 用时：" + elapsed
		}
		if activity.TimeoutBudget > 0 {
			detail += " · 无响应超时：" + visibleDuration(activity.TimeoutBudget)
		}
		if activity.StableCode != "" {
			detail += " · 代码：" + safeTerminalText(activity.StableCode)
		}
		lines = append(lines, toolDetailStyle.Render(detail))
	}
	return thinkingStyle.Width(width).Render(strings.Join(lines, "\n"))
}

func renderToolGroup(activities []agentloop.Activity, width int, expanded bool) string {
	if len(activities) == 0 {
		return ""
	}
	succeeded, failed, waiting, running := 0, 0, 0, 0
	for _, activity := range activities {
		switch normalizedEventStatus(activity.Event.Status) {
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
	summary := fmt.Sprintf("工具调用 · %d 项", len(activities))
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
	for _, activity := range activities {
		event := activity.Event
		status := normalizedEventStatus(event.Status)
		icon, statusText, style := toolEventAppearance(status)
		name := toolDisplayName(event.Tool)
		line := fmt.Sprintf("%s %s", icon, name)
		statusSuffix := " · " + statusText
		if activity.File != nil && activity.File.Path != "" {
			pathWidth := max(4, width-2-lipgloss.Width(line)-lipgloss.Width(statusSuffix)-3)
			line += " · " + truncateDisplayWidth(safeSingleLineTerminalText(activity.File.Path), pathWidth)
		}
		line += statusSuffix
		if summaryText := strings.TrimSpace(safeSingleLineTerminalText(event.Summary)); summaryText != "" {
			available := max(0, width-2-lipgloss.Width(line)-3)
			if available >= 8 {
				line += " · " + truncateDisplayWidth(summaryText, available)
			}
		}
		lines = append(lines, style.Render(truncateDisplayWidth(line, max(20, width-2))))
		if expanded {
			baseDetail := fmt.Sprintf("状态：%s · 阶段：%s · 工具：%s", statusText, phaseLabel(activity.Phase), safeTerminalText(event.Tool))
			lines = appendWrappedToolDetail(lines, "    "+baseDetail, max(20, width-4), 3)
			lifecycle := make([]string, 0, 3)
			if elapsed := activityElapsed(activity); elapsed != "" {
				lifecycle = append(lifecycle, "用时："+elapsed)
			}
			if activity.TimeoutBudget > 0 {
				lifecycle = append(lifecycle, "超时预算："+visibleDuration(activity.TimeoutBudget))
			}
			code := activity.StableCode
			if code == "" {
				code = event.Detail
			}
			if strings.TrimSpace(code) != "" {
				lifecycle = append(lifecycle, "代码："+safeTerminalText(code))
			}
			if len(lifecycle) > 0 {
				lines = appendWrappedToolDetail(lines, "    "+strings.Join(lifecycle, " · "), max(20, width-4), 3)
			}
			lines = append(lines, renderFileActivityDetails(activity.File, max(20, width-4))...)
		}
	}
	return toolGroupStyle.Width(width).Render(strings.Join(lines, "\n"))
}

func renderFileActivityDetails(detail *agentloop.FileActivityDetail, width int) []string {
	if detail == nil {
		return nil
	}
	lines := make([]string, 0, 12)
	appendLine := func(value string) {
		lines = appendWrappedToolDetail(lines, "    "+value, width, 4)
	}
	if detail.Path != "" {
		appendLine("路径：" + safeSingleLineTerminalText(detail.Path))
	}
	operation := make([]string, 0, 3)
	if detail.Operation != "" {
		operation = append(operation, "操作："+safeSingleLineTerminalText(detail.Operation))
	}
	if detail.PublicationOutcome != "" {
		operation = append(operation, "发布结果："+safeSingleLineTerminalText(detail.PublicationOutcome))
	}
	if detail.FirstChangedLine > 0 {
		operation = append(operation, fmt.Sprintf("首个变化行：%d", detail.FirstChangedLine))
	}
	if len(operation) > 0 {
		appendLine(strings.Join(operation, " · "))
	}
	if detail.HasReturned {
		appendLine(fmt.Sprintf("返回：%d 项", detail.Returned))
	}
	if detail.HasRange {
		if detail.EndLine >= detail.StartLine {
			appendLine(fmt.Sprintf("范围：第 %d-%d 行", detail.StartLine, detail.EndLine))
		} else {
			appendLine(fmt.Sprintf("范围：从第 %d 行开始", detail.StartLine))
		}
	}
	if detail.HasBytes {
		appendLine(fmt.Sprintf("返回字节：%d", detail.Bytes))
	}
	if detail.HasScanned {
		scan := fmt.Sprintf("扫描：%d 个文件 · %d 字节", detail.ScannedFiles, detail.ScannedBytes)
		if detail.HasMatches {
			scan += fmt.Sprintf(" · 匹配：%d", detail.Matches)
		}
		appendLine(scan)
	} else if detail.HasMatches {
		appendLine(fmt.Sprintf("匹配：%d", detail.Matches))
	}
	if detail.TruncationReason != "" {
		appendLine("截断原因：" + safeSingleLineTerminalText(detail.TruncationReason))
	}
	if detail.HasContinuation {
		continuation := fmt.Sprintf("继续：next_offset=%d", detail.NextOffset)
		if detail.HasRange || detail.HasBytes {
			continuation += fmt.Sprintf(" · next_byte_offset=%d", detail.NextByteOffset)
		}
		appendLine(continuation)
	}
	if detail.PreviewKind != "" {
		appendLine("预览类型：" + safeSingleLineTerminalText(detail.PreviewKind))
	}
	if detail.PreviewTruncated {
		appendLine("最终预览已按安全上限截断")
	}
	if detail.Preview != "" {
		label := "最终预览："
		if detail.PreviewKind == "diff" {
			label = "最终差异："
		}
		appendLine(label)
		preview, truncated := boundedActivityPreview(detail.Preview, 4<<10)
		previewWidth := max(8, width-6)
		if previewDisplayLineCount(preview, previewWidth) > 32 {
			truncated = true
		}
		wrapped := wrapDisplayLines(preview, previewWidth, 32)
		for _, line := range wrapped {
			lines = append(lines, toolDetailStyle.Render("      "+line))
		}
		if truncated {
			appendLine("终端详情已按 4 KiB 上限收敛")
		}
	}
	return lines
}

func appendWrappedToolDetail(lines []string, value string, width, maxLines int) []string {
	for _, line := range wrapDisplayLines(value, width, maxLines) {
		lines = append(lines, toolDetailStyle.Render(line))
	}
	return lines
}

func boundedActivityPreview(value string, limit int) (string, bool) {
	value = safeTerminalText(value)
	if limit <= 0 || len(value) <= limit {
		return value, false
	}
	value = value[:limit]
	for value != "" && !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value, true
}

func previewDisplayLineCount(value string, width int) int {
	if value == "" || width <= 0 {
		return 0
	}
	count := 0
	for line := range strings.SplitSeq(value, "\n") {
		count += max(1, (lipgloss.Width(line)+width-1)/width)
	}
	return count
}

func activityElapsed(activity agentloop.Activity) string {
	if activity.StartedAt.IsZero() {
		return ""
	}
	end := activity.UpdatedAt
	if normalizedEventStatus(activity.Event.Status) == agentloop.EventRunning {
		end = time.Now()
	}
	if end.IsZero() || end.Before(activity.StartedAt) {
		end = activity.StartedAt
	}
	duration := end.Sub(activity.StartedAt).Round(100 * time.Millisecond)
	if duration < time.Second {
		return fmt.Sprintf("%.1fs", duration.Seconds())
	}
	return duration.Round(time.Second).String()
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
		"ask_user_question":          "询问用户",
		"remember_preference":        "保存长期偏好",
		"list":                       "列出工作区",
		"read":                       "读取文件",
		"search":                     "搜索文件",
		"write":                      "写入文件",
		"edit":                       "编辑文件",
	}
	if label, ok := labels[name]; ok {
		return label
	}
	return "未知工具"
}

func eventKey(event agentloop.Event) string {
	return strings.Join([]string{event.ID, event.Tool, string(normalizedEventStatus(event.Status)), event.Summary, event.Detail}, "\x00")
}

func upsertAssistantDelta(entries []transcriptEntry, turnID uint64, delta string) []transcriptEntry {
	delta = safeTerminalText(delta)
	if delta == "" {
		return entries
	}
	for index := len(entries) - 1; index >= 0; index-- {
		if entries[index].kind == entryAssistant && entries[index].turnID == turnID && entries[index].streaming {
			entries[index].text += delta
			return entries
		}
	}
	return append(entries, transcriptEntry{kind: entryAssistant, text: delta, turnID: turnID, streaming: true})
}

func finalizeAssistant(entries []transcriptEntry, turnID uint64, text string) []transcriptEntry {
	text = safeTerminalText(text)
	for index := len(entries) - 1; index >= 0; index-- {
		if entries[index].kind == entryAssistant && entries[index].turnID == turnID && entries[index].streaming {
			if text != "" {
				entries[index].text = text
			}
			entries[index].streaming = false
			return entries
		}
	}
	if strings.TrimSpace(text) != "" {
		entries = append(entries, transcriptEntry{kind: entryAssistant, text: text, turnID: turnID})
	}
	return entries
}

func markAssistant(entries []transcriptEntry, turnID uint64, state string) []transcriptEntry {
	for index := len(entries) - 1; index >= 0; index-- {
		if entries[index].kind == entryAssistant && entries[index].turnID == turnID && entries[index].streaming {
			entries[index].streaming = false
			entries[index].stopped = state == "stopped"
			entries[index].failed = state == "failed"
			return entries
		}
	}
	if state == "stopped" {
		return append(entries, transcriptEntry{
			kind: entryAssistant, text: "当前轮次已停止，未提交回答。", turnID: turnID, stopped: true,
		})
	}
	return entries
}

func upsertActivity(entries []transcriptEntry, turnID uint64, activity agentloop.Activity) []transcriptEntry {
	if activity.Kind == agentloop.ActivityThinking {
		if activity.Event.ID != "" {
			for index := len(entries) - 1; index >= 0; index-- {
				if entries[index].kind == entryUser && entries[index].turnID != turnID {
					break
				}
				if entries[index].kind == entryThinking && entries[index].turnID == turnID && entries[index].activity.Event.ID == activity.Event.ID {
					entries[index].activity = activity
					return entries
				}
			}
		}
		return append(entries, transcriptEntry{kind: entryThinking, activity: activity, turnID: turnID})
	}
	if activity.Kind != agentloop.ActivityTool {
		return entries
	}
	if activity.Event.ID != "" {
		for entryIndex := len(entries) - 1; entryIndex >= 0; entryIndex-- {
			if entries[entryIndex].kind != entryTools || entries[entryIndex].turnID != turnID {
				continue
			}
			for activityIndex := len(entries[entryIndex].activities) - 1; activityIndex >= 0; activityIndex-- {
				if entries[entryIndex].activities[activityIndex].Event.ID == activity.Event.ID {
					entries[entryIndex].activities[activityIndex] = activity
					return entries
				}
			}
		}
	}
	if len(entries) > 0 && entries[len(entries)-1].kind == entryTools && entries[len(entries)-1].turnID == turnID {
		entries[len(entries)-1].activities = append(entries[len(entries)-1].activities, activity)
		return entries
	}
	return append(entries, transcriptEntry{kind: entryTools, activities: []agentloop.Activity{activity}, turnID: turnID})
}

func appendToolEvents(entries []transcriptEntry, turnID uint64, events []agentloop.Event) []transcriptEntry {
	for _, event := range events {
		entries = upsertActivity(entries, turnID, agentloop.Activity{Kind: agentloop.ActivityTool, Event: event, Phase: agentloop.ActivityExecutingTool})
	}
	return entries
}

func updatePreferenceToolStatus(entries []transcriptEntry, status agentloop.EventStatus, summary, detail string) []transcriptEntry {
	for entryIndex := len(entries) - 1; entryIndex >= 0; entryIndex-- {
		if entries[entryIndex].kind != entryTools {
			continue
		}
		for activityIndex := len(entries[entryIndex].activities) - 1; activityIndex >= 0; activityIndex-- {
			activity := &entries[entryIndex].activities[activityIndex]
			if activity.Event.Tool == "remember_preference" {
				activity.Event.Status = status
				activity.Event.Summary = summary
				activity.Event.Detail = detail
				activity.StableCode = detail
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
	questionLabelStyle        = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("6"))
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
	questionStyle             = lipgloss.NewStyle().Border(lipgloss.NormalBorder()).BorderForeground(lipgloss.Color("6")).Padding(0, 1)
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
