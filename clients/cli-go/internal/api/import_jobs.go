package api

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/url"
	"time"
)

type ImportJobItem struct {
	Path   string `json:"path"`
	Digest string `json:"sha256"`
	Bytes  int    `json:"bytes"`
}
type ImportJobBatch struct {
	RequestHash     string         `json:"request_hash,omitempty"`
	Item            ImportJobItem  `json:"item"`
	OperationID     string         `json:"operation_id"`
	Status          string         `json:"status"`
	Error           string         `json:"error,omitempty"`
	Preview         *ImportPreview `json:"preview,omitempty"`
	Result          *ImportResult  `json:"result,omitempty"`
	PlannedRevision string         `json:"planned_revision,omitempty"`
	PlannedManifest string         `json:"planned_manifest,omitempty"`
}
type ImportJob struct {
	BatchCount     int              `json:"batch_count"`
	ID             string           `json:"id"`
	Actor          string           `json:"actor_device_id"`
	Space          string           `json:"space_id"`
	Collection     string           `json:"collection_id"`
	Generation     int64            `json:"generation"`
	Version        int64            `json:"version"`
	PlanVersion    int64            `json:"plan_version"`
	Approved       bool             `json:"approved"`
	Status         string           `json:"status"`
	Created        time.Time        `json:"created_at"`
	Expires        time.Time        `json:"expires_at"`
	Batches        []ImportJobBatch `json:"batches"`
	CleanupPending bool             `json:"cleanup_pending"`
}
type ImportJobCommand struct {
	ContentBase64       string               `json:"content_base64,omitempty"`
	ID                  string               `json:"id"`
	Action              string               `json:"action"`
	Items               []ImportJobItem      `json:"items,omitempty"`
	Batch               int                  `json:"batch,omitempty"`
	Document            *ImportDocument      `json:"document,omitempty"`
	PlanVersion         int64                `json:"plan_version,omitempty"`
	DocumentResolutions []DocumentResolution `json:"document_resolutions,omitempty"`
	NodeResolutions     []NodeResolution     `json:"node_resolutions,omitempty"`
}

func (c *Client) validJob(j ImportJob, id string) bool {
	space, collection := c.learningSpace, c.knowledgeCollection
	if space == "" {
		space = DefaultLearningSpaceID
	}
	if collection == "" {
		collection = "00000000-0000-4000-8000-000000000002"
	}
	if !validLearningUUID(j.ID) || (id != "" && j.ID != id) || j.Space != space || j.Collection != collection || !validLearningUUID(j.Actor) || (len(j.Batches) == 0 && (id != "" || j.BatchCount < 1 || j.BatchCount > 1000)) || len(j.Batches) > 1000 || j.Version < 1 {
		return false
	}
	for _, b := range j.Batches {
		if !validLearningUUID(b.OperationID) {
			return false
		}
		if b.Result != nil && (b.Result.Summary == nil || !c.validImportSummary(*b.Result.Summary, b.OperationID, true)) {
			return false
		}
	}
	return true
}

type ImportJobPage struct {
	Items      []ImportJob `json:"items"`
	NextCursor string      `json:"next_cursor,omitempty"`
}

func (c *Client) ImportJobs(ctx context.Context, cursor string) (ImportJobPage, error) {
	var page ImportJobPage
	err := c.doJSON(ctx, "GET", "/v1/knowledge/import-jobs?cursor="+url.QueryEscape(cursor), true, nil, map[int]bool{200: true}, true, &page)
	if err == nil {
		for _, j := range page.Items {
			if !c.validJob(j, "") {
				return page, &ProtocolError{Category: "invalid_import_job"}
			}
		}
	}
	if err == nil && page.NextCursor != "" && (!validLearningUUID(page.NextCursor) || page.NextCursor == cursor) {
		err = &ProtocolError{Category: "invalid_import_job_cursor"}
	}
	return page, err
}
func (c *Client) ImportJob(ctx context.Context, id string) (ImportJob, error) {
	var j ImportJob
	err := c.doJSON(ctx, "GET", "/v1/knowledge/import-jobs/"+url.PathEscape(id), true, nil, map[int]bool{200: true}, true, &j)
	if err == nil && !c.validJob(j, id) {
		err = &ProtocolError{Category: "invalid_import_job"}
	}
	return j, err
}
func (c *Client) ImportJobBatch(ctx context.Context, id string, index int) (ImportJobBatch, error) {
	var b ImportJobBatch
	err := c.doJSON(ctx, "GET", fmt.Sprintf("/v1/knowledge/import-jobs/%s?batch=%d", url.PathEscape(id), index), true, nil, map[int]bool{200: true}, true, &b)
	return b, err
}
func (c *Client) RunImportJob(ctx context.Context, request ImportJobCommand) (ImportJob, error) {
	if request.Action == "upload" && request.Document != nil && request.Document.Markdown != "" {
		request.ContentBase64 = base64.StdEncoding.EncodeToString([]byte(request.Document.Markdown))
		request.Document = nil
	}
	var j ImportJob
	method, path := "POST", "/v1/knowledge/import-jobs"
	var body any = request
	if request.Action == "cancel" || request.Action == "clean" {
		method = "DELETE"
		path += "/" + url.PathEscape(request.ID)
		body = nil
	}
	err := c.doJSON(ctx, method, path, true, body, map[int]bool{200: true}, request.Action != "continue", &j)
	if err == nil && !c.validJob(j, request.ID) {
		err = &ProtocolError{Category: "invalid_import_job"}
	}
	return j, err
}
