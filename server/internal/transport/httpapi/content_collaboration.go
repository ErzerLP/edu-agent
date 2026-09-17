package httpapi

import (
	"net/http"
	"strconv"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/learningcontent"
	"github.com/go-chi/chi/v5"
)

func (a *API) contentLibrary(w http.ResponseWriter, r *http.Request) {
	q, ok := strictLearningQuery(w, r, "goal_id", "kind", "node_id", "source_status", "favorite", "after", "before", "limit", "cursor")
	if !ok {
		return
	}
	input := learningcontent.LibraryQuery{GoalID: q.Get("goal_id"), Kind: q.Get("kind"), NodeID: q.Get("node_id"), SourceStatus: q.Get("source_status"), Cursor: q.Get("cursor"), Limit: 20}
	if q.Has("limit") {
		n, err := strconv.Atoi(q.Get("limit"))
		if err != nil {
			a.contentError(w, r, learningcontent.ErrInvalid)
			return
		}
		input.Limit = n
	}
	if q.Has("favorite") {
		value, err := strconv.ParseBool(q.Get("favorite"))
		if err != nil {
			a.contentError(w, r, learningcontent.ErrInvalid)
			return
		}
		input.Favorite = value
	}
	for key, dest := range map[string]**time.Time{"after": &input.After, "before": &input.Before} {
		if q.Has(key) {
			v, err := time.Parse(time.RFC3339, q.Get(key))
			if err != nil {
				a.contentError(w, r, learningcontent.ErrInvalid)
				return
			}
			*dest = &v
		}
	}
	actor, _ := credentialFromContext(r.Context())
	result, err := a.learningContent.Library(r.Context(), actor, input)
	if err != nil {
		a.contentError(w, r, err)
		return
	}
	writeJSON(w, 200, result)
}

func (a *API) contentPreference(w http.ResponseWriter, r *http.Request) {
	if _, ok := strictLearningQuery(w, r); !ok {
		return
	}
	var input *learningcontent.Preference
	if r.Method == http.MethodPut {
		input = &learningcontent.Preference{}
		if !a.decodeLearning(w, r, input) {
			return
		}
	}
	actor, _ := credentialFromContext(r.Context())
	result, err := a.learningContent.Preference(r.Context(), actor, chi.URLParam(r, "artifactID"), input)
	if err != nil {
		a.contentError(w, r, err)
		return
	}
	writeJSON(w, 200, result)
}

func (a *API) contentRestore(w http.ResponseWriter, r *http.Request) {
	if _, ok := strictLearningQuery(w, r); !ok {
		return
	}
	var input learningcontent.Restore
	if !a.decodeLearning(w, r, &input) {
		return
	}
	actor, _ := credentialFromContext(r.Context())
	result, err := a.learningContent.Restore(r.Context(), actor, chi.URLParam(r, "artifactID"), input)
	if err != nil {
		a.contentError(w, r, err)
		return
	}
	writeJSON(w, 201, result)
}

func (a *API) contentReuse(w http.ResponseWriter, r *http.Request) {
	if _, ok := strictLearningQuery(w, r); !ok {
		return
	}
	var input learningcontent.Reuse
	if !a.decodeLearning(w, r, &input) {
		return
	}
	actor, _ := credentialFromContext(r.Context())
	result, err := a.learningContent.Reuse(r.Context(), actor, chi.URLParam(r, "artifactID"), input)
	if err != nil {
		a.contentError(w, r, err)
		return
	}
	writeJSON(w, 201, result)
}

func (a *API) contentExport(w http.ResponseWriter, r *http.Request) {
	q, ok := strictLearningQuery(w, r, "version", "format")
	if !ok {
		return
	}
	version, err := strconv.ParseInt(q.Get("version"), 10, 64)
	if err != nil || version < 1 {
		a.contentError(w, r, learningcontent.ErrInvalid)
		return
	}
	actor, _ := credentialFromContext(r.Context())
	result, err := a.learningContent.Export(r.Context(), actor, chi.URLParam(r, "artifactID"), version, q.Get("format"))
	if err != nil {
		a.contentError(w, r, err)
		return
	}
	writeJSON(w, 200, result)
}

func (a *API) contentCitation(w http.ResponseWriter, r *http.Request) {
	q, ok := strictLearningQuery(w, r, "version")
	if !ok {
		return
	}
	version, err := strconv.ParseInt(q.Get("version"), 10, 64)
	if err != nil || version < 1 || !validLearningUUID(chi.URLParam(r, "referenceID")) {
		a.contentError(w, r, learningcontent.ErrInvalid)
		return
	}
	actor, _ := credentialFromContext(r.Context())
	result, err := a.learningContent.Citation(r.Context(), actor, chi.URLParam(r, "artifactID"), version, chi.URLParam(r, "referenceID"))
	if err != nil {
		a.contentError(w, r, err)
		return
	}
	writeJSON(w, 200, result)
}
