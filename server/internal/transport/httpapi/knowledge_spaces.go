package httpapi

import (
	"context"
	"github.com/edu-agent/edu-agent/server/internal/knowledge"
	"github.com/edu-agent/edu-agent/server/internal/privacy"
	"github.com/go-chi/chi/v5"
	"net/http"
	"strings"
)

type knowledgeSpaces interface {
	SupportsKnowledgeScopes() bool
	Collections(context.Context, bool) ([]knowledge.Collection, error)
	ChangeCollection(context.Context, knowledge.CollectionCommand) (knowledge.Collection, error)
	FreezeScope(context.Context, knowledge.ScopeSnapshot) (knowledge.ScopeSnapshot, error)
	ReadScope(context.Context, string) (knowledge.ScopeSnapshot, error)
}

func collectionSelectionPath(path string) bool {
	if path == "/v1/knowledge/import-jobs" || strings.HasPrefix(path, "/v1/knowledge/import-jobs/") {
		return true
	}
	if strings.HasPrefix(path, "/v1/knowledge/imports/") {
		return true
	}
	return path == "/admin/api/knowledge" || path == "/v1/knowledge/imports" || path == "/v1/knowledge/retrievals" || strings.HasPrefix(path, "/v1/knowledge/revisions/")
}

func (a *API) mountKnowledgeSpaces(r chi.Router) {
	s, ok := a.knowledge.(knowledgeSpaces)
	if !ok || !s.SupportsKnowledgeScopes() {
		return
	}
	r.With(a.requireScope("knowledge:read"), a.responseReadPermit("content_redacted", privacy.OwnerKnowledge)).Get("/v1/knowledge/scopes/{scopeID}/tree", func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "scopeID")
		if _, err := s.ReadScope(r.Context(), id); err != nil {
			a.writeKnowledgeFailure(w, r, "read_scope", err)
			return
		}
		item, err := a.knowledge.Tree(r.Context(), id)
		if err != nil {
			a.writeKnowledgeFailure(w, r, "scope_tree", err)
			return
		}
		writeJSON(w, 200, item)
	})
	r.With(a.requireScope("knowledge:read"), a.responseReadPermit("content_redacted", privacy.OwnerKnowledge)).Get("/v1/knowledge/scopes/{scopeID}/export", func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "scopeID")
		if _, err := s.ReadScope(r.Context(), id); err != nil {
			a.writeKnowledgeFailure(w, r, "read_scope", err)
			return
		}
		item, err := a.knowledge.Export(r.Context(), id)
		if err != nil {
			a.writeKnowledgeFailure(w, r, "scope_export", err)
			return
		}
		writeJSON(w, 200, item)
	})
	r.With(a.requireScope("knowledge:read"), a.responseReadPermit("content_redacted", privacy.OwnerKnowledge)).Get("/v1/knowledge/collections", func(w http.ResponseWriter, r *http.Request) {
		items, err := s.Collections(r.Context(), r.URL.Query().Get("shared") == "true")
		if err != nil {
			a.writeKnowledgeFailure(w, r, "collections", err)
			return
		}
		writeJSON(w, 200, map[string]any{"items": items})
	})
	r.With(a.requireScope("knowledge:write"), a.requireScope("knowledge:approve"), a.responseReadPermit("privacy_clear_in_progress", privacy.OwnerKnowledge)).Post("/v1/knowledge/collections", func(w http.ResponseWriter, r *http.Request) {
		var c knowledge.CollectionCommand
		if err := decodeJSON(w, r, 16<<10, &c); err != nil {
			a.writeKnowledgeFailure(w, r, "collections", &knowledge.Error{Code: knowledge.CodeInvalidRequest})
			return
		}
		item, err := s.ChangeCollection(r.Context(), c)
		if err != nil {
			a.writeKnowledgeFailure(w, r, "collections", err)
			return
		}
		writeJSON(w, 200, item)
	})
	r.With(a.requireScope("knowledge:write"), a.responseReadPermit("privacy_clear_in_progress", privacy.OwnerKnowledge)).Post("/v1/knowledge/scopes", func(w http.ResponseWriter, r *http.Request) {
		var c knowledge.ScopeSnapshot
		if err := decodeJSON(w, r, 256<<10, &c); err != nil {
			a.writeKnowledgeFailure(w, r, "freeze_scope", &knowledge.Error{Code: knowledge.CodeInvalidRequest})
			return
		}
		item, err := s.FreezeScope(r.Context(), c)
		if err != nil {
			a.writeKnowledgeFailure(w, r, "freeze_scope", err)
			return
		}
		writeJSON(w, 200, item)
	})
	r.With(a.requireScope("knowledge:read"), a.responseReadPermit("content_redacted", privacy.OwnerKnowledge)).Get("/v1/knowledge/scopes/{scopeID}", func(w http.ResponseWriter, r *http.Request) {
		item, err := s.ReadScope(r.Context(), chi.URLParam(r, "scopeID"))
		if err != nil {
			a.writeKnowledgeFailure(w, r, "read_scope", err)
			return
		}
		writeJSON(w, 200, item)
	})
}

func scopedKnowledgePath(path string) bool {
	if path == "/v1/knowledge/import-jobs" || strings.HasPrefix(path, "/v1/knowledge/import-jobs/") {
		return true
	}
	if strings.HasPrefix(path, "/v1/knowledge/imports/") {
		return true
	}
	switch path {
	case "/admin/api/knowledge":
		return true
	case "/v1/knowledge/collections", "/v1/knowledge/scopes", "/v1/knowledge/imports", "/v1/knowledge/retrievals", "/v1/knowledge/revisions/head":
		return true
	}
	return len(path) > len("/v1/knowledge/revisions/") && path[:len("/v1/knowledge/revisions/")] == "/v1/knowledge/revisions/" || len(path) > len("/v1/knowledge/scopes/") && path[:len("/v1/knowledge/scopes/")] == "/v1/knowledge/scopes/"
}
