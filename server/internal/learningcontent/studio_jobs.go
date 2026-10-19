package learningcontent

import (
	"time"

	"github.com/edu-agent/edu-agent/server/internal/knowledge"
	"github.com/edu-agent/edu-agent/server/internal/learning"
)

type StudioPlanSection struct {
	ID        string                           `json:"id"`
	Topic     string                           `json:"topic"`
	Objective string                           `json:"objective"`
	Chapters  []knowledge.SourceDirectoryEntry `json:"chapters"`
	UnitCount int                              `json:"unit_count"`
}
type StudioPlan struct {
	Revision         int64               `json:"revision"`
	Title            string              `json:"title"`
	Coverage         string              `json:"coverage"`
	Sections         []StudioPlanSection `json:"sections"`
	Uncovered        []string            `json:"uncovered"`
	InputFingerprint string              `json:"input_fingerprint"`
	SliceBytes       int                 `json:"slice_bytes"`
}
type StudioPlanMessage struct {
	OperationID  string    `json:"operation_id"`
	Text         string    `json:"text"`
	Explanation  string    `json:"explanation"`
	Status       string    `json:"status"`
	PlanRevision int64     `json:"plan_revision"`
	CreatedAt    time.Time `json:"created_at"`
}

// StudioJobInput is a frozen encrypted snapshot, never a raw conversation.
type StudioJobInput struct {
	Manifest                knowledge.SourceManifest     `json:"manifest"`
	DirectionID             string                       `json:"direction_id"`
	DirectionVersion        int64                        `json:"direction_version"`
	Direction               string                       `json:"direction"`
	LearningState           learning.StudioLearningState `json:"learning_state"`
	TutoringGeneration      int64                        `json:"tutoring_generation"`
	Focus                   string                       `json:"focus"`
	ArtifactID              string                       `json:"artifact_id,omitempty"`
	ExpectedArtifactVersion int64                        `json:"expected_artifact_version,omitempty"`
	Fingerprint             string                       `json:"fingerprint"`
	Reason                  string                       `json:"reason,omitempty"`
	Configuration           string                       `json:"configuration,omitempty"`
	Destination             string                       `json:"destination,omitempty"`
	Summary                 *StudioSummary               `json:"summary,omitempty"`
	Result                  *StudioJobResult             `json:"result,omitempty"`
}
type StudioJobResult struct {
	ArtifactID string `json:"artifact_id"`
	Version    int64  `json:"version"`
}
type StudioJob struct {
	ID                    string              `json:"id"`
	SpaceID               string              `json:"-"`
	DeviceID              string              `json:"-"`
	Generation            int64               `json:"privacy_generation"`
	Version               int64               `json:"version"`
	Kind                  string              `json:"kind"`
	Status                string              `json:"status"`
	ActivePlanRevision    int64               `json:"active_plan_revision"`
	ConfirmedPlanRevision int64               `json:"confirmed_plan_revision"`
	RunID                 string              `json:"run_id,omitempty"`
	Plan                  *StudioPlan         `json:"plan,omitempty"`
	Messages              []StudioPlanMessage `json:"messages"`
	SuggestedFocus        []string            `json:"suggested_focus"`
	Reason                string              `json:"reason,omitempty"`
	CreatedAt             time.Time           `json:"created_at"`
	UpdatedAt             time.Time           `json:"updated_at"`
	Input                 StudioJobInput      `json:"-"`
	CompletedUnits        int                 `json:"completed_units"`
	TotalUnits            int                 `json:"total_units"`
	CurrentUnit           string              `json:"current_unit,omitempty"`
	Result                *StudioJobResult    `json:"result,omitempty"`
	ResultUnknown         bool                `json:"result_unknown"`
	CostUnknown           bool                `json:"cost_unknown"`
}
type StudioJobPage struct {
	Items      []StudioJob `json:"items"`
	NextCursor string      `json:"next_cursor,omitempty"`
}
