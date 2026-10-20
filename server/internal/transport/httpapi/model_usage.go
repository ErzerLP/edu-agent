package httpapi

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/identity"
	"github.com/edu-agent/edu-agent/server/internal/learningspace"
	"github.com/edu-agent/edu-agent/server/internal/modelusage"
	"github.com/edu-agent/edu-agent/server/internal/privacy"
	"github.com/go-chi/chi/v5"
)

func usagePath(path string) bool {
	return path == "/v1/learning/usage" || path == "/v1/learning/usage/tasks" || len(path) > len("/v1/learning/usage/tasks/") && path[:len("/v1/learning/usage/tasks/")] == "/v1/learning/usage/tasks/"
}
func (a *API) mountUsage(r chi.Router) {
	if a.modelUsage == nil {
		return
	}
	read := r.With(a.requireScope("learning:read"), a.responseReadPermit("content_redacted", privacy.OwnerIdentity, privacy.OwnerLearning))
	read.Get("/v1/learning/usage", a.usageSummary)
	read.Get("/v1/learning/usage/tasks", a.usageTasks)
	read.Get("/v1/learning/usage/tasks/{taskID}/calls", a.usageCalls)
	read.Get("/v1/learning/usage/tasks/{taskID}/search-calls", a.usageSearchCalls)
}

func (a *API) usageSearchCalls(w http.ResponseWriter, r *http.Request) {
	q, ok := usageQuery(w, r)
	if !ok {
		return
	}
	actor, _ := credentialFromContext(r.Context())
	result, err := a.modelUsage.SearchCalls(r.Context(), actor, q, chi.URLParam(r, "taskID"))
	if err != nil {
		a.usageFailure(w, r, err)
		return
	}
	writeJSON(w, 200, result)
}
func usageQuery(w http.ResponseWriter, r *http.Request) (modelusage.Query, bool) {
	q := modelusage.Query{Timezone: "UTC", Grain: "day", Limit: 20}
	values, ok := strictLearningQuery(w, r, "from", "to", "timezone", "grain", "model", "space_id", "cursor", "limit", "source", "actor_device_id", "cli_session_id")
	if !ok {
		return q, false
	}
	var err error
	q.From, err = time.Parse(time.RFC3339Nano, values.Get("from"))
	if err != nil {
		writeLearningInvalid(w, r)
		return q, false
	}
	q.To, err = time.Parse(time.RFC3339Nano, values.Get("to"))
	if err != nil {
		writeLearningInvalid(w, r)
		return q, false
	}
	if values.Has("timezone") {
		q.Timezone = values.Get("timezone")
	}
	if values.Has("grain") {
		q.Grain = values.Get("grain")
	}
	q.Model = values.Get("model")
	q.Source = values.Get("source")
	q.ActorDeviceID = values.Get("actor_device_id")
	q.CLISessionID = values.Get("cli_session_id")
	q.SpaceID = values.Get("space_id")
	q.Cursor = values.Get("cursor")
	if values.Has("limit") {
		q.Limit, err = strconv.Atoi(values.Get("limit"))
		if err != nil {
			writeLearningInvalid(w, r)
			return q, false
		}
	}
	if err = q.Validate(); err != nil || q.SpaceID != r.Header.Get(learningspace.Header) {
		writeLearningInvalid(w, r)
		return q, false
	}
	w.Header().Set("Cache-Control", "no-store")
	return q, true
}
func (a *API) usageFailure(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, modelusage.ErrInvalid):
		writeLearningInvalid(w, r)
	case errors.Is(err, modelusage.ErrNotFound):
		writeError(w, r, 404, "usage_task_not_found", "未找到此任务的用量")
	case errors.Is(err, identity.ErrForbidden):
		writeError(w, r, 403, "forbidden", "没有读取用量的权限")
	default:
		a.writeLearningFailure(w, r, "usage", err)
	}
}
func (a *API) usageSummary(w http.ResponseWriter, r *http.Request) {
	q, ok := usageQuery(w, r)
	if !ok {
		return
	}
	actor, _ := credentialFromContext(r.Context())
	result, err := a.modelUsage.Summary(r.Context(), actor, q)
	if err != nil {
		a.usageFailure(w, r, err)
		return
	}
	writeJSON(w, 200, result)
}
func (a *API) usageTasks(w http.ResponseWriter, r *http.Request) {
	q, ok := usageQuery(w, r)
	if !ok {
		return
	}
	actor, _ := credentialFromContext(r.Context())
	result, err := a.modelUsage.Tasks(r.Context(), actor, q)
	if err != nil {
		a.usageFailure(w, r, err)
		return
	}
	writeJSON(w, 200, result)
}
func (a *API) usageCalls(w http.ResponseWriter, r *http.Request) {
	q, ok := usageQuery(w, r)
	if !ok {
		return
	}
	actor, _ := credentialFromContext(r.Context())
	result, err := a.modelUsage.Calls(r.Context(), actor, q, chi.URLParam(r, "taskID"))
	if err != nil {
		a.usageFailure(w, r, err)
		return
	}
	writeJSON(w, 200, result)
}
