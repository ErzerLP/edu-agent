package agentloop

import (
	"context"
	"errors"
	"time"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/api"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/modelclient"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/workspace"
)

type Model interface {
	Complete(context.Context, modelclient.Request) (modelclient.Response, error)
}

// StreamingModel is an optional foreground capability. Production conversations
// prefer it when available, while Observer/Reflector and simple test fakes keep
// using Model.Complete.
type StreamingModel interface {
	Stream(context.Context, modelclient.Request, func(modelclient.StreamEvent) error) (modelclient.Response, error)
}

type Server interface {
	RetrieveKnowledge(context.Context, api.KnowledgeRetrievalRequest) (api.KnowledgeRetrievalResult, error)
	CurrentSession(context.Context) (api.SessionView, error)
	Reviews(context.Context, string, int, *time.Time) (api.ReviewsPage, error)
	ExportMemory(context.Context, string, int) (api.MemoryExportPage, error)
	MemoryCandidate(context.Context, string) (api.MemoryCandidateView, error)
	CreateMemoryCandidate(context.Context, api.MemoryCandidateRequest) (api.MemoryOperationResponse, error)
	DecideMemoryCandidate(context.Context, string, api.MemoryCandidateDecisionRequest) (api.MemoryOperationResponse, error)
}

type LearningStatus struct {
	Active bool
	View   api.SessionView
}

type UUIDSource func() (string, error)
type ContextIDSource func(string) (string, error)

type WorkspaceStatus = workspace.Status
type WorkspaceReference = workspace.Reference

type DurabilitySink interface {
	BeginTurn(context.Context, DirtyIntent) error
	BeforePreferenceWrite(context.Context, PreferenceWriteAhead) error
	BeforeFilePublication(context.Context, FileWriteAhead) error
}

type DirtyIntent struct {
	TurnSequence      uint64
	OperationClass    string
	MayHaveSideEffect bool
}

type SessionSwitchState struct {
	ActiveTurn          bool
	PendingQuestion     bool
	PendingPreference   bool
	PendingFileMutation bool
	Resolving           bool
	Closed              bool
}

type PreferenceStage string

const (
	PreferenceStageCreate PreferenceStage = "create"
	PreferenceStageAdmit  PreferenceStage = "admit"
	PreferenceStageReject PreferenceStage = "reject"
)

type PreferenceOutcome string

const (
	PreferenceOutcomeUnknown   PreferenceOutcome = "unknown"
	PreferenceOutcomeCompleted PreferenceOutcome = "completed"
	PreferenceOutcomeRejected  PreferenceOutcome = "rejected"
)

type PreferencePayload struct {
	Content     string
	Reason      string
	Category    string
	Sensitivity string
	Stability   string
	ValidUntil  time.Time
}

type PreferenceWriteAhead struct {
	ToolCallID        string
	CreateOperationID string
	AdmitOperationID  string
	RejectOperationID string
	Payload           PreferencePayload
	CandidateID       string
	CandidateRevision int64
	Stage             PreferenceStage
	StableCode        string
	Outcome           PreferenceOutcome
}

const (
	FilePublicationCompletedCode = "workspace_write_completed"
	FilePublicationUnknownCode   = "workspace_write_outcome_unknown"
)

type FileWriteAhead struct {
	ToolCallID         string
	Operation          string
	Path               string
	Kind               string
	ContentHash        string
	InvalidateObserved bool
	StableCode         string
	PublicationOutcome workspace.PublicationOutcome
}

type Options struct {
	ContextWindow     int
	MaxToolRounds     int
	ContextCompaction string
	ReasoningEffort   modelclient.ReasoningEffort
	ModelTimeout      time.Duration
	ToolTimeout       time.Duration
	Now               func() time.Time
	NewUUID           UUIDSource
	ContextIDSource   ContextIDSource
	Workspace         workspace.Executor
	WorkspaceStatus   WorkspaceStatus
	Durability        DurabilitySink
}

type EventStatus string

const (
	EventRunning              EventStatus = "running"
	EventSucceeded            EventStatus = "succeeded"
	EventFailed               EventStatus = "failed"
	EventInvalid              EventStatus = "invalid"
	EventConfirmationRequired EventStatus = "confirmation_required"
	EventOutcomeUnknown       EventStatus = "outcome_unknown"
)

type Event struct {
	ID      string
	Tool    string
	Summary string
	Status  EventStatus
	Detail  string
}

type ActivityKind string

const (
	ActivityThinking  ActivityKind = "thinking"
	ActivityTool      ActivityKind = "tool"
	ActivityTextDelta ActivityKind = "text_delta"
)

type ActivityPhase string

const (
	ActivityPreparingContext    ActivityPhase = "preparing_context"
	ActivityWaitingModel        ActivityPhase = "waiting_model"
	ActivityReceivingStream     ActivityPhase = "receiving_stream"
	ActivityValidatingResponse  ActivityPhase = "validating_response"
	ActivityAssemblingTools     ActivityPhase = "assembling_tools"
	ActivityExecutingTool       ActivityPhase = "executing_tool"
	ActivityWaitingUser         ActivityPhase = "waiting_user"
	ActivityContinuingAfterTool ActivityPhase = "continuing_after_tools"
	ActivityStopping            ActivityPhase = "stopping"
	ActivityStopped             ActivityPhase = "stopped"
)

type ActivityProgress struct {
	Completed int
	Total     int
}

// FileActivityDetail contains bounded, presentation-safe workspace lifecycle
// data. It never contains raw tool arguments, hashes, provider reasoning,
// absolute workspace roots, or operating-system errors.
type FileActivityDetail struct {
	Path               string
	Operation          string
	Returned           int
	HasReturned        bool
	StartLine          int
	EndLine            int
	HasRange           bool
	Bytes              int64
	HasBytes           bool
	ScannedFiles       int
	ScannedBytes       int64
	HasScanned         bool
	Matches            int
	HasMatches         bool
	TruncationReason   string
	NextOffset         int
	NextByteOffset     int
	HasContinuation    bool
	PreviewKind        string
	Preview            string
	PreviewTruncated   bool
	FirstChangedLine   int
	PublicationOutcome string
}

// Activity contains presentation-safe lifecycle data only. Delta never contains
// provider reasoning, tool arguments, credentials, or raw provider errors.
type Activity struct {
	Kind            ActivityKind
	Event           Event
	Phase           ActivityPhase
	ReasoningEffort modelclient.ReasoningEffort
	TimeoutBudget   time.Duration
	StartedAt       time.Time
	UpdatedAt       time.Time
	Progress        *ActivityProgress
	File            *FileActivityDetail
	StableCode      string
	Delta           string
}

type activityReporter func(Activity)
type activityReporterContextKey struct{}

// WithActivityReporter attaches a safe, presentation-level activity sink to one Agent operation.
// Activities contain lifecycle summaries only; they never contain model reasoning or tool arguments.
func WithActivityReporter(ctx context.Context, reporter func(Activity)) context.Context {
	if reporter == nil {
		return ctx
	}
	return context.WithValue(ctx, activityReporterContextKey{}, activityReporter(reporter))
}

func PublishActivity(ctx context.Context, activity Activity) {
	if reporter, ok := ctx.Value(activityReporterContextKey{}).(activityReporter); ok && reporter != nil {
		reportActivitySafely(reporter, activity)
	}
}

func reportActivitySafely(reporter activityReporter, activity Activity) {
	defer func() {
		_ = recover()
	}()
	reporter(activity)
}

type FileAuthorizationMode string

const (
	FileAuthorizationConfirm FileAuthorizationMode = "confirm"
	FileAuthorizationYOLO    FileAuthorizationMode = "yolo"
)

type FileMutationResolution string

const (
	FileMutationApprove FileMutationResolution = "approve"
	FileMutationDecline FileMutationResolution = "decline"
)

type PendingFileMutation struct {
	CallID      string
	Tool        string
	Operation   string
	Path        string
	PreviewKind string
	Preview     string
	Truncated   bool
	BaseVersion string
}

type PreferenceConfirmation struct {
	Content     string
	Reason      string
	Category    string
	Sensitivity string
	Stability   string
	RetryOnly   bool
}

type PreferenceResolution string

const (
	PreferenceSave        PreferenceResolution = "save"
	PreferenceSessionOnly PreferenceResolution = "session_only"
	PreferenceDecline     PreferenceResolution = "decline"
	PreferenceRetry       PreferenceResolution = "retry"
)

type QuestionMode string

const (
	QuestionSingle   QuestionMode = "single"
	QuestionMultiple QuestionMode = "multiple"
)

type QuestionOption struct {
	ID          string
	Label       string
	Description string
}

type PendingQuestion struct {
	ID          string
	Header      string
	Question    string
	Mode        QuestionMode
	Options     []QuestionOption
	AllowCustom bool
}

type QuestionAnswerStatus string

const (
	QuestionAnswered    QuestionAnswerStatus = "answered"
	QuestionCancelled   QuestionAnswerStatus = "cancelled"
	QuestionUnavailable QuestionAnswerStatus = "unavailable"
)

type QuestionAnswer struct {
	QuestionID string
	Status     QuestionAnswerStatus
	OptionIDs  []string
	Custom     string
}

var (
	ErrPreferenceOutcomeUnknown = errors.New("长期偏好保存结果未知")
	ErrActiveTurn               = errors.New("当前已有进行中的Agent轮次")
)

type Result struct {
	Text                string
	Events              []Event
	Pending             *PreferenceConfirmation
	PendingQuestion     *PendingQuestion
	PendingFileMutation *PendingFileMutation
}
