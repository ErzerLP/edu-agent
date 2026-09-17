// Package learningchange 协调已授权教学变更；不拥有目标、评分或知识事实。
package learningchange

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/edu-agent/edu-agent/server/internal/learning"
	"github.com/edu-agent/edu-agent/server/internal/learningcontent"
	"github.com/edu-agent/edu-agent/server/internal/tutoring"
	"github.com/google/uuid"
	"strings"
	"time"
	"unicode/utf8"
)

var (
	ErrInvalid     = errors.New("learning_change_invalid")
	ErrConflict    = errors.New("learning_change_conflict")
	ErrNotFound    = errors.New("learning_change_not_found")
	ErrForbidden   = errors.New("learning_change_forbidden")
	ErrInactive    = errors.New("learning_change_inactive")
	ErrUnavailable = errors.New("learning_change_unavailable")
)

type Base struct {
	GoalVersion     int64  `json:"goal_version"`
	GoalRevisionID  string `json:"goal_revision_id"`
	SessionVersion  int64  `json:"session_version"`
	RouteRevisionID string `json:"route_revision_id"`
	ContextID       string `json:"context_id"`
	ActivityID      string `json:"activity_id"`
	ArtifactID      string `json:"artifact_id"`
	ArtifactVersion int64  `json:"artifact_version"`
}
type Step struct {
	NodeRevisionID string `json:"node_revision_id"`
	Name           string `json:"name"`
	Criterion      string `json:"criterion"`
	Prompt         string `json:"prompt"`
	Difficulty     int    `json:"difficulty"`
	Prerequisites  []int  `json:"prerequisites"`
}
type Candidate struct {
	Kind        string                `json:"kind"`
	Trigger     string                `json:"trigger"`
	Reason      string                `json:"reason"`
	EvidenceIDs []string              `json:"evidence_ids"`
	ContextID   string                `json:"context_id"`
	Steps       []Step                `json:"steps"`
	Goal        *learning.GoalDetails `json:"goal,omitempty"`
	Explanation string                `json:"explanation"`
}
type Diff struct {
	BeforeSteps   []Step                  `json:"before_steps"`
	AfterSteps    []Step                  `json:"after_steps"`
	BeforeGoal    learning.GoalDetails    `json:"before_goal"`
	AfterGoal     learning.GoalDetails    `json:"after_goal"`
	BeforeContent []learningcontent.Block `json:"before_content"`
	AfterContent  []learningcontent.Block `json:"after_content"`
}
type Change struct {
	ID            string    `json:"id"`
	SpaceID       string    `json:"learning_space_id"`
	GoalID        string    `json:"goal_id"`
	SessionID     string    `json:"session_id"`
	Revision      int64     `json:"revision"`
	Hash          string    `json:"hash"`
	InteractionID string    `json:"interaction_id"`
	Status        string    `json:"status"`
	Risk          string    `json:"risk"`
	Policy        string    `json:"policy"`
	Reason        string    `json:"status_reason"`
	Base          Base      `json:"base"`
	Candidate     Candidate `json:"candidate"`
	Diff          Diff      `json:"diff"`
	Applied       *Base     `json:"applied,omitempty"`
	Compensates   string    `json:"compensates,omitempty"`
	Impact        string    `json:"impact"`
	FrameID       string    `json:"frame_id"`
	Restored      bool      `json:"restored"`
	CreatedAt     time.Time `json:"created_at"`
	Generation    int64     `json:"privacy_generation"`
}
type Command struct {
	OperationID      string     `json:"operation_id"`
	Action           string     `json:"action"`
	ExpectedRevision int64      `json:"expected_revision"`
	Hash             string     `json:"hash"`
	InteractionID    string     `json:"interaction_id"`
	Base             *Base      `json:"base,omitempty"`
	Candidate        *Candidate `json:"candidate,omitempty"`
	Immediate        bool       `json:"immediate"`
}
type Mode struct {
	Mode    string `json:"mode"`
	Version int64  `json:"version"`
}
type Snapshot struct {
	AvailableContextID string                        `json:"available_context_id,omitempty"`
	Base               Base                          `json:"base"`
	Goal               learning.GoalRevision         `json:"goal"`
	Session            tutoring.Session              `json:"session"`
	Steps              []Step                        `json:"steps"`
	Content            *learningcontent.Revision     `json:"content,omitempty"`
	Sources            []learning.KnowledgeReference `json:"sources"`
	Evidence           []learning.AcceptedEvidence   `json:"evidence"`
	Mode               Mode                          `json:"mode"`
}

func digest(v any) string {
	raw, _ := json.Marshal(v)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
func validText(v string, n int) bool {
	return utf8.ValidString(v) && strings.TrimSpace(v) != "" && len(v) <= n && !strings.ContainsRune(v, 0)
}
func (c Candidate) Validate() error {
	if !validText(c.Reason, 2000) || len(c.EvidenceIDs) > 100 || len(c.Steps) > 30 {
		return ErrInvalid
	}
	switch c.Trigger {
	case "user_request", "activity_feedback", "authorized_source", "goal_constraint":
	default:
		return ErrInvalid
	}
	if c.Trigger == "activity_feedback" && len(c.EvidenceIDs) == 0 {
		return ErrInvalid
	}
	for _, id := range c.EvidenceIDs {
		if uuid.Validate(id) != nil {
			return ErrInvalid
		}
	}
	if c.ContextID != "" && uuid.Validate(c.ContextID) != nil {
		return ErrInvalid
	}
	switch c.Kind {
	case "explanation":
		if !validText(c.Explanation, 12000) || c.Goal != nil || len(c.Steps) > 0 || c.ContextID != "" {
			return ErrInvalid
		}
	case "route":
		if c.Goal != nil || c.Explanation != "" {
			return ErrInvalid
		}
	case "goal":
		if c.Goal == nil || c.Goal.Validate() != nil || len(c.Steps) > 0 || c.Explanation != "" || c.ContextID != "" {
			return ErrInvalid
		}
	default:
		return ErrInvalid
	}
	nodes := map[string]bool{}
	for i, s := range c.Steps {
		if nodes[s.NodeRevisionID] {
			return ErrInvalid
		}
		nodes[s.NodeRevisionID] = true
		if uuid.Validate(s.NodeRevisionID) != nil || !validText(s.Name, 500) || !validText(s.Criterion, 2000) || !validText(s.Prompt, 8000) || s.Difficulty < 1 || s.Difficulty > 5 {
			return ErrInvalid
		}
		seen := map[int]bool{}
		for _, p := range s.Prerequisites {
			if p < 0 || p >= i || seen[p] {
				return ErrInvalid
			}
			seen[p] = true
		}
	}
	return nil
}
func safe(state tutoring.State) bool {
	switch state {
	case tutoring.StateGoalReady, tutoring.StateDiagnostic, tutoring.StateRouteActive, tutoring.StateCompleted:
		return true
	}
	return false
}

// 候选摘要只绑定不可变输入；排队和回执状态不会改变已审阅的内容。
func candidateHash(c Change) string {
	return digest(struct {
		ID, Space, Goal, Session string
		Revision                 int64
		Base                     Base
		Candidate                Candidate
		Diff                     Diff
		Compensates              string
	}{c.ID, c.SpaceID, c.GoalID, c.SessionID, c.Revision, c.Base, c.Candidate, c.Diff, c.Compensates})
}
