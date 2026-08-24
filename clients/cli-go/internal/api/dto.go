package api

import "time"

type ErrorBody struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	RequestID string `json:"request_id"`
}

type ErrorResponse struct {
	Error              ErrorBody         `json:"error"`
	CurrentRevisionID  *string           `json:"current_revision_id,omitempty"`
	IdentityReview     *IdentityReview   `json:"identity_review,omitempty"`
	Conflict           *LearningConflict `json:"conflict,omitempty"`
	CurrentDisposition string            `json:"current_disposition,omitempty"`
}

type LearningConflict struct {
	AggregateType   string `json:"aggregate_type"`
	AggregateID     string `json:"aggregate_id"`
	ExpectedVersion int64  `json:"expected_version"`
	CurrentVersion  int64  `json:"current_version"`
	AsOfEventSeq    int64  `json:"as_of_event_seq"`
}

type Device struct {
	ID          string     `json:"id"`
	DisplayName string     `json:"display_name"`
	Scopes      []string   `json:"scopes,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	LastUsedAt  *time.Time `json:"last_used_at,omitempty"`
	RevokedAt   *time.Time `json:"revoked_at,omitempty"`
}

type IssuedCredential struct {
	Device Device `json:"device"`
	Token  string `json:"token"`
}

type DevicesResponse struct {
	Devices []Device `json:"devices"`
}

type HealthComponent struct {
	Status string `json:"status"`
	Reason string `json:"reason,omitempty"`
}

type Readiness struct {
	Status     string                     `json:"status"`
	Components map[string]HealthComponent `json:"components"`
	Warnings   []string                   `json:"warnings,omitempty"`
}

type ModelCapabilities struct {
	Profile                     string   `json:"profile"`
	Compatible                  bool     `json:"compatible"`
	ContextWindow               int      `json:"context_window"`
	MinimumContextWindow        int      `json:"minimum_context_window"`
	SystemUserAssistantMessages bool     `json:"system_user_assistant_messages"`
	NonStreaming                bool     `json:"non_streaming"`
	StructuredJSON              bool     `json:"structured_json"`
	NativeJSONSchema            bool     `json:"native_json_schema"`
	Streaming                   bool     `json:"streaming"`
	ToolCalls                   bool     `json:"tool_calls"`
	IncompatibilityReasons      []string `json:"incompatibility_reasons"`
}

type ImportDocument struct {
	Path     string `json:"path"`
	Markdown string `json:"markdown"`
}

type DocumentResolution struct {
	Locator    string `json:"locator"`
	Action     string `json:"action"`
	DocumentID string `json:"document_id,omitempty"`
	Reason     string `json:"reason"`
}

type NodeResolution struct {
	Locator               string   `json:"locator"`
	Action                string   `json:"action"`
	SourceNodeRevisionIDs []string `json:"source_node_revision_ids,omitempty"`
	Reason                string   `json:"reason"`
}

type ImportRequest struct {
	OperationID               string               `json:"operation_id"`
	ExpectedParentRevisionID  *string              `json:"expected_parent_revision_id"`
	Source                    string               `json:"source"`
	Documents                 []ImportDocument     `json:"documents"`
	IdentityReviewBasisHash   string               `json:"identity_review_basis_hash,omitempty"`
	IdentityReviewOperationID string               `json:"identity_review_operation_id,omitempty"`
	IdentityReviewReceipt     string               `json:"identity_review_receipt,omitempty"`
	DocumentResolutions       []DocumentResolution `json:"document_resolutions,omitempty"`
	NodeResolutions           []NodeResolution     `json:"node_resolutions,omitempty"`
}

type ImportResult struct {
	Revision  KnowledgeRevision `json:"revision"`
	Unchanged bool              `json:"unchanged"`
	Replayed  bool              `json:"replayed,omitempty"`
}

type HeadResponse struct {
	Revision KnowledgeRevision `json:"revision"`
}

type KnowledgeRevision struct {
	RevisionID            string             `json:"revision_id"`
	RevisionNo            int64              `json:"revision_no"`
	ParentRevisionID      *string            `json:"parent_revision_id"`
	ManifestHash          string             `json:"manifest_hash"`
	Source                string             `json:"source"`
	CreatedByDeviceID     string             `json:"created_by_device_id"`
	CreatedAt             time.Time          `json:"created_at"`
	CanonicalizerVersion  string             `json:"canonicalizer_version"`
	ParserVersion         string             `json:"parser_version"`
	IndexerVersion        string             `json:"indexer_version"`
	IdentityPolicyVersion string             `json:"identity_policy_version"`
	Documents             []SnapshotDocument `json:"documents,omitempty"`
	Lineages              []NodeLineage      `json:"lineages,omitempty"`
}

type SnapshotDocument struct {
	Path     string           `json:"path"`
	Document DocumentRevision `json:"document"`
}

type DocumentRevision struct {
	DocumentRevisionID string         `json:"document_revision_id"`
	DocumentID         string         `json:"document_id"`
	RootNodeID         string         `json:"root_node_id"`
	CanonicalHash      string         `json:"canonical_hash"`
	SemanticHash       string         `json:"semantic_hash"`
	Nodes              []NodeRevision `json:"nodes"`
}

type NodeRevision struct {
	NodeRevisionID        string      `json:"node_revision_id"`
	NodeID                string      `json:"node_id"`
	DocumentRevisionID    string      `json:"document_revision_id"`
	ParentNodeRevisionID  *string     `json:"parent_node_revision_id"`
	SiblingIndex          int         `json:"sibling_index"`
	HeadingLevel          int         `json:"heading_level"`
	Title                 string      `json:"title"`
	AncestorTitles        []string    `json:"ancestor_titles"`
	HeadingRange          SourceRange `json:"heading_range"`
	LocalBodyRange        SourceRange `json:"local_body_range"`
	SectionRange          SourceRange `json:"section_range"`
	SemanticLocalBodyHash string      `json:"semantic_local_body_hash"`
	Children              []string    `json:"children,omitempty"`
}

type SourceRange struct {
	Start     int `json:"start"`
	End       int `json:"end"`
	StartLine int `json:"start_line"`
	EndLine   int `json:"end_line"`
}

type NodeLineage struct {
	LineageID           string          `json:"lineage_id"`
	KnowledgeRevisionID string          `json:"knowledge_revision_id"`
	Action              string          `json:"action"`
	ActorDeviceID       string          `json:"actor_device_id"`
	Reason              string          `json:"reason"`
	PolicyVersion       string          `json:"policy_version"`
	CreatedAt           time.Time       `json:"created_at"`
	Members             []LineageMember `json:"members"`
}

type LineageMember struct {
	Role           string `json:"role"`
	NodeRevisionID string `json:"node_revision_id"`
}

type IdentityCandidate struct {
	StableID   string         `json:"stable_id"`
	RevisionID string         `json:"revision_id"`
	ReasonCode string         `json:"reason_code"`
	Score      int            `json:"score,omitempty"`
	Evidence   map[string]any `json:"evidence,omitempty"`
}

type DocumentIdentityReview struct {
	Path       string              `json:"path"`
	Locator    string              `json:"locator"`
	ReasonCode string              `json:"reason_code"`
	Candidates []IdentityCandidate `json:"candidates"`
}

type NodeIdentityReview struct {
	Path       string              `json:"path"`
	Locator    string              `json:"locator"`
	Preorder   int                 `json:"preorder"`
	ReasonCode string              `json:"reason_code"`
	Candidates []IdentityCandidate `json:"candidates"`
}

type IdentityReview struct {
	BasisHash   string                   `json:"identity_review_basis_hash"`
	OperationID string                   `json:"identity_review_operation_id"`
	Receipt     string                   `json:"identity_review_receipt"`
	Documents   []DocumentIdentityReview `json:"document_reviews"`
	Nodes       []NodeIdentityReview     `json:"node_reviews"`
}
