package httpapi

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"

	"github.com/edu-agent/edu-agent/server/internal/knowledge"
	"github.com/edu-agent/edu-agent/server/internal/transport/problem"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

type createKnowledgeProposalRequest struct {
	RequestID                 string                         `json:"request_id"`
	BaseRevisionID            string                         `json:"base_revision_id"`
	Sources                   []knowledge.ProposalSource     `json:"sources"`
	CandidateSnapshot         []knowledge.ImportDocument     `json:"candidate_snapshot"`
	IdentityReviewBasisHash   string                         `json:"identity_review_basis_hash,omitempty"`
	IdentityReviewOperationID string                         `json:"identity_review_operation_id,omitempty"`
	IdentityReviewReceipt     string                         `json:"identity_review_receipt,omitempty"`
	DocumentResolutions       []knowledge.DocumentResolution `json:"document_resolutions,omitempty"`
	NodeResolutions           []knowledge.NodeResolution     `json:"node_resolutions,omitempty"`
}

type createKnowledgeRollbackRequest struct {
	RequestID        string                     `json:"request_id"`
	BaseRevisionID   string                     `json:"base_revision_id"`
	TargetRevisionID string                     `json:"target_revision_id"`
	Sources          []knowledge.ProposalSource `json:"sources"`
}

type decideKnowledgeProposalRequest struct {
	OperationID string `json:"operation_id"`
	Reason      string `json:"reason"`
}

func (a *API) knowledgeMaintenanceCreate(w http.ResponseWriter, r *http.Request) {
	var request createKnowledgeProposalRequest
	if !a.decodeKnowledgeMaintenanceRequest(w, r, &request) {
		return
	}
	credential, ok := credentialFromContext(r.Context())
	if !ok {
		writeError(w, r, http.StatusUnauthorized, "authentication_failed", "Device credentials are invalid")
		return
	}
	proposal, err := a.knowledge.Create(r.Context(), knowledge.CreateProposalCommand{
		RequestID: request.RequestID, BaseRevisionID: request.BaseRevisionID,
		Sources: request.Sources, CandidateSnapshot: request.CandidateSnapshot,
		IdentityReviewBasisHash:   request.IdentityReviewBasisHash,
		IdentityReviewOperationID: request.IdentityReviewOperationID,
		IdentityReviewReceipt:     request.IdentityReviewReceipt,
		DocumentResolutions:       request.DocumentResolutions, NodeResolutions: request.NodeResolutions,
		ActorDeviceID: credential.Device.ID,
	})
	if err != nil {
		a.writeKnowledgeMaintenanceFailure(w, r, "create_proposal", err)
		return
	}
	status := http.StatusCreated
	if proposal.Replayed {
		status = http.StatusOK
	}
	writeJSON(w, status, proposal)
}

func (a *API) knowledgeMaintenanceRollback(w http.ResponseWriter, r *http.Request) {
	var request createKnowledgeRollbackRequest
	if !a.decodeKnowledgeMaintenanceRequest(w, r, &request) {
		return
	}
	credential, ok := credentialFromContext(r.Context())
	if !ok {
		writeError(w, r, http.StatusUnauthorized, "authentication_failed", "Device credentials are invalid")
		return
	}
	proposal, err := a.knowledge.CreateRollback(r.Context(), knowledge.CreateRollbackCommand{
		RequestID: request.RequestID, BaseRevisionID: request.BaseRevisionID,
		TargetRevisionID: request.TargetRevisionID, Sources: request.Sources,
		ActorDeviceID: credential.Device.ID,
	})
	if err != nil {
		a.writeKnowledgeMaintenanceFailure(w, r, "create_rollback", err)
		return
	}
	status := http.StatusCreated
	if proposal.Replayed {
		status = http.StatusOK
	}
	writeJSON(w, status, proposal)
}

func (a *API) knowledgeMaintenanceList(w http.ResponseWriter, r *http.Request) {
	command, ok := knowledgeMaintenanceListCommand(r.URL.RawQuery)
	if !ok {
		writeError(w, r, http.StatusBadRequest, knowledge.CodeInvalidRequest, "Knowledge maintenance request is invalid")
		return
	}
	page, err := a.knowledge.List(r.Context(), command)
	if err != nil {
		a.writeKnowledgeMaintenanceFailure(w, r, "list_proposals", err)
		return
	}
	if page.Items == nil {
		page.Items = []knowledge.Proposal{}
	}
	writeJSON(w, http.StatusOK, page)
}

func (a *API) knowledgeMaintenanceGet(w http.ResponseWriter, r *http.Request) {
	proposal, err := a.knowledge.Get(r.Context(), chi.URLParam(r, "proposalID"))
	if err != nil {
		a.writeKnowledgeMaintenanceFailure(w, r, "get_proposal", err)
		return
	}
	writeJSON(w, http.StatusOK, proposal)
}

func (a *API) knowledgeMaintenanceApprove(w http.ResponseWriter, r *http.Request) {
	a.knowledgeMaintenanceDecide(w, r, "approve")
}

func (a *API) knowledgeMaintenanceReject(w http.ResponseWriter, r *http.Request) {
	a.knowledgeMaintenanceDecide(w, r, "reject")
}

func (a *API) knowledgeMaintenanceDecide(w http.ResponseWriter, r *http.Request, decision string) {
	var request decideKnowledgeProposalRequest
	if !a.decodeKnowledgeMaintenanceRequest(w, r, &request) {
		return
	}
	credential, ok := credentialFromContext(r.Context())
	if !ok {
		writeError(w, r, http.StatusUnauthorized, "authentication_failed", "Device credentials are invalid")
		return
	}
	proposal, err := a.knowledge.Decide(r.Context(), knowledge.ProposalDecisionCommand{
		OperationID: request.OperationID, ProposalID: chi.URLParam(r, "proposalID"),
		Decision: decision, Reason: request.Reason, ActorDeviceID: credential.Device.ID,
	})
	if err != nil {
		a.writeKnowledgeMaintenanceFailure(w, r, "decide_proposal", err)
		return
	}
	writeJSON(w, http.StatusOK, proposal)
}

func (a *API) decodeKnowledgeMaintenanceRequest(w http.ResponseWriter, r *http.Request, target any) bool {
	if err := decodeJSON(w, r, a.maxKnowledgeRequestBody, target); err != nil {
		var maxBytes *http.MaxBytesError
		if errors.As(err, &maxBytes) {
			writeError(w, r, http.StatusRequestEntityTooLarge, knowledge.CodePayloadTooLarge, "Request body exceeds the knowledge maintenance limit")
		} else {
			writeError(w, r, http.StatusBadRequest, knowledge.CodeInvalidRequest, "Knowledge maintenance request is invalid")
		}
		return false
	}
	return true
}

func knowledgeMaintenanceListCommand(rawQuery string) (knowledge.ProposalListCommand, bool) {
	query, err := url.ParseQuery(rawQuery)
	if err != nil {
		return knowledge.ProposalListCommand{}, false
	}
	for key, values := range query {
		if key != "status" && key != "cursor" && key != "limit" || len(values) != 1 {
			return knowledge.ProposalListCommand{}, false
		}
	}
	command := knowledge.ProposalListCommand{Status: query.Get("status"), Cursor: query.Get("cursor")}
	if value := query.Get("limit"); value != "" {
		command.Limit, err = strconv.Atoi(value)
		if err != nil {
			return knowledge.ProposalListCommand{}, false
		}
	}
	return command, true
}

func knowledgeMaintenanceProblem(err error) problem.Problem {
	code := knowledge.ErrorCode(err)
	switch code {
	case knowledge.CodeInvalidRequest, knowledge.CodeInvalidPath, knowledge.CodeInvalidMarkdown, knowledge.CodeInvalidIdentityMarker:
		return problem.InvalidRequest("Knowledge maintenance request is invalid")
	case knowledge.CodePayloadTooLarge:
		return problem.PayloadTooLarge("Knowledge maintenance payload exceeds the configured limit")
	case knowledge.CodeNotFound:
		return problem.Problem{Status: http.StatusNotFound, Code: knowledge.CodeNotFound, Message: "Knowledge proposal was not found"}
	case knowledge.CodeIdentityReviewRequired:
		return problem.Knowledge(err)
	case knowledge.CodeRevisionConflict, knowledge.CodeIdempotencyConflict, knowledge.CodeStaleIdentityReview,
		knowledge.CodeProposalStale, knowledge.CodeProposalClosed, knowledge.CodeDuplicateDocumentIdentity, knowledge.CodePathOccupied:
		mapped := problem.Problem{Status: http.StatusConflict, Code: "operation_conflict", Message: "Knowledge maintenance operation conflicts with current state"}
		domain := problem.Knowledge(err)
		mapped.Detail = domain.Detail
		return mapped
	case knowledge.CodeContentRedacted:
		return problem.Knowledge(err)
	default:
		return problem.Internal()
	}
}

func (a *API) writeKnowledgeMaintenanceFailure(w http.ResponseWriter, r *http.Request, operation string, err error) {
	mapped := knowledgeMaintenanceProblem(err)
	if mapped.Code == "internal_error" {
		a.logger.ErrorContext(r.Context(), "knowledge maintenance request failed",
			"request_id", middleware.GetReqID(r.Context()), "operation", operation, "error_category", "internal")
	}
	writeProblem(w, r, mapped)
}
