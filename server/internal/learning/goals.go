package learning

import (
	"context"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/edu-agent/edu-agent/server/internal/learningspace"
)

// GoalDetails 只保存用户自述；能力评估与证据由教学模块负责。
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

type GoalQuery struct {
	Search, Status, Cursor string
	Limit                  int
}
type GoalPage struct {
	Items      []GoalRevision `json:"items"`
	NextCursor string         `json:"next_cursor,omitempty"`
}
type GoalStore interface {
	GetGoal(context.Context, string) (GoalRevision, error)
	ListGoals(context.Context, GoalQuery) (GoalPage, error)
	GoalHistory(context.Context, string, GoalQuery) (GoalPage, error)
}

func ValidGoalStatus(s string) bool {
	return s == "draft" || s == "active" || s == "paused" || s == "completed" || s == "archived"
}
func (g GoalRevision) LearningSpaceID() string {
	if g.SpaceID == "" {
		return learningspace.DefaultID
	}
	return g.SpaceID
}
func (g GoalRevision) GoalManagement() GoalManagement {
	if g.Management != nil {
		return *g.Management
	}
	name := []rune(g.Text)
	if len(name) > 120 {
		name = name[:120]
	}
	return GoalManagement{Details: GoalDetails{Name: string(name), Priority: "normal"}, Status: "active", CriteriaVerification: "unverified", ChangedFields: []string{}}
}
func (d GoalDetails) Validate() error {
	if strings.TrimSpace(d.Name) == "" || utf8.RuneCountInString(d.Name) > 120 {
		return &Error{Code: CodeInvalidRequest, Reason: "invalid_goal_name"}
	}
	for _, s := range []string{d.Name, d.ExpectedOutcome, d.Scope, d.Exclusions, d.SelfAssessment, d.Purpose, d.CompletionCriteria} {
		if !utf8.ValidString(s) || utf8.RuneCountInString(s) > 4000 {
			return &Error{Code: CodeInvalidRequest, Reason: "invalid_goal_details"}
		}
	}
	if d.Priority != "" && d.Priority != "low" && d.Priority != "normal" && d.Priority != "high" {
		return &Error{Code: CodeInvalidRequest, Reason: "invalid_goal_priority"}
	}
	if d.ScopeSnapshotID != "" && !learningspace.ValidID(d.ScopeSnapshotID) {
		return &Error{Code: CodeKnowledgeReferenceInvalid}
	}
	if d.WeeklyMinutes != nil && (*d.WeeklyMinutes < 1 || *d.WeeklyMinutes > 10080) {
		return &Error{Code: CodeInvalidRequest, Reason: "invalid_time_budget"}
	}
	if d.Timezone == "" {
		if d.Deadline != nil || d.WeeklyMinutes != nil {
			return &Error{Code: CodeInvalidRequest, Reason: "timezone_required"}
		}
	} else {
		if d.Timezone == "Local" {
			return &Error{Code: CodeInvalidRequest, Reason: "explicit_timezone_required"}
		}
		loc, err := time.LoadLocation(d.Timezone)
		if err != nil {
			return &Error{Code: CodeInvalidRequest, Reason: "invalid_timezone"}
		}
		if d.Deadline != nil {
			_, supplied := d.Deadline.Zone()
			_, expected := d.Deadline.In(loc).Zone()
			if supplied != expected || d.Deadline.IsZero() {
				return &Error{Code: CodeInvalidRequest, Reason: "deadline_timezone_mismatch"}
			}
		}
	}
	return nil
}

func nextGoalManagement(previous *GoalRevision, c GoalCommand, actor string, now time.Time) (*GoalManagement, error) {
	m := GoalManagement{Details: GoalDetails{Name: strings.TrimSpace(c.Text), Priority: "normal"}, Status: "draft", CriteriaVerification: "unverified", ChangedFields: []string{}}
	if previous != nil {
		m = previous.GoalManagement()
		m.ChangedFields = []string{}
		m.RouteAdjustmentNeeded = false
	}
	if previous == nil && c.Details == nil {
		// 一句话学习意图最长 4000 字，列表名称使用可编辑的短摘要。
		r := []rune(m.Details.Name)
		if len(r) > 120 {
			m.Details.Name = string(r[:120])
		}
	}
	if c.Details != nil {
		if c.Action != "" {
			return nil, &Error{Code: CodeInvalidRequest, Reason: "goal_content_and_action_are_separate"}
		}
		if err := c.Details.Validate(); err != nil {
			return nil, err
		}
		old := m.Details
		m.Details = *c.Details
		if m.Details.Priority == "" {
			m.Details.Priority = "normal"
		}
		changes := []struct{ name, a, b string }{
			{"name", old.Name, m.Details.Name}, {"expected_outcome", old.ExpectedOutcome, m.Details.ExpectedOutcome},
			{"scope", old.Scope, m.Details.Scope}, {"exclusions", old.Exclusions, m.Details.Exclusions},
			{"self_assessment", old.SelfAssessment, m.Details.SelfAssessment}, {"purpose", old.Purpose, m.Details.Purpose},
			{"completion_criteria", old.CompletionCriteria, m.Details.CompletionCriteria}, {"priority", old.Priority, m.Details.Priority},
			{"scope_snapshot_id", old.ScopeSnapshotID, m.Details.ScopeSnapshotID}, {"timezone", old.Timezone, m.Details.Timezone},
		}
		if previous != nil {
			for _, change := range changes {
				if change.a != change.b {
					m.ChangedFields = append(m.ChangedFields, change.name)
					switch change.name {
					case "scope", "exclusions", "expected_outcome", "completion_criteria", "scope_snapshot_id":
						m.RouteAdjustmentNeeded = true
					}
				}
			}
			a, _ := HashJSON(old.Deadline)
			b, _ := HashJSON(m.Details.Deadline)
			if a != b {
				m.ChangedFields = append(m.ChangedFields, "deadline")
			}
			a, _ = HashJSON(old.WeeklyMinutes)
			b, _ = HashJSON(m.Details.WeeklyMinutes)
			if a != b {
				m.ChangedFields = append(m.ChangedFields, "weekly_minutes")
			}
		}
	}
	if previous != nil && c.Text != previous.Text {
		m.ChangedFields = append(m.ChangedFields, "text")
		m.RouteAdjustmentNeeded = true
	}
	if c.Action == "" {
		if c.CompletionReason != "" {
			return nil, &Error{Code: CodeInvalidRequest}
		}
		return &m, nil
	}
	if previous == nil || c.Text != previous.Text {
		return nil, &Error{Code: CodeInvalidRequest, Reason: "goal_action_requires_existing_content"}
	}
	valid := false
	switch c.Action {
	case "start":
		valid = m.Status == "draft"
		if valid {
			m.Status = "active"
		}
	case "pause":
		valid = m.Status == "active"
		if valid {
			m.Status = "paused"
		}
	case "resume":
		valid = m.Status == "paused"
		if valid {
			m.Status = "active"
		}
	case "complete":
		valid = m.Status == "draft" || m.Status == "active" || m.Status == "paused"
		if strings.TrimSpace(c.CompletionReason) == "" || !utf8.ValidString(c.CompletionReason) || utf8.RuneCountInString(c.CompletionReason) > 4000 {
			return nil, &Error{Code: CodeInvalidRequest, Reason: "completion_reason_required"}
		}
		if valid {
			m.Status = "completed"
			m.Completion = &GoalCompletion{Kind: "manual", Reason: c.CompletionReason, ActorDeviceID: actor, At: now}
		}
	case "archive":
		valid = m.Status != "archived"
		if valid {
			m.ArchivedFrom = m.Status
			m.Status = "archived"
		}
	case "restore":
		valid = m.Status == "archived" && ValidGoalStatus(m.ArchivedFrom) && m.ArchivedFrom != "archived"
		if valid {
			m.Status = m.ArchivedFrom
			m.ArchivedFrom = ""
		}
	}
	if c.Action != "complete" && c.CompletionReason != "" {
		return nil, &Error{Code: CodeInvalidRequest}
	}
	if !valid {
		return nil, &Error{Code: CodeInvalidTransition, Reason: "invalid_goal_transition"}
	}
	m.ChangedFields = append(m.ChangedFields, "status")
	return &m, nil
}

func (s *Service) SupportsGoalManagement() bool { _, ok := s.authority.(GoalStore); return ok }
func (s *Service) GetGoal(ctx context.Context, id string) (GoalRevision, error) {
	if !learningspace.ValidID(id) {
		return GoalRevision{}, &Error{Code: CodeInvalidRequest}
	}
	if store, ok := s.authority.(GoalStore); ok {
		return store.GetGoal(ctx, id)
	}
	return GoalRevision{}, &Error{Code: CodeNotFound}
}
func (s *Service) ListGoals(ctx context.Context, q GoalQuery) (GoalPage, error) {
	if store, ok := s.authority.(GoalStore); ok {
		return store.ListGoals(ctx, q)
	}
	return GoalPage{}, &Error{Code: CodeNotFound}
}
func (s *Service) GoalHistory(ctx context.Context, id string, q GoalQuery) (GoalPage, error) {
	if !learningspace.ValidID(id) {
		return GoalPage{}, &Error{Code: CodeInvalidRequest}
	}
	if store, ok := s.authority.(GoalStore); ok {
		return store.GoalHistory(ctx, id, q)
	}
	return GoalPage{}, &Error{Code: CodeNotFound}
}

// CanStartGoal 是供教学 owner 使用的窄查询；不会改变目标或教学状态。
func (s *Service) CanStartGoal(ctx context.Context, id string) (bool, error) {
	g, err := s.GetGoal(ctx, id)
	if err != nil {
		return false, err
	}
	status := g.GoalManagement().Status
	return status == "draft" || status == "active", nil
}
