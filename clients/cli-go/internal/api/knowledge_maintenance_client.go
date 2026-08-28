package api

import (
	"context"
	"net/http"
	"net/url"
)

func DecodeKnowledgeMaintenanceProposalRequest(data []byte) (KnowledgeMaintenanceProposalRequest, error) {
	var request KnowledgeMaintenanceProposalRequest
	if err := decodeStrict(data, &request); err != nil {
		return request, err
	}
	return request, ValidateKnowledgeMaintenanceProposalRequest(request)
}

func DecodeKnowledgeMaintenanceRollbackRequest(data []byte) (KnowledgeMaintenanceRollbackRequest, error) {
	var request KnowledgeMaintenanceRollbackRequest
	if err := decodeStrict(data, &request); err != nil {
		return request, err
	}
	return request, ValidateKnowledgeMaintenanceRollbackRequest(request)
}

func (c *Client) CreateKnowledgeMaintenanceProposal(ctx context.Context, request KnowledgeMaintenanceProposalRequest) (KnowledgeMaintenanceProposal, error) {
	if err := ValidateKnowledgeMaintenanceProposalRequest(request); err != nil {
		return KnowledgeMaintenanceProposal{}, &ProtocolError{Category: "invalid_knowledge_maintenance_proposal_request"}
	}
	var response KnowledgeMaintenanceProposal
	status, err := c.doJSONStatus(ctx, http.MethodPost, "/v1/knowledge/maintenance/proposals", true, request, map[int]bool{http.StatusOK: true, http.StatusCreated: true}, true, &response)
	if err != nil {
		return response, err
	}
	if err := ValidateKnowledgeMaintenanceProposal(response); err != nil || response.RequestID != request.RequestID || response.BaseRevisionID != request.BaseRevisionID || response.Kind != KnowledgeMaintenanceKindCandidate || status == http.StatusOK && !response.Replayed || status == http.StatusCreated && response.Replayed {
		return response, &ProtocolError{Category: "invalid_success_response"}
	}
	return response, nil
}

func (c *Client) CreateKnowledgeMaintenanceRollback(ctx context.Context, request KnowledgeMaintenanceRollbackRequest) (KnowledgeMaintenanceProposal, error) {
	if err := ValidateKnowledgeMaintenanceRollbackRequest(request); err != nil {
		return KnowledgeMaintenanceProposal{}, &ProtocolError{Category: "invalid_knowledge_maintenance_rollback_request"}
	}
	var response KnowledgeMaintenanceProposal
	status, err := c.doJSONStatus(ctx, http.MethodPost, "/v1/knowledge/maintenance/rollbacks", true, request, map[int]bool{http.StatusOK: true, http.StatusCreated: true}, true, &response)
	if err != nil {
		return response, err
	}
	if err := ValidateKnowledgeMaintenanceProposal(response); err != nil || response.RequestID != request.RequestID || response.BaseRevisionID != request.BaseRevisionID || response.RollbackTargetRevisionID != request.TargetRevisionID || response.Kind != KnowledgeMaintenanceKindRollback || status == http.StatusOK && !response.Replayed || status == http.StatusCreated && response.Replayed {
		return response, &ProtocolError{Category: "invalid_success_response"}
	}
	return response, nil
}

func (c *Client) KnowledgeMaintenanceProposals(ctx context.Context, status, cursor string, limit int) (KnowledgeMaintenanceProposalPage, error) {
	if err := ValidateKnowledgeMaintenanceQuery(status, cursor, limit); err != nil {
		return KnowledgeMaintenanceProposalPage{}, &ProtocolError{Category: "invalid_knowledge_maintenance_query"}
	}
	values := url.Values{}
	if status != "" && status != "all" {
		values.Set("status", status)
	}
	setPageQuery(values, cursor, limit)
	var response KnowledgeMaintenanceProposalPage
	if err := c.doJSON(ctx, http.MethodGet, withQuery("/v1/knowledge/maintenance/proposals", values), true, nil, map[int]bool{http.StatusOK: true}, true, &response); err != nil {
		return response, err
	}
	if err := ValidateKnowledgeMaintenanceProposalPage(response); err != nil || limit > 0 && len(response.Items) > limit {
		return response, &ProtocolError{Category: "invalid_success_response"}
	}
	return response, nil
}

func (c *Client) KnowledgeMaintenanceProposal(ctx context.Context, proposalID string) (KnowledgeMaintenanceProposal, error) {
	if !validLearningUUID(proposalID) {
		return KnowledgeMaintenanceProposal{}, &ProtocolError{Category: "invalid_knowledge_maintenance_proposal_id"}
	}
	var response KnowledgeMaintenanceProposal
	path := "/v1/knowledge/maintenance/proposals/" + url.PathEscape(proposalID)
	if err := c.doJSON(ctx, http.MethodGet, path, true, nil, map[int]bool{http.StatusOK: true}, true, &response); err != nil {
		return response, err
	}
	if err := ValidateKnowledgeMaintenanceProposal(response); err != nil || response.ProposalID != proposalID {
		return response, &ProtocolError{Category: "invalid_success_response"}
	}
	return response, nil
}

func (c *Client) DecideKnowledgeMaintenanceProposal(ctx context.Context, proposalID, decision string, request KnowledgeMaintenanceDecisionRequest) (KnowledgeMaintenanceProposal, error) {
	if err := ValidateKnowledgeMaintenanceDecisionRequest(proposalID, request); err != nil || decision != "approve" && decision != "reject" {
		return KnowledgeMaintenanceProposal{}, &ProtocolError{Category: "invalid_knowledge_maintenance_decision_request"}
	}
	var response KnowledgeMaintenanceProposal
	path := "/v1/knowledge/maintenance/proposals/" + url.PathEscape(proposalID) + "/" + decision
	if err := c.doJSON(ctx, http.MethodPost, path, true, request, map[int]bool{http.StatusOK: true}, true, &response); err != nil {
		return response, err
	}
	if err := ValidateKnowledgeMaintenanceProposal(response); err != nil || response.ProposalID != proposalID || response.Decision == nil || response.Decision.OperationID != request.OperationID || response.Decision.RequestedDecision != decision {
		return response, &ProtocolError{Category: "invalid_success_response"}
	}
	return response, nil
}
