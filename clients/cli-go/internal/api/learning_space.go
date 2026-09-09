package api

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const DefaultLearningSpaceID = "00000000-0000-4000-8000-000000000001"

type LearningSpace struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	Status      string    `json:"status"`
	Version     int64     `json:"version"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}
type LearningSpaceCommand struct {
	OperationID     string `json:"operation_id"`
	ExpectedVersion int64  `json:"expected_version"`
	Name            string `json:"name"`
	Description     string `json:"description"`
	Status          string `json:"status"`
}
type LearningSpacePage struct {
	Items      []LearningSpace `json:"items"`
	NextCursor string          `json:"next_cursor,omitempty"`
}
type LearningSpaceCapabilities struct {
	Version        int               `json:"version"`
	DefaultSpaceID string            `json:"default_space_id"`
	LegacyScope    string            `json:"legacy_scope"`
	Modules        map[string]string `json:"modules"`
}

// WithLearningSpace returns a copy: selections never mutate a shared client.
func (c *Client) WithLearningSpace(id string) *Client {
	copy := *c
	copy.learningSpace = id
	return &copy
}
func spaceBusinessPath(path string) bool {
	for _, p := range []string{"/v1/learning/", "/v1/tutoring/", "/v1/knowledge/", "/v1/memory/"} {
		if strings.HasPrefix(path, p) {
			return true
		}
	}
	return false
}
func (c *Client) LearningSpacesCapabilities(ctx context.Context) (LearningSpaceCapabilities, error) {
	var result LearningSpaceCapabilities
	status, err := c.doJSONStatus(ctx, "GET", "/v1/learning-spaces/capabilities", true, nil, map[int]bool{200: true}, true, &result)
	if status == 404 || status == 405 || status == 501 {
		return result, &APIError{Code: "learning_spaces_unsupported", Status: 501}
	}
	if err == nil && (result.Version != 1 || result.DefaultSpaceID != DefaultLearningSpaceID || result.LegacyScope != "fixed_default" || len(result.Modules) != 4) {
		err = &ProtocolError{Category: "invalid_learning_space_capabilities"}
	}
	if err == nil {
		for _, module := range []string{"knowledge", "learning", "tutoring", "memory"} {
			if result.Modules[module] != "default_only" && !(module == "knowledge" && result.Modules[module] == "collections_v1") && !(module == "learning" && result.Modules[module] == "goals_v1") && !(module == "tutoring" && result.Modules[module] == "sessions_v1") {
				err = &ProtocolError{Category: "invalid_learning_space_capabilities"}
			}
		}
	}
	return result, err
}
func (c *Client) LearningSpaces(ctx context.Context, search, status, cursor string, limit int) (LearningSpacePage, error) {
	var result LearningSpacePage
	if _, err := c.LearningSpacesCapabilities(ctx); err != nil {
		return result, err
	}
	q := url.Values{"search": {search}, "status": {status}, "cursor": {cursor}, "limit": {strconv.Itoa(limit)}}
	err := c.doJSON(ctx, "GET", "/v1/learning-spaces?"+q.Encode(), true, nil, map[int]bool{200: true}, true, &result)
	if err == nil && result.Items == nil {
		err = &ProtocolError{Category: "invalid_learning_space_page"}
	}
	if err == nil {
		for _, item := range result.Items {
			if !validSpace(item) {
				err = &ProtocolError{Category: "invalid_learning_space_page"}
				break
			}
		}
	}
	return result, err
}
func (c *Client) LearningSpace(ctx context.Context, id string) (LearningSpace, error) {
	var result LearningSpace
	if !validLearningUUID(id) {
		return result, &ProtocolError{Category: "invalid_learning_space"}
	}
	if _, err := c.LearningSpacesCapabilities(ctx); err != nil {
		return result, err
	}
	err := c.doJSON(ctx, "GET", "/v1/learning-spaces/"+url.PathEscape(id), true, nil, map[int]bool{200: true}, true, &result)
	if err == nil && (result.ID != id || !validSpace(result)) {
		err = &ProtocolError{Category: "invalid_learning_space_response"}
	}
	return result, err
}
func (c *Client) MutateLearningSpace(ctx context.Context, id string, request LearningSpaceCommand) (LearningSpace, error) {
	var result LearningSpace
	if _, err := c.LearningSpacesCapabilities(ctx); err != nil {
		return result, err
	}
	method, path := http.MethodPost, "/v1/learning-spaces"
	if id != "" {
		if !validLearningUUID(id) {
			return result, &ProtocolError{Category: "invalid_learning_space"}
		}
		method = http.MethodPut
		path += "/" + url.PathEscape(id)
	}
	err := c.doJSON(ctx, method, path, true, request, map[int]bool{200: true}, true, &result)
	if err == nil && (!validSpace(result) || (id != "" && result.ID != id) || result.Version != request.ExpectedVersion+1) {
		err = &ProtocolError{Category: "invalid_learning_space_response"}
	}
	return result, err
}

func validSpace(s LearningSpace) bool {
	return validLearningUUID(s.ID) && s.Name != "" && s.Version > 0 && (s.Status == "active" || s.Status == "archived") && !s.CreatedAt.IsZero() && !s.UpdatedAt.IsZero()
}
