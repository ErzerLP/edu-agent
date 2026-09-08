package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	space "github.com/edu-agent/edu-agent/server/internal/learningspace"
	"github.com/edu-agent/edu-agent/server/internal/privacy"
	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type LearningSpaceService interface {
	Get(context.Context, string) (space.Space, error)
	List(context.Context, space.Query) (space.Page, error)
	Mutate(context.Context, string, string, space.Command) (space.Space, error)
}

func (a *API) mountLearningSpaces(r chi.Router) {
	r.With(a.requireScope("learning:read")).Get("/v1/learning-spaces/capabilities", func(w http.ResponseWriter, r *http.Request) {
		if a.learningSpaces == nil {
			spaceFailure(w, r, &space.Error{Code: "learning_spaces_unsupported"})
			return
		}
		writeJSON(w, 200, map[string]any{"version": 1, "default_space_id": space.DefaultID, "legacy_scope": "fixed_default", "modules": map[string]string{"knowledge": "default_only", "learning": "default_only", "tutoring": "default_only", "memory": "default_only"}})
	})
	if a.learningSpaces == nil {
		return
	}
	r.With(a.requireScope("learning:read"), a.responseReadPermit("content_redacted", privacy.OwnerLearning)).Get("/v1/learning-spaces", a.listLearningSpaces)
	r.With(a.requireScope("learning:read"), a.responseReadPermit("content_redacted", privacy.OwnerLearning)).Get("/v1/learning-spaces/{spaceID}", a.getLearningSpace)
	r.With(a.requireScope("learning:write"), a.responseReadPermit("privacy_clear_in_progress", privacy.OwnerLearning)).Post("/v1/learning-spaces", a.mutateLearningSpace)
	r.With(a.requireScope("learning:write"), a.responseReadPermit("privacy_clear_in_progress", privacy.OwnerLearning)).Put("/v1/learning-spaces/{spaceID}", a.mutateLearningSpace)
}
func spaceFailure(w http.ResponseWriter, r *http.Request, err error) {
	code := "internal_error"
	status := 500
	var domain *space.Error
	var pg *pgconn.PgError
	if errors.As(err, &domain) {
		code = domain.Code
	}
	if errors.As(err, &pg) && (pg.Message == "privacy_clear_in_progress" || pg.Message == "content_redacted" || pg.Message == "learning_space_archived") {
		code = pg.Message
	}
	if p := privacy.ErrorCode(err); p != "" {
		code = p
	}
	switch code {
	case "invalid_learning_space":
		status = 400
	case "learning_space_not_found":
		status = 404
	case "learning_space_archived", "version_conflict", "idempotency_conflict":
		status = 409
	case "learning_spaces_unsupported", "learning_space_module_unavailable":
		status = 501
	case "privacy_clear_in_progress", "content_redacted":
		status = 503
	}
	writeError(w, r, status, code, "Learning space request could not be completed: "+code)
}
func (a *API) listLearningSpaces(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit := 50
	if q.Has("limit") {
		var err error
		limit, err = strconv.Atoi(q.Get("limit"))
		if err != nil {
			spaceFailure(w, r, space.Invalid())
			return
		}
	}
	page, err := a.learningSpaces.List(r.Context(), space.Query{Search: q.Get("search"), Status: q.Get("status"), Cursor: q.Get("cursor"), Limit: limit})
	if err != nil {
		spaceFailure(w, r, err)
		return
	}
	writeJSON(w, 200, page)
}
func (a *API) getLearningSpace(w http.ResponseWriter, r *http.Request) {
	item, err := a.learningSpaces.Get(r.Context(), chi.URLParam(r, "spaceID"))
	if err != nil {
		spaceFailure(w, r, err)
		return
	}
	writeJSON(w, 200, item)
}
func (a *API) mutateLearningSpace(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 16<<10))
	if err != nil || rejectDuplicateJSONKeys(raw) != nil {
		spaceFailure(w, r, space.Invalid())
		return
	}
	var c space.Command
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil {
		spaceFailure(w, r, space.Invalid())
		return
	}
	for _, key := range []string{"operation_id", "expected_version", "name", "description", "status"} {
		value, ok := fields[key]
		if !ok || string(value) == "null" {
			spaceFailure(w, r, space.Invalid())
			return
		}
	}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&c) != nil {
		spaceFailure(w, r, space.Invalid())
		return
	}
	actor, _ := credentialFromContext(r.Context())
	item, err := a.learningSpaces.Mutate(r.Context(), actor.Device.ID, chi.URLParam(r, "spaceID"), c)
	if err != nil {
		spaceFailure(w, r, err)
		return
	}
	writeJSON(w, 200, item)
}
func legacyBusinessPath(path string) bool {
	for _, p := range []string{"/v1/knowledge/", "/v1/learning/", "/v1/tutoring/", "/v1/memory/"} {
		if strings.HasPrefix(path, p) {
			return true
		}
	}
	return false
}
func (a *API) resolveLearningSpace(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Has("learning_space_id") || r.URL.Query().Has("space_id") {
			spaceFailure(w, r, space.Invalid())
			return
		}
		values, present := r.Header[http.CanonicalHeaderKey(space.Header)]
		id := space.DefaultID
		if present {
			if len(values) != 1 || !space.ValidID(values[0]) {
				spaceFailure(w, r, space.Invalid())
				return
			}
			id = values[0]
		}
		business := legacyBusinessPath(r.URL.Path)
		if present || business {
			if a.learningSpaces == nil {
				if present {
					spaceFailure(w, r, &space.Error{Code: "learning_spaces_unsupported"})
					return
				}
			} else {
				item, err := a.learningSpaces.Get(r.Context(), id)
				if err != nil {
					spaceFailure(w, r, err)
					return
				}
				if business {
					if item.Status == "archived" && r.Method != http.MethodGet && r.Method != http.MethodHead && r.URL.Path != "/v1/knowledge/retrievals" {
						spaceFailure(w, r, &space.Error{Code: "learning_space_archived"})
						return
					}
					if id != space.DefaultID {
						spaceFailure(w, r, &space.Error{Code: "learning_space_module_unavailable"})
						return
					}
				}
			}
		}
		ctx, _ := space.WithScope(r.Context(), id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
func (a *API) rejectMCPSpace(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := r.Header[http.CanonicalHeaderKey(space.Header)]; ok || r.URL.Query().Has("learning_space_id") || r.URL.Query().Has("space_id") {
			spaceFailure(w, r, &space.Error{Code: "learning_spaces_unsupported"})
			return
		}
		next.ServeHTTP(w, r)
	})
}
