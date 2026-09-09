package api

import (
	"context"
	"net/url"
)

type ImportSummary struct {
	DocumentIDs   []string `json:"document_ids"`
	OperationID   string   `json:"operation_id"`
	SpaceID       string   `json:"space_id"`
	CollectionID  string   `json:"collection_id"`
	ActorDeviceID string   `json:"actor_device_id"`
	Added         int      `json:"added"`
	Updated       int      `json:"updated"`
	Unchanged     int      `json:"unchanged"`
}
type ImportPreview struct {
	Status           string                             `json:"status"`
	Receipt          string                             `json:"receipt,omitempty"`
	Summary          ImportSummary                      `json:"summary"`
	Diff             []KnowledgeMaintenanceDocumentDiff `json:"diff"`
	Review           *IdentityReview                    `json:"identity_review,omitempty"`
	Before           []ImportDocument                   `json:"before,omitempty"`
	AffectedEvidence int                                `json:"affected_evidence"`
	ImpactKnown      bool                               `json:"impact_known"`
}
type ConfirmImportRequest struct {
	Request ImportRequest `json:"request"`
	Receipt string        `json:"receipt"`
}

func (c *Client) PreviewImport(ctx context.Context, request ImportRequest) (ImportPreview, error) {
	var result ImportPreview
	err := c.doJSON(ctx, "POST", "/v1/knowledge/imports/previews", true, request, map[int]bool{200: true}, true, &result)
	if err == nil && (!c.validImportSummary(result.Summary, request.OperationID, result.Status == "ready") || (result.Status != "ready" && result.Status != "review") || (result.Status == "ready" && (result.Receipt == "" || result.Review != nil || len(result.Summary.DocumentIDs) != len(request.Documents))) || (result.Status == "review" && (result.Review == nil || len(result.Review.Documents)+len(result.Review.Nodes) == 0))) {
		err = &ProtocolError{Category: "invalid_import_preview"}
	}
	return result, err
}
func (c *Client) ConfirmImport(ctx context.Context, request ConfirmImportRequest) (ImportResult, error) {
	var result ImportResult
	err := c.doJSON(ctx, "POST", "/v1/knowledge/imports/confirm", true, request, map[int]bool{200: true}, true, &result)
	if err == nil && (result.Summary == nil || !c.validImportSummary(*result.Summary, request.Request.OperationID, true) || len(result.Summary.DocumentIDs) != len(request.Request.Documents) || !validLearningUUID(result.Revision.RevisionID)) {
		err = &ProtocolError{Category: "invalid_import_result"}
	}
	return result, err
}
func (c *Client) ImportOperation(ctx context.Context, operation string) (ImportResult, error) {
	var result ImportResult
	err := c.doJSON(ctx, "GET", "/v1/knowledge/imports/operations/"+url.PathEscape(operation), true, nil, map[int]bool{200: true}, true, &result)
	if err == nil && (result.Summary == nil || !c.validImportSummary(*result.Summary, operation, true) || !validLearningUUID(result.Revision.RevisionID)) {
		err = &ProtocolError{Category: "invalid_import_operation"}
	}
	return result, err
}

func (c *Client) validImportSummary(s ImportSummary, operation string, complete bool) bool {
	space, collection := c.learningSpace, c.knowledgeCollection
	if space == "" {
		space = DefaultLearningSpaceID
	}
	if collection == "" {
		collection = "00000000-0000-4000-8000-000000000002"
	}
	if s.OperationID != operation || s.SpaceID != space || s.CollectionID != collection || !validLearningUUID(s.ActorDeviceID) || s.Added < 0 || s.Updated < 0 || s.Unchanged < 0 {
		return false
	}
	if complete && (len(s.DocumentIDs) == 0 || len(s.DocumentIDs) > 1000 || len(s.DocumentIDs) != s.Added+s.Updated+s.Unchanged) {
		return false
	}
	seen := map[string]bool{}
	for _, id := range s.DocumentIDs {
		if !validLearningUUID(id) || seen[id] {
			return false
		}
		seen[id] = true
	}
	return true
}
