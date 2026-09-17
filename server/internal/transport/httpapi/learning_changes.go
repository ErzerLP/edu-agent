package httpapi

import (
	"errors"
	"github.com/edu-agent/edu-agent/server/internal/learningchange"
	"github.com/edu-agent/edu-agent/server/internal/privacy"
	"github.com/go-chi/chi/v5"
	"net/http"
	"strconv"
)

func (a *API) mountLearningChanges(r chi.Router) {
	r.With(a.requireScope("learning:read")).Get("/v1/learning/changes/capabilities", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"protocol_version": 1, "available": a.learningChanges.Available(), "modes": []string{"adaptive", "cautious"}})
	})
	if a.learningChanges == nil {
		return
	}
	read := r.With(a.requireScope("learning:read"), a.changeProtocol, a.responseReadPermit("content_redacted", privacy.OwnerLearning, privacy.OwnerTutoring, privacy.OwnerKnowledge))
	write := r.With(a.requireScope("learning:write"), a.changeProtocol, a.responseReadPermit("privacy_clear_in_progress", privacy.OwnerLearning, privacy.OwnerTutoring, privacy.OwnerKnowledge))
	read.Get("/v1/learning/goals/{goalID}/changes", func(w http.ResponseWriter, r *http.Request) {
		if _, ok := strictLearningQuery(w, r); !ok {
			return
		}
		actor, _ := credentialFromContext(r.Context())
		v, err := a.learningChanges.List(r.Context(), actor, chi.URLParam(r, "goalID"))
		if err != nil {
			a.changeError(w, r, err)
			return
		}
		writeJSON(w, 200, map[string]any{"items": v})
	})
	read.Get("/v1/learning/goals/{goalID}/changes/{changeID}", func(w http.ResponseWriter, r *http.Request) {
		q, ok := strictLearningQuery(w, r, "revision")
		if !ok {
			return
		}
		var revision int64
		var err error
		if q.Has("revision") {
			revision, err = strconv.ParseInt(q.Get("revision"), 10, 64)
			if err != nil || revision < 1 {
				a.changeError(w, r, learningchange.ErrInvalid)
				return
			}
		}
		actor, _ := credentialFromContext(r.Context())
		v, err := a.learningChanges.Read(r.Context(), actor, chi.URLParam(r, "goalID"), chi.URLParam(r, "changeID"), revision)
		if err != nil {
			a.changeError(w, r, err)
			return
		}
		writeJSON(w, 200, v)
	})
	read.Get("/v1/learning/goals/{goalID}/change-context", func(w http.ResponseWriter, r *http.Request) {
		q, ok := strictLearningQuery(w, r, "session_id")
		if !ok {
			return
		}
		actor, _ := credentialFromContext(r.Context())
		v, err := a.learningChanges.Snapshot(r.Context(), actor, chi.URLParam(r, "goalID"), q.Get("session_id"))
		if err != nil {
			a.changeError(w, r, err)
			return
		}
		writeJSON(w, 200, v)
	})
	read.Get("/v1/learning/goals/{goalID}/adaptive-mode", func(w http.ResponseWriter, r *http.Request) {
		if _, ok := strictLearningQuery(w, r); !ok {
			return
		}
		actor, _ := credentialFromContext(r.Context())
		v, err := a.learningChanges.Mode(r.Context(), actor, chi.URLParam(r, "goalID"), nil)
		if err != nil {
			a.changeError(w, r, err)
			return
		}
		writeJSON(w, 200, v)
	})
	write.Post("/v1/learning/goals/{goalID}/adaptive-mode", func(w http.ResponseWriter, r *http.Request) {
		var c learningchange.Mode
		if _, ok := strictLearningQuery(w, r); !ok || !a.decodeLearning(w, r, &c) {
			return
		}
		actor, _ := credentialFromContext(r.Context())
		v, err := a.learningChanges.Mode(r.Context(), actor, chi.URLParam(r, "goalID"), &c)
		if err != nil {
			a.changeError(w, r, err)
			return
		}
		writeJSON(w, 200, v)
	})
	write.Post("/v1/learning/goals/{goalID}/changes/{changeID}", func(w http.ResponseWriter, r *http.Request) {
		var c struct {
			SessionID string `json:"session_id"`
			learningchange.Command
		}
		if _, ok := strictLearningQuery(w, r); !ok || !a.decodeLearning(w, r, &c) {
			return
		}
		actor, _ := credentialFromContext(r.Context())
		v, err := a.learningChanges.Change(r.Context(), actor, chi.URLParam(r, "goalID"), c.SessionID, chi.URLParam(r, "changeID"), c.Command)
		if err != nil {
			a.changeError(w, r, err)
			return
		}
		writeJSON(w, 200, v)
	})
}
func (a *API) changeProtocol(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if len(r.Header.Values("X-Learning-Change-Version")) != 1 || r.Header.Get("X-Learning-Change-Version") != "1" || len(r.Header.Values("X-Learning-Space-ID")) != 1 || !validLearningUUID(r.Header.Get("X-Learning-Space-ID")) {
			a.changeError(w, r, learningchange.ErrInvalid)
			return
		}
		next.ServeHTTP(w, r)
	})
}
func (a *API) changeError(w http.ResponseWriter, r *http.Request, err error) {
	status := 0
	switch {
	case errors.Is(err, learningchange.ErrInvalid):
		status = 400
	case errors.Is(err, learningchange.ErrConflict), errors.Is(err, learningchange.ErrInactive):
		status = 409
	case errors.Is(err, learningchange.ErrNotFound):
		status = 404
	case errors.Is(err, learningchange.ErrForbidden):
		status = 403
	case errors.Is(err, learningchange.ErrUnavailable):
		status = 503
	}
	if status == 0 {
		a.contentError(w, r, err)
		return
	}
	writeError(w, r, status, err.Error(), "教学变更未完成："+err.Error())
}
