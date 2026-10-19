package httpapi

import (
	"net/http"
	"strconv"

	"github.com/edu-agent/edu-agent/server/internal/learningcontent"
	"github.com/edu-agent/edu-agent/server/internal/mentorrun"
	"github.com/go-chi/chi/v5"
)

func (a *API) studioUnits(w http.ResponseWriter, r *http.Request) {
	q, ok := strictLearningQuery(w, r, "cursor", "limit", "plan_revision")
	if !ok {
		return
	}
	limit := 16
	revision := int64(0)
	var err error
	if q.Has("limit") {
		limit, err = strconv.Atoi(q.Get("limit"))
	}
	if err == nil && q.Has("plan_revision") {
		revision, err = strconv.ParseInt(q.Get("plan_revision"), 10, 64)
	}
	if err != nil {
		mentorFailure(w, r, mentorrun.ErrInvalid)
		return
	}
	if a.mentorRuns == nil {
		mentorFailure(w, r, mentorrun.ErrStorage)
		return
	}
	actor, _ := credentialFromContext(r.Context())
	page, err := a.mentorRuns.StudioUnits(r.Context(), actor, chi.URLParam(r, "jobID"), q.Get("cursor"), revision, limit)
	if err != nil {
		mentorFailure(w, r, err)
		return
	}
	writeJSON(w, 200, page)
}
func (a *API) studioSections(w http.ResponseWriter, r *http.Request) {
	q, ok := strictLearningQuery(w, r, "version", "cursor", "limit")
	if !ok {
		return
	}
	version, err := strconv.ParseInt(q.Get("version"), 10, 64)
	after, limit := 0, 16
	if err == nil && q.Has("cursor") {
		after, err = strconv.Atoi(q.Get("cursor"))
	}
	if err == nil && q.Has("limit") {
		limit, err = strconv.Atoi(q.Get("limit"))
	}
	if err != nil || !validLearningUUID(chi.URLParam(r, "artifactID")) {
		mentorFailure(w, r, mentorrun.ErrInvalid)
		return
	}
	actor, _ := credentialFromContext(r.Context())
	page, err := a.learningContent.StudioSections(r.Context(), actor, chi.URLParam(r, "artifactID"), version, after, limit)
	if err != nil {
		mentorFailure(w, r, err)
		return
	}
	writeJSON(w, 200, page)
}
func (a *API) studioSectionSave(w http.ResponseWriter, r *http.Request) {
	if _, ok := strictLearningQuery(w, r); !ok {
		return
	}
	var input learningcontent.StudioSectionSave
	if !decodeStudioJobRequest(w, r, &input, []string{"operation_id", "privacy_generation", "expected_version", "markdown"}, nil) {
		return
	}
	actor, _ := credentialFromContext(r.Context())
	result, err := a.learningContent.StudioSaveSection(r.Context(), actor, chi.URLParam(r, "artifactID"), chi.URLParam(r, "sectionID"), input)
	if err != nil {
		mentorFailure(w, r, err)
		return
	}
	writeJSON(w, 200, result)
}
func studioSchemaAccepted(w http.ResponseWriter, r *http.Request, revision learningcontent.StudioRevision) bool {
	w.Header().Set("X-Studio-Supported-Body-Schemas", "1,2")
	if revision.Body.Schema == 2 && r.Header.Get("X-Studio-Body-Schema") != "2" {
		writeError(w, r, 422, "unsupported_content_schema", "此产物使用分段格式，请使用支持分段内容的客户端打开。")
		return false
	}
	return true
}
