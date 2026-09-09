package learning

import "context"

type SessionQuery struct {
	GoalID string
	Status string
	Cursor string
	Limit  int
}

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

type SessionLister interface {
	ListSessions(context.Context, SessionQuery) (SessionPage, error)
}

type SessionScopeValidator interface {
	ValidateSessionScope(context.Context, string) error
}

func (s *Service) validateSessionScope(ctx context.Context, id string) error {
	if store, ok := s.authority.(SessionScopeValidator); ok {
		return store.ValidateSessionScope(ctx, id)
	}
	return nil
}

func (s *Service) SupportsSessionSelection() bool { _, ok := s.queries.(SessionLister); return ok }
func (s *Service) ListSessions(ctx context.Context, q SessionQuery) (SessionPage, error) {
	if store, ok := s.queries.(SessionLister); ok {
		return store.ListSessions(ctx, q)
	}
	return SessionPage{}, &Error{Code: CodeInvalidRequest, Reason: "session_selection_unavailable"}
}
