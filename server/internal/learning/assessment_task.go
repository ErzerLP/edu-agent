package learning

import (
	"context"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/identity"
)

type assessmentActorKey struct{}

// WithAssessmentActor carries the authenticated submitter without changing the
// public action protocol. Only mapped exercises require this authority.
func WithAssessmentActor(ctx context.Context, actor identity.Credential) context.Context {
	return context.WithValue(ctx, assessmentActorKey{}, actor)
}
func AssessmentActor(ctx context.Context) (identity.Credential, bool) {
	actor, ok := ctx.Value(assessmentActorKey{}).(identity.Credential)
	return actor, ok
}

type AssessmentTask struct {
	AttemptID      string
	ExerciseID     string
	DeviceID       string
	TokenID        string
	SpaceID        string
	SessionID      string
	SessionVersion int64
	RequestID      string
	OperationID    string
	RunID          string
	FailureReason  string
}

// Recovery is an observation, never permission to resend an unknown paid call.
type AssessmentRecovery struct {
	OperationStatus    string
	ProposalStatus     string
	ProposalID         string
	ProposalLeaseUntil *time.Time
}
