package httpapi

import (
	"net/http"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/mentorrun"
	"github.com/edu-agent/edu-agent/server/internal/privacy"
	"github.com/go-chi/chi/v5"
)

func (a *API) mountStudioExercises(router chi.Router) {
	if a.mentorRuns == nil {
		return
	}
	read := router.With(a.requireScope("learning:read"), a.requireScope("knowledge:read"), a.studioScope, a.responseReadPermit("content_redacted", privacy.OwnerIdentity, privacy.OwnerLearning, privacy.OwnerKnowledge))
	write := router.With(a.requireScope("learning:write"), a.requireScope("knowledge:read"), a.studioScope, a.responseReadPermit("content_redacted", privacy.OwnerIdentity, privacy.OwnerLearning, privacy.OwnerKnowledge))
	read.Get("/v1/learning/studio/exercises/{exerciseID}", a.studioExerciseRead)
	read.Get("/v1/learning/studio/exercises/{exerciseID}/help/{operationID}", a.studioExerciseHelpOperation)
	write.Post("/v1/learning/studio/exercises/{exerciseID}/help", a.studioExerciseHelp)
	write.Post("/v1/learning/studio/exercises/{exerciseID}/assessment/resume", a.studioExerciseResume)
}
func (a *API) studioExerciseRead(w http.ResponseWriter, r *http.Request) {
	if _, ok := strictLearningQuery(w, r); !ok {
		return
	}
	actor, _ := credentialFromContext(r.Context())
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(5 * time.Second))
	started := false
	err := a.mentorRuns.ReadStudioExercise(r.Context(), actor, chi.URLParam(r, "exerciseID"), func(v mentorrun.StudioExerciseView) error {
		started = true
		writeJSON(w, 200, v)
		return http.NewResponseController(w).Flush()
	})
	if err != nil && !started {
		mentorFailure(w, r, err)
	}
}
func (a *API) studioExerciseHelp(w http.ResponseWriter, r *http.Request) {
	if _, ok := strictLearningQuery(w, r); !ok {
		return
	}
	var c mentorrun.StudioHelpRequest
	if !a.decodeLearning(w, r, &c) {
		return
	}
	actor, _ := credentialFromContext(r.Context())
	v, err := a.mentorRuns.PublishStudioHelp(r.Context(), actor, chi.URLParam(r, "exerciseID"), c)
	if err != nil {
		mentorFailure(w, r, err)
		return
	}
	writeJSON(w, 200, v)
}
func (a *API) studioExerciseHelpOperation(w http.ResponseWriter, r *http.Request) {
	if _, ok := strictLearningQuery(w, r); !ok {
		return
	}
	actor, _ := credentialFromContext(r.Context())
	v, err := a.mentorRuns.StudioHelpOperation(r.Context(), actor, chi.URLParam(r, "exerciseID"), chi.URLParam(r, "operationID"))
	if err != nil {
		mentorFailure(w, r, err)
		return
	}
	writeJSON(w, 200, v)
}
func (a *API) studioExerciseResume(w http.ResponseWriter, r *http.Request) {
	if _, ok := strictLearningQuery(w, r); !ok {
		return
	}
	var c mentorrun.StudyAssessmentResume
	if !a.decodeLearning(w, r, &c) {
		return
	}
	actor, _ := credentialFromContext(r.Context())
	v, err := a.mentorRuns.ResumeStudioAssessment(r.Context(), actor, chi.URLParam(r, "exerciseID"), c)
	if err != nil {
		mentorFailure(w, r, err)
		return
	}
	writeJSON(w, 200, v)
}
