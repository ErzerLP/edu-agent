package httpapi

import (
	"context"
	"net/http"
	"strconv"
	"strings"

	"github.com/edu-agent/edu-agent/server/internal/learning"
)

type sessionSelectionService interface {
	SupportsSessionSelection() bool
	ListSessions(context.Context, learning.SessionQuery) (learning.SessionPage, error)
}

func (a *API) handleSessions(w http.ResponseWriter, r *http.Request) {
	service, ok := a.learning.(sessionSelectionService)
	if !ok || !service.SupportsSessionSelection() {
		writeError(w, r, 501, "session_selection_unavailable", "服务端尚不支持教学会话选择")
		return
	}
	q, ok := strictLearningQuery(w, r, "goal_id", "status", "cursor", "limit")
	if !ok {
		return
	}
	query := learning.SessionQuery{GoalID: q.Get("goal_id"), Status: q.Get("status"), Cursor: q.Get("cursor"), Limit: 50}
	if q.Has("limit") {
		n, err := strconv.Atoi(q.Get("limit"))
		if err != nil {
			writeLearningInvalid(w, r)
			return
		}
		query.Limit = n
	}
	page, err := service.ListSessions(r.Context(), query)
	if err != nil {
		a.writeLearningFailure(w, r, "session_list", err)
		return
	}
	writeJSON(w, 200, page)
}

func scopedTutoringPath(path string) bool {
	return path == "/v1/tutoring/sessions" || strings.HasPrefix(path, "/v1/tutoring/sessions/") || path == "/v1/tutoring/proposals" || strings.HasPrefix(path, "/v1/learning/offline/") || strings.HasPrefix(path, "/v1/learning/assessments/")
}
