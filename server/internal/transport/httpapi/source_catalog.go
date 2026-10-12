package httpapi

import (
	"context"
	"github.com/edu-agent/edu-agent/server/internal/knowledge"
	"github.com/edu-agent/edu-agent/server/internal/privacy"
	"github.com/go-chi/chi/v5"
	"net/http"
	"strconv"
)

func (a *API) mountSourceCatalog(r chi.Router) {
	catalog, ok := a.knowledge.(interface {
		Sources(context.Context, knowledge.SourceQuery) (knowledge.SourcePage, error)
		Source(context.Context, string, string) (knowledge.SourceDetail, error)
	})
	if !ok {
		return
	}
	r.With(a.requireScope("knowledge:read"), a.responseReadPermit("content_redacted", privacy.OwnerKnowledge)).Get("/v1/knowledge/sources", func(w http.ResponseWriter, r *http.Request) {
		q := knowledge.SourceQuery{Cursor: r.URL.Query().Get("cursor")}
		if raw := r.URL.Query().Get("limit"); raw != "" {
			n, err := strconv.Atoi(raw)
			if err != nil || n < 1 {
				a.writeKnowledgeFailure(w, r, "sources", &knowledge.Error{Code: knowledge.CodeInvalidRequest})
				return
			}
			q.Limit = n
		}
		page, err := catalog.Sources(r.Context(), q)
		if err != nil {
			a.writeKnowledgeFailure(w, r, "sources", err)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, 200, page)
	})
	r.With(a.requireScope("knowledge:read"), a.responseReadPermit("content_redacted", privacy.OwnerKnowledge)).Get("/v1/knowledge/sources/{revisionID}/documents/{documentRevisionID}", func(w http.ResponseWriter, r *http.Request) {
		detail, err := catalog.Source(r.Context(), chi.URLParam(r, "revisionID"), chi.URLParam(r, "documentRevisionID"))
		if err != nil {
			a.writeKnowledgeFailure(w, r, "source", err)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, 200, detail)
	})
}
