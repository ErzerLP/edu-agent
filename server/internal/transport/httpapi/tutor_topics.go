package httpapi

import (
	"net/http"
	"strconv"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/learningspace"
	"github.com/edu-agent/edu-agent/server/internal/mentorrun"
	"github.com/go-chi/chi/v5"
)

func topicPageLimit(r *http.Request) (int, error) {
	if r.URL.Query().Has("limit") {
		return strconv.Atoi(r.URL.Query().Get("limit"))
	}
	return 20, nil
}
func topicResponse[T any](w http.ResponseWriter, started *bool) func(T) error {
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(5 * time.Second))
	return func(page T) error {
		*started = true
		writeJSON(w, 200, page)
		return http.NewResponseController(w).Flush()
	}
}
func (a *API) tutorTopics(w http.ResponseWriter, r *http.Request) {
	limit, err := topicPageLimit(r)
	if err != nil {
		mentorFailure(w, r, mentorrun.ErrInvalid)
		return
	}
	actor, _ := credentialFromContext(r.Context())
	started := false
	err = a.mentorRuns.ReadTopics(r.Context(), actor, learningspace.Scope(r.Context()), chi.URLParam(r, "conversationID"), r.URL.Query().Get("cursor"), limit, topicResponse[mentorrun.TopicPage](w, &started))
	if err != nil && !started {
		mentorFailure(w, r, err)
	}
}
func (a *API) tutorTopicAnchors(w http.ResponseWriter, r *http.Request) {
	limit, err := topicPageLimit(r)
	if err != nil {
		mentorFailure(w, r, mentorrun.ErrInvalid)
		return
	}
	q := r.URL.Query()
	cursor := int64(0)
	if q.Has("cursor") {
		cursor, err = strconv.ParseInt(q.Get("cursor"), 10, 64)
	}
	if err != nil || q.Has("before") && q.Get("before") != "true" && q.Get("before") != "false" {
		mentorFailure(w, r, mentorrun.ErrInvalid)
		return
	}
	actor, _ := credentialFromContext(r.Context())
	started := false
	err = a.mentorRuns.ReadTopicAnchors(r.Context(), actor, learningspace.Scope(r.Context()), chi.URLParam(r, "conversationID"), chi.URLParam(r, "topicID"), cursor, q.Get("before") == "true", limit, topicResponse[mentorrun.TopicAnchorPage](w, &started))
	if err != nil && !started {
		mentorFailure(w, r, err)
	}
}
func (a *API) tutorMessage(w http.ResponseWriter, r *http.Request) {
	actor, _ := credentialFromContext(r.Context())
	started := false
	err := a.mentorRuns.ReadMessage(r.Context(), actor, learningspace.Scope(r.Context()), chi.URLParam(r, "conversationID"), chi.URLParam(r, "messageID"), topicResponse[mentorrun.TurnPage](w, &started))
	if err != nil && !started {
		mentorFailure(w, r, err)
	}
}
func (a *API) tutorTopicIndex(w http.ResponseWriter, r *http.Request) {
	var request mentorrun.TopicIndexRequest
	if decodeJSON(w, r, 64<<10, &request) != nil {
		mentorFailure(w, r, mentorrun.ErrInvalid)
		return
	}
	actor, _ := credentialFromContext(r.Context())
	receipt, err := a.mentorRuns.RequestTopicIndex(r.Context(), actor, learningspace.Scope(r.Context()), chi.URLParam(r, "conversationID"), request)
	if err != nil {
		mentorFailure(w, r, err)
		return
	}
	writeJSON(w, 202, receipt)
}
