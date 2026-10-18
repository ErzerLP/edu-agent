package mentorrun

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/knowledge"
	"github.com/google/uuid"
)

type TopicIndexRequest struct {
	OperationID        string                     `json:"operation_id"`
	ExpectedRevision   int64                      `json:"expected_index_revision"`
	Generation         int64                      `json:"privacy_generation"`
	DirectionVersion   int64                      `json:"direction_version,omitempty"`
	SourceSelection    *knowledge.SourceSelection `json:"source_selection,omitempty"`
	ConfirmDestination string                     `json:"confirm_destination,omitempty"`
}
type TopicIndexState struct {
	MemoryGeneration int64  `json:"memory_generation,omitempty"`
	Revision         int64  `json:"revision"`
	TargetTurn       int64  `json:"target_turn"`
	Scope            string `json:"scope"`
}

// Evidence is attached by the server, never accepted as model-provided authority.
type TopicEvidence struct {
	MemorySources       []MemorySource `json:"memory_sources,omitempty"`
	AnchorIDs           []string       `json:"anchor_ids"`
	Scope               string         `json:"scope_fingerprint"`
	DirectionRevision   string         `json:"direction_revision,omitempty"`
	SourceFingerprint   string         `json:"source_fingerprint,omitempty"`
	ExerciseIDs         []string       `json:"exercise_ids"`
	Generation          int64          `json:"privacy_generation"`
	KnowledgeGeneration int64          `json:"knowledge_generation,omitempty"`
	MemoryGeneration    int64          `json:"memory_generation,omitempty"`
	ObservedAt          time.Time      `json:"observed_at"`
}
type TopicPoint struct {
	Text string `json:"text"`
	TopicEvidence
}
type TopicObservation struct {
	State    string `json:"state"`
	Text     string `json:"text"`
	Evidence string `json:"evidence"`
	TopicEvidence
}
type Topic struct {
	ID              string             `json:"id"`
	Title           string             `json:"title"`
	Points          []TopicPoint       `json:"points"`
	Observations    []TopicObservation `json:"observations"`
	AnchorCount     int                `json:"anchor_count"`
	Revision        int64              `json:"revision"`
	ExerciseRelated bool               `json:"exercise_related"`
}
type topicBody struct {
	Topic
	SourceManifest      *knowledge.SourceManifest `json:"source_manifest,omitempty"`
	MemorySources       []MemorySource            `json:"memory_sources,omitempty"`
	KnowledgeGeneration int64                     `json:"knowledge_generation,omitempty"`
	MemoryGeneration    int64                     `json:"memory_generation,omitempty"`
}
type TopicPage struct {
	ConversationID  string  `json:"conversation_id"`
	Generation      int64   `json:"privacy_generation"`
	Revision        int64   `json:"revision"`
	CoveredThrough  int64   `json:"covered_through"`
	CoveredTail     int64   `json:"covered_tail"`
	CoveredMessages int     `json:"covered_messages"`
	TotalMessages   int     `json:"total_messages"`
	UnmappedTurns   int     `json:"unmapped_turns"`
	Status          string  `json:"status"`
	Items           []Topic `json:"items"`
	NextCursor      string  `json:"next_cursor,omitempty"`
}
type MessageAnchor struct {
	MessageID string `json:"message_id"`
	RunID     string `json:"run_id"`
	Slot      int    `json:"message_slot"`
	Ordinal   int64  `json:"ordinal"`
}
type TopicAnchorPage struct {
	ConversationID string          `json:"conversation_id"`
	Generation     int64           `json:"privacy_generation"`
	Items          []MessageAnchor `json:"items"`
	NextCursor     int64           `json:"next_cursor,omitempty"`
}

// -1 denotes the displayed final answer. Its identity never depends on how many
// internal model/tool messages preceded it. User slots retain the raw array slot.
func messageIdentity(run string, slot int) string {
	return uuid.NewSHA1(uuid.MustParse(run), []byte(fmt.Sprintf("display:v1:%d", slot))).String()
}
func displayAnchors(run string, ordinal int64, body Body) []MessageAnchor {
	result := []MessageAnchor{}
	for slot, m := range body.Messages {
		if m.Role == "user" {
			result = append(result, MessageAnchor{messageIdentity(run, slot), run, slot, ordinal*MaxBody + int64(slot) + 1})
		}
	}
	if body.Output != "" {
		result = append(result, MessageAnchor{messageIdentity(run, -1), run, -1, (ordinal+1)*MaxBody - 1})
	}
	return result
}
func topicScope(body Body) string {
	source, direction := "legacy", ""
	if body.SourceManifest != nil {
		source = body.SourceManifest.SHA256
	}
	if body.Study != nil {
		direction = body.Study.ID
	}
	hash := sha256.Sum256([]byte(source + "\n" + direction))
	return hex.EncodeToString(hash[:])
}
