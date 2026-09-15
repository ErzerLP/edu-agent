package agentcore

import (
	"errors"
	"fmt"
	"unicode/utf8"

	"github.com/edu-agent/edu-agent/packages/agentcore/modelclient"
)

const (
	ContextCompactionAuto       = "auto"
	ContextCompactionRecentOnly = "recent-only"
	ContextCompactionOff        = "off"
	ContextBudgetInvalid        = "context_budget_invalid"
	ContextTurnTooLarge         = "context_turn_too_large"
	ContextRecentTurnsTooLarge  = "context_recent_turns_too_large"
)

// ContextError 在发出模型请求前报告稳定的预算错误。
type ContextError struct {
	Code string
	Err  error
}

func (e *ContextError) Error() string {
	if e == nil {
		return ""
	}
	if e.Err == nil {
		return e.Code
	}
	return fmt.Sprintf("%s: %v", e.Code, e.Err)
}

func (e *ContextError) Unwrap() error { return e.Err }

func contextError(code, message string) error {
	return &ContextError{Code: code, Err: errors.New(message)}
}

// ContextMemoryProjection 由宿主的证据有效性检查生成，不接受模型直接赋权。
type ContextMemoryProjection struct {
	Instruction string
	Items       []string
}

type ContextPlan struct {
	ProjectedTurns  int
	Request         modelclient.Request
	EstimatedInput  int
	ReservedOutput  int
	SafetyMargin    int
	SoftPressure    bool
	TotalTurns      int
	SelectedTurns   int
	DroppedTurns    int
	MemoryItemCount int
	UsedMemory      bool
}

func messageGroups(messages []modelclient.Message) [][]modelclient.Message {
	groups := make([][]modelclient.Message, 0)
	for _, message := range messages {
		if message.Role == "user" || len(groups) == 0 {
			groups = append(groups, []modelclient.Message{message})
		} else {
			groups[len(groups)-1] = append(groups[len(groups)-1], message)
		}
	}
	return groups
}

func cloneModelMessage(message modelclient.Message) modelclient.Message {
	message.ToolCalls = append([]modelclient.ToolCall(nil), message.ToolCalls...)
	return message
}

func truncateUTF8(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	value = value[:limit]
	for !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value
}
