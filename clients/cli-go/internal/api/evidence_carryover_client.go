package api

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
)

func (c *Client) EvidenceCarryovers(ctx context.Context, status, cursor string, limit int) (EvidenceCarryoverPage, error) {
	if err := ValidateEvidenceCarryoverQuery(status, cursor, limit); err != nil {
		return EvidenceCarryoverPage{}, &ProtocolError{Category: "invalid_evidence_carryover_query"}
	}
	values := url.Values{}
	values.Set("status", status)
	values.Set("limit", strconv.Itoa(limit))
	if cursor != "" {
		values.Set("cursor", cursor)
	}
	var response EvidenceCarryoverPage
	if err := c.doJSON(ctx, http.MethodGet, withQuery("/v1/learning/evidence-carryovers", values), true, nil, map[int]bool{http.StatusOK: true}, true, &response); err != nil {
		return response, err
	}
	if err := ValidateEvidenceCarryoverPage(response, limit); err != nil {
		return response, &ProtocolError{Category: "invalid_success_response"}
	}
	return response, nil
}

func (c *Client) EvidenceCarryover(ctx context.Context, proposalID string) (EvidenceCarryoverProposal, error) {
	if err := ValidateEvidenceCarryoverProposalID(proposalID); err != nil {
		return EvidenceCarryoverProposal{}, &ProtocolError{Category: "invalid_evidence_carryover_proposal_id"}
	}
	var response EvidenceCarryoverProposal
	path := "/v1/learning/evidence-carryovers/" + url.PathEscape(proposalID)
	if err := c.doJSON(ctx, http.MethodGet, path, true, nil, map[int]bool{http.StatusOK: true}, true, &response); err != nil {
		return response, err
	}
	if err := ValidateEvidenceCarryoverProposal(response); err != nil || response.ProposalID != proposalID || response.Replayed {
		return response, &ProtocolError{Category: "invalid_success_response"}
	}
	return response, nil
}

func (c *Client) DecideEvidenceCarryover(ctx context.Context, proposalID, decision string, request EvidenceCarryoverDecisionRequest) (EvidenceCarryoverProposal, error) {
	if err := ValidateEvidenceCarryoverDecisionRequest(proposalID, decision, request); err != nil {
		return EvidenceCarryoverProposal{}, &ProtocolError{Category: "invalid_evidence_carryover_decision_request"}
	}
	var response EvidenceCarryoverProposal
	path := "/v1/learning/evidence-carryovers/" + url.PathEscape(proposalID) + "/" + decision
	if err := c.doJSON(ctx, http.MethodPost, path, true, request, map[int]bool{http.StatusOK: true}, true, &response); err != nil {
		return response, err
	}
	if err := ValidateEvidenceCarryoverProposal(response); err != nil || response.ProposalID != proposalID || response.Decision == nil ||
		response.Decision.OperationID != request.OperationID || response.Decision.RequestedDecision != decision {
		return response, &ProtocolError{Category: "invalid_success_response"}
	}
	return response, nil
}
