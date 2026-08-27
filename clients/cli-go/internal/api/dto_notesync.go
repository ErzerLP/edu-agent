package api

import "time"

const (
	NotesyncResolutionAcceptRemote  = "accept_remote"
	NotesyncResolutionKeepCanonical = "keep_canonical"
	NotesyncResolutionMerged        = "merged"
	NotesyncResolutionSuperseded    = "superseded"
	NotesyncResolutionPrivacy       = "privacy_redaction"
)

type NotesyncStatus struct {
	Configured              bool   `json:"configured"`
	Compatible              bool   `json:"compatible"`
	Reason                  string `json:"reason"`
	Version                 string `json:"version,omitempty"`
	Vault                   string `json:"vault,omitempty"`
	PathPrefix              string `json:"path_prefix,omitempty"`
	ExternalCleanupRequired bool   `json:"external_cleanup_required"`
}

type NotesyncPreviewRequest struct {
	Path     string `json:"path,omitempty"`
	Page     int    `json:"page,omitempty"`
	PageSize int    `json:"page_size,omitempty"`
}

type NotesyncReviewSnapshotSummary struct {
	Missing             bool   `json:"missing"`
	KnowledgeRevisionID string `json:"knowledge_revision_id,omitempty"`
	KnowledgeRevisionNo int64  `json:"knowledge_revision_no,omitempty"`
	DocumentRevisionID  string `json:"document_revision_id,omitempty"`
	SourceRevisionID    string `json:"source_revision_id,omitempty"`
	Path                string `json:"path,omitempty"`
	SHA256              string `json:"sha256,omitempty"`
	RemoteVersion       int64  `json:"remote_version,omitempty"`
	RemoteLastTime      int64  `json:"remote_last_time,omitempty"`
}

type NotesyncReviewSnapshot struct {
	Missing             bool   `json:"missing"`
	KnowledgeRevisionID string `json:"knowledge_revision_id,omitempty"`
	KnowledgeRevisionNo int64  `json:"knowledge_revision_no,omitempty"`
	DocumentRevisionID  string `json:"document_revision_id,omitempty"`
	SourceRevisionID    string `json:"source_revision_id,omitempty"`
	Path                string `json:"path,omitempty"`
	Markdown            string `json:"markdown"`
	SHA256              string `json:"sha256,omitempty"`
	RemoteVersion       int64  `json:"remote_version,omitempty"`
	RemoteLastTime      int64  `json:"remote_last_time,omitempty"`
}

type NotesyncThreeWayDiff struct {
	BaseToLocal     string `json:"base_to_local"`
	BaseToRemote    string `json:"base_to_remote"`
	LocalTruncated  bool   `json:"local_truncated"`
	RemoteTruncated bool   `json:"remote_truncated"`
}

type NotesyncPreviewItem struct {
	Category   string                        `json:"category"`
	ReasonCode string                        `json:"reason_code"`
	ReviewID   string                        `json:"review_id,omitempty"`
	BasisHash  string                        `json:"basis_hash"`
	DocumentID string                        `json:"document_id,omitempty"`
	RemotePath string                        `json:"remote_path"`
	Base       NotesyncReviewSnapshotSummary `json:"base"`
	Local      NotesyncReviewSnapshotSummary `json:"local"`
	Remote     NotesyncReviewSnapshotSummary `json:"remote"`
	Diff       NotesyncThreeWayDiff          `json:"diff"`
}

type NotesyncPreviewResult struct {
	Items     []NotesyncPreviewItem `json:"items"`
	Page      int                   `json:"page"`
	PageSize  int                   `json:"page_size"`
	NextPage  int                   `json:"next_page,omitempty"`
	TotalRows int                   `json:"total_rows"`
}

type NotesyncReview struct {
	ReviewID                    string                 `json:"review_id"`
	Category                    string                 `json:"category"`
	ReasonCode                  string                 `json:"reason_code"`
	Status                      string                 `json:"status"`
	BasisHash                   string                 `json:"basis_hash"`
	Generation                  int64                  `json:"generation"`
	HeadRevisionID              string                 `json:"head_revision_id"`
	HeadRevisionNo              int64                  `json:"head_revision_no"`
	DocumentID                  string                 `json:"document_id,omitempty"`
	RemoteDocumentID            string                 `json:"remote_document_id,omitempty"`
	CanonicalPath               string                 `json:"canonical_path"`
	RemoteVault                 string                 `json:"remote_vault"`
	RemotePath                  string                 `json:"remote_path"`
	Base                        NotesyncReviewSnapshot `json:"base"`
	Local                       NotesyncReviewSnapshot `json:"local"`
	Remote                      NotesyncReviewSnapshot `json:"remote"`
	Diff                        NotesyncThreeWayDiff   `json:"diff"`
	ResolutionKind              string                 `json:"resolution_kind,omitempty"`
	ResolutionOperationID       string                 `json:"resolution_operation_id,omitempty"`
	ResolvedByDeviceID          string                 `json:"resolved_by_device_id,omitempty"`
	ResolvedKnowledgeRevisionID string                 `json:"resolved_knowledge_revision_id,omitempty"`
	ResolvedDocumentID          string                 `json:"resolved_document_id,omitempty"`
	ResolvedDocumentRevisionID  string                 `json:"resolved_document_revision_id,omitempty"`
	CreatedAt                   time.Time              `json:"created_at"`
	UpdatedAt                   time.Time              `json:"updated_at"`
	ResolvedAt                  *time.Time             `json:"resolved_at,omitempty"`
}

type NotesyncReviewSummary struct {
	ReviewID                    string                        `json:"review_id"`
	Category                    string                        `json:"category"`
	ReasonCode                  string                        `json:"reason_code"`
	Status                      string                        `json:"status"`
	BasisHash                   string                        `json:"basis_hash"`
	Generation                  int64                         `json:"generation"`
	HeadRevisionID              string                        `json:"head_revision_id"`
	HeadRevisionNo              int64                         `json:"head_revision_no"`
	DocumentID                  string                        `json:"document_id,omitempty"`
	RemoteDocumentID            string                        `json:"remote_document_id,omitempty"`
	CanonicalPath               string                        `json:"canonical_path"`
	RemoteVault                 string                        `json:"remote_vault"`
	RemotePath                  string                        `json:"remote_path"`
	Base                        NotesyncReviewSnapshotSummary `json:"base"`
	Local                       NotesyncReviewSnapshotSummary `json:"local"`
	Remote                      NotesyncReviewSnapshotSummary `json:"remote"`
	ResolutionKind              string                        `json:"resolution_kind,omitempty"`
	ResolutionOperationID       string                        `json:"resolution_operation_id,omitempty"`
	ResolvedByDeviceID          string                        `json:"resolved_by_device_id,omitempty"`
	ResolvedKnowledgeRevisionID string                        `json:"resolved_knowledge_revision_id,omitempty"`
	ResolvedDocumentID          string                        `json:"resolved_document_id,omitempty"`
	ResolvedDocumentRevisionID  string                        `json:"resolved_document_revision_id,omitempty"`
	CreatedAt                   time.Time                     `json:"created_at"`
	UpdatedAt                   time.Time                     `json:"updated_at"`
	ResolvedAt                  *time.Time                    `json:"resolved_at,omitempty"`
}

type NotesyncReviewPage struct {
	Items      []NotesyncReviewSummary `json:"items"`
	NextCursor string                  `json:"next_cursor,omitempty"`
}

type NotesyncResolutionRequest struct {
	BasisHash      string  `json:"basis_hash"`
	OperationID    string  `json:"operation_id"`
	Kind           string  `json:"kind"`
	MergedMarkdown *string `json:"merged_markdown,omitempty"`
}

type NotesyncResolutionResult struct {
	ReviewID            string `json:"review_id"`
	ResolutionKind      string `json:"resolution_kind"`
	KnowledgeRevisionID string `json:"knowledge_revision_id,omitempty"`
	DocumentID          string `json:"document_id,omitempty"`
	DocumentRevisionID  string `json:"document_revision_id,omitempty"`
	Unchanged           bool   `json:"unchanged"`
	Replayed            bool   `json:"replayed,omitempty"`
}
