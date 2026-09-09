package api

import (
	"context"
	"net/url"
	"strconv"
	"time"
)

type ProgressQuery struct {
	LearningSpaceID string `json:"learning_space_id,omitempty"`
	Global          bool   `json:"global,omitempty"`
	GoalID          string `json:"goal_id,omitempty"`
	Status          string `json:"status,omitempty"`
	Order           string `json:"order,omitempty"`
	Cursor          string `json:"cursor,omitempty"`
	Limit           int    `json:"limit,omitempty"`
}
type RouteProgress struct {
	Route               RouteRevision `json:"route"`
	CompletedSteps      []string      `json:"completed_steps"`
	Numerator           int           `json:"numerator"`
	Denominator         int           `json:"denominator"`
	Percent             *float64      `json:"percent"`
	Basis               string        `json:"basis"`
	CurrentGoalRevision bool          `json:"current_goal_revision"`
}
type PendingAssessment struct {
	AssessmentID   string   `json:"assessment_id"`
	NodeRevisionID string   `json:"node_revision_id"`
	Reasons        []string `json:"reasons"`
}
type GoalProgress struct {
	LearningSpaceID  string              `json:"learning_space_id"`
	SpaceName        string              `json:"space_name"`
	EvidenceBasis    string              `json:"evidence_basis"`
	EvidenceSources  []EvidenceSource    `json:"evidence_sources"`
	Goal             GoalRevision        `json:"goal"`
	Routes           []RouteProgress     `json:"routes"`
	Nodes            []NodeReduction     `json:"nodes"`
	Sessions         []SessionSummary    `json:"sessions"`
	Recent           []TimelineItem      `json:"recent_activity"`
	RecentHasMore    bool                `json:"recent_has_more"`
	Reviews          []ReviewSchedule    `json:"reviews"`
	Pending          []PendingAssessment `json:"pending_assessments"`
	EvidenceCount    int                 `json:"evidence_count"`
	EstimatedSeconds int64               `json:"estimated_active_seconds"`
	Estimated        bool                `json:"estimated"`
}

type EvidenceSource struct {
	EvidenceID          string `json:"evidence_id"`
	GoalRevisionID      string `json:"goal_revision_id"`
	KnowledgeRevisionID string `json:"knowledge_revision_id"`
	NodeRevisionID      string `json:"node_revision_id"`
	CurrentGoalRevision bool   `json:"current_goal_revision"`
}
type ProgressPage struct {
	Metadata   ProjectionMetadata `json:"metadata"`
	UpdatedAt  time.Time          `json:"updated_at"`
	HighWater  int64              `json:"committed_event_high_water"`
	Items      []GoalProgress     `json:"items"`
	Total      int                `json:"total"`
	NextCursor string             `json:"next_cursor,omitempty"`
}

func progressValues(q ProgressQuery) url.Values {
	return url.Values{"global": {strconv.FormatBool(q.Global)}, "goal_id": {q.GoalID}, "status": {q.Status}, "order": {q.Order}, "cursor": {q.Cursor}, "limit": {strconv.Itoa(q.Limit)}}
}
func (c *Client) Progress(ctx context.Context, q ProgressQuery) (ProgressPage, error) {
	if q.LearningSpaceID != "" {
		c = c.WithLearningSpace(q.LearningSpaceID)
	}
	var result ProgressPage
	err := c.doJSON(ctx, "GET", "/v1/learning/progress?"+progressValues(q).Encode(), true, nil, map[int]bool{200: true}, true, &result)
	if err == nil && (result.Items == nil || result.Total < len(result.Items) || result.Metadata.Generation == "") {
		err = &ProtocolError{Category: "invalid_progress_page"}
	}
	return result, err
}
func (c *Client) ScopedReviews(ctx context.Context, q ProgressQuery, due *time.Time) (ReviewsPage, error) {
	if q.LearningSpaceID != "" {
		c = c.WithLearningSpace(q.LearningSpaceID)
	}
	var result ReviewsPage
	values := progressValues(q)
	values.Del("order")
	if due != nil {
		values.Set("due_before", due.UTC().Format(time.RFC3339Nano))
	}
	err := c.doJSON(ctx, "GET", "/v1/learning/reviews?"+values.Encode(), true, nil, map[int]bool{200: true}, true, &result)
	if err == nil && (result.Items == nil || result.Total < len(result.Items)) {
		err = &ProtocolError{Category: "invalid_reviews_page"}
	}
	return result, err
}
