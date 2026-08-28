package httpapi

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/edu-agent/edu-agent/server/internal/learning"
	"github.com/edu-agent/edu-agent/server/internal/transport/problem"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

type evidenceCarryoverDecisionRequest struct {
	OperationID string `json:"operation_id"`
	Decision    string `json:"decision"`
	Reason      string `json:"reason"`
}

func (a *API) learningEvidenceCarryoverList(w http.ResponseWriter, r *http.Request) {
	query, ok := strictLearningQuery(w, r, "status", "cursor", "limit")
	if !ok {
		return
	}
	status := strings.TrimSpace(query.Get("status"))
	if !validEvidenceCarryoverStatus(status) {
		writeLearningInvalid(w, r)
		return
	}
	if status == "all" {
		status = ""
	}
	limit := 50
	if values, present := query["limit"]; present {
		parsed, err := strconv.Atoi(values[0])
		if err != nil || parsed < 1 || parsed > 100 {
			writeLearningInvalid(w, r)
			return
		}
		limit = parsed
	}
	cursor := query.Get("cursor")
	if len(cursor) > 4096 {
		writeLearningInvalid(w, r)
		return
	}
	page, err := a.learning.ListEvidenceCarryovers(r.Context(), learning.EvidenceCarryoverListCommand{
		Status: status, Cursor: cursor, Limit: limit,
	})
	if err != nil {
		a.writeEvidenceCarryoverFailure(w, r, "list", err)
		return
	}
	if page.Items == nil {
		page.Items = []learning.EvidenceCarryoverProposal{}
	}
	writeJSON(w, http.StatusOK, page)
}

func (a *API) learningEvidenceCarryoverGet(w http.ResponseWriter, r *http.Request) {
	if _, ok := strictLearningQuery(w, r); !ok {
		return
	}
	proposalID := chi.URLParam(r, "proposalID")
	if !validLearningUUID(proposalID) {
		writeLearningInvalid(w, r)
		return
	}
	proposal, err := a.learning.GetEvidenceCarryover(r.Context(), proposalID)
	if err != nil {
		a.writeEvidenceCarryoverFailure(w, r, "get", err)
		return
	}
	writeJSON(w, http.StatusOK, proposal)
}

func (a *API) learningEvidenceCarryoverApprove(w http.ResponseWriter, r *http.Request) {
	a.learningEvidenceCarryoverDecide(w, r, "approve")
}

func (a *API) learningEvidenceCarryoverReject(w http.ResponseWriter, r *http.Request) {
	a.learningEvidenceCarryoverDecide(w, r, "reject")
}

func (a *API) learningEvidenceCarryoverDecide(w http.ResponseWriter, r *http.Request, expectedDecision string) {
	if _, ok := strictLearningQuery(w, r); !ok {
		return
	}
	proposalID := chi.URLParam(r, "proposalID")
	if !validLearningUUID(proposalID) {
		writeLearningInvalid(w, r)
		return
	}
	var request evidenceCarryoverDecisionRequest
	if err := decodeJSON(w, r, a.maxLearningRequestBody, &request); err != nil {
		writeEvidenceCarryoverDecodeFailure(w, r, err)
		return
	}
	if !validLearningUUID(request.OperationID) || request.Decision != expectedDecision ||
		!utf8.ValidString(request.Reason) || strings.TrimSpace(request.Reason) == "" ||
		utf8.RuneCountInString(request.Reason) > learning.MaxCarryoverDecisionRunes {
		writeLearningInvalid(w, r)
		return
	}
	credential, ok := credentialFromContext(r.Context())
	if !ok {
		writeError(w, r, http.StatusUnauthorized, "authentication_failed", "Device credentials are invalid")
		return
	}
	proposal, err := a.learning.DecideEvidenceCarryover(r.Context(), credential.Device.ID, learning.EvidenceCarryoverDecisionCommand{
		OperationID: request.OperationID, ProposalID: proposalID, Decision: request.Decision, Reason: request.Reason,
	})
	if err != nil {
		a.writeEvidenceCarryoverFailure(w, r, "decide", err)
		return
	}
	writeJSON(w, http.StatusOK, proposal)
}

func writeEvidenceCarryoverDecodeFailure(w http.ResponseWriter, r *http.Request, err error) {
	var max *http.MaxBytesError
	if errors.As(err, &max) {
		writeError(w, r, http.StatusRequestEntityTooLarge, "payload_too_large", "Request body exceeds the learning limit")
		return
	}
	writeLearningInvalid(w, r)
}

func validEvidenceCarryoverStatus(value string) bool {
	switch learning.EvidenceCarryoverStatus(value) {
	case "", "all", learning.EvidenceCarryoverOpen, learning.EvidenceCarryoverApproved,
		learning.EvidenceCarryoverRejected, learning.EvidenceCarryoverStale, learning.EvidenceCarryoverRedacted:
		return true
	default:
		return false
	}
}

func evidenceCarryoverProblem(err error) problem.Problem {
	switch learning.ErrorCode(err) {
	case learning.CodeOperationConflict, learning.CodeEvidenceCarryoverClosed:
		return problem.Problem{Status: http.StatusConflict, Code: learning.ErrorCode(err), Message: "Evidence carryover decision conflicts with current state"}
	case learning.CodeEvidenceCarryoverNoCandidates:
		return problem.Problem{Status: http.StatusUnprocessableEntity, Code: learning.CodeEvidenceCarryoverNoCandidates, Message: "Evidence carryover has no approvable candidates"}
	default:
		return problem.Learning(err)
	}
}

func (a *API) writeEvidenceCarryoverFailure(w http.ResponseWriter, r *http.Request, operation string, err error) {
	mapped := evidenceCarryoverProblem(err)
	if mapped.Code == "internal_error" {
		a.logger.ErrorContext(r.Context(), "evidence carryover request failed",
			"request_id", middleware.GetReqID(r.Context()), "operation", operation, "error_category", "internal")
	}
	writeProblem(w, r, mapped)
}
