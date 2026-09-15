package modelclient

import "errors"

type ErrorCode string

const (
	ErrorCodeInvalidReasoningEffort     ErrorCode = "invalid_reasoning_effort"
	ErrorCodeReasoningEffortUnsupported ErrorCode = "reasoning_effort_unsupported"
	ErrorCodeResponseProtocol           ErrorCode = "response_protocol_error"
	ErrorCodeResponseTruncated          ErrorCode = "response_truncated"
	ErrorCodeContentFiltered            ErrorCode = "content_filtered"
	ErrorCodeStreamProtocol             ErrorCode = "stream_protocol_error"
	ErrorCodeStreamResponseTooLarge     ErrorCode = "stream_response_too_large"
)

type ClientError struct {
	Code    ErrorCode
	Message string
}

func (e *ClientError) Error() string {
	if e == nil {
		return ""
	}
	return e.Message
}

func StableErrorCode(err error) ErrorCode {
	var target *ClientError
	if errors.As(err, &target) {
		return target.Code
	}
	return ""
}

func clientError(code ErrorCode, message string) error {
	return &ClientError{Code: code, Message: message}
}
