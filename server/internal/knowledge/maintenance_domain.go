package knowledge

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"time"
)

const (
	MaintenanceDiffVersion      = "knowledge-diff-v1"
	MaintenanceRiskVersion      = "knowledge-risk-v1"
	MaintenanceAutoPolicy       = "knowledge-auto-apply-v1"
	MaintenanceBasisVersion     = "knowledge-proposal-basis-v1"
	MaintenanceOriginVersion    = "knowledge-revision-origin-v1"
	MaxMaintenanceSources       = 20
	MaxMaintenanceSourceRunes   = 4000
	MaxMaintenanceDiffBytes     = 256 << 10
	MaxMaintenanceDiffInput     = 512 << 10
	MaxMaintenanceDiffLines     = 10000
	MaxAutoApplyDocuments       = 3
	MaxAutoApplyNodes           = 20
	MaxAutoApplyChangedBodyByte = 32 << 10
)

type ProposalStatus string

const (
	ProposalOpen     ProposalStatus = "open"
	ProposalApplied  ProposalStatus = "applied"
	ProposalRejected ProposalStatus = "rejected"
	ProposalStale    ProposalStatus = "stale"
	ProposalRedacted ProposalStatus = "redacted"
)

type ProposalKind string

const (
	ProposalCandidate ProposalKind = "candidate"
	ProposalRollback  ProposalKind = "rollback"
)

type ProposalSource struct {
	Kind    string `json:"kind"`
	Locator string `json:"locator"`
	Title   string `json:"title,omitempty"`
	Excerpt string `json:"excerpt,omitempty"`
	SHA256  string `json:"sha256"`
}

type CreateProposalCommand struct {
	RequestID                 string               `json:"request_id"`
	BaseRevisionID            string               `json:"base_revision_id"`
	Sources                   []ProposalSource     `json:"sources"`
	CandidateSnapshot         []ImportDocument     `json:"candidate_snapshot"`
	IdentityReviewBasisHash   string               `json:"identity_review_basis_hash,omitempty"`
	IdentityReviewOperationID string               `json:"identity_review_operation_id,omitempty"`
	IdentityReviewReceipt     string               `json:"identity_review_receipt,omitempty"`
	DocumentResolutions       []DocumentResolution `json:"document_resolutions,omitempty"`
	NodeResolutions           []NodeResolution     `json:"node_resolutions,omitempty"`
	ActorDeviceID             string               `json:"-"`
}

func (c *CreateProposalCommand) UnmarshalJSON(data []byte) error {
	type alias CreateProposalCommand
	var decoded alias
	if err := decodeClosedJSON(data, &decoded); err != nil {
		return err
	}
	*c = CreateProposalCommand(decoded)
	return nil
}

type CreateRollbackCommand struct {
	RequestID        string           `json:"request_id"`
	BaseRevisionID   string           `json:"base_revision_id"`
	TargetRevisionID string           `json:"target_revision_id"`
	Sources          []ProposalSource `json:"sources"`
	ActorDeviceID    string           `json:"-"`
}

func (c *CreateRollbackCommand) UnmarshalJSON(data []byte) error {
	type alias CreateRollbackCommand
	var decoded alias
	if err := decodeClosedJSON(data, &decoded); err != nil {
		return err
	}
	*c = CreateRollbackCommand(decoded)
	return nil
}

type ProposalDecisionCommand struct {
	OperationID   string `json:"operation_id"`
	ProposalID    string `json:"proposal_id"`
	Decision      string `json:"decision"`
	Reason        string `json:"reason"`
	ActorDeviceID string `json:"-"`
}

func (c *ProposalDecisionCommand) UnmarshalJSON(data []byte) error {
	type alias ProposalDecisionCommand
	var decoded alias
	if err := decodeClosedJSON(data, &decoded); err != nil {
		return err
	}
	*c = ProposalDecisionCommand(decoded)
	return nil
}

func decodeClosedJSON(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if decoder.More() {
		return fmt.Errorf("multiple JSON values")
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return fmt.Errorf("multiple JSON values")
		}
		return err
	}
	return nil
}

type ProposalListCommand struct {
	Status             string    `json:"status,omitempty"`
	Limit              int       `json:"limit,omitempty"`
	Cursor             string    `json:"cursor,omitempty"`
	ExpectedGeneration int64     `json:"-"`
	AfterCreatedAt     time.Time `json:"-"`
	AfterProposalID    string    `json:"-"`
}

func (c *ProposalListCommand) UnmarshalJSON(data []byte) error {
	type alias ProposalListCommand
	var decoded alias
	if err := decodeClosedJSON(data, &decoded); err != nil {
		return err
	}
	*c = ProposalListCommand(decoded)
	return nil
}

type ProposalPage struct {
	Items      []Proposal `json:"items"`
	NextCursor string     `json:"next_cursor,omitempty"`
}

type DocumentDiff struct {
	DocumentID       string   `json:"document_id"`
	BeforePath       string   `json:"before_path,omitempty"`
	AfterPath        string   `json:"after_path,omitempty"`
	Kind             string   `json:"kind"`
	Unified          string   `json:"unified_diff,omitempty"`
	Truncated        bool     `json:"truncated"`
	AddedNodeIDs     []string `json:"added_node_ids,omitempty"`
	RemovedNodeIDs   []string `json:"removed_node_ids,omitempty"`
	EditedNodeIDs    []string `json:"edited_node_ids,omitempty"`
	TitleNodeIDs     []string `json:"title_node_ids,omitempty"`
	StructureNodeIDs []string `json:"structure_node_ids,omitempty"`
	LocalBodyOnly    bool     `json:"local_body_only"`
	ChangedBodyBytes int      `json:"changed_body_bytes"`
}

type IdentityImpact struct {
	PreservedDocumentIDs []string `json:"preserved_document_ids"`
	AddedDocumentIDs     []string `json:"added_document_ids"`
	RemovedDocumentIDs   []string `json:"removed_document_ids"`
	MovedDocumentIDs     []string `json:"moved_document_ids"`
	PreservedNodeIDs     []string `json:"preserved_node_ids"`
	AddedNodeIDs         []string `json:"added_node_ids"`
	RemovedNodeIDs       []string `json:"removed_node_ids"`
	Uncertain            bool     `json:"uncertain"`
}

type LineageImpact struct {
	Lineages []Lineage `json:"lineages"`
	Move     bool      `json:"move"`
	Delete   bool      `json:"delete"`
	Restore  bool      `json:"restore"`
	Rollback bool      `json:"rollback"`
}

type AcceptedEvidenceReference struct {
	EvidenceID          string `json:"evidence_id"`
	NodeRevisionID      string `json:"node_revision_id"`
	KnowledgeRevisionID string `json:"knowledge_revision_id"`
}

type AcceptedEvidenceImpact struct {
	Count       int                         `json:"count"`
	References  []AcceptedEvidenceReference `json:"references"`
	Fingerprint string                      `json:"fingerprint"`
	Generation  int64                       `json:"generation"`
}

type ProposalRisk struct {
	Level         string   `json:"level"`
	Reasons       []string `json:"reasons"`
	AutoApply     bool     `json:"auto_apply"`
	PolicyVersion string   `json:"policy_version"`
}

type ProposalDecision struct {
	ID                string    `json:"decision_id"`
	OperationID       string    `json:"operation_id,omitempty"`
	RequestedDecision string    `json:"requested_decision"`
	Outcome           string    `json:"outcome"`
	Reason            string    `json:"reason,omitempty"`
	ActorDeviceID     string    `json:"actor_device_id"`
	CreatedAt         time.Time `json:"created_at"`
}

type RevisionOrigin struct {
	Version                  string  `json:"version"`
	Kind                     string  `json:"kind"`
	ProposalID               string  `json:"proposal_id"`
	BaseRevisionID           string  `json:"base_revision_id"`
	RollbackTargetRevisionID *string `json:"rollback_target_revision_id,omitempty"`
	BasisHash                string  `json:"basis_hash"`
}

type Proposal struct {
	ID                       string                 `json:"proposal_id"`
	RequestID                string                 `json:"request_id"`
	RequestHash              string                 `json:"-"`
	Kind                     ProposalKind           `json:"kind"`
	Status                   ProposalStatus         `json:"status"`
	BaseRevisionID           string                 `json:"base_revision_id"`
	CurrentRevisionID        string                 `json:"current_revision_id,omitempty"`
	RollbackTargetRevisionID string                 `json:"rollback_target_revision_id,omitempty"`
	Sources                  []ProposalSource       `json:"sources,omitempty"`
	CandidateSnapshot        []ImportDocument       `json:"candidate_snapshot,omitempty"`
	Diff                     []DocumentDiff         `json:"diff,omitempty"`
	IdentityImpact           IdentityImpact         `json:"identity_impact"`
	LineageImpact            LineageImpact          `json:"lineage_impact"`
	EvidenceImpact           AcceptedEvidenceImpact `json:"accepted_learning_evidence_impact"`
	AffectedNodeRevisionIDs  []string               `json:"-"`
	Risk                     ProposalRisk           `json:"risk"`
	BasisHash                string                 `json:"basis_hash,omitempty"`
	KnowledgeGeneration      int64                  `json:"knowledge_generation"`
	CanonicalizerVersion     string                 `json:"canonicalizer_version"`
	IdentityPolicyVersion    string                 `json:"identity_policy_version"`
	DiffVersion              string                 `json:"diff_version"`
	RiskVersion              string                 `json:"risk_version"`
	AutoPolicyVersion        string                 `json:"auto_apply_policy_version"`
	Decision                 *ProposalDecision      `json:"decision,omitempty"`
	AppliedRevisionID        string                 `json:"applied_revision_id,omitempty"`
	PlannedRevisionID        string                 `json:"-"`
	PlannedRevisionNo        int64                  `json:"-"`
	PlannedManifestHash      string                 `json:"-"`
	Origin                   *RevisionOrigin        `json:"origin,omitempty"`
	CreatedByDeviceID        string                 `json:"created_by_device_id"`
	CreatedAt                time.Time              `json:"created_at"`
	UpdatedAt                time.Time              `json:"updated_at"`
	Redacted                 bool                   `json:"redacted"`
	Replayed                 bool                   `json:"replayed,omitempty"`
}

type PreparedProposal struct {
	Proposal Proposal
	Commit   PreparedCommit
}

type PreparedProposalDecision struct {
	OperationID           string
	RequestHash           string
	ProposalID            string
	RequestedDecision     string
	Reason                string
	ActorDeviceID         string
	DecisionID            string
	DecidedAt             time.Time
	EvidenceFingerprint   string
	EvidenceGeneration    int64
	CanonicalizerVersion  string
	IdentityPolicyVersion string
	DiffVersion           string
	RiskVersion           string
	AutoPolicyVersion     string
}

type MaintenanceOperationRecord struct {
	RequestHash string
	ProposalID  string
}

type MaintenanceBaseSnapshot struct {
	Revision            KnowledgeRevision
	HeadRevisionID      string
	KnowledgeGeneration int64
}

type MaintenanceStore interface {
	MaintenanceBase(context.Context, string) (MaintenanceBaseSnapshot, error)
	LookupMaintenanceOperation(context.Context, string) (MaintenanceOperationRecord, bool, error)
	SaveProposal(context.Context, PreparedProposal) (Proposal, error)
	ListProposals(context.Context, ProposalListCommand) (ProposalPage, error)
	Proposal(context.Context, string) (Proposal, error)
	DecideProposal(context.Context, PreparedProposalDecision) (Proposal, error)
}

type EvidenceImpactReader interface {
	AcceptedEvidenceImpact(context.Context, []string) (AcceptedEvidenceImpact, error)
}

func EncodeProposalCursor(generation int64, createdAt time.Time, proposalID string) string {
	value, _ := json.Marshal(struct {
		Version    int    `json:"v"`
		Generation int64  `json:"generation"`
		CreatedAt  string `json:"created_at"`
		ProposalID string `json:"proposal_id"`
	}{Version: 1, Generation: generation, CreatedAt: createdAt.UTC().Format(time.RFC3339Nano), ProposalID: proposalID})
	return base64.RawURLEncoding.EncodeToString(value)
}

func DecodeProposalCursor(value string) (int64, time.Time, string, error) {
	if value == "" {
		return 0, time.Time{}, "", nil
	}
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return 0, time.Time{}, "", &Error{Code: CodeProposalStale}
	}
	var wire struct {
		Version    int    `json:"v"`
		Generation int64  `json:"generation"`
		CreatedAt  string `json:"created_at"`
		ProposalID string `json:"proposal_id"`
	}
	if err := decodeClosedJSON(decoded, &wire); err != nil || wire.Version != 1 || wire.Generation < 1 || !validUUID(wire.ProposalID) {
		return 0, time.Time{}, "", &Error{Code: CodeProposalStale}
	}
	createdAt, err := time.Parse(time.RFC3339Nano, wire.CreatedAt)
	if err != nil {
		return 0, time.Time{}, "", &Error{Code: CodeProposalStale}
	}
	return wire.Generation, createdAt.UTC(), wire.ProposalID, nil
}
