package learning

import (
	"context"
	"encoding/json"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/platform/outbox"
	"github.com/edu-agent/edu-agent/server/internal/tutoring"
)

type AggregateExpectation struct {
	Type            string `json:"aggregate_type"`
	ID              string `json:"aggregate_id"`
	ExpectedVersion int64  `json:"expected_version"`
}

type EventDraft struct {
	Type          EventType       `json:"event_type"`
	AggregateType string          `json:"aggregate_type"`
	AggregateID   string          `json:"aggregate_id"`
	Payload       json.RawMessage `json:"payload"`
}

type EvidenceInvalidation struct {
	ID         string    `json:"invalidation_id"`
	EvidenceID string    `json:"evidence_id"`
	DecisionID *string   `json:"decision_id,omitempty"`
	Reason     string    `json:"reason"`
	EventSeq   int64     `json:"event_seq"`
	CreatedAt  time.Time `json:"created_at"`
}

type CommandBatch struct {
	GoalRevision    *GoalRevision
	RouteRevision   *RouteRevision
	Session         *tutoring.Session
	FocusFrame      *tutoring.FocusFrame
	InvalidateFrame bool
	ResumeFrame     bool
	FreeQuestion    *tutoring.FreeQuestion
	FreeAnswer      *tutoring.FreeAnswer
	Activity        *Activity
	Attempt         *Attempt
	Assessment      *AssessmentArtifact
	Decisions       []AssessmentDecision
	Evidence        []AcceptedEvidence
	Invalidations   []EvidenceInvalidation
	Exposures       []Exposure
	Misconceptions  []MisconceptionHypothesis
	Outbox          []outbox.Message
	Events          []EventDraft
	TypedResult     json.RawMessage
	ResultSession   bool
	TutoringState   string
	Disposition     Disposition
}

type CommitRequest struct {
	DeviceID     string
	Operation    OperationEnvelope
	RequestHash  string
	Expectations []AggregateExpectation
	Batch        CommandBatch
	ReceivedAt   time.Time
}

type OperationLookup struct {
	DeviceID    string
	OperationID string
	RequestHash string
}

type OperationRejection struct {
	Lookup        OperationLookup
	AggregateType string
	AggregateID   string
	Expectations  []AggregateExpectation
	Error         Error
	CompletedAt   time.Time
}

type OperationArchive interface {
	LookupOperation(context.Context, OperationLookup) (OperationResult, error, bool)
	ArchiveRejection(context.Context, OperationRejection) (OperationResult, error)
}

type CommandStore interface {
	Commit(context.Context, CommitRequest) (OperationResult, error)
}

type SessionAuthority struct {
	Session           tutoring.Session `json:"session"`
	AsOfEventSequence int64            `json:"as_of_event_seq"`
}

type AuthorityStore interface {
	CommandStore
	OperationArchive
	LoadSessionAuthority(context.Context, string) (SessionAuthority, error)
	LoadAggregateVersion(context.Context, string, string) (int64, error)
	LoadGoalRevision(context.Context, string) (GoalRevision, error)
	LoadRouteRevision(context.Context, string) (RouteRevision, error)
	LoadActivity(context.Context, string) (Activity, error)
	LoadAttempt(context.Context, string) (Attempt, error)
	LoadAssessment(context.Context, string) (AssessmentArtifact, AssessmentDecision, error)
	LoadProposal(context.Context, string) (ProposalArtifact, error)
	LoadFreeQuestion(context.Context, string) (tutoring.FreeQuestion, error)
	LoadFreeAnswer(context.Context, string) (tutoring.FreeAnswer, error)
	LoadValidEvidence(context.Context, string) ([]AcceptedEvidence, error)
	LoadMisconceptions(context.Context, string) ([]MisconceptionHypothesis, error)
	LatestFreeQuestion(context.Context, string) (string, error)
}

type ApplicationStore interface {
	AuthorityStore
	QueryStore
}

type CursorPageRequest struct {
	Cursor string
	Limit  int
}

type TimelineQuery struct {
	Page      CursorPageRequest
	SessionID string
}

type EvidenceQuery struct {
	Page           CursorPageRequest
	NodeRevisionID string
}

type ReviewQuery struct {
	Page      CursorPageRequest
	DueBefore *time.Time
}

type ProjectionStatus struct {
	Metadata               ProjectionMetadata `json:"metadata"`
	HighWater              int64              `json:"committed_event_high_water"`
	Fingerprint            string             `json:"fingerprint"`
	ActiveGenerationID     string             `json:"active_generation_id"`
	RebuildingGenerationID *string            `json:"rebuilding_generation_id,omitempty"`
}

type TimelinePage struct {
	Metadata   ProjectionMetadata `json:"metadata"`
	Items      []TimelineItem     `json:"items"`
	NextCursor string             `json:"next_cursor,omitempty"`
}

type RoutesPage struct {
	Metadata   ProjectionMetadata `json:"metadata"`
	Items      []RouteProjection  `json:"items"`
	NextCursor string             `json:"next_cursor,omitempty"`
}

type EvidencePage struct {
	Metadata   ProjectionMetadata `json:"metadata"`
	Items      []AcceptedEvidence `json:"items"`
	NextCursor string             `json:"next_cursor,omitempty"`
}

type ReviewsPage struct {
	Metadata   ProjectionMetadata `json:"metadata"`
	Items      []ReviewSchedule   `json:"items"`
	NextCursor string             `json:"next_cursor,omitempty"`
}

type SessionView struct {
	Metadata ProjectionMetadata `json:"metadata"`
	Session  tutoring.Session   `json:"session"`
	Estimate ActiveTimeEstimate `json:"estimated_active_time"`
}

type NodeView struct {
	Metadata ProjectionMetadata `json:"metadata"`
	Node     NodeReduction      `json:"node"`
	Evidence []AcceptedEvidence `json:"evidence"`
}

type QueryStore interface {
	CurrentSession(context.Context) (SessionView, error)
	Session(context.Context, string) (SessionView, error)
	Timeline(context.Context, TimelineQuery) (TimelinePage, error)
	Routes(context.Context, CursorPageRequest) (RoutesPage, error)
	Node(context.Context, string) (NodeView, error)
	EvidenceList(context.Context, EvidenceQuery) (EvidencePage, error)
	Reviews(context.Context, ReviewQuery) (ReviewsPage, error)
	ProjectionStatus(context.Context) (ProjectionStatus, error)
	Rebuild(context.Context) (ProjectionStatus, error)
}
