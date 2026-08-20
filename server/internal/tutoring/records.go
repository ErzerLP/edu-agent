package tutoring

import "time"

type FreeQuestion struct {
	ID                  string            `json:"free_question_id"`
	SessionID           string            `json:"session_id"`
	FocusFrameID        string            `json:"focus_frame_id"`
	Text                string            `json:"text"`
	KnowledgeRevisionID string            `json:"knowledge_revision_id"`
	References          []FrozenReference `json:"references"`
	ActorDeviceID       string            `json:"actor_device_id"`
	OccurredAt          *time.Time        `json:"occurred_at,omitempty"`
	ReceivedAt          time.Time         `json:"received_at"`
}

type FreeAnswer struct {
	ID                  string            `json:"free_answer_id"`
	SessionID           string            `json:"session_id"`
	FocusFrameID        string            `json:"focus_frame_id"`
	FreeQuestionID      string            `json:"free_question_id"`
	Text                string            `json:"text"`
	KnowledgeRevisionID string            `json:"knowledge_revision_id"`
	References          []FrozenReference `json:"references"`
	SourceProposalID    string            `json:"source_proposal_id,omitempty"`
	ReceivedAt          time.Time         `json:"received_at"`
}

type FrozenReference struct {
	KnowledgeRevisionID string `json:"knowledge_revision_id"`
	NodeID              string `json:"node_id"`
	NodeRevisionID      string `json:"node_revision_id"`
	DocumentRevisionID  string `json:"document_revision_id"`
	Start               int    `json:"start"`
	End                 int    `json:"end"`
	Slice               string `json:"slice"`
	SliceSHA256         string `json:"slice_sha256"`
}
