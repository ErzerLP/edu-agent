package learning

import (
	"context"

	"github.com/edu-agent/edu-agent/server/internal/tutoring"
	"github.com/google/uuid"
)

// ReviewSessionSource 只保存原任务身份；新承载不修改原会话或复制 Evidence。
type ReviewSessionSource struct {
	TaskID     string `json:"task_id"`
	EvidenceID string `json:"evidence_id"`
	AttemptID  string `json:"attempt_id"`
}

func (s *Service) prepareReviewSession(ctx context.Context, command SessionCommand, session *tutoring.Session, batch *CommandBatch, expectations *[]AggregateExpectation) error {
	source := command.ReviewSource
	if uuid.Validate(source.TaskID) != nil || uuid.Validate(source.EvidenceID) != nil || uuid.Validate(source.AttemptID) != nil {
		return &Error{Code: CodeInvalidRequest}
	}
	view, err := s.Feedback(ctx, source.AttemptID, false)
	if err != nil {
		return err
	}
	activity := view.Activity
	if view.Goal.ID != command.GoalRevisionID || source.TaskID != ReviewTaskID(view.Goal.GoalID, activity.TargetNodeRevisionID) {
		return &Error{Code: CodeInvalidRequest, Reason: "review_source_mismatch"}
	}
	found := false
	for _, evidence := range view.Evidence {
		if evidence.ID == source.EvidenceID {
			found = true
		}
	}
	if !found {
		return &Error{Code: CodeInvalidTransition, Reason: "review_evidence_unavailable"}
	}
	session.State = tutoring.StateRouteActive
	session.Context = tutoring.FocusContext{GoalRevisionID: activity.GoalRevisionID, RouteRevisionID: activity.RouteRevisionID, RouteStepID: activity.RouteStepID, KnowledgeRevisionID: activity.KnowledgeRevisionID, FocusNodeRevisionID: activity.TargetNodeRevisionID}
	batch.ReviewSource = source
	// 原会话锁保证复核/作废与创建承载不会越过已读取的证据版本。
	*expectations = append(*expectations, AggregateExpectation{Type: "session", ID: activity.SessionID, ExpectedVersion: view.SessionVersion})
	return nil
}
