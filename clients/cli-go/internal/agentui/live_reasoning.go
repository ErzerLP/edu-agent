package agentui

import (
	"strings"
	"time"
	"unicode/utf8"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/agentlimits"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/agentloop"
)

// UI-only state. Never attach this text to Activity metadata or durable entries.
type liveReasoning struct {
	text      strings.Builder
	truncated bool
	evicted   bool
}

// Total retained reasoning across this UI, independent of model token settings.
const liveReasoningBytes = agentlimits.MaxAssistantTextBytes

func (m *model) handleLiveReasoning(turnID uint64, activity agentloop.Activity) bool {
	if activity.Kind != agentloop.ActivityReasoningDelta && activity.Kind != agentloop.ActivityResponseProgress {
		return false
	}
	for i := len(m.entries) - 1; i >= 0; i-- {
		entry := &m.entries[i]
		if entry.kind != entryThinking || entry.turnID != turnID || entry.activity.Event.ID != activity.Event.ID {
			continue
		}
		// A delayed delta must never reopen a settled request.
		if normalizedEventStatus(entry.activity.Event.Status) != agentloop.EventRunning {
			return true
		}
		if activity.Kind == agentloop.ActivityResponseProgress {
			if activity.ReceivedAt.After(entry.lastResponseAt) {
				entry.lastResponseAt = activity.ReceivedAt
			}
			return true
		}
		if entry.reasoning == nil {
			entry.reasoning = &liveReasoning{}
		}
		if entry.reasoning.truncated {
			return true
		}
		text := safeTerminalText(activity.Delta)
		used := 0
		for _, current := range m.entries {
			if current.reasoning != nil {
				used += current.reasoning.text.Len()
			}
		}
		for j := range m.entries {
			if used+len(text) <= liveReasoningBytes {
				break
			}
			old := m.entries[j].reasoning
			if j == i || old == nil {
				continue
			}
			used -= old.text.Len()
			old.text.Reset()
			old.evicted = true
		}
		keep := min(len(text), liveReasoningBytes-used)
		for keep > 0 && !utf8.ValidString(text[:keep]) {
			keep--
		}
		entry.reasoning.text.WriteString(text[:keep])
		entry.reasoning.truncated = keep < len(text)
		return true
	}
	return true // Unknown request identity cannot create a new card.
}

func renderThinkingEntry(entry transcriptEntry, width int, expanded bool) string {
	activity := entry.activity
	if !expanded && entry.reasoning != nil && entry.reasoning.text.Len() > 0 {
		activity.Event.Summary += " · Ctrl+O 查看推理"
	}
	result := renderThinkingActivity(activity, width, expanded)
	if !expanded {
		return result
	}
	lines := []string{}
	if entry.lastResponseAt.IsZero() {
		lines = append(lines, "尚无响应正文接收记录；恢复的历史不保留此信息。")
	} else {
		end := time.Now()
		if normalizedEventStatus(activity.Event.Status) != agentloop.EventRunning {
			end = activity.UpdatedAt
		}
		elapsed := max(time.Duration(0), end.Sub(entry.lastResponseAt)).Round(time.Second)
		lines = append(lines, "距最后接收响应数据："+elapsed.String()+"（含心跳，不代表已有回答）")
	}
	if entry.reasoning == nil {
		lines = append(lines, "接口尚未提供可显示的推理文本；不会推测或补写思考链。")
	} else {
		lines = append(lines, "模型推理 · 接口返回，仅内存展示，不作为执行依据")
		if entry.reasoning.text.Len() > 0 {
			lines = append(lines, entry.reasoning.text.String())
		}
		if entry.reasoning.truncated {
			lines = append(lines, "推理展示已截断：仅保留内存预算内的前缀；模型请求继续。")
		}
		if entry.reasoning.evicted {
			lines = append(lines, "该请求的推理正文已从内存回收，不可从历史恢复。")
		}
	}
	return result + "\n" + thinkingStyle.Width(width).Render(strings.Join(lines, "\n"))
}
