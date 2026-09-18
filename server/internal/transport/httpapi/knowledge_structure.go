package httpapi

import (
	"context"
	"net/http"
	"strconv"

	"github.com/edu-agent/edu-agent/server/internal/knowledge"
	"github.com/edu-agent/edu-agent/server/internal/privacy"
	"github.com/go-chi/chi/v5"
)

type knowledgeStructure interface {
	SupportsStructure() bool
	ReadStructure(context.Context, knowledge.StructureQuery) (knowledge.StructurePage, error)
	ReadConcept(context.Context, string, string) (knowledge.StructureNode, error)
	CreateStructureProposal(context.Context, knowledge.StructureCommand) (knowledge.StructureProposal, error)
	ReadStructureProposal(context.Context, string) (knowledge.StructureProposal, error)
	ListStructureProposals(context.Context, string, int) (knowledge.StructureProposalPage, error)
	DecideStructureProposal(context.Context, string, knowledge.StructureDecision) (knowledge.StructureProposal, error)
}

func (a *API) mountKnowledgeStructure(r chi.Router) {
	s, ok := a.knowledge.(knowledgeStructure)
	r.With(a.requireScope("knowledge:read")).Get("/v1/knowledge/structure/capabilities", func(w http.ResponseWriter, r *http.Request) {
		actor, _ := credentialFromContext(r.Context())
		has := func(scope string) bool {
			for _, v := range actor.Scopes {
				if v == scope {
					return true
				}
			}
			return false
		}
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, 200, map[string]any{"protocol_version": 1, "available": ok && s.SupportsStructure(), "can_propose": has("knowledge:write"), "can_decide": has("knowledge:approve"), "max_nodes": 100, "max_edits": 20})
	})
	if !ok || !s.SupportsStructure() {
		return
	}
	r = r.With(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Cache-Control", "no-store")
			next.ServeHTTP(w, r)
		})
	})
	read := r.With(a.requireScope("knowledge:read"), a.responseReadPermit("content_redacted", privacy.OwnerKnowledge, privacy.OwnerLearning))
	read.Get("/v1/knowledge/structure", func(w http.ResponseWriter, r *http.Request) {
		q, ok := strictLearningQuery(w, r, "goal_id", "root_id", "search", "cursor", "limit")
		if !ok {
			return
		}
		limit := 30
		var err error
		if q.Has("limit") {
			limit, err = strconv.Atoi(q.Get("limit"))
			if err != nil {
				a.writeKnowledgeMaintenanceFailure(w, r, "structure", &knowledge.Error{Code: knowledge.CodeInvalidRequest})
				return
			}
		}
		v, err := s.ReadStructure(r.Context(), knowledge.StructureQuery{GoalID: q.Get("goal_id"), RootID: q.Get("root_id"), Search: q.Get("search"), Cursor: q.Get("cursor"), Limit: limit})
		if err != nil {
			a.writeKnowledgeMaintenanceFailure(w, r, "structure", err)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, 200, v)
	})
	read.Get("/v1/knowledge/structure/concepts/{conceptID}", func(w http.ResponseWriter, r *http.Request) {
		q, ok := strictLearningQuery(w, r, "revision_id")
		if !ok {
			return
		}
		v, err := s.ReadConcept(r.Context(), chi.URLParam(r, "conceptID"), q.Get("revision_id"))
		if err != nil {
			a.writeKnowledgeMaintenanceFailure(w, r, "concept", err)
			return
		}
		writeJSON(w, 200, v)
	})
	read.Get("/v1/knowledge/structure/proposals", func(w http.ResponseWriter, r *http.Request) {
		q, ok := strictLearningQuery(w, r, "cursor", "limit")
		if !ok {
			return
		}
		limit := 20
		var err error
		if q.Has("limit") {
			limit, err = strconv.Atoi(q.Get("limit"))
			if err != nil {
				a.writeKnowledgeMaintenanceFailure(w, r, "structure_proposals", &knowledge.Error{Code: knowledge.CodeInvalidRequest})
				return
			}
		}
		v, err := s.ListStructureProposals(r.Context(), q.Get("cursor"), limit)
		if err != nil {
			a.writeKnowledgeMaintenanceFailure(w, r, "structure_proposals", err)
			return
		}
		writeJSON(w, 200, v)
	})
	read.Get("/v1/knowledge/structure/proposals/{proposalID}", func(w http.ResponseWriter, r *http.Request) {
		v, err := s.ReadStructureProposal(r.Context(), chi.URLParam(r, "proposalID"))
		if err != nil {
			a.writeKnowledgeMaintenanceFailure(w, r, "structure_proposal", err)
			return
		}
		writeJSON(w, 200, v)
	})
	r.With(a.requireScope("knowledge:write"), a.responseReadPermit("privacy_clear_in_progress", privacy.OwnerKnowledge, privacy.OwnerLearning)).Post("/v1/knowledge/structure/proposals", func(w http.ResponseWriter, r *http.Request) {
		var c knowledge.StructureCommand
		if _, ok := strictLearningQuery(w, r); !ok || !a.decodeKnowledgeMaintenanceRequest(w, r, &c) {
			return
		}
		actor, _ := credentialFromContext(r.Context())
		c.ActorDeviceID = actor.Device.ID
		if c.Kind == "compensate" {
			allowed := false
			for _, scope := range actor.Scopes {
				if scope == "knowledge:approve" {
					allowed = true
				}
			}
			if !allowed {
				writeError(w, r, 403, "scope_required", "补偿需要知识审批权限")
				return
			}
		}
		v, err := s.CreateStructureProposal(r.Context(), c)
		if err != nil {
			a.writeKnowledgeMaintenanceFailure(w, r, "create_structure_proposal", err)
			return
		}
		writeJSON(w, 200, v)
	})
	r.With(a.requireScope("knowledge:approve"), a.responseReadPermit("privacy_clear_in_progress", privacy.OwnerKnowledge, privacy.OwnerLearning)).Post("/v1/knowledge/structure/proposals/{proposalID}/decisions", func(w http.ResponseWriter, r *http.Request) {
		var c knowledge.StructureDecision
		if _, ok := strictLearningQuery(w, r); !ok || !a.decodeKnowledgeMaintenanceRequest(w, r, &c) {
			return
		}
		actor, _ := credentialFromContext(r.Context())
		c.ActorDeviceID = actor.Device.ID
		v, err := s.DecideStructureProposal(r.Context(), chi.URLParam(r, "proposalID"), c)
		if err != nil {
			a.writeKnowledgeMaintenanceFailure(w, r, "decide_structure_proposal", err)
			return
		}
		writeJSON(w, 200, v)
	})
}
