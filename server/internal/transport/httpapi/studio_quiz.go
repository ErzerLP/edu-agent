package httpapi

import (
	"net/http"
	"strconv"

	"github.com/edu-agent/edu-agent/server/internal/knowledge"
	"github.com/edu-agent/edu-agent/server/internal/learningcontent"
	"github.com/go-chi/chi/v5"
)

func (a *API) studioQuizError(w http.ResponseWriter, r *http.Request, err error) {
	if knowledge.ErrorCode(err) != "" {
		mentorFailure(w, r, err)
		return
	}
	a.contentError(w, r, err)
}

func (a *API) studioQuizGet(w http.ResponseWriter, r *http.Request) {
	q, ok := strictLearningQuery(w, r, "version")
	if !ok {
		return
	}
	version, err := strconv.ParseInt(q.Get("version"), 10, 64)
	if err != nil || version < 1 {
		a.contentError(w, r, learningcontent.ErrInvalid)
		return
	}
	if a.studioQuiz == nil {
		a.contentError(w, r, learningcontent.ErrUnavailable)
		return
	}
	actor, _ := credentialFromContext(r.Context())
	result, err := a.studioQuiz.StudioQuiz(r.Context(), actor, chi.URLParam(r, "artifactID"), version)
	if err != nil {
		a.studioQuizError(w, r, err)
		return
	}
	writeJSON(w, 200, result)
}
func (a *API) studioQuizStart(w http.ResponseWriter, r *http.Request) {
	if _, ok := strictLearningQuery(w, r); !ok {
		return
	}
	var c learningcontent.StudioQuizStart
	if !a.decodeLearning(w, r, &c) {
		return
	}
	if c.Validate() != nil {
		a.contentError(w, r, learningcontent.ErrInvalid)
		return
	}
	if a.studioQuiz == nil {
		a.contentError(w, r, learningcontent.ErrUnavailable)
		return
	}
	actor, _ := credentialFromContext(r.Context())
	result, err := a.studioQuiz.StartStudioQuiz(r.Context(), actor, chi.URLParam(r, "artifactID"), c)
	if err != nil {
		a.studioQuizError(w, r, err)
		return
	}
	writeJSON(w, 200, result)
}
func (a *API) studioQuizOperation(w http.ResponseWriter, r *http.Request) {
	if _, ok := strictLearningQuery(w, r); !ok {
		return
	}
	if a.studioQuiz == nil {
		a.contentError(w, r, learningcontent.ErrUnavailable)
		return
	}
	actor, _ := credentialFromContext(r.Context())
	result, err := a.studioQuiz.StudioQuizOperation(r.Context(), actor, chi.URLParam(r, "operationID"))
	if err != nil {
		a.studioQuizError(w, r, err)
		return
	}
	writeJSON(w, 200, result)
}
