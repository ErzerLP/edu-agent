package httpapi

import (
	"net/http"

	"github.com/edu-agent/edu-agent/server/internal/learningchange"
	"github.com/edu-agent/edu-agent/server/internal/privacy"
	"github.com/go-chi/chi/v5"
)

func (a *API) mountReferences(r chi.Router) {
	if a.learningChanges == nil {
		return
	}
	read := r.With(a.requireScope("references:manage"), a.requireScope("learning:read"), a.responseReadPermit("content_redacted", privacy.OwnerKnowledge, privacy.OwnerLearning, privacy.OwnerTutoring))
	write := r.With(a.requireScope("references:manage"), a.requireScope("knowledge:approve"), a.requireScope("learning:write"), a.responseReadPermit("content_redacted", privacy.OwnerKnowledge, privacy.OwnerLearning, privacy.OwnerTutoring))
	read.Get("/v1/learning/goals/{goalID}/references", func(w http.ResponseWriter, r *http.Request) {
		q, ok := strictLearningQuery(w, r, "session_id")
		if !ok {
			return
		}
		actor, _ := credentialFromContext(r.Context())
		v, err := a.learningChanges.ReadReferences(r.Context(), actor, chi.URLParam(r, "goalID"), q.Get("session_id"))
		if err != nil {
			a.changeError(w, r, err)
			return
		}
		writeJSON(w, 200, v)
	})
	read.Get("/v1/learning/goals/{goalID}/references/operations/{operationID}", func(w http.ResponseWriter, r *http.Request) {
		actor, _ := credentialFromContext(r.Context())
		v, err := a.learningChanges.ReferenceOperation(r.Context(), actor, chi.URLParam(r, "goalID"), chi.URLParam(r, "operationID"))
		if err != nil {
			a.changeError(w, r, err)
			return
		}
		writeJSON(w, 200, v)
	})
	write.Post("/v1/learning/goals/{goalID}/references/previews", func(w http.ResponseWriter, r *http.Request) {
		var c learningchange.ReferenceRequest
		if !a.decodeLearning(w, r, &c) {
			return
		}
		actor, _ := credentialFromContext(r.Context())
		v, err := a.learningChanges.PreviewReferences(r.Context(), actor, chi.URLParam(r, "goalID"), c)
		if err != nil {
			a.changeError(w, r, err)
			return
		}
		writeJSON(w, 200, v)
	})
	write.Post("/v1/learning/goals/{goalID}/references/confirm", func(w http.ResponseWriter, r *http.Request) {
		var c learningchange.ReferenceConfirmation
		if !a.decodeLearning(w, r, &c) {
			return
		}
		actor, _ := credentialFromContext(r.Context())
		v, err := a.learningChanges.ConfirmReferences(r.Context(), actor, chi.URLParam(r, "goalID"), c)
		if err != nil {
			a.changeError(w, r, err)
			return
		}
		writeJSON(w, 200, v)
	})
}
