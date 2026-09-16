package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/learningspace"
	"github.com/edu-agent/edu-agent/server/internal/mentorrun"
	"github.com/go-chi/chi/v5"
)

func mentorPath(path string) bool {
	return strings.HasPrefix(path, "/v1/learning/runs/") || strings.HasPrefix(path, "/v1/learning/operations/") || strings.HasPrefix(path, "/v1/learning/goals/") && (strings.HasSuffix(path, "/runs") || strings.HasSuffix(path, "/research"))
}

func (a *API) mountMentorRuns(router chi.Router) {
	if a.mentorRuns == nil {
		return
	}
	router.With(a.requireScope("learning:write"), a.mentorScope).Post("/v1/learning/goals/{goalID}/runs", a.mentorCreate)
	router.With(a.requireScope("learning:read"), a.mentorScope).Get("/v1/learning/goals/{goalID}/runs", a.mentorCurrent)
	router.With(a.requireScope("learning:read"), a.mentorScope).Get("/v1/learning/runs/{runID}", a.mentorSnapshot)
	router.With(a.requireScope("learning:read"), a.mentorScope).Get("/v1/learning/runs/{runID}/events", a.mentorEvents)
	router.With(a.requireScope("learning:write"), a.mentorScope).Post("/v1/learning/runs/{runID}/commands", a.mentorCommand)
	router.With(a.requireScope("learning:read"), a.mentorScope).Get("/v1/learning/operations/{operationID}", a.mentorOperation)
	router.With(a.requireScope("learning:read"), a.mentorScope).Get("/v1/learning/goals/{goalID}/research", a.mentorCurrent)
	router.With(a.requireScope("learning:read"), a.mentorScope).Get("/v1/learning/runs/{runID}/sources", a.researchSources)
	router.With(a.requireScope("learning:read"), a.mentorScope).Get("/v1/learning/runs/{runID}/sources/{sourceID}", a.researchSource)
	router.With(a.requireScope("learning:write"), a.mentorScope).Post("/v1/learning/runs/{runID}/sources/{sourceID}/decisions", a.researchDecision)
}

func (a *API) mentorScope(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if len(r.Header.Values(learningspace.Header)) != 1 {
			mentorFailure(w, r, mentorrun.ErrInvalid)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

func mentorFailure(w http.ResponseWriter, r *http.Request, err error) {
	status, code := 503, "run_unavailable"
	for _, candidate := range []error{mentorrun.ErrInvalid, mentorrun.ErrConflict, mentorrun.ErrOperation, mentorrun.ErrNotFound, mentorrun.ErrForbidden, mentorrun.ErrInactive, mentorrun.ErrResync, mentorrun.ErrStorage, mentorrun.ErrLimit, mentorrun.ErrModel} {
		if errors.Is(err, candidate) {
			code = candidate.Error()
			break
		}
	}
	switch code {
	case "invalid_run_request":
		status = 400
	case "version_conflict", "idempotency_conflict", "run_context_changed", "resync_required":
		status = 409
	case "run_not_found":
		status = 404
	case "run_forbidden":
		status = 403
	case "run_storage_limit":
		status = 429
	}
	writeError(w, r, status, code, "导师运行请求未完成："+code)
}

func (a *API) mentorCreate(w http.ResponseWriter, r *http.Request) {
	var command mentorrun.Create
	raw, decodeErr := readJSONBody(w, r, 24<<10)
	var fields map[string]json.RawMessage
	if decodeErr != nil || decodeJSONData(raw, &command) != nil || json.Unmarshal(raw, &fields) != nil || (string(fields["save"]) != "true" && string(fields["save"]) != "false") {
		mentorFailure(w, r, mentorrun.ErrInvalid)
		return
	}
	actor, _ := credentialFromContext(r.Context())
	receipt, err := a.mentorRuns.Create(r.Context(), actor, learningspace.Scope(r.Context()), chi.URLParam(r, "goalID"), command)
	if err != nil {
		mentorFailure(w, r, err)
		return
	}
	w.Header().Set("Location", "/v1/learning/runs/"+receipt.RunID)
	writeJSON(w, 202, receipt)
}

func (a *API) mentorCurrent(w http.ResponseWriter, r *http.Request) {
	actor, _ := credentialFromContext(r.Context())
	space := learningspace.Scope(r.Context())
	kind := "mentor"
	if strings.HasSuffix(r.URL.Path, "/research") {
		kind = "research"
	}
	id, err := a.mentorRuns.Current(r.Context(), actor, space, chi.URLParam(r, "goalID"), kind)
	if err != nil {
		mentorFailure(w, r, err)
		return
	}
	response := map[string]any{"run": nil, "save_available": a.mentorRuns.CanSave()}
	if id == "" {
		writeJSON(w, 200, response)
		return
	}
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(5 * time.Second))
	started := false
	err = a.mentorRuns.ReadSnapshot(r.Context(), actor, space, id, func(snapshot mentorrun.Snapshot) error {
		response["run"] = snapshot
		started = true
		writeJSON(w, 200, response)
		return http.NewResponseController(w).Flush()
	})
	if err != nil && !started {
		mentorFailure(w, r, err)
	}
}

func (a *API) mentorSnapshot(w http.ResponseWriter, r *http.Request) {
	actor, _ := credentialFromContext(r.Context())
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(5 * time.Second))
	started := false
	err := a.mentorRuns.ReadSnapshot(r.Context(), actor, learningspace.Scope(r.Context()), chi.URLParam(r, "runID"), func(snapshot mentorrun.Snapshot) error {
		started = true
		writeJSON(w, 200, snapshot)
		return http.NewResponseController(w).Flush()
	})
	if err != nil && !started {
		mentorFailure(w, r, err)
	}
}

func (a *API) mentorCommand(w http.ResponseWriter, r *http.Request) {
	var command mentorrun.Command
	if decodeJSON(w, r, 16<<10, &command) != nil {
		mentorFailure(w, r, mentorrun.ErrInvalid)
		return
	}
	actor, _ := credentialFromContext(r.Context())
	receipt, err := a.mentorRuns.Command(r.Context(), actor, learningspace.Scope(r.Context()), chi.URLParam(r, "runID"), command)
	if err != nil {
		mentorFailure(w, r, err)
		return
	}
	writeJSON(w, 202, receipt)
}

func (a *API) mentorOperation(w http.ResponseWriter, r *http.Request) {
	actor, _ := credentialFromContext(r.Context())
	receipt, err := a.mentorRuns.Operation(r.Context(), actor, learningspace.Scope(r.Context()), chi.URLParam(r, "operationID"))
	if err != nil {
		mentorFailure(w, r, err)
		return
	}
	writeJSON(w, 200, receipt)
}

func (a *API) mentorEvents(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	if len(query) != 1 || len(query["after"]) != 1 {
		mentorFailure(w, r, mentorrun.ErrInvalid)
		return
	}
	after, err := strconv.ParseInt(query.Get("after"), 10, 64)
	if err != nil || after < 0 {
		mentorFailure(w, r, mentorrun.ErrInvalid)
		return
	}
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
	heartbeat, timeout := a.mentorHeartbeat, a.mentorWriteTimeout
	if heartbeat <= 0 {
		heartbeat = 10 * time.Second
	}
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	controller := http.NewResponseController(w)
	started := false
	last := time.Time{}
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
		err = a.mentorRuns.Events(r.Context(), actor, learningspace.Scope(r.Context()), chi.URLParam(r, "runID"), after, func(events []mentorrun.Event) error {
			if started && len(events) == 0 && time.Since(last) < heartbeat {
				return nil
			}
			if err := controller.SetWriteDeadline(time.Now().Add(timeout)); err != nil {
				return err
			}
			if !started {
				w.Header().Set("Content-Type", "text/event-stream")
				w.Header().Set("X-Accel-Buffering", "no")
				w.WriteHeader(200)
				started = true
			}
			if len(events) == 0 {
				if _, e := fmt.Fprint(w, ": heartbeat\n\n"); e != nil {
					return e
				}
			}
			for _, event := range events {
				raw, e := json.Marshal(event)
				if e != nil {
					return e
				}
				if _, e = fmt.Fprintf(w, "id: %d\nevent: run\ndata: %s\n\n", event.Seq, raw); e != nil {
					return e
				}
				after = event.Seq
			}
			last = time.Now()
			return controller.Flush()
		})
		if err != nil {
			if !started {
				mentorFailure(w, r, err)
			}
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
		}
	}
}
