package mentorrun

import (
	"context"
	"errors"
	"strings"

	"github.com/edu-agent/edu-agent/packages/agentcore"
	"github.com/edu-agent/edu-agent/packages/agentcore/modelclient"
)

var ErrContextConfiguration = errors.New("context_configuration_invalid")

const historyNotice = "较早内容未纳入；本次回答仅依据已提供的话题要点和最近原文。"

func contextMargin(c int) int { return max(256, (c+19)/20) }
func messageCost(m []modelclient.Message) int {
	return agentcore.NewTokenEstimator().EstimateRequest(modelclient.Request{Messages: m}) - 8
}

func projectTools(messages []modelclient.Message, limit int) []modelclient.Message {
	result := append([]modelclient.Message(nil), messages...)
	for i, m := range result {
		if m.Role == "tool" && len(m.Content) > limit {
			result[i].Content = bounded(m.Content, limit) + "\n（工具结果仅纳入部分内容）"
		}
	}
	return result
}

func projectOldTools(messages []modelclient.Message, limit int) []modelclient.Message {
	latest := len(messages)
	for i, message := range messages {
		if len(message.ToolCalls) > 0 {
			latest = i
		}
	}
	result := projectTools(messages[:latest], limit)
	return append(result, messages[latest:]...)
}
func messageGroups(messages []modelclient.Message) [][]modelclient.Message {
	messages = committedMessages(messages)
	groups := [][]modelclient.Message{}
	for i := 0; i < len(messages); {
		end := i + 1 + len(messages[i].ToolCalls)
		groups = append(groups, messages[i:end])
		i = end
	}
	return groups
}
func (h *executionHost) withContextNotice(output string) string {
	if h.body.ContextNotice != "" && !strings.Contains(output, h.body.ContextNotice) {
		return bounded(output, MaxOutput-len(h.body.ContextNotice)-5) + "\n\n> " + h.body.ContextNotice
	}
	return output
}

// fixed includes system, direction and schemas. Source data and historical turns
// are charged separately; current user input and completed tool groups go first.
func (h *executionHost) assembleContext(ctx context.Context, fixed modelclient.Request, source []modelclient.Message, current []modelclient.Message) (agentcore.ContextPlan, error) {
	fixed.Messages = append([]modelclient.Message(nil), fixed.Messages...)
	fixed.Messages[0].Content += " 上下文可能只含部分历史与资料；只回答实际纳入的内容，不声称已读完全部。"
	est := agentcore.NewTokenEstimator()
	capacity := h.limits.ContextTokens - fixed.MaxTokens - contextMargin(h.limits.ContextTokens) - est.EstimateRequest(fixed) - h.schemaTokens
	if capacity < 0 {
		return agentcore.ContextPlan{}, ErrContextConfiguration
	}
	notice := modelclient.Message{Role: "user", Content: historyNotice}
	available := max(0, capacity-messageCost([]modelclient.Message{notice})-48)
	sourceUsed := messageCost(source)
	// Source readers have already bounded and frozen this array. Preserve citation
	// numbering while allocating remaining room to current atomic message groups.
	currentRoom := max(0, available-sourceUsed)
	groups := messageGroups(projectOldTools(current, 1024))
	latestUser := -1
	for i, g := range groups {
		if len(g) == 1 && g[0].Role == "user" {
			latestUser = i
		}
	}
	order := []int{}
	if latestUser >= 0 {
		order = append(order, latestUser)
	}
	for i := len(groups) - 1; i >= 0; i-- {
		if i != latestUser {
			order = append(order, i)
		}
	}
	selected := map[int][]modelclient.Message{}
	used := 0
	omitted := false
	for _, i := range order {
		group := groups[i]
		cost := messageCost(group)
		if used+cost > currentRoom && len(group[0].ToolCalls) > 0 {
			group = projectTools(group, min(1024, max(0, currentRoom-used-128)))
			cost = messageCost(group)
		}
		if used+cost > currentRoom {
			if i == latestUser {
				message, err := h.projectCurrentInput(ctx, group[0], max(0, currentRoom-used))
				if err != nil {
					return agentcore.ContextPlan{}, err
				}
				group = []modelclient.Message{message}
				cost = messageCost(group)
				if cost <= currentRoom-used {
					selected[i] = group
					used += cost
				}
			}
			omitted = true
			continue
		}
		selected[i] = group
		used += cost
	}
	room := max(0, available-used-sourceUsed)
	// Inspect only a bounded tail. Crossing 75% enables the topic pool; unused
	// topic/source room is returned to complete recent turns.
	history, missing, err := h.historyContext(ctx, room)
	if err != nil {
		return agentcore.ContextPlan{}, err
	}
	historyUsed := 0
	for _, g := range history {
		historyUsed += messageCost(g)
	}
	compressed := missing || est.EstimateRequest(fixed)+used+sourceUsed+historyUsed+fixed.MaxTokens >= h.limits.ContextTokens*3/4
	topics := []modelclient.Message{}
	if compressed {
		topics, _, err = h.contextTopics(ctx, min(8192, capacity/5, room))
		if err != nil {
			return agentcore.ContextPlan{}, err
		}
		topicCost := messageCost(topics)
		for len(history) > 0 && historyUsed+topicCost > room {
			historyUsed -= messageCost(history[len(history)-1])
			history = history[:len(history)-1]
			missing = true
		}
	}
	omitted = omitted || missing
	request := fixed
	request.Messages = append(request.Messages, source...)
	request.Messages = append(request.Messages, topics...)
	for i := len(history) - 1; i >= 0; i-- {
		request.Messages = append(request.Messages, history[i]...)
	}
	if omitted {
		if h.body.ContextNotice == "" {
			h.body.ContextNotice = historyNotice
		}
		notice.Content = h.body.ContextNotice
		request.Messages = append(request.Messages, notice)
	}
	for i := range groups {
		request.Messages = append(request.Messages, selected[i]...)
	}
	estimate := est.EstimateRequest(request)
	if estimate+fixed.MaxTokens+contextMargin(h.limits.ContextTokens)+h.schemaTokens > h.limits.ContextTokens {
		return agentcore.ContextPlan{}, ErrContextConfiguration
	}
	return agentcore.ContextPlan{Request: request, EstimatedInput: estimate, ReservedOutput: fixed.MaxTokens}, nil
}
