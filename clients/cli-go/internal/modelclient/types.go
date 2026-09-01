package modelclient

import "encoding/json"

type ReasoningEffort string

const (
	ReasoningEffortAuto    ReasoningEffort = "auto"
	ReasoningEffortNone    ReasoningEffort = "none"
	ReasoningEffortMinimal ReasoningEffort = "minimal"
	ReasoningEffortLow     ReasoningEffort = "low"
	ReasoningEffortMedium  ReasoningEffort = "medium"
	ReasoningEffortHigh    ReasoningEffort = "high"
	ReasoningEffortXHigh   ReasoningEffort = "xhigh"
	ReasoningEffortMax     ReasoningEffort = "max"
)

type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function ToolFunction `json:"function"`
}

type ToolFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type Tool struct {
	Type     string         `json:"type"`
	Function ToolDefinition `json:"function"`
}

type ToolDefinition struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

type Request struct {
	Messages        []Message
	Tools           []Tool
	MaxTokens       int
	ReasoningEffort ReasoningEffort
}

type PromptTokensDetails struct {
	CachedTokens *int `json:"cached_tokens,omitempty"`
}

type Usage struct {
	PromptTokens         int                  `json:"prompt_tokens,omitempty"`
	CompletionTokens     int                  `json:"completion_tokens,omitempty"`
	TotalTokens          int                  `json:"total_tokens,omitempty"`
	PromptTokensDetails  *PromptTokensDetails `json:"prompt_tokens_details,omitempty"`
	PromptCacheHitTokens *int                 `json:"prompt_cache_hit_tokens,omitempty"`
}

func (u Usage) CacheReadTokens() (int, bool) {
	var nested *int
	if u.PromptTokensDetails != nil {
		nested = u.PromptTokensDetails.CachedTokens
	}
	if nested != nil && u.PromptCacheHitTokens != nil {
		if *nested != *u.PromptCacheHitTokens {
			return 0, false
		}
		return *nested, true
	}
	if nested != nil {
		return *nested, true
	}
	if u.PromptCacheHitTokens != nil {
		return *u.PromptCacheHitTokens, true
	}
	return 0, false
}

type Response struct {
	Message               Message
	FinishReason          string
	Usage                 *Usage
	CompatibilityFallback bool
}

type StreamEventKind string

const (
	StreamEventResponseStarted       StreamEventKind = "response_started"
	StreamEventTextDelta             StreamEventKind = "text_delta"
	StreamEventCompatibilityFallback StreamEventKind = "compatibility_fallback"
)

type StreamEvent struct {
	Kind StreamEventKind
	Text string
}
