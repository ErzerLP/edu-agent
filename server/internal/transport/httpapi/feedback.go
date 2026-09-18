package httpapi

import (
	"net/http"
	"strconv"

	"github.com/edu-agent/edu-agent/server/internal/learning"
	"github.com/go-chi/chi/v5"
)

func (a *API) learningFeedbackList(w http.ResponseWriter, r *http.Request) {
	reader, ok := a.learning.(learning.FeedbackReader)
	if !ok {
		writeError(w, r, http.StatusNotImplemented, "feedback_unavailable", "当前服务不支持在线评估详情")
		return
	}
	query, ok := strictLearningQuery(w, r, "status", "session_id", "cursor", "limit")
	if !ok {
		return
	}
	q := learning.FeedbackQuery{Status: query.Get("status"), SessionID: query.Get("session_id"), Page: learning.CursorPageRequest{Limit: 20, Cursor: query.Get("cursor")}}
	if q.Status == "" {
		q.Status = "all"
	}
	if value, exists := query["limit"]; exists {
		limit, err := strconv.Atoi(value[0])
		if err != nil {
			writeLearningInvalid(w, r)
			return
		}
		q.Page.Limit = limit
	}
	page, err := reader.ListFeedback(r.Context(), q)
	if err != nil {
		a.writeLearningFailure(w, r, "feedback_list", err)
		return
	}
	writeJSON(w, http.StatusOK, page)
}

func (a *API) learningFeedback(w http.ResponseWriter, r *http.Request) {
	reader, ok := a.learning.(learning.FeedbackReader)
	if !ok {
		writeError(w, r, http.StatusNotImplemented, "feedback_unavailable", "当前服务不支持在线评估详情")
		return
	}
	if _, ok := strictLearningQuery(w, r); !ok {
		return
	}
	id := chi.URLParam(r, "assessmentID")
	byAssessment := id != ""
	if !byAssessment {
		id = chi.URLParam(r, "attemptID")
	}
	if !validLearningUUID(id) {
		writeLearningInvalid(w, r)
		return
	}
	view, err := reader.Feedback(r.Context(), id, byAssessment)
	if err != nil {
		a.writeLearningFailure(w, r, "feedback", err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}
