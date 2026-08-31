package api

import (
	"context"
	"net/http"
	"net/url"
)

func (c *Client) MemoryCandidates(ctx context.Context, cursor string, limit int) (MemoryCandidatePage, error) {
	values := make(url.Values)
	setPageQuery(values, cursor, limit)
	var response MemoryCandidatePage
	err := c.doJSON(ctx, http.MethodGet, withQuery("/v1/memory/candidates", values), true, nil, map[int]bool{http.StatusOK: true}, true, &response)
	return response, err
}

func (c *Client) ExportMemory(ctx context.Context, cursor string, limit int) (MemoryExportPage, error) {
	if err := validatePageRequest(cursor, limit); err != nil {
		return MemoryExportPage{}, &ProtocolError{Category: "invalid_memory_export_request"}
	}
	values := make(url.Values)
	setPageQuery(values, cursor, limit)
	var response MemoryExportPage
	err := c.doJSON(ctx, http.MethodGet, withQuery("/v1/memory/export", values), true, nil, map[int]bool{http.StatusOK: true}, true, &response)
	return response, err
}

func (c *Client) MemoryCandidate(ctx context.Context, candidateID string) (MemoryCandidateView, error) {
	var response MemoryCandidateView
	err := c.doJSON(ctx, http.MethodGet, "/v1/memory/candidates/"+url.PathEscape(candidateID), true, nil, map[int]bool{http.StatusOK: true}, true, &response)
	return response, err
}

func (c *Client) DecideMemoryCandidate(ctx context.Context, candidateID string, request MemoryCandidateDecisionRequest) (MemoryOperationResponse, error) {
	var response MemoryOperationResponse
	err := c.doJSON(ctx, http.MethodPost, "/v1/memory/candidates/"+url.PathEscape(candidateID)+"/decisions", true, request, map[int]bool{http.StatusOK: true}, false, &response)
	return response, err
}

func (c *Client) CreateMemoryCandidate(ctx context.Context, request MemoryCandidateRequest) (MemoryOperationResponse, error) {
	var response MemoryOperationResponse
	err := c.doJSON(ctx, http.MethodPost, "/v1/memory/candidates", true, request, map[int]bool{http.StatusOK: true, http.StatusCreated: true}, false, &response)
	return response, err
}
