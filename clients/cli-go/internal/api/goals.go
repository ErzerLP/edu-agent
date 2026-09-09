package api

import (
	"context"
	"net/url"
	"strconv"
	"time"
)

type GoalDetails struct {
	Name               string     `json:"name"`
	ExpectedOutcome    string     `json:"expected_outcome"`
	Scope              string     `json:"scope"`
	Exclusions         string     `json:"exclusions"`
	SelfAssessment     string     `json:"self_assessment"`
	Purpose            string     `json:"purpose"`
	CompletionCriteria string     `json:"completion_criteria"`
	Priority           string     `json:"priority"`
	ScopeSnapshotID    string     `json:"scope_snapshot_id,omitempty"`
	Timezone           string     `json:"timezone,omitempty"`
	Deadline           *time.Time `json:"deadline,omitempty"`
	WeeklyMinutes      *int       `json:"weekly_minutes,omitempty"`
}
type GoalCompletion struct {
	Kind          string    `json:"kind"`
	Reason        string    `json:"reason"`
	ActorDeviceID string    `json:"actor_device_id"`
	At            time.Time `json:"at"`
}
type GoalManagement struct {
	Details               GoalDetails     `json:"details"`
	Status                string          `json:"status"`
	ArchivedFrom          string          `json:"archived_from,omitempty"`
	Completion            *GoalCompletion `json:"completion,omitempty"`
	CriteriaVerification  string          `json:"criteria_verification"`
	ChangedFields         []string        `json:"changed_fields"`
	RouteAdjustmentNeeded bool            `json:"route_adjustment_needed"`
}
type GoalPage struct {
	Items      []GoalRevision `json:"items"`
	NextCursor string         `json:"next_cursor,omitempty"`
}

func (g GoalRevision) GoalManagement() GoalManagement {
	if g.Management != nil {
		return *g.Management
	}
	name := []rune(g.Text)
	if len(name) > 120 {
		name = name[:120]
	}
	return GoalManagement{Details: GoalDetails{Name: string(name), Priority: "normal"}, Status: "active", CriteriaVerification: "unverified"}
}
func (c *Client) Goals(ctx context.Context, search, status, cursor string, limit int) (GoalPage, error) {
	q := url.Values{"search": {search}, "status": {status}, "cursor": {cursor}, "limit": {strconv.Itoa(limit)}}
	var page GoalPage
	err := c.doJSON(ctx, "GET", "/v1/learning/goals?"+q.Encode(), true, nil, map[int]bool{200: true}, true, &page)
	if err == nil && page.Items == nil {
		err = &ProtocolError{Category: "invalid_goal_page"}
	}
	return page, err
}
func (c *Client) Goal(ctx context.Context, id string) (GoalRevision, error) {
	var g GoalRevision
	if !validLearningUUID(id) {
		return g, &ProtocolError{Category: "invalid_goal_id"}
	}
	err := c.doJSON(ctx, "GET", "/v1/learning/goals/"+id, true, nil, map[int]bool{200: true}, true, &g)
	if err == nil && (g.GoalID != id || g.Revision < 1) {
		err = &ProtocolError{Category: "invalid_goal_response"}
	}
	return g, err
}
func (c *Client) GoalRevisions(ctx context.Context, id, cursor string, limit int) (GoalPage, error) {
	var page GoalPage
	if !validLearningUUID(id) {
		return page, &ProtocolError{Category: "invalid_goal_id"}
	}
	q := url.Values{"cursor": {cursor}, "limit": {strconv.Itoa(limit)}}
	err := c.doJSON(ctx, "GET", "/v1/learning/goals/"+id+"/revisions?"+q.Encode(), true, nil, map[int]bool{200: true}, true, &page)
	if err == nil && page.Items == nil {
		err = &ProtocolError{Category: "invalid_goal_page"}
	}
	return page, err
}
func (c *Client) ReviseGoal(ctx context.Context, r LearningGoalRequest) (GoalOperationResult, error) {
	var result GoalOperationResult
	if err := validateGoalRequest(r); err != nil {
		return result, err
	}
	err := c.doJSON(ctx, "PUT", "/v1/learning/goals/"+r.AggregateID, true, r, map[int]bool{200: true, 201: true}, true, &result)
	if err == nil && (result.Result.GoalID != r.AggregateID || result.Result.Revision != r.ExpectedVersion+1) {
		err = &ProtocolError{Category: "invalid_goal_response"}
	}
	return result, err
}
