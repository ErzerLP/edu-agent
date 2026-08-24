package command

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/api"
)

const (
	ExitOK          = 0
	ExitInput       = 2
	ExitAuth        = 3
	ExitConflict    = 4
	ExitUnavailable = 5
	ExitInternal    = 6
)

type Error struct {
	Code      string
	RequestID string
	Detail    string
	Next      string
	ExitCode  int
}

func (e *Error) Error() string {
	text := "error[" + e.Code + "]"
	if e.RequestID != "" {
		text += " request_id=" + e.RequestID
	}
	if e.Detail != "" {
		text += ": " + e.Detail
	}
	if e.Next != "" {
		text += "; next: " + e.Next
	}
	return text
}

func commandError(code, detail, next string, exit int) *Error {
	return &Error{Code: code, Detail: detail, Next: next, ExitCode: exit}
}

func mapAPIError(err error) *Error {
	var apiErr *api.APIError
	if errors.As(err, &apiErr) {
		mapped := &Error{Code: apiErr.Code, RequestID: apiErr.RequestID, ExitCode: ExitConflict}
		switch apiErr.Code {
		case "authentication_failed":
			mapped.ExitCode = ExitAuth
			mapped.Detail = "authentication failed or the credential may have been revoked"
			mapped.Next = "run edu-agent pair after repairing local state"
		case "pairing_failed":
			mapped.ExitCode = ExitAuth
			mapped.Detail = "the pairing code is invalid or expired"
			mapped.Next = "create a new pairing code on the server"
		case "forbidden":
			mapped.ExitCode = ExitAuth
			mapped.Detail = "the device does not have the required scope"
			mapped.Next = "use a device credential with the required scope"
		case "revision_conflict":
			mapped.Detail = "knowledge head changed; the import was not rebased or overwritten"
			if apiErr.CurrentRevisionID != nil {
				mapped.Detail += " current_revision_id=" + *apiErr.CurrentRevisionID
			}
			mapped.Next = "review the current head and run the import again"
		case "idempotency_conflict":
			mapped.Detail = "the operation ID was previously used with different content"
			mapped.Next = "start a new import operation without changing a replay body"
		case "identity_review_required", "stale_identity_review":
			mapped.Detail = "knowledge identity requires explicit review"
			mapped.Next = "review the candidates and submit new explicit decisions"
		case "rate_limited":
			mapped.ExitCode = ExitUnavailable
			mapped.Detail = "the server rate limit was reached"
			mapped.Next = "wait before retrying"
		case "content_redacted":
			mapped.ExitCode = ExitUnavailable
			mapped.Detail = "content is unavailable because it was redacted"
			mapped.Next = "discard displayed content and retry only after the server is ready"
		case "privacy_clear_in_progress", "projection_unavailable", "dependency_unavailable", "unavailable", "service_unavailable", "temporarily_unavailable", "upstream_unavailable":
			mapped.ExitCode = ExitUnavailable
			mapped.Detail = "a required service is unavailable"
			mapped.Next = "retry later; no offline operation was queued"
		case "invalid_request", "invalid_path", "invalid_markdown", "invalid_identity_marker", "payload_too_large":
			mapped.ExitCode = ExitInput
			mapped.Detail = "the request was rejected as invalid"
			mapped.Next = "correct the local input and retry"
		case "internal_error":
			mapped.ExitCode = ExitInternal
			mapped.Detail = "the server could not complete the request"
			mapped.Next = "use the request ID to inspect server logs"
		default:
			if apiErr.Status >= http.StatusInternalServerError {
				mapped.ExitCode = ExitUnavailable
			}
			mapped.Detail = "the server rejected the operation"
			mapped.Next = "inspect the stable error code and retry only after resolving it"
		}
		return mapped
	}
	var protocolErr *api.ProtocolError
	if errors.As(err, &protocolErr) {
		return commandError("protocol_error", "the server response did not match the public API contract", "check the server version and endpoint", ExitInternal)
	}
	var transportErr *api.TransportError
	if errors.As(err, &transportErr) {
		return commandError("service_unavailable", "the server could not be reached within the deadline", "retry later; no offline operation was queued", ExitUnavailable)
	}
	return commandError("internal_error", fmt.Sprintf("operation failed in category %T", err), "inspect the local installation", ExitInternal)
}
