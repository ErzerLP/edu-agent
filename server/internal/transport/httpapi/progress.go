package httpapi

import (
	"context"
	"net/http"
	"strconv"

	"github.com/edu-agent/edu-agent/server/internal/learning"
)

type progressService interface {
	SupportsProgress() bool
	Progress(context.Context, learning.ProgressQuery) (learning.ProgressPage, error)
}

func (a *API) handleProgress(w http.ResponseWriter, r *http.Request) {
	service, ok := a.learning.(progressService)
	if !ok || !service.SupportsProgress() {
		writeError(w, r, 501, "progress_unavailable", "服务端尚不支持目标进度")
		return
	}
	values, ok := strictLearningQuery(w, r, "global", "goal_id", "status", "order", "cursor", "limit")
	if !ok {
		return
	}
	q := learning.ProgressQuery{GoalID: values.Get("goal_id"), Status: values.Get("status"), Order: values.Get("order"), Cursor: values.Get("cursor"), Limit: 50}
	var err error
	if values.Has("global") {
		q.Global, err = strconv.ParseBool(values.Get("global"))
		if err != nil {
			writeLearningInvalid(w, r)
			return
		}
	}
	if values.Has("limit") {
		q.Limit, err = strconv.Atoi(values.Get("limit"))
		if err != nil {
			writeLearningInvalid(w, r)
			return
		}
	}
	page, err := service.Progress(r.Context(), q)
	if err != nil {
		a.writeLearningFailure(w, r, "progress", err)
		return
	}
	normalizeProjectionMetadata(&page.Metadata)
	for i := range page.Items {
		for j := range page.Items[i].Nodes {
			normalizeNodeReduction(&page.Items[i].Nodes[j])
		}
	}
	writeJSON(w, 200, page)
}
