package api

import "time"

const (
	KnowledgeMaintenanceKindCandidate = "candidate"
	KnowledgeMaintenanceKindRollback  = "rollback"
)

type KnowledgeMaintenanceSource struct {
	Kind    string `json:"kind"`
	Locator string `json:"locator"`
	Title   string `json:"title,omitempty"`
	Excerpt string `json:"excerpt,omitempty"`
	SHA256  string `json:"sha256"`
}

type KnowledgeMaintenanceProposalRequest struct {
	RequestID                 string                       `json:"request_id"`
	BaseRevisionID            string                       `json:"base_revision_id"`
	Sources                   []KnowledgeMaintenanceSource `json:"sources"`
	CandidateSnapshot         []ImportDocument             `json:"candidate_snapshot"`
	IdentityReviewBasisHash   string                       `json:"identity_review_basis_hash,omitempty"`
	IdentityReviewOperationID string                       `json:"identity_review_operation_id,omitempty"`
	IdentityReviewReceipt     string                       `json:"identity_review_receipt,omitempty"`
	DocumentResolutions       []DocumentResolution         `json:"document_resolutions,omitempty"`
	NodeResolutions           []NodeResolution             `json:"node_resolutions,omitempty"`
}

type KnowledgeMaintenanceRollbackRequest struct {
	RequestID        string                       `json:"request_id"`
	BaseRevisionID   string                       `json:"base_revision_id"`
	TargetRevisionID string                       `json:"target_revision_id"`
	Sources          []KnowledgeMaintenanceSource `json:"sources"`
}

type KnowledgeMaintenanceDecisionRequest struct {
	OperationID string `json:"operation_id"`
	Reason      string `json:"reason"`
}

type KnowledgeMaintenanceDocumentDiff struct {
	DocumentID       string   `json:"document_id"`
	BeforePath       string   `json:"before_path,omitempty"`
	AfterPath        string   `json:"after_path,omitempty"`
	Kind             string   `json:"kind"`
	UnifiedDiff      string   `json:"unified_diff,omitempty"`
	Truncated        bool     `json:"truncated"`
	AddedNodeIDs     []string `json:"added_node_ids,omitempty"`
	RemovedNodeIDs   []string `json:"removed_node_ids,omitempty"`
	EditedNodeIDs    []string `json:"edited_node_ids,omitempty"`
	TitleNodeIDs     []string `json:"title_node_ids,omitempty"`
	StructureNodeIDs []string `json:"structure_node_ids,omitempty"`
	LocalBodyOnly    bool     `json:"local_body_only"`
	ChangedBodyBytes int      `json:"changed_body_bytes"`
}

type KnowledgeMaintenanceIdentityImpact struct {
	PreservedDocumentIDs []string `json:"preserved_document_ids"`
	AddedDocumentIDs     []string `json:"added_document_ids"`
	RemovedDocumentIDs   []string `json:"removed_document_ids"`
	MovedDocumentIDs     []string `json:"moved_document_ids"`
	PreservedNodeIDs     []string `json:"preserved_node_ids"`
	AddedNodeIDs         []string `json:"added_node_ids"`
	RemovedNodeIDs       []string `json:"removed_node_ids"`
	Uncertain            bool     `json:"uncertain"`
}

type KnowledgeMaintenanceLineageImpact struct {
	Lineages []NodeLineage `json:"lineages"`
	Move     bool          `json:"move"`
	Delete   bool          `json:"delete"`
	Restore  bool          `json:"restore"`
	Rollback bool          `json:"rollback"`
}

type KnowledgeMaintenanceEvidenceReference struct {
	EvidenceID          string `json:"evidence_id"`
	NodeRevisionID      string `json:"node_revision_id"`
	KnowledgeRevisionID string `json:"knowledge_revision_id"`
}

type KnowledgeMaintenanceEvidenceImpact struct {
	Count       int                                     `json:"count"`
	References  []KnowledgeMaintenanceEvidenceReference `json:"references"`
	Fingerprint string                                  `json:"fingerprint"`
	Generation  int64                                   `json:"generation"`
}

type KnowledgeMaintenanceRisk struct {
	Level         string   `json:"level"`
	Reasons       []string `json:"reasons"`
	AutoApply     bool     `json:"auto_apply"`
	PolicyVersion string   `json:"policy_version"`
}

type KnowledgeMaintenanceDecision struct {
	DecisionID        string    `json:"decision_id"`
	OperationID       string    `json:"operation_id,omitempty"`
	RequestedDecision string    `json:"requested_decision"`
	Outcome           string    `json:"outcome"`
	Reason            string    `json:"reason,omitempty"`
	ActorDeviceID     string    `json:"actor_device_id"`
	CreatedAt         time.Time `json:"created_at"`
}

type KnowledgeMaintenanceRevisionOrigin struct {
	Version                  string `json:"version"`
	Kind                     string `json:"kind"`
	ProposalID               string `json:"proposal_id"`
	BaseRevisionID           string `json:"base_revision_id"`
	RollbackTargetRevisionID string `json:"rollback_target_revision_id,omitempty"`
	BasisHash                string `json:"basis_hash"`
}

type KnowledgeMaintenanceProposal struct {
	ProposalID                     string                              `json:"proposal_id"`
	RequestID                      string                              `json:"request_id"`
	Kind                           string                              `json:"kind"`
	Status                         string                              `json:"status"`
	BaseRevisionID                 string                              `json:"base_revision_id"`
	CurrentRevisionID              string                              `json:"current_revision_id,omitempty"`
	RollbackTargetRevisionID       string                              `json:"rollback_target_revision_id,omitempty"`
	Sources                        []KnowledgeMaintenanceSource        `json:"sources,omitempty"`
	CandidateSnapshot              []ImportDocument                    `json:"candidate_snapshot,omitempty"`
	Diff                           []KnowledgeMaintenanceDocumentDiff  `json:"diff,omitempty"`
	IdentityImpact                 KnowledgeMaintenanceIdentityImpact  `json:"identity_impact"`
	LineageImpact                  KnowledgeMaintenanceLineageImpact   `json:"lineage_impact"`
	AcceptedLearningEvidenceImpact KnowledgeMaintenanceEvidenceImpact  `json:"accepted_learning_evidence_impact"`
	Risk                           KnowledgeMaintenanceRisk            `json:"risk"`
	BasisHash                      string                              `json:"basis_hash,omitempty"`
	KnowledgeGeneration            int64                               `json:"knowledge_generation"`
	CanonicalizerVersion           string                              `json:"canonicalizer_version"`
	IdentityPolicyVersion          string                              `json:"identity_policy_version"`
	DiffVersion                    string                              `json:"diff_version"`
	RiskVersion                    string                              `json:"risk_version"`
	AutoApplyPolicyVersion         string                              `json:"auto_apply_policy_version"`
	Decision                       *KnowledgeMaintenanceDecision       `json:"decision,omitempty"`
	AppliedRevisionID              string                              `json:"applied_revision_id,omitempty"`
	Origin                         *KnowledgeMaintenanceRevisionOrigin `json:"origin,omitempty"`
	CreatedByDeviceID              string                              `json:"created_by_device_id"`
	CreatedAt                      time.Time                           `json:"created_at"`
	UpdatedAt                      time.Time                           `json:"updated_at"`
	Redacted                       bool                                `json:"redacted"`
	Replayed                       bool                                `json:"replayed,omitempty"`
}

type KnowledgeMaintenanceProposalPage struct {
	Items      []KnowledgeMaintenanceProposal `json:"items"`
	NextCursor string                         `json:"next_cursor,omitempty"`
}
