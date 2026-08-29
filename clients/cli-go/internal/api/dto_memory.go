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

type MemoryOperationResponse struct {
	Candidate *MemoryCandidateView `json:"candidate,omitempty"`
	Record    json.RawMessage      `json:"record,omitempty"`
	Delivery  json.RawMessage      `json:"delivery,omitempty"`
	Replayed  bool                 `json:"replayed"`
}
