package learning

import (
	"context"
	"github.com/edu-agent/edu-agent/server/internal/tutoring"
)

// PrepareAdaptiveBoundary 仅供已复验候选的应用服务在外层事务内调用。
// 旧 ApplyAction 状态机保持不变；已完成路线可追加新的建议，不改写已学事实。
func (s *Service) PrepareAdaptiveBoundary(ctx context.Context, device string, op OperationEnvelope, immediate bool, goalRevision, scope string) (OperationResult, error) {
	if err := ValidateOperation(op); err != nil {
		return OperationResult{}, err
	}
	a, err := s.authority.LoadSessionAuthority(ctx, op.AggregateID)
	if err != nil {
		return OperationResult{}, err
	}
	v := a.Session
	if op.AggregateType != "session" || v.AggregateVer != op.ExpectedVersion {
		return OperationResult{}, &Error{Code: CodeVersionConflict}
	}
	before := v.State
	events := []string{"TutoringStateChanged"}
	switch v.State {
	case tutoring.StateGoalReady, tutoring.StateDiagnostic, tutoring.StateRouteActive, tutoring.StateCompleted:
	case tutoring.StateActivityIssued, tutoring.StateAwaitingResponse, tutoring.StateEvaluating, tutoring.StateFeedback:
		if !immediate || v.ActiveFrame != nil {
			return OperationResult{}, &Error{Code: CodeInvalidTransition}
		}
		v.ActiveFrame = &tutoring.FocusFrame{ID: s.newUUID(), SessionID: v.ID, SavedState: v.State, Context: v.Context, SavedAggregateVersion: v.AggregateVer}
		events = []string{"FocusSuspended", "TutoringStateChanged"}
	default:
		return OperationResult{}, &Error{Code: CodeInvalidTransition}
	}
	v.State, v.CompletedRoute = tutoring.StateRouteActive, false
	oldGoal, err := s.authority.LoadGoalRevision(ctx, v.Context.GoalRevisionID)
	if err != nil {
		return OperationResult{}, err
	}
	newGoal, err := s.authority.LoadGoalRevision(ctx, goalRevision)
	if err != nil {
		return OperationResult{}, err
	}
	if oldGoal.GoalID != newGoal.GoalID || oldGoal.LearningSpaceID() != newGoal.LearningSpaceID() {
		return OperationResult{}, &Error{Code: CodeInvalidRequest}
	}
	if v.Context.GoalRevisionID != goalRevision || v.Context.KnowledgeRevisionID != scope {
		v.Context.RouteRevisionID = ""
		v.Context.RouteStepID = ""
		v.Context.FocusNodeRevisionID = ""
	}
	v.Context.GoalRevisionID, v.Context.KnowledgeRevisionID = goalRevision, scope
	v.Context.ActivityID, v.Context.AttemptID = nil, nil
	batch := CommandBatch{Session: &v, FocusFrame: v.ActiveFrame, TutoringState: string(v.State), ResultSession: true}
	batch.Events = transitionDrafts(v.ID, tutoring.Transition{Before: before, After: v.State, Session: v, Events: events}, nil)
	return s.commit(ctx, device, op, []AggregateExpectation{{Type: "session", ID: v.ID, ExpectedVersion: op.ExpectedVersion}}, batch, struct{ Immediate bool }{immediate}, s.now().UTC())
}

// ResumeAdaptiveFocus 恢复原题和已提交答案的状态，不重新提交答案或结算证据。
func (s *Service) ResumeAdaptiveFocus(ctx context.Context, device string, op OperationEnvelope, frame string) (OperationResult, error) {
	if err := ValidateOperation(op); err != nil {
		return OperationResult{}, err
	}
	a, err := s.authority.LoadSessionAuthority(ctx, op.AggregateID)
	if err != nil {
		return OperationResult{}, err
	}
	v := a.Session
	if op.AggregateType != "session" || v.AggregateVer != op.ExpectedVersion {
		return OperationResult{}, &Error{Code: CodeVersionConflict}
	}
	if v.ActiveFrame == nil || v.ActiveFrame.Invalidated || v.ActiveFrame.ID != frame {
		return OperationResult{}, &Error{Code: CodeFocusFrameInvalidated}
	}
	// 新题已经提交时先处理它，避免丢失正在结算的答案。
	if v.State == tutoring.StateEvaluating || v.State == tutoring.StateFeedback || v.State == tutoring.StateFreeQuestion || v.State == tutoring.StateFreeAnswer {
		return OperationResult{}, &Error{Code: CodeInvalidTransition}
	}
	before := v.State
	v.State, v.Context = v.ActiveFrame.SavedState, v.ActiveFrame.Context
	v.ActiveFrame, v.CompletedRoute = nil, false
	batch := CommandBatch{Session: &v, ResumeFrame: true, TutoringState: string(v.State), ResultSession: true}
	batch.Events = transitionDrafts(v.ID, tutoring.Transition{Before: before, After: v.State, Session: v, Events: []string{"FocusResumed", "TutoringStateChanged"}}, nil)
	return s.commit(ctx, device, op, []AggregateExpectation{{Type: "session", ID: v.ID, ExpectedVersion: op.ExpectedVersion}}, batch, struct{ Frame string }{frame}, s.now().UTC())
}
