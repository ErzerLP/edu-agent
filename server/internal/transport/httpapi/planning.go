package httpapi

import (
	"context"
	"github.com/edu-agent/edu-agent/server/internal/learning"
	"github.com/edu-agent/edu-agent/server/internal/privacy"
	"github.com/go-chi/chi/v5"
	"net/http"
)

type planningService interface {
	PlanningList(context.Context, string) ([]learning.PlanningDraft, error)
	Planning(context.Context, string, string) (learning.PlanningDraft, error)
	ChangePlanning(context.Context, string, string, string, learning.PlanningCommand) (learning.PlanningDraft, error)
}

func (a *API) mountPlanning(r chi.Router) {
	s, ok := a.learning.(planningService)
	if !ok {
		return
	}
	base := "/v1/learning/goals/{goalID}/plans"
	read := r.With(a.requireScope("learning:read"), a.requireScope("knowledge:read"), a.responseReadPermit("content_redacted", privacy.OwnerKnowledge, privacy.OwnerLearning, privacy.OwnerTutoring))
	read.Get(base, func(w http.ResponseWriter, r *http.Request) {
		if _, ok := strictLearningQuery(w, r); !ok {
			return
		}
		v, e := s.PlanningList(r.Context(), chi.URLParam(r, "goalID"))
		if e != nil {
			a.writeLearningFailure(w, r, "planning_list", e)
			return
		}
		writeJSON(w, 200, map[string]any{"items": v})
	})
	read.Get(base+"/{planID}", func(w http.ResponseWriter, r *http.Request) {
		if _, ok := strictLearningQuery(w, r); !ok {
			return
		}
		v, e := s.Planning(r.Context(), chi.URLParam(r, "goalID"), chi.URLParam(r, "planID"))
		if e != nil {
			a.writeLearningFailure(w, r, "planning_read", e)
			return
		}
		writeJSON(w, 200, v)
	})
	r.With(a.requireScope("learning:write"), a.requireScope("knowledge:read"), a.responseReadPermit("content_redacted", privacy.OwnerKnowledge, privacy.OwnerLearning, privacy.OwnerTutoring)).Post(base+"/{planID}", func(w http.ResponseWriter, r *http.Request) {
		var c learning.PlanningCommand
		if e := decodeJSON(w, r, 256<<10, &c); e != nil {
			importInputFailure(w, r, e)
			return
		}
		actor, _ := credentialFromContext(r.Context())
		v, e := s.ChangePlanning(r.Context(), actor.Device.ID, chi.URLParam(r, "goalID"), chi.URLParam(r, "planID"), c)
		if e != nil {
			a.writeLearningFailure(w, r, "planning_write", e)
			return
		}
		writeJSON(w, 200, v)
	})
}
