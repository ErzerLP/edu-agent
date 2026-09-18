package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"github.com/edu-agent/edu-agent/server/internal/integrations/notesync"
	"github.com/edu-agent/edu-agent/server/internal/knowledge"
	"github.com/edu-agent/edu-agent/server/internal/learningspace"
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
	if !notesyncQuery(w, r) || !a.notesyncMapping(w, r) {
		return
	}
	if a.notesync == nil {
		writeJSON(w, http.StatusOK, notesync.ReviewStatus{
			Configured: false, Compatible: false, Reason: "not_configured",
			ExternalCleanupRequired: true,
		})
		return
	}
	status := a.notesync.Status(r.Context())
	// 只为学习浏览器补充配置来源，保持旧严格解码 CLI 的响应兼容。
	if a.webUI.Enabled {
		if _, err := r.Cookie(a.webCookieName()); err == nil {
			status.ConfigurationSource = "environment"
			if a.adminUI.NotesyncSource == "admin_settings" {
				status.ConfigurationSource = "admin_settings"
			}
		}
	}
	writeJSON(w, http.StatusOK, status)
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
	a.notesyncResolveRequest(w, r, false)
}

func (a *API) notesyncResolutionPreview(w http.ResponseWriter, r *http.Request) {
	a.notesyncResolveRequest(w, r, true)
}

func (a *API) notesyncResolveRequest(w http.ResponseWriter, r *http.Request, preview bool) {
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
	// 研究采纳的写权限不等于同步审批；浏览器复用显式导入/参考管理档案。
	if !preview && a.webUI.Enabled {
		if _, err := r.Cookie(a.webCookieName()); err == nil && !contains(credential.Scopes, "knowledge:approve") {
			writeError(w, r, http.StatusForbidden, "forbidden", "同步解决需要明确的知识审批权限")
			return
		}
	}
	command := notesync.ResolutionCommand{
		ReviewID: chi.URLParam(r, "reviewID"), BasisHash: request.BasisHash,
		OperationID: request.OperationID, DeviceID: credential.Device.ID, Kind: request.Kind,
		MergedMarkdown:            request.MergedMarkdown,
		IdentityReviewBasisHash:   request.IdentityReviewBasisHash,
		IdentityReviewOperationID: request.IdentityReviewOperationID,
		IdentityReviewReceipt:     request.IdentityReviewReceipt,
		DocumentResolutions:       request.DocumentResolutions,
		NodeResolutions:           request.NodeResolutions,
	}
	if preview {
		service, ok := a.notesync.(interface {
			PreviewResolution(context.Context, notesync.ResolutionCommand) (knowledge.ImportPreview, error)
		})
		if !ok {
			writeError(w, r, http.StatusNotImplemented, "not_supported", "NoteSync resolution preview is unavailable")
			return
		}
		result, err := service.PreviewResolution(r.Context(), command)
		if err != nil {
			a.writeNotesyncFailure(w, r, "resolution_preview", err)
			return
		}
		writeJSON(w, http.StatusOK, result)
		return
	}
	result, err := a.notesync.Resolve(r.Context(), command)
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
	if !a.notesyncMapping(w, r) {
		return false
	}
	if a.notesync != nil {
		return true
	}
	writeError(w, r, http.StatusServiceUnavailable, notesyncNotConfigured, "NoteSync is not configured")
	return false
}

// 每次读取都核对固定映射及当前引用，包括不访问正文存储的状态探测。
func (a *API) notesyncMapping(w http.ResponseWriter, r *http.Request) bool {
	if learningspace.Scope(r.Context()) != learningspace.DefaultID || knowledge.CollectionID(r.Context()) != knowledge.DefaultCollectionID {
		writeError(w, r, http.StatusNotFound, knowledge.CodeNotFound, "NoteSync source mapping is unavailable")
		return false
	}
	if service, ok := a.knowledge.(knowledgeSpaces); ok && service.SupportsKnowledgeScopes() {
		collections, err := service.Collections(r.Context(), false)
		if err != nil {
			a.writeKnowledgeFailure(w, r, "notesync_mapping", err)
			return false
		}
		for _, collection := range collections {
			if collection.ID == knowledge.DefaultCollectionID {
				return true
			}
		}
		writeError(w, r, http.StatusNotFound, knowledge.CodeNotFound, "NoteSync source mapping is unavailable")
		return false
	}
	return true
}

func (a *API) notesyncOperation(w http.ResponseWriter, r *http.Request) {
	if !notesyncQuery(w, r) || !a.notesyncConfigured(w, r) {
		return
	}
	service, ok := a.notesync.(interface {
		Operation(context.Context, string, string) (notesync.ResolutionResult, error)
	})
	if !ok {
		writeError(w, r, http.StatusNotImplemented, "not_supported", "NoteSync operation lookup is unavailable")
		return
	}
	credential, _ := credentialFromContext(r.Context())
	result, err := service.Operation(r.Context(), credential.Device.ID, chi.URLParam(r, "operationID"))
	if err != nil {
		a.writeNotesyncFailure(w, r, "operation", err)
		return
	}
	writeJSON(w, http.StatusOK, result)
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
