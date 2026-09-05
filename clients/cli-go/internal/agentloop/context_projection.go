package agentloop

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/agentlimits"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/modelclient"
)

type ContextPlanner struct {
	ContextWindow          int
	MaxTokens              int
	Mode                   string
	Estimator              TokenEstimator
	Memory                 ContextMemoryProjection
	ReservedOutputOverride int
	// Source IDs refer to the existing exact-ID recall ledger. The hash and
	// turn index still identify a projection in recent-only mode (no ledger).
	AssistantSources map[int]map[string]string
	ProtectedGroups  map[int]bool
}

func (p ContextPlanner) Plan(messages []modelclient.Message, tools []modelclient.Tool, history map[string]string) (ContextPlan, error) {
	if p.ContextWindow < 1 || p.Estimator == nil {
		return ContextPlan{}, contextError(ContextBudgetInvalid, "上下文预算配置无效")
	}
	mode := p.Mode
	if mode == "" {
		mode = ContextCompactionAuto
	}
	if mode != ContextCompactionAuto && mode != ContextCompactionRecentOnly && mode != ContextCompactionOff {
		return ContextPlan{}, contextError(ContextBudgetInvalid, "上下文压缩模式无效")
	}
	outputCeiling := p.MaxTokens
	if outputCeiling == 0 {
		outputCeiling = agentlimits.MaxOutputTokens
	}
	if !agentlimits.ValidMaxTokens(outputCeiling) {
		return ContextPlan{}, contextError(ContextBudgetInvalid, "最大输出 tokens 配置无效")
	}
	if p.ReservedOutputOverride > 0 {
		outputCeiling = min(outputCeiling, p.ReservedOutputOverride)
	}
	safetyMargin := divideRoundUp(p.ContextWindow*5, 100)
	// Legacy/small windows cannot reserve the configured large ceiling. Keep
	// useful input capacity (including committed memory) instead of spending
	// every spare token on output. There is no old 8192-token output cap.
	if outputCeiling+safetyMargin >= p.ContextWindow {
		outputCeiling = min(outputCeiling, max(1024, divideRoundUp(p.ContextWindow*15, 100)))
	}
	minimumOutput := min(512, outputCeiling)
	maximumInput := p.ContextWindow - safetyMargin - minimumOutput
	if maximumInput <= 0 {
		return ContextPlan{}, contextError(ContextBudgetInvalid, "模型窗口无法保留回答和安全余量")
	}
	system, conversational := splitSystemMessages(messages)
	fixed := modelclient.Request{Messages: system, Tools: tools}
	fixedTokens := p.Estimator.EstimateRequest(fixed)
	if fixedTokens > maximumInput {
		return ContextPlan{}, contextError(ContextBudgetInvalid, "系统规则和工具定义超过模型上下文预算")
	}
	groups := messageGroups(conversational)
	if len(groups) == 0 {
		fixed.MaxTokens = min(outputCeiling, p.ContextWindow-safetyMargin-fixedTokens)
		return ContextPlan{Request: fixed, EstimatedInput: fixedTokens, ReservedOutput: fixed.MaxTokens, SafetyMargin: safetyMargin}, nil
	}
	current := groups[len(groups)-1]
	if fixedTokens+p.estimateAdditional(current) > maximumInput && mode != ContextCompactionOff {
		current = projectCompletedToolCallArguments(current)
	}
	if fixedTokens+p.estimateAdditional(current) > maximumInput {
		return ContextPlan{}, contextError(ContextTurnTooLarge, "当前完整对话轮次超过上下文上限，请缩短输入或减少工具结果后重试")
	}
	if mode == ContextCompactionOff {
		request := modelclient.Request{Messages: appendCopy(system, conversational), Tools: tools}
		estimated := p.Estimator.EstimateRequest(request)
		if estimated > maximumInput {
			return ContextPlan{}, contextError(ContextBudgetInvalid, "完整对话历史超过上下文预算，context_compaction=off 禁止裁剪")
		}
		request.MaxTokens = min(outputCeiling, p.ContextWindow-safetyMargin-estimated)
		return ContextPlan{Request: request, EstimatedInput: estimated, ReservedOutput: request.MaxTokens, SafetyMargin: safetyMargin, TotalTurns: len(groups), SelectedTurns: len(groups)}, nil
	}

	projected := make([][]modelclient.Message, len(groups))
	selected := make([]bool, len(groups))
	bounded := make([]bool, len(groups))
	total, fullEstimate := fixedTokens, fixedTokens
	recentStart := max(0, len(groups)-3)
	for index, group := range groups {
		if index == len(groups)-1 {
			projected[index] = current
		} else {
			projected[index] = projectHistoryGroup(group, history)
		}
		fullEstimate += p.estimateAdditional(projected[index])
		// A tool chain is never dropped to make room for prose. This also
		// retains completed/unknown effects and their authorization context.
		selected[index] = index >= recentStart || p.ProtectedGroups[index] || groupHasTools(group)
		if selected[index] {
			total += p.estimateAdditional(projected[index])
		}
	}
	// Prefer recent originals, first shrinking output down to its effective
	// minimum. If originals still cannot fit, bound only older assistant
	// prose, leaving user statements and entire tool call/result pairs intact.
	for index := 0; total > maximumInput && index < len(groups)-1; index++ {
		if !selected[index] || p.ProtectedGroups[index] {
			continue
		}
		candidate, changed := p.boundHistoryProse(projected[index], index)
		if changed {
			total += p.estimateAdditional(candidate) - p.estimateAdditional(projected[index])
			projected[index], bounded[index] = candidate, true
		}
	}
	if total > maximumInput {
		return ContextPlan{}, contextError(ContextRecentTurnsTooLarge, "历史轮次的用户陈述、工具结果或受保护事实超过上下文预算，请开启新会话")
	}
	reservedOutput := min(outputCeiling, p.ContextWindow-safetyMargin-total)
	inputLimit := p.ContextWindow - safetyMargin - reservedOutput
	var memoryMessage *modelclient.Message
	memoryItemCount := 0
	if mode == ContextCompactionAuto && len(p.Memory.Items) > 0 {
		memoryCap := min(divideRoundUp(p.ContextWindow*20, 100), inputLimit-total)
		if memoryCap > 0 {
			memoryMessage, memoryItemCount = p.selectMemoryMessage(memoryCap)
			if memoryMessage != nil {
				total += p.estimateAdditional([]modelclient.Message{*memoryMessage})
			}
		}
	}
	for index := len(groups) - 1; index >= 0; index-- {
		if selected[index] {
			continue
		}
		size := p.estimateAdditional(projected[index])
		if total+size > inputLimit {
			continue
		}
		selected[index] = true
		total += size
	}
	selectedMessages := append([]modelclient.Message(nil), system...)
	if memoryMessage != nil {
		selectedMessages = append(selectedMessages, *memoryMessage)
	}
	selectedCount, projectedCount := 0, 0
	for index, group := range projected {
		if !selected[index] {
			continue
		}
		selectedCount++
		if bounded[index] {
			projectedCount++
		}
		selectedMessages = append(selectedMessages, group...)
	}
	request := modelclient.Request{Messages: selectedMessages, Tools: tools, MaxTokens: reservedOutput}
	estimated := p.Estimator.EstimateRequest(request)
	request.MaxTokens = min(reservedOutput, p.ContextWindow-safetyMargin-estimated)
	if request.MaxTokens < minimumOutput {
		return ContextPlan{}, contextError(ContextBudgetInvalid, "上下文估算无法满足回答与安全余量")
	}
	return ContextPlan{
		Request: request, EstimatedInput: estimated, ReservedOutput: request.MaxTokens, SafetyMargin: safetyMargin,
		SoftPressure: fullEstimate >= divideRoundUp(inputLimit*72, 100), TotalTurns: len(groups), SelectedTurns: selectedCount,
		DroppedTurns: len(groups) - selectedCount, ProjectedTurns: projectedCount, MemoryItemCount: memoryItemCount, UsedMemory: memoryMessage != nil,
	}, nil
}

func groupHasTools(group []modelclient.Message) bool {
	for _, message := range group {
		if message.Role == "tool" || len(message.ToolCalls) > 0 {
			return true
		}
	}
	return false
}

func (p ContextPlanner) boundHistoryProse(group []modelclient.Message, turnIndex int) ([]modelclient.Message, bool) {
	result := append([]modelclient.Message(nil), group...)
	changed := false
	for index, message := range result {
		if message.Role != "assistant" || len(message.Content) <= 2048 {
			continue
		}
		hash := sha256.Sum256([]byte(message.Content))
		projection := struct {
			Degraded      bool   `json:"degraded"`
			Code          string `json:"code"`
			HistoryTurn   int    `json:"history_turn"`
			SourceID      string `json:"source_id,omitempty"`
			SHA256        string `json:"sha256"`
			OriginalBytes int    `json:"original_bytes"`
			Excerpt       string `json:"excerpt"`
			Notice        string `json:"notice"`
		}{true, "context_history_projected", turnIndex + 1, p.AssistantSources[turnIndex][message.Content], hex.EncodeToString(hash[:]), len(message.Content), truncateUTF8(message.Content, 1024), "历史助手正文节选，非完整原文或用户授权；来源回查同样有界。"}
		data, _ := json.Marshal(projection)
		result[index].Content = string(data)
		changed = true
	}
	return result, changed
}

func (p ContextPlanner) selectMemoryMessage(tokenCap int) (*modelclient.Message, int) {
	instruction := strings.TrimSpace(p.Memory.Instruction)
	if instruction == "" || len(p.Memory.Items) == 0 {
		return nil, 0
	}
	selected := make([]string, 0, len(p.Memory.Items))
	for _, item := range p.Memory.Items {
		candidate := instruction + "\n\n会话记忆：\n" + strings.Join(append(selected, item), "\n")
		message := modelclient.Message{Role: "system", Content: candidate}
		if p.estimateAdditional([]modelclient.Message{message}) > tokenCap {
			break
		}
		selected = append(selected, item)
	}
	if len(selected) == 0 {
		return nil, 0
	}
	return &modelclient.Message{Role: "system", Content: instruction + "\n\n会话记忆：\n" + strings.Join(selected, "\n")}, len(selected)
}

func (p ContextPlanner) estimateAdditional(messages []modelclient.Message) int {
	base := p.Estimator.EstimateRequest(modelclient.Request{})
	total := p.Estimator.EstimateRequest(modelclient.Request{Messages: messages}) - base
	if total < 0 {
		return 0
	}
	return total
}

func splitSystemMessages(messages []modelclient.Message) ([]modelclient.Message, []modelclient.Message) {
	system := make([]modelclient.Message, 0, 1)
	conversation := make([]modelclient.Message, 0, len(messages))
	for _, message := range messages {
		if message.Role == "system" {
			system = append(system, message)
		} else {
			conversation = append(conversation, message)
		}
	}
	return system, conversation
}

func projectCompletedToolCallArguments(group []modelclient.Message) []modelclient.Message {
	completed := make(map[string]struct{})
	for _, message := range group {
		if message.Role == "tool" && message.ToolCallID != "" {
			completed[message.ToolCallID] = struct{}{}
		}
	}
	result := make([]modelclient.Message, len(group))
	for index, message := range group {
		result[index] = cloneModelMessage(message)
		if message.Role != "assistant" || len(message.ToolCalls) == 0 {
			continue
		}
		for callIndex := range result[index].ToolCalls {
			if _, ok := completed[result[index].ToolCalls[callIndex].ID]; ok {
				result[index].ToolCalls[callIndex].Function.Arguments = `{}`
			}
		}
	}
	return result
}

func projectHistoryGroup(group []modelclient.Message, history map[string]string) []modelclient.Message {
	result := append([]modelclient.Message(nil), group...)
	for index := range result {
		if result[index].Role != "tool" {
			continue
		}
		if projected, ok := history[result[index].ToolCallID]; ok && projected != "" {
			result[index].Content = projected
		}
	}
	return result
}

func appendCopy(first, second []modelclient.Message) []modelclient.Message {
	result := make([]modelclient.Message, 0, len(first)+len(second))
	result = append(result, first...)
	result = append(result, second...)
	return result
}

func clampInt(value, minimum, maximum int) int {
	if value < minimum {
		return minimum
	}
	if value > maximum {
		return maximum
	}
	return value
}
