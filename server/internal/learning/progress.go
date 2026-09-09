package learning

import (
	"context"
	"sort"
	"time"

	"github.com/google/uuid"
)

// ProgressQuery 的全局读取必须显式请求，默认使用调用上下文中的学习区。
type ProgressQuery struct {
	Global bool   `json:"global,omitempty"`
	GoalID string `json:"goal_id,omitempty"`
	Status string `json:"status,omitempty"`
	Order  string `json:"order,omitempty"`
	Cursor string `json:"cursor,omitempty"`
	Limit  int    `json:"limit,omitempty"`
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

type ProgressStore interface {
	Progress(context.Context, ProgressQuery) (ProgressPage, error)
}

func (s *Service) SupportsProgress() bool { _, ok := s.queries.(ProgressStore); return ok }
func (s *Service) Progress(ctx context.Context, q ProgressQuery) (ProgressPage, error) {
	if store, ok := s.queries.(ProgressStore); ok {
		return store.Progress(ctx, q)
	}
	return ProgressPage{}, &Error{Code: CodeProjectionUnavailable, Reason: "progress_unavailable"}
}

// ReviewTaskID 绑定真实目标和不可变节点版本，资料引用不会创建新任务。
func ReviewTaskID(goal, node string) string {
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte("edu-agent/review/v1/"+goal+"/"+node)).String()
}

// BuildGoalProgress 仅使用权威投影，评分与间隔继续由 ReduceNode 计算。
func BuildGoalProgress(goal GoalRevision, revisions map[string]GoalRevision, p Projection, activities map[string]Activity, pendingGoals map[string]string, completed map[string]map[string]bool) GoalProgress {
	r := GoalProgress{Goal: goal, Routes: []RouteProgress{}, Nodes: []NodeReduction{}, Sessions: []SessionSummary{}, Recent: []TimelineItem{}, Reviews: []ReviewSchedule{}, Pending: []PendingAssessment{}, Estimated: true}
	belongs := func(revision string) bool { return revisions[revision].GoalID == goal.GoalID }
	nodes := map[string]bool{}
	for _, route := range p.Routes {
		if !belongs(route.Route.GoalRevisionID) {
			continue
		}
		rp := RouteProgress{Route: route.Route, CompletedSteps: []string{}, Denominator: len(route.Route.Steps), Basis: "acknowledged_route_steps", CurrentGoalRevision: route.Route.GoalRevisionID == goal.ID}
		for _, step := range route.Route.Steps {
			nodes[step.NodeRevisionID] = true
			if completed[route.Route.ID][step.ID] {
				rp.CompletedSteps = append(rp.CompletedSteps, step.ID)
			}
		}
		rp.Numerator = len(rp.CompletedSteps)
		if rp.Denominator > 0 {
			percent := 100 * float64(rp.Numerator) / float64(rp.Denominator)
			rp.Percent = &percent
		}
		r.Routes = append(r.Routes, rp)
	}
	var evidence []AcceptedEvidence
	for _, e := range p.Evidence {
		if belongs(e.GoalRevisionID) && !p.Invalidated[e.ID] {
			evidence = append(evidence, e)
			nodes[e.NodeRevisionID] = true
		}
	}
	SortEvidence(evidence)
	r.EvidenceCount = len(evidence)
	r.LearningSpaceID = goal.LearningSpaceID()
	r.EvidenceBasis = "same_goal_valid_history"
	r.EvidenceSources = []EvidenceSource{}
	for _, e := range evidence {
		r.EvidenceSources = append(r.EvidenceSources, EvidenceSource{EvidenceID: e.ID, GoalRevisionID: e.GoalRevisionID, KnowledgeRevisionID: e.KnowledgeRevisionID, NodeRevisionID: e.NodeRevisionID, CurrentGoalRevision: e.GoalRevisionID == goal.ID})
	}
	for id, pending := range p.Pending {
		if belongs(pendingGoals[id]) {
			r.Pending = append(r.Pending, pending)
			nodes[pending.NodeRevisionID] = true
		}
	}
	sort.Slice(r.Pending, func(i, j int) bool { return r.Pending[i].AssessmentID < r.Pending[j].AssessmentID })
	for node := range nodes {
		var pending []PendingAssessment
		for _, a := range r.Pending {
			if a.NodeRevisionID == node {
				pending = append(pending, a)
			}
		}
		n := ReduceNode(node, evidence, nil, pending)
		r.Nodes = append(r.Nodes, n)
		if n.Review != nil {
			review := *n.Review
			review.TaskID = ReviewTaskID(goal.GoalID, node)
			review.GoalID = goal.GoalID
			review.LearningSpaceID = goal.LearningSpaceID()
			for _, e := range evidence {
				if e.NodeRevisionID == node {
					review.GoalRevisionID = e.GoalRevisionID
					review.KnowledgeRevisionID = e.KnowledgeRevisionID
					review.EvidenceID = e.ID
					review.SessionID = activities[e.ActivityID].SessionID
					if review.SessionID == "" {
						for _, event := range p.Timeline {
							if event.EventSequence == e.AcceptedEventSequence {
								review.SessionID = event.AggregateID
								if event.ParentSessionID != "" {
									review.SessionID = event.ParentSessionID
								}
								break
							}
						}
					}
					review.RouteRevisionID = e.RouteRevisionID
				}
			}
			r.Reviews = append(r.Reviews, review)
		}
	}
	sort.Slice(r.Nodes, func(i, j int) bool { return r.Nodes[i].Mastery.NodeRevisionID < r.Nodes[j].Mastery.NodeRevisionID })
	sort.Slice(r.Reviews, func(i, j int) bool { return r.Reviews[i].TaskID < r.Reviews[j].TaskID })
	sessions := map[string]bool{}
	for id, sp := range p.Sessions {
		s := sp.Session
		if !belongs(s.Context.GoalRevisionID) {
			continue
		}
		sessions[id] = true
		item := SessionSummary{SessionID: id, LearningSpaceID: goal.LearningSpaceID(), GoalID: goal.GoalID, GoalRevisionID: s.Context.GoalRevisionID, ScopeSnapshotID: revisions[s.Context.GoalRevisionID].GoalManagement().Details.ScopeSnapshotID, Name: goal.GoalManagement().Details.Name, GoalStatus: goal.GoalManagement().Status, State: string(s.State), RouteStepID: s.Context.RouteStepID, LastEventSequence: sp.UpdatedEventSequence, Position: string(s.State)}
		if s.Context.ActivityID != nil {
			item.ActivityID = *s.Context.ActivityID
		}
		item.NodeRevisionID = s.Context.FocusNodeRevisionID
		item.RouteRevisionID = s.Context.RouteRevisionID
		for _, r := range p.Routes {
			if r.Route.ID == s.Context.RouteRevisionID {
				for _, step := range r.Route.Steps {
					if step.ID == s.Context.RouteStepID {
						item.Position = string(s.State) + " · " + step.TeachingIntent
					}
				}
			}
		}
		r.Sessions = append(r.Sessions, item)
		r.EstimatedSeconds += p.Stats[id].DurationSeconds
	}
	sort.Slice(r.Sessions, func(i, j int) bool {
		if r.Sessions[i].LastEventSequence == r.Sessions[j].LastEventSequence {
			return r.Sessions[i].SessionID < r.Sessions[j].SessionID
		}
		return r.Sessions[i].LastEventSequence > r.Sessions[j].LastEventSequence
	})
	for i := len(p.Timeline) - 1; i >= 0; i-- {
		event := p.Timeline[i]
		if sessions[event.AggregateID] || sessions[event.ParentSessionID] {
			if len(r.Recent) == 10 {
				r.RecentHasMore = true
				break
			}
			r.Recent = append(r.Recent, event)
		}
	}
	return r
}
