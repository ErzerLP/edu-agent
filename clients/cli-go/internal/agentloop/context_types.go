package agentloop

import (
	"errors"
	"fmt"
	"time"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/modelclient"
)

const (
	ContextCompactionAuto       = "auto"
	ContextCompactionRecentOnly = "recent-only"
	ContextCompactionOff        = "off"

	ContextBudgetInvalid       = "context_budget_invalid"
	ContextTurnTooLarge        = "context_turn_too_large"
	ContextRecentTurnsTooLarge = "context_recent_turns_too_large"
	ContextObserverFailed      = "context_observer_failed"
	ContextReflectorFailed     = "context_reflector_failed"
	ContextSourceUnavailable   = "context_source_unavailable"
	ContextCompactionDegraded  = "context_compaction_degraded"
	ContextMemoryNotFound      = "context_memory_not_found"

	maxContextSourceRecallBytes = 16 << 10
	maxContextWarmEvidenceBytes = 32 << 20
)

// ContextError is a stable, machine-readable failure raised before a model
// request is sent when the safe context budget cannot be satisfied.
type ContextError struct {
	Code string
	Err  error
}

func (e *ContextError) Error() string {
	if e == nil {
		return ""
	}
	if e.Err == nil {
		return e.Code
	}
	return fmt.Sprintf("%s: %v", e.Code, e.Err)
}

func (e *ContextError) Unwrap() error { return e.Err }

func contextError(code, message string) error {
	return &ContextError{Code: code, Err: errors.New(message)}
}

type SourceKind string

const (
	SourceUser      SourceKind = "user"
	SourceAssistant SourceKind = "assistant"
	SourceTool      SourceKind = "tool"
)

type RetentionClass string

const (
	RetentionHot      RetentionClass = "hot"
	RetentionWarm     RetentionClass = "warm"
	RetentionMetadata RetentionClass = "metadata_only"
)

type AuthorityClass string

const (
	AuthoritySessionStatement  AuthorityClass = "session_statement"
	AuthorityServerSnapshot    AuthorityClass = "server_snapshot"
	AuthorityServerReference   AuthorityClass = "server_reference"
	AuthorityWorkspaceSnapshot AuthorityClass = "workspace_snapshot"
)

type FreshnessClass string

const (
	FreshnessSessionCurrent      FreshnessClass = "session_current"
	FreshnessHistorical          FreshnessClass = "historical_snapshot"
	FreshnessInvalidated         FreshnessClass = "invalidated"
	FreshnessWorkspaceObserved   FreshnessClass = "workspace_observed"
	FreshnessWorkspaceSuperseded FreshnessClass = "workspace_superseded"
)

type Relevance string

const (
	RelevanceLow      Relevance = "low"
	RelevanceMedium   Relevance = "medium"
	RelevanceHigh     Relevance = "high"
	RelevanceCritical Relevance = "critical"
)

type ObservationKind string

const (
	ObservationUserIntent     ObservationKind = "user_intent"
	ObservationUserConstraint ObservationKind = "user_constraint"
	ObservationCorrection     ObservationKind = "correction"
	ObservationDecision       ObservationKind = "decision"
	ObservationCompletion     ObservationKind = "completion"
	ObservationOpenQuestion   ObservationKind = "open_question"
	ObservationToolSnapshot   ObservationKind = "tool_snapshot"
	ObservationFailure        ObservationKind = "failure"
	ObservationPreferenceFlow ObservationKind = "preference_flow"
)

type ReflectionKind string

const (
	ReflectionUserIntent     ReflectionKind = "user_intent"
	ReflectionUserConstraint ReflectionKind = "user_constraint"
	ReflectionCorrection     ReflectionKind = "correction"
	ReflectionDecision       ReflectionKind = "decision"
	ReflectionCompletion     ReflectionKind = "completion"
	ReflectionOpenBlocker    ReflectionKind = "open_blocker"
	ReflectionServerState    ReflectionKind = "server_state"
	ReflectionWorkspaceState ReflectionKind = "workspace_state"
	ReflectionPreferenceFlow ReflectionKind = "preference_flow"
)

type CoverageFidelity string

const (
	CoveragePartial CoverageFidelity = "partial"
	CoverageExact   CoverageFidelity = "exact"
)

type DropReason string

const (
	DropSuperseded    DropReason = "superseded"
	DropExactCoverage DropReason = "exact_coverage"
	DropDuplicate     DropReason = "duplicate_identity"
	DropLowValue      DropReason = "low_value_expiry"
	DropNewerSnapshot DropReason = "newer_versioned_snapshot"
)

// ServerReference records only version/revision metadata needed to keep a
// tool-derived source honest about being a historical server snapshot.
type ServerReference struct {
	Tool              string
	Entity            string
	EntityID          string
	Revision          string
	Version           int64
	Generation        int64
	LearnerGeneration int64
	MemoryGeneration  int64
}

func (r *ServerReference) Identity() string {
	if r == nil {
		return ""
	}
	return r.Tool + "\x00" + r.Entity + "\x00" + r.EntityID
}

type SourceEntry struct {
	ID                 string
	TurnID             string
	Kind               SourceKind
	CreatedAt          time.Time
	ModelMessage       modelclient.Message
	HasModelMessage    bool
	RecallText         string
	ContentHash        string
	SourceAvailable    bool
	TokenEstimate      int
	Retention          RetentionClass
	Authority          AuthorityClass
	Freshness          FreshnessClass
	ServerReference    *ServerReference
	WorkspaceReference *WorkspaceReference
}

type Observation struct {
	ID             string
	Content        string
	CreatedAt      time.Time
	Relevance      Relevance
	Kind           ObservationKind
	SourceEntryIDs []string
	Authority      AuthorityClass
	Freshness      FreshnessClass
	TokenEstimate  int
}

type CoverageEdge struct {
	ObservationID string
	Fidelity      CoverageFidelity
}

type Reflection struct {
	ID            string
	Content       string
	Kind          ReflectionKind
	Support       []CoverageEdge
	Authority     AuthorityClass
	Freshness     FreshnessClass
	CreatedAt     time.Time
	TokenEstimate int
}

type Supersession struct {
	OlderObservationID string
	NewerObservationID string
	Reason             string
}

type ObservationTombstone struct {
	ObservationID string
	Reason        DropReason
	DroppedAt     time.Time
}

// ToolResultProjection separates the representation used in the active tool
// chain from compact history and future exact-ID recall representations.
type ToolResultProjection struct {
	Live               string
	History            string
	Recall             string
	ServerReference    *ServerReference
	WorkspaceReference *WorkspaceReference
}

type ContextMemoryProjection struct {
	Instruction string
	Items       []string
}

type ContextStatus struct {
	Estimated             bool
	WindowPercent         int
	CurrentTokens         int
	ContextWindow         int
	CachePromptTokens     int64
	CacheReadTokens       int64
	CacheHitRate          float64
	CacheHitRateAvailable bool
	RecentCompleteTurns   int
	ObservationCount      int
	ReflectionCount       int
	MemoryItemCount       int
	Mode                  string
	Phase                 string
	Degraded              bool
	DegradedCode          string
}

type ContextEventKind string

const (
	ContextEventPrepared          ContextEventKind = "prepared"
	ContextEventStatus            ContextEventKind = "status"
	ContextEventCompacted         ContextEventKind = "compacted"
	ContextEventDegraded          ContextEventKind = "degraded"
	ContextEventSourceUnavailable ContextEventKind = "source_unavailable"
)

type ContextEvent struct {
	Kind             ContextEventKind
	Code             string
	Phase            string
	TotalTurns       int
	SelectedTurns    int
	DroppedTurns     int
	RecentTurns      int
	ObservationCount int
	ReflectionCount  int
	MemoryItemCount  int
	Status           ContextStatus
}

type ContextPlan struct {
	ProjectedTurns  int
	Request         modelclient.Request
	EstimatedInput  int
	ReservedOutput  int
	SafetyMargin    int
	SoftPressure    bool
	TotalTurns      int
	SelectedTurns   int
	DroppedTurns    int
	MemoryItemCount int
	UsedMemory      bool
}
