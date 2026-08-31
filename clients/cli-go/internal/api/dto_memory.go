package api

import (
	"encoding/json"
	"time"
)

type MemoryGenerationStamp struct {
	LearnerGeneration int64 `json:"learner_generation"`
	MemoryGeneration  int64 `json:"memory_generation"`
}

type MemorySourceReference struct {
	EventID        string   `json:"event_id,omitempty"`
	OperationID    string   `json:"operation_id,omitempty"`
	ModelID        string   `json:"model_id,omitempty"`
	PromptRevision string   `json:"prompt_revision,omitempty"`
	SourceHashes   []string `json:"source_hashes,omitempty"`
}

type MemoryCandidate struct {
	ID              string                `json:"candidate_id"`
	URI             string                `json:"candidate_uri"`
	LogicalMemoryID string                `json:"logical_memory_id,omitempty"`
	PayloadID       string                `json:"payload_id"`
	ContentHash     string                `json:"content_sha256"`
	Source          string                `json:"source_kind"`
	SourceReference MemorySourceReference `json:"source_reference"`
	ProposerID      string                `json:"proposer_id"`
	Reason          string                `json:"reason"`
	Category        string                `json:"category"`
	Sensitivity     string                `json:"sensitivity"`
	Stability       string                `json:"stability"`
	ValidUntil      time.Time             `json:"valid_until"`
	PolicyVersion   string                `json:"admission_policy_version"`
	Status          string                `json:"status"`
	Revision        int64                 `json:"revision"`
	CreatedAt       time.Time             `json:"created_at"`
}

type MemoryCandidateView struct {
	Candidate       MemoryCandidate        `json:"candidate"`
	ContentStatus   string                 `json:"content_status"`
	ProposedContent string                 `json:"proposed_content,omitempty"`
	ReadGeneration  *MemoryGenerationStamp `json:"read_generation,omitempty"`
}

type MemoryCandidatePage struct {
	Items          []MemoryCandidateView `json:"items"`
	NextCursor     string                `json:"next_cursor,omitempty"`
	ReadGeneration MemoryGenerationStamp `json:"read_generation"`
}

type MemoryCandidateRequest struct {
	OperationID          string    `json:"operation_id"`
	PayloadSchemaVersion int       `json:"payload_schema_version"`
	Content              string    `json:"content"`
	Reason               string    `json:"reason"`
	Category             string    `json:"category"`
	Sensitivity          string    `json:"sensitivity"`
	Stability            string    `json:"stability"`
	ValidUntil           time.Time `json:"valid_until"`
}

type MemoryCandidateDecisionRequest struct {
	OperationID          string `json:"operation_id"`
	PayloadSchemaVersion int    `json:"payload_schema_version"`
	ExpectedRevision     int64  `json:"expected_revision"`
	Decision             string `json:"decision"`
	Reason               string `json:"reason"`
}

type MemoryOperationResponse struct {
	Candidate *MemoryCandidateView `json:"candidate,omitempty"`
	Record    json.RawMessage      `json:"record,omitempty"`
	Delivery  json.RawMessage      `json:"delivery,omitempty"`
	Replayed  bool                 `json:"replayed"`
}

type MemoryRecord struct {
	LogicalMemoryID          string     `json:"logical_memory_id"`
	RecordRevisionID         string     `json:"record_revision_id"`
	Revision                 int64      `json:"revision"`
	RecordGeneration         int64      `json:"record_generation"`
	LearnerGeneration        int64      `json:"learner_generation"`
	CandidateID              string     `json:"candidate_id"`
	PreviousRecordRevisionID string     `json:"previous_record_revision_id,omitempty"`
	ExternalURI              string     `json:"external_uri"`
	ExternalURISHA256        string     `json:"external_uri_sha256"`
	ExternalNodeID           string     `json:"external_node_id,omitempty"`
	ExternalMemoryID         int64      `json:"external_memory_id,omitempty"`
	ContentSHA256            string     `json:"content_sha256"`
	Status                   string     `json:"status"`
	DeliveryID               string     `json:"delivery_id"`
	ReceiptID                string     `json:"receipt_id"`
	CreatedAt                time.Time  `json:"created_at"`
	AppliedAt                *time.Time `json:"applied_at,omitempty"`
	SupersededAt             *time.Time `json:"superseded_at,omitempty"`
	DeletedAt                *time.Time `json:"deleted_at,omitempty"`
}

type MemoryDeliveryReceipt struct {
	ID                 string    `json:"receipt_id"`
	DeliveryID         string    `json:"delivery_id"`
	Version            int64     `json:"version"`
	Status             string    `json:"status"`
	Reason             string    `json:"reason"`
	VerificationMethod string    `json:"verification_method"`
	EvidenceDigest     string    `json:"evidence_digest,omitempty"`
	CreatedAt          time.Time `json:"created_at"`
}

type MemoryExportItem struct {
	Record         MemoryRecord          `json:"record"`
	DeliveryStatus string                `json:"delivery_status"`
	Receipt        MemoryDeliveryReceipt `json:"receipt"`
	ContentStatus  string                `json:"content_status"`
	Content        string                `json:"content,omitempty"`
}

type MemoryExportPage struct {
	Items          []MemoryExportItem    `json:"items"`
	NextCursor     string                `json:"next_cursor,omitempty"`
	ReadGeneration MemoryGenerationStamp `json:"read_generation"`
	Degraded       bool                  `json:"degraded"`
	ReasonCodes    []string              `json:"reason_codes"`
}
