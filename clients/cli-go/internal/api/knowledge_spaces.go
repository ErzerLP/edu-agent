package api

import (
	"context"
	"net/url"
)

type KnowledgeCollection struct {
	ID             string  `json:"id"`
	Name           string  `json:"name"`
	Source         string  `json:"source"`
	Shared         bool    `json:"shared"`
	HeadRevisionID *string `json:"head_revision_id"`
	Version        int64   `json:"version"`
}
type KnowledgeCollectionCommand struct {
	ID              string `json:"id"`
	Action          string `json:"action"`
	Name            string `json:"name"`
	Source          string `json:"source"`
	Shared          bool   `json:"shared"`
	ExpectedVersion int64  `json:"expected_version"`
}
type KnowledgeScopeEntry struct {
	CollectionID string `json:"collection_id"`
	RevisionID   string `json:"revision_id"`
	DocumentID   string `json:"document_id,omitempty"`
	NodeID       string `json:"node_id,omitempty"`
}
type KnowledgeScopeSnapshot struct {
	Updates []KnowledgeScopeEntry `json:"updates,omitempty"`
	ID      string                `json:"id"`
	SpaceID string                `json:"space_id,omitempty"`
	Entries []KnowledgeScopeEntry `json:"entries"`
}

func (c *Client) WithCollection(id string) *Client {
	copy := *c
	copy.knowledgeCollection = id
	return &copy
}
func (c *Client) KnowledgeCollections(ctx context.Context, shared bool) ([]KnowledgeCollection, error) {
	path := "/v1/knowledge/collections"
	if shared {
		path += "?shared=true"
	}
	var result struct {
		Items []KnowledgeCollection `json:"items"`
	}
	err := c.doJSON(ctx, "GET", path, true, nil, map[int]bool{200: true}, true, &result)
	return result.Items, err
}
func (c *Client) ChangeKnowledgeCollection(ctx context.Context, command KnowledgeCollectionCommand) (KnowledgeCollection, error) {
	var result KnowledgeCollection
	err := c.doJSON(ctx, "POST", "/v1/knowledge/collections", true, command, map[int]bool{200: true}, true, &result)
	return result, err
}
func (c *Client) FreezeKnowledgeScope(ctx context.Context, command KnowledgeScopeSnapshot) (KnowledgeScopeSnapshot, error) {
	var result KnowledgeScopeSnapshot
	err := c.doJSON(ctx, "POST", "/v1/knowledge/scopes", true, command, map[int]bool{200: true}, true, &result)
	return result, err
}
func (c *Client) KnowledgeLibraryView(ctx context.Context, id, kind string, scope bool) (any, error) {
	base := "/v1/knowledge/revisions/"
	if scope {
		base = "/v1/knowledge/scopes/"
	}
	var result any
	path := base + url.PathEscape(id)
	if kind != "" {
		path += "/" + kind
	}
	err := c.doJSON(ctx, "GET", path, true, nil, map[int]bool{200: true}, true, &result)
	return result, err
}
