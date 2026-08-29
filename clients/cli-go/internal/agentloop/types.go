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
	Routes(context.Context, string, int, bool) (api.RoutesPage, error)
	Reviews(context.Context, string, int, *time.Time) (api.ReviewsPage, error)
	MemoryCandidates(context.Context, string, int) (api.MemoryCandidatePage, error)
	CreateMemoryCandidate(context.Context, api.MemoryCandidateRequest) (api.MemoryOperationResponse, error)
}

type UUIDSource func() (string, error)

type Options struct {
	ContextWindow int
	MaxToolRounds int
	Now           func() time.Time
	NewUUID       UUIDSource
}

type Event struct {
	Tool    string
	Summary string
}

type PreferenceConfirmation struct {
	Content     string
	Reason      string
	Category    string
	Sensitivity string
	Stability   string
	RetryOnly   bool
}

var ErrPreferenceOutcomeUnknown = errors.New("长期偏好候选提交结果未知")

type Result struct {
	Text    string
	Events  []Event
	Pending *PreferenceConfirmation
}
