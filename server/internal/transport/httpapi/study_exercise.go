package httpapi

import (
	"net/http"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/mentorrun"
	"github.com/edu-agent/edu-agent/server/internal/privacy"
	"github.com/go-chi/chi/v5"
)

func (a *API) mountStudyExercises(router chi.Router) {
	read := router.With(a.requireScope("learning:read"), a.requireScope("knowledge:read"), a.mentorScope, a.responseReadPermit("content_redacted", privacy.OwnerIdentity, privacy.OwnerLearning, privacy.OwnerKnowledge))
	write := router.With(a.requireScope("learning:write"), a.requireScope("knowledge:read"), a.mentorScope, a.responseReadPermit("content_redacted", privacy.OwnerIdentity, privacy.OwnerLearning, privacy.OwnerKnowledge))
	read.Get("/v1/learning/exercises/{exerciseID}", a.studyExerciseRead)
	read.Get("/v1/learning/exercises/{exerciseID}/citations/{referenceID}", a.studyExerciseCitation)
	read.Get("/v1/learning/exercises/{exerciseID}/help/{operationID}", a.studyExerciseHelpOperation)
	write.Post("/v1/learning/exercises/{exerciseID}/commands", a.studyExerciseCommand)
	write.Post("/v1/learning/exercises/{exerciseID}/help", a.studyExerciseHelp)
	write.Post("/v1/learning/exercises/{exerciseID}/assessment/resume", a.studyExerciseResume)
}
func (a *API) studyExerciseRead(w http.ResponseWriter, r *http.Request) {
	if _, ok := strictLearningQuery(w, r); !ok {
		return
	}
	actor, _ := credentialFromContext(r.Context())
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(5 * time.Second))
	started := false
	err := a.mentorRuns.ReadStudyExercise(r.Context(), actor, chi.URLParam(r, "exerciseID"), func(v mentorrun.StudyExerciseView) error {
		started = true
		writeJSON(w, 200, v)
		return http.NewResponseController(w).Flush()
	})
	if err != nil && !started {
		mentorFailure(w, r, err)
	}
}
func (a *API) studyExerciseCitation(w http.ResponseWriter, r *http.Request) {
	if _, ok := strictLearningQuery(w, r); !ok {
		return
	}
	actor, _ := credentialFromContext(r.Context())
	started := false
	err := a.mentorRuns.ReadStudyExerciseCitation(r.Context(), actor, chi.URLParam(r, "exerciseID"), chi.URLParam(r, "referenceID"), func(v mentorrun.SourceCitation) error {
		started = true
		writeJSON(w, 200, v)
		return http.NewResponseController(w).Flush()
	})
	if err != nil && !started {
		mentorFailure(w, r, err)
	}
}
func (a *API) studyExerciseCommand(w http.ResponseWriter, r *http.Request) {
	if _, ok := strictLearningQuery(w, r); !ok {
		return
	}
	var c mentorrun.StudyExerciseCommand
	if !a.decodeLearning(w, r, &c) {
		return
	}
	actor, _ := credentialFromContext(r.Context())
	result, err := a.mentorRuns.ChangeStudyExercise(r.Context(), actor, chi.URLParam(r, "exerciseID"), c)
	if err != nil {
		mentorFailure(w, r, err)
		return
	}
	writeJSON(w, 200, result)
}
func (a *API) studyExerciseHelp(w http.ResponseWriter, r *http.Request) {
	if _, ok := strictLearningQuery(w, r); !ok {
		return
	}
	var c mentorrun.StudyHelpRequest
	if !a.decodeLearning(w, r, &c) {
		return
	}
	actor, _ := credentialFromContext(r.Context())
	result, err := a.mentorRuns.PublishStudyHelp(r.Context(), actor, chi.URLParam(r, "exerciseID"), c)
	if err != nil {
		mentorFailure(w, r, err)
		return
	}
	writeJSON(w, 200, result)
}
func (a *API) studyExerciseHelpOperation(w http.ResponseWriter, r *http.Request) {
	if _, ok := strictLearningQuery(w, r); !ok {
		return
	}
	actor, _ := credentialFromContext(r.Context())
	result, err := a.mentorRuns.StudyHelpOperation(r.Context(), actor, chi.URLParam(r, "exerciseID"), chi.URLParam(r, "operationID"))
	if err != nil {
		mentorFailure(w, r, err)
		return
	}
	writeJSON(w, 200, result)
}
func (a *API) studyExerciseResume(w http.ResponseWriter, r *http.Request) {
	if _, ok := strictLearningQuery(w, r); !ok {
		return
	}
	var c mentorrun.StudyAssessmentResume
	if !a.decodeLearning(w, r, &c) {
		return
	}
	actor, _ := credentialFromContext(r.Context())
	result, err := a.mentorRuns.ResumeStudyAssessment(r.Context(), actor, chi.URLParam(r, "exerciseID"), c)
	if err != nil {
		mentorFailure(w, r, err)
		return
	}
	writeJSON(w, 200, result)
}
