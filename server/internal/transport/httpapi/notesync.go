package httpapi

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/edu-agent/edu-agent/server/internal/integrations/notesync"
	"github.com/edu-agent/edu-agent/server/internal/knowledge"
	"github.com/edu-agent/edu-agent/server/internal/memory"
	"github.com/edu-agent/edu-agent/server/internal/privacy"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

const notesyncNotConfigured = "notesync_not_configured"

type notesyncPreviewRequest struct {
	Path     string `json:"path,omitempty"`
	Page     int    `json:"page,omitempty"`
	PageSize int    `json:"page_size,omitempty"`
}

type notesyncResolutionRequest struct {
	BasisHash                 string                         `json:"basis_hash"`
	OperationID               string                         `json:"operation_id"`
	Kind                      string                         `json:"kind"`
	MergedMarkdown            string                         `json:"merged_markdown,omitempty"`
	IdentityReviewBasisHash   string                         `json:"identity_review_basis_hash,omitempty"`
	IdentityReviewOperationID string                         `json:"identity_review_operation_id,omitempty"`
	IdentityReviewReceipt     string                         `json:"identity_review_receipt,omitempty"`
	DocumentResolutions       []knowledge.DocumentResolution `json:"document_resolutions,omitempty"`
	NodeResolutions           []knowledge.NodeResolution     `json:"node_resolutions,omitempty"`
}

func (a *API) notesyncStatus(w http.ResponseWriter, r *http.Request) {
	if !notesyncQuery(w, r) {
		return
	}
	if a.notesync == nil {
		writeJSON(w, http.StatusOK, notesync.ReviewStatus{
			Configured: false, Compatible: false, Reason: "not_configured",
			ExternalCleanupRequired: true,
		})
		return
	}
	writeJSON(w, http.StatusOK, a.notesync.Status(r.Context()))
}

func (a *API) notesyncPreview(w http.ResponseWriter, r *http.Request) {
	if !notesyncQuery(w, r) || !a.notesyncConfigured(w, r) {
		return
	}
	var request notesyncPreviewRequest
	if err := decodeJSON(w, r, a.maxKnowledgeRequestBody, &request); err != nil {
		var maxBytes *http.MaxBytesError
		if errors.As(err, &maxBytes) {
			writeError(w, r, http.StatusRequestEntityTooLarge, knowledge.CodePayloadTooLarge, "NoteSync preview request exceeds the knowledge request limit")
			return
		}
		writeError(w, r, http.StatusBadRequest, notesync.CodeReviewInvalidRequest, "NoteSync preview request is invalid")
		return
	}
	result, err := a.notesync.Preview(r.Context(), notesync.PreviewCommand{
		Path: request.Path, Page: request.Page, PageSize: request.PageSize,
	})
	if err != nil {
		a.writeNotesyncFailure(w, r, "preview", err)
		return
	}
	if result.Items == nil {
		result.Items = []notesync.PreviewItem{}
	}
	writeJSON(w, http.StatusOK, result)
}

func (a *API) notesyncReviews(w http.ResponseWriter, r *http.Request) {
	if !a.notesyncConfigured(w, r) {
		return
	}
	query := r.URL.Query()
	for key := range query {
		if key != "status" && key != "cursor" && key != "limit" {
			writeError(w, r, http.StatusBadRequest, notesync.CodeReviewInvalidRequest, "NoteSync review query is invalid")
			return
		}
		if len(query[key]) != 1 {
			writeError(w, r, http.StatusBadRequest, notesync.CodeReviewInvalidRequest, "NoteSync review query is invalid")
			return
		}
	}
	limit := 0
	if value := query.Get("limit"); value != "" {
		parsed, err := strconv.Atoi(value)
		if err != nil {
			writeError(w, r, http.StatusBadRequest, notesync.CodeReviewInvalidRequest, "NoteSync review query is invalid")
			return
		}
		limit = parsed
	}
	result, err := a.notesync.ListReviews(r.Context(), notesync.ReviewListCommand{
		Status: query.Get("status"), Cursor: query.Get("cursor"), Limit: limit,
	})
	if err != nil {
		a.writeNotesyncFailure(w, r, "list_reviews", err)
		return
	}
	if result.Items == nil {
		result.Items = []notesync.ReviewSummary{}
	}
	writeJSON(w, http.StatusOK, result)
}

func (a *API) notesyncReview(w http.ResponseWriter, r *http.Request) {
	if !notesyncQuery(w, r) || !a.notesyncConfigured(w, r) {
		return
	}
	result, err := a.notesync.Review(r.Context(), chi.URLParam(r, "reviewID"))
	if err != nil {
		a.writeNotesyncFailure(w, r, "review", err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (a *API) notesyncResolution(w http.ResponseWriter, r *http.Request) {
	if !notesyncQuery(w, r) || !a.notesyncConfigured(w, r) {
		return
	}
	var request notesyncResolutionRequest
	if err := decodeJSON(w, r, a.maxKnowledgeRequestBody, &request); err != nil {
		var maxBytes *http.MaxBytesError
		if errors.As(err, &maxBytes) {
			writeError(w, r, http.StatusRequestEntityTooLarge, knowledge.CodePayloadTooLarge, "NoteSync resolution request exceeds the knowledge request limit")
			return
		}
		writeError(w, r, http.StatusBadRequest, notesync.CodeReviewInvalidRequest, "NoteSync resolution request is invalid")
		return
	}
	credential, ok := credentialFromContext(r.Context())
	if !ok {
		writeError(w, r, http.StatusUnauthorized, "authentication_failed", "Device credentials are invalid")
		return
	}
	result, err := a.notesync.Resolve(r.Context(), notesync.ResolutionCommand{
		ReviewID: chi.URLParam(r, "reviewID"), BasisHash: request.BasisHash,
		OperationID: request.OperationID, DeviceID: credential.Device.ID, Kind: request.Kind,
		MergedMarkdown:            request.MergedMarkdown,
		IdentityReviewBasisHash:   request.IdentityReviewBasisHash,
		IdentityReviewOperationID: request.IdentityReviewOperationID,
		IdentityReviewReceipt:     request.IdentityReviewReceipt,
		DocumentResolutions:       request.DocumentResolutions,
		NodeResolutions:           request.NodeResolutions,
	})
	if err != nil {
		a.writeNotesyncFailure(w, r, "resolve", err)
		return
	}
	status := http.StatusOK
	if result.KnowledgeRevisionID != "" && !result.Unchanged && !result.Replayed {
		status = http.StatusCreated
	}
	writeJSON(w, status, result)
}

func (a *API) notesyncConfigured(w http.ResponseWriter, r *http.Request) bool {
	if a.notesync != nil {
		return true
	}
	writeError(w, r, http.StatusServiceUnavailable, notesyncNotConfigured, "NoteSync is not configured")
	return false
}

func notesyncQuery(w http.ResponseWriter, r *http.Request) bool {
	if len(r.URL.Query()) == 0 {
		return true
	}
	writeError(w, r, http.StatusBadRequest, notesync.CodeReviewInvalidRequest, "NoteSync request query is invalid")
	return false
}

func (a *API) writeNotesyncFailure(w http.ResponseWriter, r *http.Request, operation string, err error) {
	if knowledge.ErrorCode(err) != "" {
		a.writeKnowledgeFailure(w, r, "notesync_"+operation, err)
		return
	}
	code := notesync.ReviewErrorCode(err)
	status := http.StatusInternalServerError
	message := "Request could not be completed"
	switch code {
	case notesync.CodeReviewInvalidRequest:
		status, message = http.StatusBadRequest, "NoteSync request is invalid"
	case notesync.CodeReviewNotFound:
		status, message = http.StatusNotFound, "NoteSync review was not found"
	case notesync.CodeReviewStale, notesync.CodeReviewIdempotencyConflict:
		status, message = http.StatusConflict, "NoteSync review conflicts with current state"
	case notesync.CodeReviewContentRedacted:
		status, message = http.StatusServiceUnavailable, "NoteSync content is unavailable"
	case notesync.CodeReviewUnavailable:
		status, message = http.StatusServiceUnavailable, "NoteSync dependency is unavailable"
	case "":
		switch privacy.ErrorCode(err) {
		case privacy.CodeContentRedacted:
			code, status, message = memory.CodeContentRedacted, http.StatusServiceUnavailable, "NoteSync content is unavailable"
		default:
			code = "internal_error"
		}
	}
	if code == "internal_error" {
		a.logger.ErrorContext(r.Context(), "notesync request failed",
			"request_id", middleware.GetReqID(r.Context()), "operation", operation, "error_category", "internal")
	}
	writeError(w, r, status, code, message)
}
