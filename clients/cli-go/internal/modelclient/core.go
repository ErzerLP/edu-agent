// Package modelclient 保留 CLI 内部兼容入口，模型通信由共享核心实现。
package modelclient

import (
	"net/http"
	"time"

	core "github.com/edu-agent/edu-agent/packages/agentcore/modelclient"
)

type Client = core.Client
type ReasoningEffort = core.ReasoningEffort
type Message = core.Message
type ToolCall = core.ToolCall
type ToolFunction = core.ToolFunction
type Tool = core.Tool
type ToolDefinition = core.ToolDefinition
type Request = core.Request
type PromptTokensDetails = core.PromptTokensDetails
type Usage = core.Usage
type Response = core.Response
type StreamEventKind = core.StreamEventKind
type StreamEvent = core.StreamEvent
type ErrorCode = core.ErrorCode
type ClientError = core.ClientError

const (
	ReasoningEffortAuto                 = core.ReasoningEffortAuto
	ReasoningEffortNone                 = core.ReasoningEffortNone
	ReasoningEffortMinimal              = core.ReasoningEffortMinimal
	ReasoningEffortLow                  = core.ReasoningEffortLow
	ReasoningEffortMedium               = core.ReasoningEffortMedium
	ReasoningEffortHigh                 = core.ReasoningEffortHigh
	ReasoningEffortXHigh                = core.ReasoningEffortXHigh
	ReasoningEffortMax                  = core.ReasoningEffortMax
	StreamEventResponseStarted          = core.StreamEventResponseStarted
	StreamEventTextDelta                = core.StreamEventTextDelta
	StreamEventReasoningDelta           = core.StreamEventReasoningDelta
	StreamEventResponseActivity         = core.StreamEventResponseActivity
	StreamEventCompatibilityFallback    = core.StreamEventCompatibilityFallback
	ErrorCodeInvalidReasoningEffort     = core.ErrorCodeInvalidReasoningEffort
	ErrorCodeReasoningEffortUnsupported = core.ErrorCodeReasoningEffortUnsupported
	ErrorCodeResponseProtocol           = core.ErrorCodeResponseProtocol
	ErrorCodeResponseTruncated          = core.ErrorCodeResponseTruncated
	ErrorCodeContentFiltered            = core.ErrorCodeContentFiltered
	ErrorCodeStreamProtocol             = core.ErrorCodeStreamProtocol
	ErrorCodeStreamResponseTooLarge     = core.ErrorCodeStreamResponseTooLarge
)

func New(baseURL, model, apiKey string, timeout time.Duration, source *http.Client) (*Client, error) {
	return core.New(baseURL, model, apiKey, timeout, source)
}

func StableErrorCode(err error) ErrorCode { return core.StableErrorCode(err) }
