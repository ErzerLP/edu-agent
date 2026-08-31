package agentloop

import (
	"strings"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/modelclient"
)

type ContextPlanner struct {
	ContextWindow int
	Mode          string
	Estimator     TokenEstimator
	Memory        ContextMemoryProjection
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

	reservedOutput := clampInt(divideRoundUp(p.ContextWindow*15, 100), 1024, 8192)
	safetyMargin := divideRoundUp(p.ContextWindow*5, 100)
	inputLimit := p.ContextWindow - reservedOutput - safetyMargin
	if inputLimit <= 0 {
		return ContextPlan{}, contextError(ContextBudgetInvalid, "模型窗口无法保留回答和安全余量")
	}

	system, conversational := splitSystemMessages(messages)
	fixed := modelclient.Request{Messages: system, Tools: tools}
	fixedTokens := p.Estimator.EstimateRequest(fixed)
	if fixedTokens > inputLimit {
		return ContextPlan{}, contextError(ContextBudgetInvalid, "系统规则和工具定义超过模型上下文预算")
	}

	groups := messageGroups(conversational)
	if len(groups) == 0 {
		request := fixed
		request.MaxTokens = reservedOutput
		return ContextPlan{Request: request, EstimatedInput: fixedTokens, ReservedOutput: reservedOutput, SafetyMargin: safetyMargin}, nil
	}

	current := groups[len(groups)-1]
	currentTokens := p.estimateAdditional(current)
	if fixedTokens+currentTokens > inputLimit {
		return ContextPlan{}, contextError(ContextTurnTooLarge, "当前完整对话轮次超过上下文上限，请缩短输入或减少工具结果后重试")
	}

	if mode == ContextCompactionOff {
		request := modelclient.Request{Messages: appendCopy(system, conversational), Tools: tools, MaxTokens: reservedOutput}
		estimated := p.Estimator.EstimateRequest(request)
		if estimated > inputLimit {
			return ContextPlan{}, contextError(ContextBudgetInvalid, "完整对话历史超过上下文预算，context_compaction=off 禁止裁剪")
		}
		return ContextPlan{
			Request: request, EstimatedInput: estimated, ReservedOutput: reservedOutput, SafetyMargin: safetyMargin,
			TotalTurns: len(groups), SelectedTurns: len(groups), DroppedTurns: 0,
		}, nil
	}

	projected := make([][]modelclient.Message, len(groups))
	for index, group := range groups {
		if index == len(groups)-1 {
			projected[index] = append([]modelclient.Message(nil), group...)
		} else {
			projected[index] = projectHistoryGroup(group, history)
		}
	}
	fullMessages := append([]modelclient.Message(nil), system...)
	for _, group := range projected {
		fullMessages = append(fullMessages, group...)
	}
	fullEstimate := p.Estimator.EstimateRequest(modelclient.Request{Messages: fullMessages, Tools: tools})
	softPressure := fullEstimate >= divideRoundUp(inputLimit*72, 100)

	minimumRecent := min(2, len(projected)-1)
	selectedStart := len(projected) - 1 - minimumRecent
	total := fixedTokens
	for index := selectedStart; index < len(projected); index++ {
		total += p.estimateAdditional(projected[index])
	}
	if total > inputLimit {
		return ContextPlan{}, contextError(ContextRecentTurnsTooLarge, "当前轮次与最近两个完整轮次无法同时放入上下文，请开启新会话或减少最近轮次中的大型工具结果")
	}

	var memoryMessage *modelclient.Message
	memoryItemCount := 0
	if mode == ContextCompactionAuto && len(p.Memory.Items) > 0 {
		memoryCap := min(divideRoundUp(p.ContextWindow*20, 100), inputLimit-total)
		if memoryCap > 0 {
			memoryMessage, memoryItemCount = p.selectMemoryMessage(memoryCap)
			if memoryMessage != nil {
				memoryTokens := p.estimateAdditional([]modelclient.Message{*memoryMessage})
				if total+memoryTokens <= inputLimit {
					total += memoryTokens
				} else {
					memoryMessage = nil
					memoryItemCount = 0
				}
			}
		}
	}

	for index := selectedStart - 1; index >= 0; index-- {
		size := p.estimateAdditional(projected[index])
		if total+size > inputLimit {
			break
		}
		selectedStart = index
		total += size
	}

	selected := make([]modelclient.Message, 0, len(system)+len(conversational)+1)
	selected = append(selected, system...)
	if memoryMessage != nil {
		selected = append(selected, *memoryMessage)
	}
	for index := selectedStart; index < len(projected); index++ {
		selected = append(selected, projected[index]...)
	}
	request := modelclient.Request{Messages: selected, Tools: tools, MaxTokens: reservedOutput}
	return ContextPlan{
		Request: request, EstimatedInput: p.Estimator.EstimateRequest(request),
		ReservedOutput: reservedOutput, SafetyMargin: safetyMargin, SoftPressure: softPressure,
		TotalTurns: len(groups), SelectedTurns: len(projected) - selectedStart,
		DroppedTurns: selectedStart, MemoryItemCount: memoryItemCount, UsedMemory: memoryMessage != nil,
	}, nil
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
