package learning

import (
	"encoding/json"
	"time"
)

// StudioLearningState contains learning-record metadata, never lesson, answer,
// assessment quotation, or conversation text. Categories share at most 24
// distinct node revisions. Coverage describes recorded concepts, not sources.
type StudioLearningState struct {
	AsOfEventSeq       int64                     `json:"as_of_event_seq"`
	DirectionRevision  string                    `json:"direction_revision"`
	RecentConcepts     []StudioLearningStateItem `json:"recent_concepts"`
	NeedsReview        []StudioLearningStateItem `json:"needs_review"`
	HelpUsed           []StudioLearningStateItem `json:"help_used"`
	MasteredEvidence   []StudioLearningStateItem `json:"mastered_evidence"`
	PendingAssessments []StudioLearningStateItem `json:"pending_assessments"`
	Coverage           string                    `json:"coverage"`
	TotalConcepts      int                       `json:"total_concepts"`
	IncludedConcepts   int                       `json:"included_concepts"`
	SuggestedFocus     []string                  `json:"suggested_focus"`
}

type StudioLearningStateItem struct {
	NodeRevisionID   string    `json:"node_revision_id"`
	GoalRevisionID   string    `json:"goal_revision_id"`
	Label            string    `json:"label"`
	RecordID         string    `json:"record_id"`
	RecordType       string    `json:"record_type"`
	OccurredAt       time.Time `json:"occurred_at"`
	Status           string    `json:"status"`
	Outcome          Outcome   `json:"outcome,omitempty"`
	Help             HelpLevel `json:"help,omitempty"`
	CurrentDirection bool      `json:"current_direction"`
}

// MarshalJSON keeps empty or unavailable snapshots usable by all clients,
// including callers constructing a zero value after a failed record lookup.
func (s StudioLearningState) MarshalJSON() ([]byte, error) {
	type snapshot StudioLearningState
	if s.Coverage == "" {
		s.Coverage = "empty"
	}
	for _, items := range []*[]StudioLearningStateItem{&s.RecentConcepts, &s.NeedsReview, &s.HelpUsed, &s.MasteredEvidence, &s.PendingAssessments} {
		if *items == nil {
			*items = []StudioLearningStateItem{}
		}
	}
	if s.SuggestedFocus == nil {
		s.SuggestedFocus = []string{}
	}
	return json.Marshal(snapshot(s))
}
