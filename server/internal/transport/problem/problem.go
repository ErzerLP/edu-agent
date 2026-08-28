package problem

import (
	"errors"
	"net/http"

	"github.com/edu-agent/edu-agent/server/internal/knowledge"
	"github.com/edu-agent/edu-agent/server/internal/learning"
	"github.com/edu-agent/edu-agent/server/internal/memory"
	"github.com/edu-agent/edu-agent/server/internal/privacy"
)

type Problem struct {
	Status  int
	Code    string
	Message string
	Detail  map[string]any
}

func (p Problem) Envelope(requestID string) map[string]any {
	value := map[string]any{"error": map[string]string{
		"code": p.Code, "message": p.Message, "request_id": requestID,
	}}
	for key, detail := range p.Detail {
		value[key] = detail
	}
	return value
}

func Internal() Problem {
	return Problem{Status: http.StatusInternalServerError, Code: "internal_error", Message: "Request could not be completed"}
}

func InvalidRequest(message string) Problem {
	return Problem{Status: http.StatusBadRequest, Code: "invalid_request", Message: message}
}

func PayloadTooLarge(message string) Problem {
	return Problem{Status: http.StatusRequestEntityTooLarge, Code: "payload_too_large", Message: message}
}

func AuthenticationFailed() Problem {
	return Problem{Status: http.StatusUnauthorized, Code: "authentication_failed", Message: "Device credentials are invalid"}
}

func Forbidden() Problem {
	return Problem{Status: http.StatusForbidden, Code: "forbidden", Message: "Device does not have the required scope"}
}

func RateLimited(message string) Problem {
	return Problem{Status: http.StatusTooManyRequests, Code: "rate_limited", Message: message}
}

func DescriptorNotFound() Problem {
	return Problem{Status: http.StatusNotFound, Code: "not_found", Message: "MCP descriptor was not found"}
}

func ContentRedacted() Problem {
	return Problem{Status: http.StatusServiceUnavailable, Code: memory.CodeContentRedacted, Message: "Content was redacted before the response completed"}
}

func Knowledge(err error) Problem {
	code := knowledge.ErrorCode(err)
	result := Internal()
	result.Code = code
	switch code {
	case knowledge.CodePayloadTooLarge:
		result.Status, result.Message = http.StatusRequestEntityTooLarge, "Knowledge payload exceeds the configured limit"
	case knowledge.CodeInvalidRequest, knowledge.CodeInvalidPath:
		result.Status, result.Message = http.StatusBadRequest, "Knowledge request is invalid"
	case knowledge.CodeInvalidMarkdown, knowledge.CodeInvalidIdentityMarker:
		result.Status, result.Message = http.StatusUnprocessableEntity, "Markdown identity or syntax is invalid"
	case knowledge.CodeDuplicateDocumentIdentity, knowledge.CodePathOccupied,
		knowledge.CodeIdentityReviewRequired, knowledge.CodeStaleIdentityReview,
		knowledge.CodeRevisionConflict, knowledge.CodeIdempotencyConflict:
		result.Status, result.Message = http.StatusConflict, "Knowledge import could not be committed"
	case knowledge.CodeNotFound:
		result.Status, result.Message = http.StatusNotFound, "Knowledge revision was not found"
	case knowledge.CodeContentRedacted:
		result.Status, result.Message = http.StatusServiceUnavailable, "Knowledge content was redacted"
	case "":
		return Internal()
	}
	var domain *knowledge.Error
	if errors.As(err, &domain) {
		result.Detail = map[string]any{}
		if domain.CurrentRevisionKnown {
			if domain.CurrentRevisionID == nil {
				result.Detail["current_revision_id"] = nil
			} else {
				result.Detail["current_revision_id"] = *domain.CurrentRevisionID
			}
		}
		if domain.Review != nil {
			result.Detail["identity_review"] = domain.Review
		}
	}
	return result
}

func Learning(err error) Problem {
	code := learning.ErrorCode(err)
	result := Internal()
	result.Code = code
	switch code {
	case learning.CodeInvalidRequest:
		result.Status, result.Message = http.StatusBadRequest, "Learning request is invalid"
	case learning.CodeNotFound, learning.CodeOfflineOperationNotFound:
		result.Status, result.Message = http.StatusNotFound, "Learning resource was not found"
	case learning.CodeKnowledgeReferenceInvalid, learning.CodeProposalRejected:
		result.Status, result.Message = http.StatusUnprocessableEntity, "Learning proposal is invalid"
	case learning.CodeModelUnavailable, learning.CodeOfflineSignerUnavailable:
		result.Status, result.Message = http.StatusServiceUnavailable, "Learning dependency is unavailable"
	case learning.CodeIdempotencyConflict, learning.CodeVersionConflict, learning.CodeInvalidTransition,
		learning.CodeActivityStateConflict, learning.CodeStaleProposal, learning.CodeAssessmentDispositionConflict,
		learning.CodeFocusFrameInvalidated, learning.CodeStaleCursor, learning.CodeOfflinePrepareUnavailable:
		result.Status, result.Message = http.StatusConflict, "Learning request conflicts with current state"
	case learning.CodeContentRedacted:
		result.Status, result.Message = http.StatusServiceUnavailable, "Learning content is unavailable"
	case learning.CodeUnsupportedEventSchema, learning.CodeProjectionUnavailable:
		result.Status, result.Message = http.StatusServiceUnavailable, "Learning projection is unavailable"
	case "":
		return Internal()
	}
	var domain *learning.Error
	if errors.As(err, &domain) {
		result.Detail = map[string]any{}
		if domain.AggregateID != "" {
			result.Detail["conflict"] = map[string]any{
				"aggregate_type": domain.AggregateType, "aggregate_id": domain.AggregateID,
				"expected_version": domain.ExpectedVersion, "current_version": domain.CurrentVersion,
				"as_of_event_seq": domain.AsOfEventSequence,
			}
		}
		if domain.CurrentDisposition != "" {
			result.Detail["current_disposition"] = domain.CurrentDisposition
		}
	}
	return result
}

func Memory(err error) Problem {
	code := memory.ErrorCode(err)
	result := Internal()
	result.Code = code
	switch code {
	case memory.CodeInvalidRequest:
		result.Status, result.Message = http.StatusBadRequest, "Memory request is invalid"
	case memory.CodeNotFound:
		result.Status, result.Message = http.StatusNotFound, "Memory resource was not found"
	case memory.CodeIdempotencyConflict, memory.CodeCandidateConflict, memory.CodeMemoryConflict,
		memory.CodeInvalidMemoryTransition, memory.CodeDeliveryConflict, memory.CodeStaleCursor:
		result.Status, result.Message = http.StatusConflict, "Memory request conflicts with current state"
	case memory.CodeMemoryPolicyRejected:
		result.Status, result.Message = http.StatusUnprocessableEntity, "Memory content is not eligible for storage"
	case memory.CodeMemoryUnavailable, memory.CodePrivacyClearInProgress, memory.CodeContentRedacted,
		"upstream_contract_mismatch":
		result.Status, result.Message = http.StatusServiceUnavailable, "Memory content is currently unavailable"
	case "":
		return Internal()
	}
	var domain *memory.Error
	if errors.As(err, &domain) && (domain.CandidateID != "" || domain.ExpectedRevision != 0 || domain.CurrentRevision != 0) {
		result.Detail = map[string]any{"candidate_conflict": map[string]any{
			"candidate_id": domain.CandidateID, "expected_revision": domain.ExpectedRevision,
			"current_revision": domain.CurrentRevision,
		}}
	}
	return result
}

func Privacy(err error) Problem {
	code := privacy.ErrorCode(err)
	result := Internal()
	result.Code = code
	switch code {
	case privacy.CodeInvalidRequest:
		result.Status, result.Message = http.StatusBadRequest, "Privacy erasure request is invalid"
	case privacy.CodeIdempotencyConflict, privacy.CodeErasureInProgress, privacy.CodeReceiptNotCurrent,
		privacy.CodeOfflinePurgeNotCurrent, privacy.CodeOfflinePurgeAckConflict:
		result.Code, result.Status, result.Message = "erasure_conflict", http.StatusConflict, "Privacy erasure conflicts with current state"
	case privacy.CodeOfflineChallengeInvalid:
		result.Status, result.Message = http.StatusForbidden, "Offline purge acknowledgment is invalid"
	case privacy.CodeContentRedacted, privacy.CodePrivacyClearInProgress, privacy.CodeVerificationFailed,
		privacy.CodeUnsupportedReceiptStore, privacy.CodeOfflineChallengeUnavailable:
		result.Status, result.Message = http.StatusServiceUnavailable, "Privacy erasure is not currently available"
	case privacy.CodeNotFound:
		result.Status, result.Message = http.StatusNotFound, "Privacy erasure receipt was not found"
	case "":
		return Internal()
	}
	return result
}
