package api

import (
	"context"
	"encoding/json"
	"strconv"
)

// LearningChange 独立协商新协议，不改变旧教学响应的严格解码合同。
type LearningChange struct {
	ID            string          `json:"id"`
	SpaceID       string          `json:"learning_space_id"`
	GoalID        string          `json:"goal_id"`
	SessionID     string          `json:"session_id"`
	Revision      int64           `json:"revision"`
	Hash          string          `json:"hash"`
	InteractionID string          `json:"interaction_id"`
	Status        string          `json:"status"`
	Risk          string          `json:"risk"`
	Policy        string          `json:"policy"`
	Reason        string          `json:"status_reason"`
	Base          json.RawMessage `json:"base"`
	Candidate     json.RawMessage `json:"candidate"`
	Diff          json.RawMessage `json:"diff"`
	Applied       json.RawMessage `json:"applied,omitempty"`
	Compensates   string          `json:"compensates,omitempty"`
	Impact        string          `json:"impact"`
	FrameID       string          `json:"frame_id"`
	Restored      bool            `json:"restored"`
	CreatedAt     string          `json:"created_at"`
	Generation    int64           `json:"privacy_generation"`
}

func (c *Client) LearningChanges(ctx context.Context, goal, id string, revision int64) ([]LearningChange, error) {
	if !validLearningUUID(goal) || id != "" && !validLearningUUID(id) || revision < 0 || id == "" && revision != 0 {
		return nil, &ProtocolError{Category: "invalid_change_query"}
	}
	path := "/v1/learning/goals/" + goal + "/changes"
	items := []LearningChange{}
	if id != "" {
		path += "/" + id
		if revision > 0 {
			path += "?revision=" + strconv.FormatInt(revision, 10)
		}
		var item LearningChange
		if err := c.doJSON(ctx, "GET", path, true, nil, map[int]bool{200: true}, true, &item); err != nil {
			return nil, err
		}
		items = append(items, item)
	} else {
		var page struct {
			Items []LearningChange `json:"items"`
		}
		if err := c.doJSON(ctx, "GET", path, true, nil, map[int]bool{200: true}, true, &page); err != nil {
			return nil, err
		}
		items = page.Items
	}
	space := c.learningSpace
	if space == "" {
		space = DefaultLearningSpaceID
	}
	for _, v := range items {
		if v.GoalID != goal || v.SpaceID != space || !validLearningUUID(v.ID) || v.Revision < 1 || id != "" && v.ID != id || revision > 0 && v.Revision != revision {
			return nil, &ProtocolError{Category: "invalid_change_response"}
		}
	}
	return items, nil
}
