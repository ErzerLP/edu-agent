package httpapi

import (
	"net/http"
	"strconv"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/learningspace"
	"github.com/edu-agent/edu-agent/server/internal/mentorrun"
	"github.com/go-chi/chi/v5"
)

func (a *API) mountConversations(router chi.Router) {
	read := router.With(a.requireScope("learning:read"), a.mentorScope)
	write := router.With(a.requireScope("learning:write"), a.mentorScope)
	read.Get("/v1/learning/conversations", a.tutorList)
	write.Post("/v1/learning/conversations", a.tutorCreate)
	read.Get("/v1/learning/conversations/{conversationID}", a.tutorRead)
	write.Patch("/v1/learning/conversations/{conversationID}", a.tutorChange)
	write.Delete("/v1/learning/conversations/{conversationID}", a.tutorChange)
	write.Post("/v1/learning/conversations/{conversationID}/turns", a.tutorTurn)
}

func (a *API) tutorList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit := 20
	if q.Has("limit") {
		n, err := strconv.Atoi(q.Get("limit"))
		if err != nil {
			mentorFailure(w, r, mentorrun.ErrInvalid)
			return
		}
		limit = n
	}
	if q.Has("all_contexts") && q.Get("all_contexts") != "true" && q.Get("all_contexts") != "false" {
		mentorFailure(w, r, mentorrun.ErrInvalid)
		return
	}
	actor, _ := credentialFromContext(r.Context())
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(5 * time.Second))
	started := false
	err := a.mentorRuns.ReadConversations(r.Context(), actor, learningspace.Scope(r.Context()), mentorrun.ConversationQuery{GoalID: q.Get("goal_id"), TeachingSessionID: q.Get("teaching_session_id"), Search: q.Get("search"), Cursor: q.Get("cursor"), AllContexts: q.Get("all_contexts") == "true", Limit: limit}, func(page mentorrun.ConversationPage) error {
		started = true
		writeJSON(w, 200, page)
		return http.NewResponseController(w).Flush()
	})
	if err != nil && !started {
		mentorFailure(w, r, err)
	}
}

func (a *API) tutorCreate(w http.ResponseWriter, r *http.Request) {
	// 保存策略必须由调用者明确提供，不能因字段缺失而推测用户选择。
	var request struct {
		ID                string `json:"id"`
		GoalID            string `json:"goal_id,omitempty"`
		TeachingSessionID string `json:"teaching_session_id,omitempty"`
		Saved             *bool  `json:"saved"`
		Title             string `json:"title,omitempty"`
	}
	if decodeJSON(w, r, 4096, &request) != nil || request.Saved == nil {
		mentorFailure(w, r, mentorrun.ErrInvalid)
		return
	}
	actor, _ := credentialFromContext(r.Context())
	id, err := a.mentorRuns.NewConversation(r.Context(), actor, learningspace.Scope(r.Context()), mentorrun.NewConversation{ID: request.ID, GoalID: request.GoalID, TeachingSessionID: request.TeachingSessionID, Saved: *request.Saved, Title: request.Title})
	if err != nil {
		mentorFailure(w, r, err)
		return
	}
	writeJSON(w, 201, map[string]string{"id": id})
}

func (a *API) tutorRead(w http.ResponseWriter, r *http.Request) {
	after := int64(0)
	limit := 20
	var err error
	if value := r.URL.Query().Get("after"); value != "" {
		after, err = strconv.ParseInt(value, 10, 64)
	}
	if err != nil {
		mentorFailure(w, r, mentorrun.ErrInvalid)
		return
	}
	if value := r.URL.Query().Get("limit"); value != "" {
		limit, err = strconv.Atoi(value)
	}
	if err != nil {
		mentorFailure(w, r, mentorrun.ErrInvalid)
		return
	}
	actor, _ := credentialFromContext(r.Context())
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(5 * time.Second))
	started := false
	err = a.mentorRuns.ReadConversation(r.Context(), actor, learningspace.Scope(r.Context()), chi.URLParam(r, "conversationID"), after, limit, func(page mentorrun.TurnPage) error {
		started = true
		writeJSON(w, 200, page)
		return http.NewResponseController(w).Flush()
	})
	if err != nil && !started {
		mentorFailure(w, r, err)
	}
}

func (a *API) tutorChange(w http.ResponseWriter, r *http.Request) {
	var request struct {
		ExpectedVersion int64  `json:"expected_version"`
		Title           string `json:"title,omitempty"`
		Confirmed       bool   `json:"confirmed,omitempty"`
	}
	remove := r.Method == http.MethodDelete
	if decodeJSON(w, r, 4096, &request) != nil || remove && (!request.Confirmed || request.Title != "") || !remove && request.Confirmed {
		mentorFailure(w, r, mentorrun.ErrInvalid)
		return
	}
	actor, _ := credentialFromContext(r.Context())
	if err := a.mentorRuns.ChangeConversation(r.Context(), actor, learningspace.Scope(r.Context()), chi.URLParam(r, "conversationID"), request.ExpectedVersion, request.Title, remove); err != nil {
		mentorFailure(w, r, err)
		return
	}
	writeJSON(w, 200, map[string]bool{"confirmed": true})
}

func (a *API) tutorTurn(w http.ResponseWriter, r *http.Request) {
	var request mentorrun.SubmitTurn
	if decodeJSON(w, r, 24<<10, &request) != nil {
		mentorFailure(w, r, mentorrun.ErrInvalid)
		return
	}
	actor, _ := credentialFromContext(r.Context())
	space := learningspace.Scope(r.Context())
	id := chi.URLParam(r, "conversationID")
	receipt, err := a.mentorRuns.Submit(r.Context(), actor, space, id, request)
	if err != nil {
		mentorFailure(w, r, err)
		return
	}
	writeJSON(w, 202, receipt)
}
