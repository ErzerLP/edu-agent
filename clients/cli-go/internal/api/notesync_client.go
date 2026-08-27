package api

import (
	"context"
	"net/http"
	"net/url"
)

func (c *Client) NotesyncStatus(ctx context.Context) (NotesyncStatus, error) {
	var response NotesyncStatus
	if err := c.doJSON(ctx, http.MethodGet, "/v1/knowledge/notesync/status", true, nil, map[int]bool{http.StatusOK: true}, true, &response); err != nil {
		return response, err
	}
	if err := ValidateNotesyncStatus(response); err != nil {
		return response, &ProtocolError{Category: "invalid_success_response"}
	}
	return response, nil
}

func (c *Client) NotesyncPreview(ctx context.Context, request NotesyncPreviewRequest) (NotesyncPreviewResult, error) {
	if err := ValidateNotesyncPreviewRequest(request); err != nil {
		return NotesyncPreviewResult{}, &ProtocolError{Category: "invalid_notesync_preview_request"}
	}
	var response NotesyncPreviewResult
	if err := c.doJSON(ctx, http.MethodPost, "/v1/knowledge/notesync/previews", true, request, map[int]bool{http.StatusOK: true}, true, &response); err != nil {
		return response, err
	}
	if err := ValidateNotesyncPreviewResult(response); err != nil {
		return response, &ProtocolError{Category: "invalid_success_response"}
	}
	if request.Page > 0 && response.Page != request.Page || request.PageSize > 0 && response.PageSize != request.PageSize {
		return response, &ProtocolError{Category: "invalid_success_response"}
	}
	return response, nil
}

func (c *Client) NotesyncReviews(ctx context.Context, status, cursor string, limit int) (NotesyncReviewPage, error) {
	if err := ValidateNotesyncReviewQuery(status, cursor, limit); err != nil {
		return NotesyncReviewPage{}, &ProtocolError{Category: "invalid_notesync_review_query"}
	}
	values := url.Values{}
	if status != "" {
		values.Set("status", status)
	}
	setPageQuery(values, cursor, limit)
	var response NotesyncReviewPage
	if err := c.doJSON(ctx, http.MethodGet, withQuery("/v1/knowledge/notesync/reviews", values), true, nil, map[int]bool{http.StatusOK: true}, true, &response); err != nil {
		return response, err
	}
	if err := ValidateNotesyncReviewPage(response); err != nil || limit > 0 && len(response.Items) > limit {
		return response, &ProtocolError{Category: "invalid_success_response"}
	}
	return response, nil
}

func (c *Client) NotesyncReview(ctx context.Context, reviewID string) (NotesyncReview, error) {
	if !validLearningUUID(reviewID) {
		return NotesyncReview{}, &ProtocolError{Category: "invalid_notesync_review_id"}
	}
	var response NotesyncReview
	path := "/v1/knowledge/notesync/reviews/" + url.PathEscape(reviewID)
	if err := c.doJSON(ctx, http.MethodGet, path, true, nil, map[int]bool{http.StatusOK: true}, true, &response); err != nil {
		return response, err
	}
	if err := ValidateNotesyncReview(response); err != nil || response.ReviewID != reviewID {
		return response, &ProtocolError{Category: "invalid_success_response"}
	}
	return response, nil
}

func (c *Client) ResolveNotesyncReview(ctx context.Context, reviewID string, request NotesyncResolutionRequest) (NotesyncResolutionResult, error) {
	if err := ValidateNotesyncResolutionRequest(reviewID, request); err != nil {
		return NotesyncResolutionResult{}, &ProtocolError{Category: "invalid_notesync_resolution_request"}
	}
	var response NotesyncResolutionResult
	path := "/v1/knowledge/notesync/reviews/" + url.PathEscape(reviewID) + "/resolutions"
	status, err := c.doJSONStatus(ctx, http.MethodPost, path, true, request, map[int]bool{http.StatusOK: true, http.StatusCreated: true}, true, &response)
	if err != nil {
		return response, err
	}
	if err := ValidateNotesyncResolutionResult(response); err != nil || response.ReviewID != reviewID || response.ResolutionKind != request.Kind {
		return response, &ProtocolError{Category: "invalid_success_response"}
	}
	if status == http.StatusCreated && (response.KnowledgeRevisionID == "" || response.Unchanged || response.Replayed) {
		return response, &ProtocolError{Category: "invalid_success_response"}
	}
	if status == http.StatusOK && response.KnowledgeRevisionID != "" && !response.Unchanged && !response.Replayed {
		return response, &ProtocolError{Category: "invalid_success_response"}
	}
	return response, nil
}
