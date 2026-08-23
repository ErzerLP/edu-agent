package knowledge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

const (
	CanonicalizerVersion  = "edu-markdown-v1"
	ParserVersion         = "goldmark-v1.8.5-commonmark-0.31.2-gfm"
	IndexerVersion        = "knowledge-indexer-v1"
	IdentityPolicyVersion = "identity-policy-v1"
	RetrieverVersion      = "retriever-v1"
	SelectorVersion       = "selector-v1"
	QueryContextVersion   = "query-context-v1"

	MaxImportDocuments = 1000
	MaxDocumentBytes   = 4 << 20
	MaxImportBodyBytes = 16 << 20
	MaxImportNodes     = 100000
	MaxSourceRunes     = 500
	MaxPathRunes       = 512
	MaxPathBytes       = 1024
)

const (
	CodeInvalidRequest            = "invalid_request"
	CodePayloadTooLarge           = "payload_too_large"
	CodeInvalidPath               = "invalid_path"
	CodeInvalidMarkdown           = "invalid_markdown"
	CodeInvalidIdentityMarker     = "invalid_identity_marker"
	CodeDuplicateDocumentIdentity = "duplicate_document_identity"
	CodePathOccupied              = "path_occupied"
	CodeIdentityReviewRequired    = "identity_review_required"
	CodeStaleIdentityReview       = "stale_identity_review"
	CodeRevisionConflict          = "revision_conflict"
	CodeIdempotencyConflict       = "idempotency_conflict"
	CodeNotFound                  = "not_found"
	CodeContentRedacted           = "content_redacted"
)

type Error struct {
	Code                 string
	CurrentRevisionID    *string
	CurrentRevisionKnown bool
	Review               *IdentityReview
	Cause                error
}

func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	if e.Cause != nil {
		return fmt.Sprintf("knowledge operation failed: code=%s: %v", e.Code, e.Cause)
	}
	return fmt.Sprintf("knowledge operation failed: code=%s", e.Code)
}

func (e *Error) Unwrap() error { return e.Cause }

func ErrorCode(err error) string {
	var domainErr *Error
	if errors.As(err, &domainErr) {
		return domainErr.Code
	}
	return ""
}

type SourceRange struct {
	Start     int `json:"start"`
	End       int `json:"end"`
	StartLine int `json:"start_line"`
	EndLine   int `json:"end_line"`
}

type NodeRevision struct {
	ID                    string      `json:"node_revision_id"`
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

type DocumentRevision struct {
	ID                string         `json:"document_revision_id"`
	DocumentID        string         `json:"document_id"`
	RootNodeID        string         `json:"root_node_id"`
	CanonicalHash     string         `json:"canonical_hash"`
	SemanticHash      string         `json:"semantic_hash"`
	CanonicalMarkdown string         `json:"-"`
	Nodes             []NodeRevision `json:"nodes"`
}

type SnapshotDocument struct {
	Path     string           `json:"path"`
	Revision DocumentRevision `json:"document"`
}

type KnowledgeRevision struct {
	ID                    string             `json:"revision_id"`
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
	Redacted              bool               `json:"-"`
	Documents             []SnapshotDocument `json:"documents,omitempty"`
	Lineages              []Lineage          `json:"lineages,omitempty"`
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

type ImportCommand struct {
	OperationID               string               `json:"operation_id"`
	ExpectedParentRevisionID  *string              `json:"expected_parent_revision_id"`
	ExpectedParentProvided    bool                 `json:"-"`
	Source                    string               `json:"source"`
	Documents                 []ImportDocument     `json:"documents"`
	IdentityReviewBasisHash   string               `json:"identity_review_basis_hash,omitempty"`
	IdentityReviewOperationID string               `json:"identity_review_operation_id,omitempty"`
	IdentityReviewReceipt     string               `json:"identity_review_receipt,omitempty"`
	DocumentResolutions       []DocumentResolution `json:"document_resolutions,omitempty"`
	NodeResolutions           []NodeResolution     `json:"node_resolutions,omitempty"`
	ActorDeviceID             string               `json:"-"`
}

func (c *ImportCommand) UnmarshalJSON(data []byte) error {
	type commandAlias ImportCommand
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var decoded commandAlias
	if err := decoder.Decode(&decoded); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	_, decoded.ExpectedParentProvided = fields["expected_parent_revision_id"]
	*c = ImportCommand(decoded)
	return nil
}

type ImportResult struct {
	Revision  KnowledgeRevision `json:"revision"`
	Unchanged bool              `json:"unchanged"`
	Replayed  bool              `json:"replayed,omitempty"`
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

type Lineage struct {
	ID                  string          `json:"lineage_id"`
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

type NodeArtifact struct {
	ID              string    `json:"artifact_id"`
	NodeRevisionID  string    `json:"node_revision_id"`
	Kind            string    `json:"kind"`
	ProducerVersion string    `json:"producer_version"`
	PromptVersion   string    `json:"prompt_version"`
	ModelVersion    string    `json:"model_version"`
	InputHash       string    `json:"input_hash"`
	Content         string    `json:"content"`
	Status          string    `json:"status"`
	CreatedAt       time.Time `json:"created_at"`
}

type ExportDocument struct {
	Path     string `json:"path"`
	Markdown string `json:"markdown"`
}

type ExportResult struct {
	RevisionID string           `json:"revision_id"`
	Documents  []ExportDocument `json:"documents"`
}

type TreeResult struct {
	Revision KnowledgeRevision `json:"revision"`
}

type RetrievalLimits struct {
	MaxDepth           int `json:"max_depth,omitempty"`
	CandidatesPerLayer int `json:"candidates_per_layer,omitempty"`
	MaxHits            int `json:"max_hits,omitempty"`
	TotalCandidates    int `json:"total_candidates,omitempty"`
}

type RetrievalCommand struct {
	Query                     string          `json:"query"`
	KnowledgeRevisionID       *string         `json:"knowledge_revision_id,omitempty"`
	QueryContextSchemaVersion string          `json:"query_context_schema_version,omitempty"`
	Context                   map[string]any  `json:"context,omitempty"`
	Limits                    RetrievalLimits `json:"limits,omitempty"`
}

type Candidate struct {
	Ordinal           int    `json:"ordinal"`
	NodeRevisionID    string `json:"node_revision_id"`
	Score             int    `json:"score"`
	Title             string `json:"title"`
	TitleSHA256       string `json:"title_sha256"`
	SummaryArtifactID string `json:"summary_artifact_id,omitempty"`
	HasChildren       bool   `json:"has_children"`
	LocalBodyScore    int    `json:"local_body_score"`
}

type Decision struct {
	NodeRevisionID string `json:"node_revision_id"`
	Action         string `json:"action"`
}

type SelectorRequest struct {
	KnowledgeRevisionID   string         `json:"knowledge_revision_id"`
	Query                 string         `json:"query"`
	QueryContextVersion   string         `json:"query_context_schema_version"`
	Context               map[string]any `json:"context,omitempty"`
	ParentNodeRevisionID  string         `json:"parent_node_revision_id"`
	Candidates            []Candidate    `json:"candidates"`
	SummarySnapshot       []NodeArtifact `json:"summary_snapshot,omitempty"`
	CandidateSetHash      string         `json:"candidate_set_hash"`
	RemainingBudget       int            `json:"remaining_budget"`
	ArtifactFailureReason string         `json:"-"`
}

type SelectorResponse struct {
	KnowledgeRevisionID string     `json:"knowledge_revision_id"`
	CandidateSetHash    string     `json:"candidate_set_hash"`
	Decisions           []Decision `json:"decisions"`
}

type Selector interface {
	Select(context.Context, SelectorRequest) (SelectorResponse, error)
}

type SelectorFailure struct {
	Reason    string
	Truncated bool
	Cause     error
}

func (e *SelectorFailure) Error() string { return "selector failed: " + e.Reason }
func (e *SelectorFailure) Unwrap() error { return e.Cause }

type RetrievalTrace struct {
	Index                int         `json:"index"`
	Depth                int         `json:"depth"`
	ParentNodeRevisionID string      `json:"parent_node_revision_id"`
	Candidates           []Candidate `json:"candidates"`
	Decisions            []Decision  `json:"decisions"`
	CandidateSetHash     string      `json:"candidate_set_hash"`
	ReasonCode           string      `json:"reason_code,omitempty"`
	Degraded             bool        `json:"degraded"`
	Truncated            bool        `json:"truncated"`
}

type RetrievalHit struct {
	DocumentID         string      `json:"document_id"`
	DocumentRevisionID string      `json:"document_revision_id"`
	NodeID             string      `json:"node_id"`
	NodeRevisionID     string      `json:"node_revision_id"`
	Path               string      `json:"path"`
	HeadingRange       SourceRange `json:"heading_range"`
	LocalBodyRange     SourceRange `json:"local_body_range"`
	SectionRange       SourceRange `json:"section_range"`
	CanonicalSlice     string      `json:"canonical_slice"`
	SliceSHA256        string      `json:"slice_sha256"`
	TraceIndex         int         `json:"trace_index"`
	Depth              int         `json:"depth"`
	Provenance         string      `json:"provenance"`
}

type RetrievalResult struct {
	KnowledgeRevisionID string           `json:"knowledge_revision_id"`
	RetrieverVersion    string           `json:"retriever_version"`
	SelectorVersion     string           `json:"selector_version"`
	QueryContextVersion string           `json:"query_context_schema_version"`
	SummarySnapshot     []string         `json:"summary_snapshot"`
	DocumentShortlist   []string         `json:"document_shortlist"`
	Trace               []RetrievalTrace `json:"trace"`
	Hits                []RetrievalHit   `json:"hits"`
	Degraded            bool             `json:"degraded"`
	Truncated           bool             `json:"truncated"`
}
