package api

import "context"

type PlanningContent struct {
	Details   GoalDetails        `json:"details"`
	Steps     []PlanningStep     `json:"steps"`
	Questions []PlanningQuestion `json:"questions"`
	Gaps      []string           `json:"gaps"`
}
type PlanningQuestion struct {
	Field    string `json:"field"`
	Question string `json:"question"`
	Answer   string `json:"answer"`
}
type PlanningStep struct {
	Name           string `json:"name"`
	Content        string `json:"content"`
	Reason         string `json:"reason"`
	Exercise       string `json:"exercise"`
	Completion     string `json:"completion"`
	Minutes        int    `json:"minutes"`
	Prerequisites  []int  `json:"prerequisites"`
	NodeRevisionID string `json:"node_revision_id"`
}
type PlanningSource struct {
	Name      string             `json:"name"`
	Reference KnowledgeReference `json:"reference"`
}
type PlanningDraft struct {
	ID                    string           `json:"id"`
	Version               int64            `json:"version"`
	Goal                  GoalRevision     `json:"goal"`
	Content               PlanningContent  `json:"content"`
	Suggestion            *PlanningContent `json:"suggestion,omitempty"`
	Sources               []PlanningSource `json:"sources"`
	SessionID             string           `json:"session_id,omitempty"`
	SessionVersion        int64            `json:"session_version,omitempty"`
	State                 string           `json:"state"`
	StaleReasons          []string         `json:"stale_reasons"`
	LastError             string           `json:"last_error,omitempty"`
	ModelSource           string           `json:"model_source"`
	AppliedSessionID      string           `json:"applied_session_id,omitempty"`
	AppliedGoalRevisionID string           `json:"applied_goal_revision_id,omitempty"`
}
type PlanningCommand struct {
	OperationID     string           `json:"operation_id"`
	ExpectedVersion int64            `json:"expected_version"`
	Action          string           `json:"action"`
	Content         *PlanningContent `json:"content,omitempty"`
	SessionID       string           `json:"session_id,omitempty"`
	Target          string           `json:"target,omitempty"`
	UpdateGoal      bool             `json:"update_goal,omitempty"`
}

func (c *Client) PlanningList(ctx context.Context, goal string) ([]PlanningDraft, error) {
	var result struct {
		Items []PlanningDraft `json:"items"`
	}
	if !validLearningUUID(goal) {
		return nil, &ProtocolError{Category: "invalid_goal_id"}
	}
	err := c.doJSON(ctx, "GET", "/v1/learning/goals/"+goal+"/plans", true, nil, map[int]bool{200: true}, true, &result)
	return result.Items, err
}
func (c *Client) Planning(ctx context.Context, goal, id string) (PlanningDraft, error) {
	var d PlanningDraft
	if !validLearningUUID(goal) || !validLearningUUID(id) {
		return d, &ProtocolError{Category: "invalid_plan_id"}
	}
	err := c.doJSON(ctx, "GET", "/v1/learning/goals/"+goal+"/plans/"+id, true, nil, map[int]bool{200: true}, true, &d)
	if err == nil && (d.ID != id || d.Goal.GoalID != goal || d.Version < 1) {
		err = &ProtocolError{Category: "invalid_plan_response"}
	}
	return d, err
}
func (c *Client) ChangePlanning(ctx context.Context, goal, id string, cmd PlanningCommand) (PlanningDraft, error) {
	var d PlanningDraft
	if !validLearningUUID(goal) || !validLearningUUID(id) || !validLearningUUID(cmd.OperationID) {
		return d, &ProtocolError{Category: "invalid_plan_id"}
	}
	err := c.doJSON(ctx, "POST", "/v1/learning/goals/"+goal+"/plans/"+id, true, cmd, map[int]bool{200: true}, true, &d)
	if err == nil && (d.ID != id || d.Goal.GoalID != goal || d.Version != cmd.ExpectedVersion+1) {
		err = &ProtocolError{Category: "invalid_plan_response"}
	}
	return d, err
}
