package httpapi

import (
	"context"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5/middleware"
)

// Only expected cancellation of this request is downgraded. An unrelated
// internal failure or cancellation without a cancelled request stays an error.
func (a *API) requestCancelled(r *http.Request, operation string, err error) bool {
	if r.Context().Err() == nil {
		return false
	}
	if errors.Is(err, context.Canceled) {
		a.logger.InfoContext(r.Context(), "request canceled", "request_id", middleware.GetReqID(r.Context()), "operation", operation, "error_category", "request_cancelled")
		return true
	}
	if errors.Is(err, context.DeadlineExceeded) {
		a.logger.WarnContext(r.Context(), "request timed out", "request_id", middleware.GetReqID(r.Context()), "operation", operation, "error_category", "request_timeout")
		return true
	}
	return false
}
