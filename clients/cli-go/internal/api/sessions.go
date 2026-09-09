package api

import (
	"context"
	"net/url"
	"strconv"
)

type SessionSummary struct {
	SessionID         string `json:"session_id"`
	LearningSpaceID   string `json:"learning_space_id"`
	GoalID            string `json:"goal_id"`
	GoalRevisionID    string `json:"goal_revision_id"`
	ScopeSnapshotID   string `json:"scope_snapshot_id,omitempty"`
	Name              string `json:"name"`
	GoalStatus        string `json:"goal_status"`
	State             string `json:"state"`
	Position          string `json:"position"`
	RouteStepID       string `json:"route_step_id,omitempty"`
	ActivityID        string `json:"activity_id,omitempty"`
	LastEventSequence int64  `json:"last_event_seq"`
	Resumable         bool   `json:"resumable"`
}
type SessionPage struct {
	Items      []SessionSummary `json:"items"`
	NextCursor string           `json:"next_cursor,omitempty"`
}

func (c *Client) Sessions(ctx context.Context, goal, status, cursor string, limit int) (SessionPage, error) {
	var page SessionPage
	if goal != "" && !validLearningUUID(goal) {
		return page, &ProtocolError{Category: "invalid_goal_id"}
	}
	q := url.Values{"goal_id": {goal}, "status": {status}, "cursor": {cursor}, "limit": {strconv.Itoa(limit)}}
	err := c.doJSON(ctx, "GET", "/v1/tutoring/sessions?"+q.Encode(), true, nil, map[int]bool{200: true}, true, &page)
	if err == nil {
		if page.Items == nil {
			return page, &ProtocolError{Category: "invalid_session_page"}
		}
		for _, item := range page.Items {
			if !validLearningUUID(item.SessionID) || !validLearningUUID(item.GoalID) || !validLearningUUID(item.GoalRevisionID) || !validLearningUUID(item.LearningSpaceID) || (goal != "" && item.GoalID != goal) {
				return SessionPage{}, &ProtocolError{Category: "invalid_session_summary"}
			}
		}
	}
	return page, err
}
