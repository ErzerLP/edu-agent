package httpapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/edu-agent/edu-agent/server/internal/mentorrun"
	"github.com/edu-agent/edu-agent/server/internal/privacy"
	"github.com/go-chi/chi/v5"
)

func (a *API) mountStudioJobs(r chi.Router) {
	read := r.With(a.requireScope("learning:read"), a.requireScope("knowledge:read"), a.studioScope, a.responseReadPermit("content_redacted", privacy.OwnerLearning, privacy.OwnerKnowledge, privacy.OwnerTutoring))
	write := r.With(a.requireScope("learning:read"), a.requireScope("learning:write"), a.requireScope("knowledge:read"), a.studioScope, a.responseReadPermit("content_redacted", privacy.OwnerLearning, privacy.OwnerKnowledge, privacy.OwnerTutoring))
	read.Get("/v1/learning/studio/focus", a.studioFocus)
	read.Get("/v1/learning/studio/jobs", a.studioJobs)
	read.Get("/v1/learning/studio/jobs/{jobID}", a.studioJob)
	read.Get("/v1/learning/studio/jobs/{jobID}/units", a.studioUnits)
	write.Post("/v1/learning/studio/jobs", a.studioJobCreate)
	write.Post("/v1/learning/studio/jobs/{jobID}/plan-messages", a.studioPlanCommand("message"))
	write.Patch("/v1/learning/studio/jobs/{jobID}/plan", a.studioPlanCommand("patch"))
	write.Post("/v1/learning/studio/jobs/{jobID}/confirm", a.studioPlanCommand("confirm"))
	write.Post("/v1/learning/studio/jobs/{jobID}/commands", a.studioPlanCommand("command"))
}

// Each command has a closed shape, even though the service shares a command
// type. Omitted versions and explicit null must not become Go zero values.
func decodeStudioJobRequest(w http.ResponseWriter, r *http.Request, target any, required, optional []string) bool {
	raw, err := readJSONBody(w, r, 64<<10)
	if err != nil {
		writeLearningDecodeFailure(w, r, err)
		return false
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil {
		mentorFailure(w, r, mentorrun.ErrInvalid)
		return false
	}
	allowed := make(map[string]bool, len(required)+len(optional))
	for _, name := range required {
		if _, ok := fields[name]; !ok {
			mentorFailure(w, r, mentorrun.ErrInvalid)
			return false
		}
		allowed[name] = true
	}
	for _, name := range optional {
		allowed[name] = true
	}
	for name, value := range fields {
		if !allowed[name] || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			mentorFailure(w, r, mentorrun.ErrInvalid)
			return false
		}
	}
	if decodeJSONData(raw, target) != nil {
		mentorFailure(w, r, mentorrun.ErrInvalid)
		return false
	}
	return true
}

func (a *API) studioJobCreate(w http.ResponseWriter, r *http.Request) {
	if _, ok := strictLearningQuery(w, r); !ok {
		return
	}
	var input mentorrun.StudioJobCreate
	if !decodeStudioJobRequest(w, r, &input,
		[]string{"operation_id", "privacy_generation", "kind", "source_selection", "direction_version"},
		[]string{"focus", "question_count", "difficulty", "artifact_id", "expected_artifact_version"}) {
		return
	}
	if input.Validate() != nil {
		mentorFailure(w, r, mentorrun.ErrInvalid)
		return
	}
	if a.mentorRuns == nil {
		mentorFailure(w, r, mentorrun.ErrStorage)
		return
	}
	actor, _ := credentialFromContext(r.Context())
	job, err := a.mentorRuns.CreateStudioJob(r.Context(), actor, input)
	if err != nil {
		mentorFailure(w, r, err)
		return
	}
	w.Header().Set("Location", "/v1/learning/studio/jobs/"+job.ID)
	writeJSON(w, http.StatusAccepted, job)
}

func (a *API) studioFocus(w http.ResponseWriter, r *http.Request) {
	if _, ok := strictLearningQuery(w, r); !ok {
		return
	}
	if a.mentorRuns == nil {
		mentorFailure(w, r, mentorrun.ErrStorage)
		return
	}
	actor, _ := credentialFromContext(r.Context())
	result, err := a.mentorRuns.StudioFocus(r.Context(), actor)
	if err != nil {
		mentorFailure(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (a *API) studioJobs(w http.ResponseWriter, r *http.Request) {
	query, ok := strictLearningQuery(w, r, "cursor", "limit")
	if !ok {
		return
	}
	limit := 30
	var err error
	if query.Has("limit") {
		limit, err = strconv.Atoi(query.Get("limit"))
	}
	if err != nil || limit < 1 || limit > 100 || len(query.Get("cursor")) > 1024 {
		mentorFailure(w, r, mentorrun.ErrInvalid)
		return
	}
	if a.mentorRuns == nil {
		mentorFailure(w, r, mentorrun.ErrStorage)
		return
	}
	actor, _ := credentialFromContext(r.Context())
	result, err := a.mentorRuns.StudioJobs(r.Context(), actor, query.Get("cursor"), limit)
	if err != nil {
		mentorFailure(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (a *API) studioJob(w http.ResponseWriter, r *http.Request) {
	if _, ok := strictLearningQuery(w, r); !ok {
		return
	}
	id := chi.URLParam(r, "jobID")
	if !validLearningUUID(id) {
		mentorFailure(w, r, mentorrun.ErrInvalid)
		return
	}
	if a.mentorRuns == nil {
		mentorFailure(w, r, mentorrun.ErrStorage)
		return
	}
	actor, _ := credentialFromContext(r.Context())
	result, err := a.mentorRuns.StudioJob(r.Context(), actor, id)
	if err != nil {
		mentorFailure(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (a *API) studioPlanCommand(action string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, ok := strictLearningQuery(w, r); !ok {
			return
		}
		id := chi.URLParam(r, "jobID")
		if !validLearningUUID(id) {
			mentorFailure(w, r, mentorrun.ErrInvalid)
			return
		}
		required := []string{"operation_id", "expected_version", "plan_revision"}
		var optional []string
		switch action {
		case "message":
			required = append(required, "text")
		case "patch":
			optional = []string{"delete_ids", "ordered_ids"}
		case "command":
			required = append(required, "kind")
		}
		var input mentorrun.StudioPlanCommand
		if !decodeStudioJobRequest(w, r, &input, required, optional) {
			return
		}
		command := action
		if action == "command" {
			command = input.Kind
		}
		if input.Validate(command) != nil || action != "command" && input.PlanRevision < 1 {
			mentorFailure(w, r, mentorrun.ErrInvalid)
			return
		}
		if a.mentorRuns == nil {
			mentorFailure(w, r, mentorrun.ErrStorage)
			return
		}
		actor, _ := credentialFromContext(r.Context())
		result, err := a.mentorRuns.ChangeStudioPlan(r.Context(), actor, id, command, input)
		if err != nil {
			mentorFailure(w, r, err)
			return
		}
		status := http.StatusOK
		if action == "message" {
			status = http.StatusAccepted
		}
		writeJSON(w, status, result)
	}
}
