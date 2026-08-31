package agentloop

import (
	"context"
	"errors"
	"time"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/api"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/modelclient"
)

type Model interface {
	Complete(context.Context, modelclient.Request) (modelclient.Response, error)
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

type Options struct {
	ContextWindow     int
	MaxToolRounds     int
	ContextCompaction string
	Now               func() time.Time
	NewUUID           UUIDSource
	ContextIDSource   ContextIDSource
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
	ActivityThinking ActivityKind = "thinking"
	ActivityTool     ActivityKind = "tool"
)

type Activity struct {
	Kind  ActivityKind
	Event Event
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
		reporter(activity)
	}
}

type PreferenceConfirmation struct {
	Content     string
	Reason      string
	Category    string
	Sensitivity string
	Stability   string
	RetryOnly   bool
}

var ErrPreferenceOutcomeUnknown = errors.New("长期偏好保存结果未知")

type Result struct {
	Text    string
	Events  []Event
	Pending *PreferenceConfirmation
}
