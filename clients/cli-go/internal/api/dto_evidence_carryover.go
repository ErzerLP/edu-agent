package api

import "time"

const EvidenceCarryoverPolicyVersion = "evidence-carryover-v1"

type EvidenceCarryoverDecisionRequest struct {
	OperationID string `json:"operation_id"`
	Decision    string `json:"decision"`
	Reason      string `json:"reason"`
}

type EvidenceCarryoverCandidate struct {
	KnowledgeRevisionID string `json:"knowledge_revision_id"`
	NodeID              string `json:"node_id"`
	NodeRevisionID      string `json:"node_revision_id"`
	DocumentRevisionID  string `json:"document_revision_id"`
}

type EvidenceCarryoverDecision struct {
	DecisionID        string    `json:"decision_id"`
	OperationID       string    `json:"operation_id"`
	RequestedDecision string    `json:"requested_decision"`
	Outcome           string    `json:"outcome"`
	Reason            string    `json:"reason,omitempty"`
	ActorDeviceID     string    `json:"actor_device_id"`
	EventID           string    `json:"event_id"`
	EventSequence     int64     `json:"event_seq"`
	CreatedAt         time.Time `json:"created_at"`
}

type EvidenceCarryoverLink struct {
	LinkID                    string    `json:"link_id"`
	ProposalID                string    `json:"proposal_id"`
	SourceEvidenceID          string    `json:"source_evidence_id,omitempty"`
	TargetKnowledgeRevisionID string    `json:"target_knowledge_revision_id,omitempty"`
	TargetNodeID              string    `json:"target_node_id,omitempty"`
	TargetNodeRevisionID      string    `json:"target_node_revision_id,omitempty"`
	TargetDocumentRevisionID  string    `json:"target_document_revision_id,omitempty"`
	DecisionID                string    `json:"decision_id"`
	EventID                   string    `json:"event_id"`
	EventSequence             int64     `json:"event_seq"`
	CreatedAt                 time.Time `json:"created_at"`
}

type EvidenceCarryoverProposal struct {
	ProposalID                  string                       `json:"proposal_id"`
	KnowledgeProposalID         string                       `json:"knowledge_proposal_id"`
	Status                      string                       `json:"status"`
	SourceEvidenceID            string                       `json:"source_evidence_id,omitempty"`
	SourceKnowledgeRevisionID   string                       `json:"source_knowledge_revision_id,omitempty"`
	SourceNodeRevisionID        string                       `json:"source_node_revision_id,omitempty"`
	TargetKnowledgeRevisionID   string                       `json:"target_knowledge_revision_id,omitempty"`
	Candidates                  []EvidenceCarryoverCandidate `json:"candidates,omitempty"`
	KnowledgeBasisHash          string                       `json:"knowledge_basis_hash,omitempty"`
	AcceptedEvidenceFingerprint string                       `json:"accepted_evidence_fingerprint,omitempty"`
	CandidateFingerprint        string                       `json:"candidate_fingerprint,omitempty"`
	BasisFingerprint            string                       `json:"basis_fingerprint,omitempty"`
	KnowledgeGeneration         int64                        `json:"knowledge_generation"`
	LearningGeneration          int64                        `json:"learning_generation"`
	PolicyVersion               string                       `json:"policy_version"`
	Decision                    *EvidenceCarryoverDecision   `json:"decision,omitempty"`
	Links                       []EvidenceCarryoverLink      `json:"links,omitempty"`
	CreatedAt                   time.Time                    `json:"created_at"`
	UpdatedAt                   time.Time                    `json:"updated_at"`
	Redacted                    bool                         `json:"redacted"`
	Replayed                    bool                         `json:"replayed,omitempty"`
}

type EvidenceCarryoverPage struct {
	Items      []EvidenceCarryoverProposal `json:"items"`
	NextCursor string                      `json:"next_cursor,omitempty"`
}
