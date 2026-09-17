package knowledge

import (
	"github.com/edu-agent/edu-agent/server/internal/research"
)

// KnowledgePolicy 是用户确认的目标语义和研究授权，不是来源版本列表。
type KnowledgePolicy struct {
	ID             string           `json:"id"`
	GoalRevisionID string           `json:"goal_revision_id"`
	Request        research.Request `json:"request"`
}

type ConceptRevision struct {
	ConceptID   string              `json:"concept_id"`
	RevisionID  string              `json:"revision_id"`
	SemanticKey string              `json:"semantic_key"`
	Name        string              `json:"name"`
	Support     []research.Citation `json:"support"`
}

type KnowledgeContextRevision struct {
	ID                 string            `json:"id"`
	Policy             KnowledgePolicy   `json:"policy"`
	ScopeSnapshotID    string            `json:"scope_snapshot_id"`
	PreviousRevisionID string            `json:"previous_revision_id,omitempty"`
	Concepts           []ConceptRevision `json:"concepts"`
}
