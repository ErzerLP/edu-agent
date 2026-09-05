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
	CachedTokens        *int `json:"cached_tokens,omitempty"`
	CacheCreationTokens *int `json:"cache_creation_tokens,omitempty"`
	CacheWriteTokens    *int `json:"cache_write_tokens,omitempty"`
}

type Usage struct {
	PromptTokens          int                  `json:"prompt_tokens,omitempty"`
	CompletionTokens      int                  `json:"completion_tokens,omitempty"`
	TotalTokens           int                  `json:"total_tokens,omitempty"`
	PromptTokensDetails   *PromptTokensDetails `json:"prompt_tokens_details,omitempty"`
	PromptCacheHitTokens  *int                 `json:"prompt_cache_hit_tokens,omitempty"`
	PromptCacheMissTokens *int                 `json:"prompt_cache_miss_tokens,omitempty"`
}

func (u Usage) CacheReadTokens() (int, bool) {
	cacheRead, reported, valid := u.normalizedCacheUsage()
	return cacheRead, reported && valid
}

func (u Usage) normalizedCacheUsage() (cacheRead int, reported, valid bool) {
	valid = true
	var nested *int
	cacheNonRead := 0
	if details := u.PromptTokensDetails; details != nil {
		nested = details.CachedTokens
		for _, value := range []*int{details.CacheCreationTokens, details.CacheWriteTokens} {
			if value == nil {
				continue
			}
			reported = true
			if *value < 0 || *value > u.PromptTokens {
				return 0, true, false
			}
			cacheNonRead = max(cacheNonRead, *value)
		}
	}
	if u.PromptCacheMissTokens != nil {
		reported = true
		if *u.PromptCacheMissTokens < 0 || *u.PromptCacheMissTokens > u.PromptTokens {
			return 0, true, false
		}
		cacheNonRead = max(cacheNonRead, *u.PromptCacheMissTokens)
	}
	if nested != nil {
		reported = true
		cacheRead = *nested
	}
	if u.PromptCacheHitTokens != nil {
		reported = true
		if nested != nil && cacheRead != *u.PromptCacheHitTokens {
			return 0, true, false
		}
		cacheRead = *u.PromptCacheHitTokens
	}
	if !reported {
		return 0, false, true
	}
	if cacheRead < 0 || cacheRead > u.PromptTokens || cacheRead > u.PromptTokens-cacheNonRead {
		return 0, true, false
	}
	return cacheRead, true, true
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
