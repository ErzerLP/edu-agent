package learning

import (
	"context"
	"time"
)

// 独立读模型保留旧 CLI 严格 DTO，不向 Activity、Attempt 或 Session 添加字段。
type FeedbackQuery struct {
	Status    string
	SessionID string
	Page      CursorPageRequest
}

type FeedbackSummary struct {
	AttemptID    string      `json:"attempt_id"`
	ActivityID   string      `json:"activity_id"`
	SessionID    string      `json:"session_id"`
	AssessmentID string      `json:"assessment_id,omitempty"`
	ReceivedAt   time.Time   `json:"received_at"`
	Status       string      `json:"status"`
	Disposition  Disposition `json:"disposition,omitempty"`
}

type FeedbackPage struct {
	Items      []FeedbackSummary `json:"items"`
	NextCursor string            `json:"next_cursor,omitempty"`
}

type FeedbackReceipt struct {
	OperationID   string    `json:"operation_id"`
	EventSequence int64     `json:"event_seq"`
	ReceivedAt    time.Time `json:"received_at"`
}

type FeedbackContent struct {
	ArtifactID string `json:"artifact_id"`
	Version    int64  `json:"version"`
}

type FeedbackView struct {
	LearningSpaceID    string               `json:"learning_space_id"`
	SessionVersion     int64                `json:"session_version"`
	Status             string               `json:"status"`
	Goal               GoalRevision         `json:"goal_revision"`
	Activity           Activity             `json:"activity"`
	Attempt            Attempt              `json:"attempt"`
	Receipt            FeedbackReceipt      `json:"receipt"`
	Content            *FeedbackContent     `json:"content,omitempty"`
	KnowledgeContextID string               `json:"knowledge_context_revision_id,omitempty"`
	Assessment         *AssessmentArtifact  `json:"assessment,omitempty"`
	Decisions          []AssessmentDecision `json:"decisions"`
	Evidence           []AcceptedEvidence   `json:"evidence"`
	Reasons            []string             `json:"reasons"`
	AllowedDecisions   []string             `json:"allowed_decisions"`
}

type FeedbackReader interface {
	ListFeedback(context.Context, FeedbackQuery) (FeedbackPage, error)
	Feedback(context.Context, string, bool) (FeedbackView, error)
}

func (s *Service) ListFeedback(ctx context.Context, q FeedbackQuery) (FeedbackPage, error) {
	if store, ok := s.queries.(FeedbackReader); ok {
		return store.ListFeedback(ctx, q)
	}
	return FeedbackPage{}, &Error{Code: CodeProjectionUnavailable}
}

func (s *Service) Feedback(ctx context.Context, id string, byAssessment bool) (FeedbackView, error) {
	if store, ok := s.queries.(FeedbackReader); ok {
		return store.Feedback(ctx, id, byAssessment)
	}
	return FeedbackView{}, &Error{Code: CodeProjectionUnavailable}
}

func FeedbackDecisions(activity Activity, attempt Attempt, artifact AssessmentArtifact, decision AssessmentDecision) []string {
	allowed := []string{}
	if decision.Disposition == DispositionVoided {
		return allowed
	}
	eligible := attempt.EvidenceEligibility && attempt.EvidenceIneligibleReason == "" && attempt.Help != HelpAnswerRevealed
	if eligible && activity.Type == ActivityOpen {
		if decision.Disposition == DispositionProvisional && ConfirmableAssessment(activity, attempt, artifact) {
			allowed = append(allowed, "confirm")
		}
		allowed = append(allowed, "override")
	}
	return append(allowed, "void")
}
