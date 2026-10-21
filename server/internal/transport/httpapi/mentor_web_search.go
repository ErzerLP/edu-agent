package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/edu-agent/edu-agent/server/internal/learningspace"
	"github.com/edu-agent/edu-agent/server/internal/mentorrun"
	"github.com/edu-agent/edu-agent/server/internal/privacy"
	"github.com/edu-agent/edu-agent/server/internal/research"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/jackc/pgx/v5/pgconn"
)

func (a *API) mountMentorWeb(r chi.Router) {
	read := r.With(a.requireScope("learning:read"), a.mentorScope, a.responseReadPermit("content_redacted", privacy.OwnerIdentity, privacy.OwnerLearning))
	read.Get("/v1/learning/web-documents/{documentID}", a.webDocument)
	read.Get("/v1/learning/web-documents/{documentID}/pages/{page}", a.webDocumentPage)
	r.With(a.requireScope("learning:write"), a.mentorScope).Patch("/v1/learning/conversations/{conversationID}/web-search", a.webSearchPreference)
	write := r.With(a.requireScope("learning:write"), a.requireScope("knowledge:read"), a.requireScope("knowledge:write"), a.requireScope("research:adopt"), a.mentorScope, a.responseReadPermit("content_redacted", privacy.OwnerIdentity, privacy.OwnerLearning, privacy.OwnerKnowledge))
	write.Post("/v1/learning/web-import-previews", a.webImportPreview)
	write.Post("/v1/learning/web-imports", a.webImport)
}
func (a *API) webSearchPreference(w http.ResponseWriter, r *http.Request) {
	var c struct {
		OperationID     string `json:"operation_id"`
		ExpectedVersion int64  `json:"expected_web_search_version"`
		Enabled         *bool  `json:"enabled"`
	}
	if decodeJSON(w, r, 4096, &c) != nil || c.Enabled == nil {
		mentorFailure(w, r, mentorrun.ErrInvalid)
		return
	}
	actor, _ := credentialFromContext(r.Context())
	result, err := a.mentorRuns.ChangeWebSearch(r.Context(), actor, learningspace.Scope(r.Context()), chi.URLParam(r, "conversationID"), mentorrun.WebSearchCommand{OperationID: c.OperationID, ExpectedVersion: c.ExpectedVersion, Enabled: *c.Enabled})
	if err != nil {
		mentorFailure(w, r, err)
		return
	}
	writeJSON(w, 200, result)
}
func (a *API) webDocument(w http.ResponseWriter, r *http.Request) {
	actor, _ := credentialFromContext(r.Context())
	err := a.mentorRuns.ReadWebDocument(r.Context(), actor, learningspace.Scope(r.Context()), chi.URLParam(r, "documentID"), r.URL.Query().Get("cursor"), func(d research.WebDocument) error { writeJSON(w, 200, d); return nil })
	if err != nil {
		mentorFailure(w, r, err)
	}
}
func (a *API) webDocumentPage(w http.ResponseWriter, r *http.Request) {
	page, err := strconv.Atoi(chi.URLParam(r, "page"))
	if err != nil {
		mentorFailure(w, r, mentorrun.ErrInvalid)
		return
	}
	actor, _ := credentialFromContext(r.Context())
	err = a.mentorRuns.ReadWebPage(r.Context(), actor, learningspace.Scope(r.Context()), chi.URLParam(r, "documentID"), page, func(png []byte) error {
		w.Header().Set("Content-Type", "image/png")
		_, err := w.Write(png)
		return err
	})
	if err != nil {
		mentorFailure(w, r, err)
	}
}
func (a *API) webImportPreview(w http.ResponseWriter, r *http.Request) {
	var c mentorrun.WebPreviewRequest
	if decodeJSON(w, r, 8192, &c) != nil {
		mentorFailure(w, r, mentorrun.ErrInvalid)
		return
	}
	actor, _ := credentialFromContext(r.Context())
	result, err := a.mentorRuns.PreviewWebImport(r.Context(), actor, learningspace.Scope(r.Context()), c)
	if err != nil {
		mentorFailure(w, r, err)
		return
	}
	writeJSON(w, 200, result)
}
func (a *API) webImport(w http.ResponseWriter, r *http.Request) {
	var c mentorrun.WebImportRequest
	if decodeJSON(w, r, 8192, &c) != nil {
		mentorFailure(w, r, mentorrun.ErrInvalid)
		return
	}
	actor, _ := credentialFromContext(r.Context())
	result, err := a.mentorRuns.ImportWebDocument(r.Context(), actor, learningspace.Scope(r.Context()), c)
	if err != nil {
		a.writeWebImportFailure(w, r, err)
		return
	}
	writeJSON(w, 200, result)
}

func (a *API) writeWebImportFailure(w http.ResponseWriter, r *http.Request, err error) {
	category := "internal"
	var database *pgconn.PgError
	switch {
	case errors.Is(err, context.Canceled):
		category = "cancelled"
	case errors.Is(err, context.DeadlineExceeded):
		category = "timeout"
	case errors.As(err, &database):
		category = "database"
		switch {
		case database.Code == "40001" || database.Code == "40P01" || database.Code == "55P03":
			category = "database_transaction_conflict"
		case strings.HasPrefix(database.Code, "23"):
			category = "database_constraint"
		case strings.HasPrefix(database.Code, "08") || strings.HasPrefix(database.Code, "53") || strings.HasPrefix(database.Code, "57"):
			category = "database_unavailable"
		}
	default:
		for _, known := range []error{mentorrun.ErrInvalid, mentorrun.ErrConflict, mentorrun.ErrOperation, mentorrun.ErrNotFound, mentorrun.ErrForbidden, mentorrun.ErrInactive, mentorrun.ErrStorage} {
			if errors.Is(err, known) {
				category = known.Error()
				break
			}
		}
	}
	// Database errors can contain source titles, body text and SQL parameters.
	// Only fixed categories and the request ID belong in application logs.
	a.logger.WarnContext(r.Context(), "web import failed", "request_id", middleware.GetReqID(r.Context()), "error_category", category)
	mentorFailure(w, r, err)
}
