package httpapi

import (
	"context"
	"net/http"
	"strconv"

	"github.com/edu-agent/edu-agent/server/internal/learning"
	"github.com/go-chi/chi/v5"
)

type goalManagementService interface {
	SupportsGoalManagement() bool
	GetGoal(context.Context, string) (learning.GoalRevision, error)
	ListGoals(context.Context, learning.GoalQuery) (learning.GoalPage, error)
	GoalHistory(context.Context, string, learning.GoalQuery) (learning.GoalPage, error)
}

func (a *API) handleGoals(w http.ResponseWriter, r *http.Request) {
	service, ok := a.learning.(goalManagementService)
	if !ok || !service.SupportsGoalManagement() {
		writeError(w, r, 501, "goal_management_unsupported", "服务端尚不支持目标管理")
		return
	}
	id := chi.URLParam(r, "goalID")
	if id != "" && !validLearningUUID(id) {
		writeLearningInvalid(w, r)
		return
	}
	if id != "" && chi.URLParam(r, "history") == "" {
		if _, ok := strictLearningQuery(w, r); !ok {
			return
		}
		g, err := service.GetGoal(r.Context(), id)
		if err != nil {
			a.writeLearningFailure(w, r, "goal_detail", err)
			return
		}
		writeJSON(w, 200, g)
		return
	}
	q, ok := strictLearningQuery(w, r, "search", "status", "cursor", "limit")
	if !ok {
		return
	}
	query := learning.GoalQuery{Search: q.Get("search"), Status: q.Get("status"), Cursor: q.Get("cursor"), Limit: 50}
	if q.Has("limit") {
		n, err := strconv.Atoi(q.Get("limit"))
		if err != nil {
			writeLearningInvalid(w, r)
			return
		}
		query.Limit = n
	}
	var page learning.GoalPage
	var err error
	if id == "" {
		page, err = service.ListGoals(r.Context(), query)
	} else {
		page, err = service.GoalHistory(r.Context(), id, query)
	}
	if err != nil {
		a.writeLearningFailure(w, r, "goal_list", err)
		return
	}
	writeJSON(w, 200, page)
}

func goalManagementPath(path string) bool {
	return path == "/v1/learning/goals" || len(path) > len("/v1/learning/goals/") && path[:len("/v1/learning/goals/")] == "/v1/learning/goals/"
}
