package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/edu-agent/edu-agent/server/internal/knowledge"
	"github.com/edu-agent/edu-agent/server/internal/learningcontent"
	"github.com/edu-agent/edu-agent/server/internal/learningspace"
	"github.com/edu-agent/edu-agent/server/internal/mentorrun"
	"github.com/edu-agent/edu-agent/server/internal/privacy"
	"github.com/go-chi/chi/v5"
)

func studioPath(path string) bool { return strings.HasPrefix(path, "/v1/learning/studio/") }
func (a *API) studioScope(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if len(r.Header.Values(learningspace.Header)) != 1 || !validLearningUUID(r.Header.Get(learningspace.Header)) {
			a.contentError(w, r, learningcontent.ErrInvalid)
			return
		}
		next.ServeHTTP(w, r)
	})
}
func (a *API) mountStudio(r chi.Router) {
	if a.learningContent == nil {
		return
	}
	read := r.With(a.requireScope("learning:read"), a.studioScope, a.responseReadPermit("content_redacted", privacy.OwnerLearning, privacy.OwnerKnowledge))
	write := r.With(a.requireScope("learning:write"), a.studioScope, a.responseReadPermit("content_redacted", privacy.OwnerLearning))
	read.Get("/v1/learning/studio/artifacts", a.studioList)
	read.Get("/v1/learning/studio/artifacts/{artifactID}", a.studioGet)
	read.Get("/v1/learning/studio/artifacts/{artifactID}/sections", a.studioSections)
	r.With(a.requireScope("learning:read"), a.requireScope("learning:write"), a.requireScope("knowledge:read"), a.studioScope, a.responseReadPermit("content_redacted", privacy.OwnerLearning, privacy.OwnerKnowledge)).Put("/v1/learning/studio/artifacts/{artifactID}/sections/{sectionID}", a.studioSectionSave)
	read.Get("/v1/learning/studio/artifacts/{artifactID}/revisions", a.studioHistory)
	read.Get("/v1/learning/studio/operations/{operationID}", a.studioOperation)
	write.Post("/v1/learning/studio/artifacts", a.studioSave)
	write.Put("/v1/learning/studio/artifacts/{artifactID}", a.studioSave)
	write.Post("/v1/learning/studio/generations", a.studioGenerate)
	read.Get("/v1/learning/studio/artifacts/{artifactID}/sources/{citationID}", a.studioCitation)
	read.Get("/v1/learning/studio/artifacts/{artifactID}/quiz", a.studioQuizGet)
	read.Get("/v1/learning/studio/quiz-operations/{operationID}", a.studioQuizOperation)
	write.Post("/v1/learning/studio/artifacts/{artifactID}/quiz/start", a.studioQuizStart)
	a.mountStudioExercises(r)
	a.mountStudioJobs(r)
	r.With(a.requireScope("learning:read"), a.requireScope("knowledge:read"), a.requireScope("knowledge:write"), a.studioScope, a.responseReadPermit("content_redacted", privacy.OwnerLearning, privacy.OwnerKnowledge)).Post("/v1/learning/studio/artifacts/{artifactID}/import-candidate", a.studioImportCandidate)
}

func (a *API) studioGenerate(w http.ResponseWriter, r *http.Request) {
	if _, ok := strictLearningQuery(w, r); !ok {
		return
	}
	var input mentorrun.StudioGenerate
	raw, err := readJSONBody(w, r, 64<<10)
	if err != nil {
		writeLearningDecodeFailure(w, r, err)
		return
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil {
		mentorFailure(w, r, mentorrun.ErrInvalid)
		return
	}
	for _, name := range []string{"operation_id", "privacy_generation", "kind", "goal_id", "goal_version", "artifact_id", "expected_version", "source_selection"} {
		value, ok := fields[name]
		if !ok || name != "goal_id" && name != "artifact_id" && bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			mentorFailure(w, r, mentorrun.ErrInvalid)
			return
		}
	}
	if value, ok := fields["mode"]; ok {
		var mode string
		if json.Unmarshal(value, &mode) != nil || mode != "direction" {
			mentorFailure(w, r, mentorrun.ErrInvalid)
			return
		}
	}
	if value, ok := fields["direction_version"]; ok && (bytes.Equal(bytes.TrimSpace(value), []byte("null")) || fields["mode"] == nil) {
		mentorFailure(w, r, mentorrun.ErrInvalid)
		return
	}
	if decodeJSONData(raw, &input) != nil || input.Validate() != nil {
		mentorFailure(w, r, mentorrun.ErrInvalid)
		return
	}
	if a.mentorRuns == nil {
		mentorFailure(w, r, mentorrun.ErrStorage)
		return
	}
	actor, _ := credentialFromContext(r.Context())
	receipt, err := a.mentorRuns.CreateStudio(r.Context(), actor, learningspace.Scope(r.Context()), input)
	if err != nil {
		mentorFailure(w, r, err)
		return
	}
	w.Header().Set("Location", "/v1/learning/runs/"+receipt.RunID)
	writeJSON(w, 202, receipt)
}
func (a *API) studioCitation(w http.ResponseWriter, r *http.Request) {
	q, ok := strictLearningQuery(w, r, "version")
	if !ok {
		return
	}
	version, err := strconv.ParseInt(q.Get("version"), 10, 64)
	if err != nil || version < 1 {
		mentorFailure(w, r, mentorrun.ErrInvalid)
		return
	}
	citationID := chi.URLParam(r, "citationID")
	// chi routes on RawPath when present; URL.Path is already decoded.
	if r.URL.RawPath != "" {
		citationID, err = url.PathUnescape(citationID)
		if err != nil {
			mentorFailure(w, r, learningcontent.ErrInvalid)
			return
		}
	}
	actor, _ := credentialFromContext(r.Context())
	result, err := a.learningContent.StudioGet(r.Context(), actor, chi.URLParam(r, "artifactID"), version)
	if err != nil {
		mentorFailure(w, r, err)
		return
	}
	for _, citation := range result.Body.Citations {
		if citation.ID == citationID {
			writeJSON(w, 200, citation)
			return
		}
	}
	if result.Body.Schema == 2 {
		citation, e := a.learningContent.StudioSectionCitation(r.Context(), actor, result.ArtifactID, result.Version, citationID)
		if e != nil {
			mentorFailure(w, r, e)
			return
		}
		writeJSON(w, 200, citation)
		return
	}
	mentorFailure(w, r, learningcontent.ErrNotFound)
}
func (a *API) studioList(w http.ResponseWriter, r *http.Request) {
	q, ok := strictLearningQuery(w, r, "cursor", "limit")
	if !ok {
		return
	}
	limit := 30
	var err error
	if q.Has("limit") {
		limit, err = strconv.Atoi(q.Get("limit"))
		if err != nil {
			a.contentError(w, r, learningcontent.ErrInvalid)
			return
		}
	}
	actor, _ := credentialFromContext(r.Context())
	result, err := a.learningContent.StudioList(r.Context(), actor, q.Get("cursor"), limit)
	if err != nil {
		mentorFailure(w, r, err)
		return
	}
	writeJSON(w, 200, result)
}
func (a *API) studioGet(w http.ResponseWriter, r *http.Request) {
	q, ok := strictLearningQuery(w, r, "version")
	if !ok {
		return
	}
	var version int64
	if q.Has("version") {
		var err error
		version, err = strconv.ParseInt(q.Get("version"), 10, 64)
		if err != nil || version < 1 {
			a.contentError(w, r, learningcontent.ErrInvalid)
			return
		}
	}
	actor, _ := credentialFromContext(r.Context())
	result, err := a.learningContent.StudioGet(r.Context(), actor, chi.URLParam(r, "artifactID"), version)
	if err != nil {
		mentorFailure(w, r, err)
		return
	}
	if !studioSchemaAccepted(w, r, result) {
		return
	}
	writeJSON(w, 200, result)
}
func (a *API) studioSave(w http.ResponseWriter, r *http.Request) {
	if _, ok := strictLearningQuery(w, r); !ok {
		return
	}
	var input learningcontent.StudioSave
	raw, err := readJSONBody(w, r, a.maxLearningRequestBody)
	if err != nil {
		writeLearningDecodeFailure(w, r, err)
		return
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil {
		a.contentError(w, r, learningcontent.ErrInvalid)
		return
	}
	for _, name := range []string{"operation_id", "expected_version", "privacy_generation", "kind", "goal_id", "title", "markdown"} {
		value, ok := fields[name]
		if !ok || name != "goal_id" && bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			a.contentError(w, r, learningcontent.ErrInvalid)
			return
		}
	}
	if err = decodeJSONData(raw, &input); err != nil {
		writeLearningDecodeFailure(w, r, err)
		return
	}
	actor, _ := credentialFromContext(r.Context())
	result, err := a.learningContent.StudioSave(r.Context(), actor, chi.URLParam(r, "artifactID"), input)
	if err != nil {
		mentorFailure(w, r, err)
		return
	}
	writeJSON(w, 200, result)
}
func (a *API) studioHistory(w http.ResponseWriter, r *http.Request) {
	if _, ok := strictLearningQuery(w, r); !ok {
		return
	}
	actor, _ := credentialFromContext(r.Context())
	result, err := a.learningContent.StudioHistory(r.Context(), actor, chi.URLParam(r, "artifactID"))
	if err != nil {
		mentorFailure(w, r, err)
		return
	}
	writeJSON(w, 200, map[string]any{"items": result})
}
func (a *API) studioOperation(w http.ResponseWriter, r *http.Request) {
	if _, ok := strictLearningQuery(w, r); !ok {
		return
	}
	actor, _ := credentialFromContext(r.Context())
	result, err := a.learningContent.StudioOperation(r.Context(), actor, chi.URLParam(r, "operationID"))
	if err != nil {
		mentorFailure(w, r, err)
		return
	}
	if !studioSchemaAccepted(w, r, result) {
		return
	}
	writeJSON(w, 200, result)
}
func (a *API) studioImportCandidate(w http.ResponseWriter, r *http.Request) {
	if _, ok := strictLearningQuery(w, r); !ok {
		return
	}
	var input struct {
		Version int64 `json:"version"`
	}
	if !a.decodeLearning(w, r, &input) {
		return
	}
	service, ok := a.knowledge.(interface {
		PrepareNoteSource(context.Context, knowledge.NoteSourceReader, string, int64) (knowledge.NoteSourceCandidate, error)
	})
	if !ok {
		writeError(w, r, 501, "studio_import_unavailable", "当前服务不支持笔记转来源")
		return
	}
	actor, _ := credentialFromContext(r.Context())
	result, err := service.PrepareNoteSource(r.Context(), func(ctx context.Context, id string, version int64) (knowledge.SavedNote, error) {
		return a.learningContent.StudioNoteSource(ctx, actor, id, version)
	}, chi.URLParam(r, "artifactID"), input.Version)
	if err != nil {
		if !contentFailure(w, r, err) {
			a.writeKnowledgeFailure(w, r, "studio_import_candidate", err)
		}
		return
	}
	writeJSON(w, 200, result)
}
