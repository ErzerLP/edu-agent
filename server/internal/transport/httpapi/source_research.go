package httpapi

import (
	"encoding/json"
	"fmt"
	"github.com/edu-agent/edu-agent/server/internal/learningspace"
	"github.com/edu-agent/edu-agent/server/internal/mentorrun"
	"github.com/edu-agent/edu-agent/server/internal/pdfsource"
	"github.com/edu-agent/edu-agent/server/internal/privacy"
	"github.com/edu-agent/edu-agent/server/internal/research"
	"github.com/go-chi/chi/v5"
	"net/http"
	"slices"
	"strconv"
	"time"
)

const sourceResearchPrefix = "/v1/learning/source-research"

func (a *API) mountSourceResearch(r chi.Router) {
	read := r.With(a.requireScope("learning:read"), a.mentorScope, a.responseReadPermit("content_redacted", privacy.OwnerIdentity, privacy.OwnerLearning))
	write := r.With(a.requireScope("learning:write"), a.mentorScope, a.responseReadPermit("content_redacted", privacy.OwnerIdentity, privacy.OwnerLearning))
	adopt := r.With(a.requireScope("learning:write"), a.requireScope("knowledge:read"), a.requireScope("knowledge:write"), a.requireScope("research:adopt"), a.mentorScope, a.responseReadPermit("content_redacted", privacy.OwnerIdentity, privacy.OwnerLearning, privacy.OwnerKnowledge))
	read.Get(sourceResearchPrefix+"/capabilities", func(w http.ResponseWriter, r *http.Request) {
		actor, _ := credentialFromContext(r.Context())
		v := a.settings.View()
		available := v.EffectiveMentor.Enabled && v.EffectiveMentor.Configured && v.Search.Enabled && v.Search.Configured && a.mentorRuns.CanSave()
		canImport := true
		for _, scope := range []string{"knowledge:read", "knowledge:write", "research:adopt"} {
			canImport = canImport && slices.Contains(actor.Scopes, scope)
		}
		writeJSON(w, 200, map[string]any{"available": available, "can_import": canImport, "limits": v.SourceResearch, "pdf": map[string]any{"max_wire_bytes": research.MaxWire, "max_decoded_bytes": research.MaxDecoded, "max_pages": pdfsource.MaxPages}})
	})
	r.With(a.requireScope("learning:write"), a.requireScope("knowledge:read"), a.mentorScope, a.responseReadPermit("content_redacted", privacy.OwnerIdentity, privacy.OwnerLearning)).Post(sourceResearchPrefix+"/jobs", func(w http.ResponseWriter, r *http.Request) {
		var c mentorrun.SourceResearchCreate
		if decodeJSON(w, r, 64<<10, &c) != nil {
			mentorFailure(w, r, mentorrun.ErrInvalid)
			return
		}
		actor, _ := credentialFromContext(r.Context())
		result, err := a.mentorRuns.CreateSourceResearch(r.Context(), actor, learningspace.Scope(r.Context()), c)
		sourceResearchResponse(w, r, 202, result, err)
	})
	read.Get(sourceResearchPrefix+"/jobs", func(w http.ResponseWriter, r *http.Request) {
		actor, _ := credentialFromContext(r.Context())
		limit, err := sourceResearchInt(r, "limit", 20)
		if err != nil {
			mentorFailure(w, r, err)
			return
		}
		items, next, err := a.mentorRuns.SourceResearchJobs(r.Context(), actor, learningspace.Scope(r.Context()), r.URL.Query().Get("cursor"), limit)
		sourceResearchResponse(w, r, 200, map[string]any{"items": items, "next_cursor": next}, err)
	})
	read.Get(sourceResearchPrefix+"/jobs/{jobID}", func(w http.ResponseWriter, r *http.Request) {
		actor, _ := credentialFromContext(r.Context())
		result, err := a.mentorRuns.SourceResearchJob(r.Context(), actor, learningspace.Scope(r.Context()), chi.URLParam(r, "jobID"))
		sourceResearchResponse(w, r, 200, result, err)
	})
	read.Get(sourceResearchPrefix+"/jobs/{jobID}/candidates", func(w http.ResponseWriter, r *http.Request) {
		actor, _ := credentialFromContext(r.Context())
		limit, err := sourceResearchInt(r, "limit", 20)
		if err != nil {
			mentorFailure(w, r, err)
			return
		}
		result, err := a.mentorRuns.SourceResearchCandidates(r.Context(), actor, learningspace.Scope(r.Context()), chi.URLParam(r, "jobID"), r.URL.Query().Get("cursor"), limit)
		sourceResearchResponse(w, r, 200, result, err)
	})
	read.Get(sourceResearchPrefix+"/jobs/{jobID}/reports/{revisionID}", func(w http.ResponseWriter, r *http.Request) {
		actor, _ := credentialFromContext(r.Context())
		after, err := sourceResearchInt(r, "after", 0)
		if err != nil {
			mentorFailure(w, r, err)
			return
		}
		result, err := a.mentorRuns.SourceResearchReport(r.Context(), actor, learningspace.Scope(r.Context()), chi.URLParam(r, "jobID"), chi.URLParam(r, "revisionID"), after, 5)
		sourceResearchResponse(w, r, 200, result, err)
	})
	write.Patch(sourceResearchPrefix+"/jobs/{jobID}/selection", func(w http.ResponseWriter, r *http.Request) {
		var c mentorrun.SourceResearchSelectionCommand
		if decodeJSON(w, r, 8192, &c) != nil {
			mentorFailure(w, r, mentorrun.ErrInvalid)
			return
		}
		actor, _ := credentialFromContext(r.Context())
		result, err := a.mentorRuns.SelectSourceResearch(r.Context(), actor, learningspace.Scope(r.Context()), chi.URLParam(r, "jobID"), c)
		sourceResearchResponse(w, r, 200, result, err)
	})
	write.Post(sourceResearchPrefix+"/jobs/{jobID}/commands", func(w http.ResponseWriter, r *http.Request) {
		var c mentorrun.SourceResearchCommand
		if decodeJSON(w, r, 4096, &c) != nil {
			mentorFailure(w, r, mentorrun.ErrInvalid)
			return
		}
		actor, _ := credentialFromContext(r.Context())
		result, err := a.mentorRuns.CommandSourceResearch(r.Context(), actor, learningspace.Scope(r.Context()), chi.URLParam(r, "jobID"), c)
		if c.Kind == "delete" && err == nil {
			w.WriteHeader(204)
			return
		}
		sourceResearchResponse(w, r, 200, result, err)
	})
	write.Post(sourceResearchPrefix+"/jobs/{jobID}/rounds", func(w http.ResponseWriter, r *http.Request) {
		var c mentorrun.SourceResearchMore
		if decodeJSON(w, r, 32<<10, &c) != nil {
			mentorFailure(w, r, mentorrun.ErrInvalid)
			return
		}
		actor, _ := credentialFromContext(r.Context())
		result, err := a.mentorRuns.MoreSourceResearch(r.Context(), actor, learningspace.Scope(r.Context()), chi.URLParam(r, "jobID"), c)
		sourceResearchResponse(w, r, 202, result, err)
	})
	read.Get(sourceResearchPrefix+"/jobs/{jobID}/reports", func(w http.ResponseWriter, r *http.Request) {
		before, err := sourceResearchInt(r, "before", 0)
		if err != nil {
			mentorFailure(w, r, err)
			return
		}
		actor, _ := credentialFromContext(r.Context())
		items, next, err := a.mentorRuns.SourceResearchReports(r.Context(), actor, learningspace.Scope(r.Context()), chi.URLParam(r, "jobID"), int64(before))
		sourceResearchResponse(w, r, 200, map[string]any{"items": items, "next_before": next}, err)
	})
	adopt.Post(sourceResearchPrefix+"/jobs/{jobID}/import-previews", func(w http.ResponseWriter, r *http.Request) {
		var c mentorrun.SourceResearchPreviewRequest
		if decodeJSON(w, r, 4096, &c) != nil {
			mentorFailure(w, r, mentorrun.ErrInvalid)
			return
		}
		actor, _ := credentialFromContext(r.Context())
		result, err := a.mentorRuns.PreviewSourceResearch(r.Context(), actor, learningspace.Scope(r.Context()), chi.URLParam(r, "jobID"), c)
		sourceResearchResponse(w, r, 200, result, err)
	})
	adopt.Post(sourceResearchPrefix+"/jobs/{jobID}/report-imports", func(w http.ResponseWriter, r *http.Request) {
		var c mentorrun.SourceResearchReportImport
		if decodeJSON(w, r, 4096, &c) != nil {
			mentorFailure(w, r, mentorrun.ErrInvalid)
			return
		}
		actor, _ := credentialFromContext(r.Context())
		result, err := a.mentorRuns.ImportSourceResearchReport(r.Context(), actor, learningspace.Scope(r.Context()), chi.URLParam(r, "jobID"), c)
		sourceResearchResponse(w, r, 200, result, err)
	})
	read.Get(sourceResearchPrefix+"/jobs/{jobID}/imports", func(w http.ResponseWriter, r *http.Request) {
		actor, _ := credentialFromContext(r.Context())
		result, err := a.mentorRuns.SourceResearchImports(r.Context(), actor, learningspace.Scope(r.Context()), chi.URLParam(r, "jobID"), r.URL.Query().Get("preview_id"))
		sourceResearchResponse(w, r, 200, result, err)
	})
	r.With(a.requireScope("learning:read"), a.mentorScope).Get(sourceResearchPrefix+"/jobs/{jobID}/events", a.sourceResearchEvents)
}
func sourceResearchResponse(w http.ResponseWriter, r *http.Request, status int, result any, err error) {
	if err != nil {
		mentorFailure(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, status, result)
}
func sourceResearchInt(r *http.Request, key string, fallback int) (int, error) {
	raw := r.URL.Query().Get(key)
	if raw == "" {
		return fallback, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return 0, mentorrun.ErrInvalid
	}
	return n, nil
}
func (a *API) sourceResearchEvents(w http.ResponseWriter, r *http.Request) {
	actor, _ := credentialFromContext(r.Context())
	a.mentorStreamsMu.Lock()
	if a.mentorStreams[actor.Device.ID] >= 4 {
		a.mentorStreamsMu.Unlock()
		mentorFailure(w, r, mentorrun.ErrLimit)
		return
	}
	a.mentorStreams[actor.Device.ID]++
	a.mentorStreamsMu.Unlock()
	defer func() {
		a.mentorStreamsMu.Lock()
		a.mentorStreams[actor.Device.ID]--
		if a.mentorStreams[actor.Device.ID] == 0 {
			delete(a.mentorStreams, actor.Device.ID)
		}
		a.mentorStreamsMu.Unlock()
	}()
	after, err := sourceResearchInt(r, "after", 0)
	if err != nil {
		mentorFailure(w, r, err)
		return
	}
	controller := http.NewResponseController(w)
	started := false
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		if a.webUI.Enabled {
			if cookie, e := r.Cookie(a.webCookieName()); e == nil {
				principal, e := a.webUI.Identity.AuthenticateWeb(r.Context(), cookie.Value)
				if e != nil || principal.Credential.TokenID != actor.TokenID {
					return
				}
			}
		}
		// Each flush reacquires a privacy permit; idle SSE never holds up erasure.
		permit, e := a.readPermits.Acquire(r.Context(), privacy.OwnerIdentity, privacy.OwnerLearning)
		if e != nil {
			return
		}
		events, e := a.mentorRuns.SourceResearchEvents(permit.Context(), actor, learningspace.Scope(r.Context()), chi.URLParam(r, "jobID"), int64(after))
		if e != nil {
			permit.Release()
			if !started {
				mentorFailure(w, r, e)
			}
			return
		}
		if !started {
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("X-Accel-Buffering", "no")
			started = true
		}
		_ = controller.SetWriteDeadline(time.Now().Add(5 * time.Second))
		e = permit.CommitResponse(func() {
			for _, event := range events {
				raw, _ := json.Marshal(event)
				if _, e = fmt.Fprintf(w, "id: %d\nevent: research\ndata: %s\n\n", event.Seq, raw); e != nil {
					return
				}
				after = int(event.Seq)
			}
			if _, e = fmt.Fprint(w, ": progress\n\n"); e != nil {
				return
			}
			_ = controller.Flush()
		})
		permit.Release()
		if e != nil {
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
		}
	}
}
