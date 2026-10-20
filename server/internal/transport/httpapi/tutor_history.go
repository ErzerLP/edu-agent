package httpapi

import (
	"encoding/json"
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
	read.Get("/v1/learning/conversations/{conversationID}/topics", a.tutorTopics)
	read.Get("/v1/learning/conversations/{conversationID}/topics/{topicID}/anchors", a.tutorTopicAnchors)
	read.Get("/v1/learning/conversations/{conversationID}/messages/{messageID}", a.tutorMessage)
	write.Post("/v1/learning/conversations/{conversationID}/topic-index", a.tutorTopicIndex)
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
	err := a.mentorRuns.ReadConversations(r.Context(), actor, learningspace.Scope(r.Context()), mentorrun.ConversationQuery{Mode: q.Get("mode"), GoalID: q.Get("goal_id"), TeachingSessionID: q.Get("teaching_session_id"), Search: q.Get("search"), Cursor: q.Get("cursor"), AllContexts: q.Get("all_contexts") == "true", Limit: limit}, func(page mentorrun.ConversationPage) error {
		started = true
		writeJSON(w, 200, page)
		return http.NewResponseController(w).Flush()
	})
	if err != nil && !started {
		mentorFailure(w, r, err)
	}
}

func (a *API) tutorCreate(w http.ResponseWriter, r *http.Request) {
	// Legacy callers still choose storage explicitly; study has its own saved default.
	var request struct {
		WebSearchEnabled  *bool  `json:"web_search_enabled,omitempty"`
		Mode              string `json:"mode,omitempty"`
		ID                string `json:"id"`
		GoalID            string `json:"goal_id,omitempty"`
		TeachingSessionID string `json:"teaching_session_id,omitempty"`
		Saved             *bool  `json:"saved"`
		Title             string `json:"title,omitempty"`
	}
	raw, decodeErr := readJSONBody(w, r, 4096)
	var fields map[string]json.RawMessage
	if decodeErr != nil || decodeJSONData(raw, &request) != nil || json.Unmarshal(raw, &fields) != nil || string(fields["saved"]) == "null" || string(fields["mode"]) == "null" || request.Saved == nil && request.Mode != "study" || request.Mode == "study" && request.Saved != nil && !*request.Saved {
		mentorFailure(w, r, mentorrun.ErrInvalid)
		return
	}
	actor, _ := credentialFromContext(r.Context())
	saved := request.Mode == "study"
	if request.Saved != nil {
		saved = *request.Saved
	}
	id, err := a.mentorRuns.NewConversation(r.Context(), actor, learningspace.Scope(r.Context()), mentorrun.NewConversation{WebSearchEnabled: request.WebSearchEnabled, Mode: request.Mode, ID: request.ID, GoalID: request.GoalID, TeachingSessionID: request.TeachingSessionID, Saved: saved, Title: request.Title})
	if err != nil {
		mentorFailure(w, r, err)
		return
	}
	preference, err := a.mentorRuns.ConversationWebPreference(r.Context(), actor, learningspace.Scope(r.Context()), id)
	if err != nil {
		mentorFailure(w, r, err)
		return
	}
	writeJSON(w, 201, map[string]any{"id": id, "web_search_enabled": preference.Enabled, "web_search_version": preference.Version})
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
	raw, err := readJSONBody(w, r, 64<<10)
	var fields map[string]json.RawMessage
	if err != nil || decodeJSONData(raw, &request) != nil || request.ExpectedWebSearchVersion == nil || *request.ExpectedWebSearchVersion < 1 || json.Unmarshal(raw, &fields) != nil || fields["source_selection"] != nil && request.SourceSelection == nil || string(fields["direction_version"]) == "null" {
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
