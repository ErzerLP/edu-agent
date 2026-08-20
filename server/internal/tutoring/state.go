package tutoring

import (
	"errors"
	"fmt"
)

type State string

const (
	StateIdle             State = "Idle"
	StateGoalReady        State = "GoalReady"
	StateDiagnostic       State = "Diagnostic"
	StateRouteActive      State = "RouteActive"
	StateActivityIssued   State = "ActivityIssued"
	StateAwaitingResponse State = "AwaitingResponse"
	StateEvaluating       State = "Evaluating"
	StateFeedback         State = "Feedback"
	StateAdvanceOrReview  State = "AdvanceOrReview"
	StateCompleted        State = "Completed"
	StateFocusSuspended   State = "FocusSuspended"
	StateFreeQuestion     State = "FreeQuestion"
	StateFreeAnswer       State = "FreeAnswer"
	StateFocusResumed     State = "FocusResumed"
)

type Action string

const (
	ActionCreateSession           Action = "create_session"
	ActionStartDiagnostic         Action = "start_diagnostic"
	ActionApplyRoute              Action = "apply_route"
	ActionIssueActivity           Action = "issue_activity"
	ActionPresentActivity         Action = "present_activity"
	ActionSubmitAttempt           Action = "submit_attempt"
	ActionRecordAssessment        Action = "record_assessment"
	ActionAcknowledgeFeedback     Action = "acknowledge_feedback"
	ActionPresentReview           Action = "present_review"
	ActionRecordExposure          Action = "record_exposure"
	ActionAskFreeQuestion         Action = "ask_free_question"
	ActionRecordFreeAnswer        Action = "record_free_answer"
	ActionConvertFreeAnswerToQuiz Action = "convert_free_answer_to_quiz"
	ActionResumeFocus             Action = "resume_focus"
	ActionEndActivity             Action = "end_activity"
	ActionSwitchGoal              Action = "switch_goal"
	ActionCompleteSession         Action = "complete_session"
)

var (
	ErrInvalidTransition = errors.New("invalid transition")
	ErrFocusFrameInvalid = errors.New("focus frame invalidated")
	ErrFocusFrameExists  = errors.New("active focus frame already exists")
	ErrSessionOwnership  = errors.New("session ownership conflict")
)

type FocusContext struct {
	GoalRevisionID      string  `json:"goal_revision_id,omitempty"`
	RouteRevisionID     string  `json:"route_revision_id,omitempty"`
	RouteStepID         string  `json:"route_step_id,omitempty"`
	KnowledgeRevisionID string  `json:"knowledge_revision_id,omitempty"`
	FocusNodeRevisionID string  `json:"focus_node_revision_id,omitempty"`
	ActivityID          *string `json:"activity_id,omitempty"`
	AttemptID           *string `json:"attempt_id,omitempty"`
}

type FocusFrame struct {
	ID                    string       `json:"focus_frame_id"`
	SessionID             string       `json:"session_id"`
	SavedState            State        `json:"saved_state"`
	Context               FocusContext `json:"context"`
	SavedAggregateVersion int64        `json:"saved_aggregate_version"`
	CreatedEventSequence  int64        `json:"created_event_seq"`
	Invalidated           bool         `json:"invalidated"`
	InvalidationReason    string       `json:"invalidation_reason,omitempty"`
}

type Session struct {
	ID             string       `json:"session_id"`
	State          State        `json:"state"`
	AggregateVer   int64        `json:"aggregate_version"`
	Context        FocusContext `json:"focus"`
	ActiveFrame    *FocusFrame  `json:"active_focus_frame,omitempty"`
	AttachedQuiz   bool         `json:"attached_quiz"`
	CompletedRoute bool         `json:"completed_route"`
}

type Command struct {
	Action               Action
	SessionID            string
	FrameID              string
	CreatedEventSequence int64
	Complete             bool
	Context              *FocusContext
}

type Transition struct {
	Before       State    `json:"before"`
	Intermediate []State  `json:"intermediate,omitempty"`
	After        State    `json:"after"`
	Events       []string `json:"events"`
	Session      Session  `json:"session"`
}

func Apply(input Session, command Command) (Transition, error) {
	if input.State == "" {
		input.State = StateIdle
	}
	if command.SessionID != "" && input.ID != "" && command.SessionID != input.ID {
		return Transition{}, ErrSessionOwnership
	}
	before := input.State
	transition := Transition{Before: before, Session: input}
	set := func(after State, events ...string) (Transition, error) {
		transition.After = after
		transition.Events = append([]string(nil), events...)
		transition.Session.State = after
		if command.Context != nil {
			transition.Session.Context = cloneContext(*command.Context)
		}
		return transition, nil
	}
	invalid := func() (Transition, error) {
		return Transition{}, fmt.Errorf("%w: action=%s state=%s", ErrInvalidTransition, command.Action, before)
	}

	switch command.Action {
	case ActionCreateSession:
		if before != StateIdle {
			return invalid()
		}
		return set(StateGoalReady, "LearningSessionStarted", "TutoringStateChanged")
	case ActionStartDiagnostic:
		if before != StateGoalReady {
			return invalid()
		}
		return set(StateDiagnostic, "TutoringStateChanged")
	case ActionApplyRoute:
		if before != StateDiagnostic && before != StateRouteActive {
			return invalid()
		}
		return set(StateRouteActive, "RouteRevisionCreated", "TutoringStateChanged")
	case ActionIssueActivity:
		if before != StateRouteActive {
			return invalid()
		}
		transition.Session.AttachedQuiz = false
		return set(StateActivityIssued, "ActivityIssued", "TutoringStateChanged")
	case ActionPresentReview:
		if before != StateRouteActive {
			return invalid()
		}
		transition.Session.AttachedQuiz = false
		return set(StateActivityIssued, "ReviewPresented", "ActivityIssued", "TutoringStateChanged")
	case ActionPresentActivity:
		if before != StateActivityIssued {
			return invalid()
		}
		return set(StateAwaitingResponse, "ActivityPresented", "TutoringStateChanged")
	case ActionSubmitAttempt:
		if before != StateAwaitingResponse {
			return invalid()
		}
		return set(StateEvaluating, "AttemptSubmitted", "TutoringStateChanged")
	case ActionRecordAssessment:
		if before != StateEvaluating {
			return invalid()
		}
		return set(StateFeedback, "AssessmentRecorded", "TutoringStateChanged")
	case ActionAcknowledgeFeedback:
		if before != StateFeedback {
			return invalid()
		}
		transition.Intermediate = []State{StateAdvanceOrReview}
		if input.AttachedQuiz && input.ActiveFrame != nil && !input.ActiveFrame.Invalidated {
			transition.Session.AttachedQuiz = false
			return set(StateFreeAnswer, "TutoringStateChanged", "TutoringStateChanged")
		}
		if command.Complete || input.CompletedRoute {
			return set(StateCompleted, "TutoringStateChanged", "TutoringStateChanged", "LearningCompleted")
		}
		return set(StateRouteActive, "TutoringStateChanged", "RouteAdvanced", "TutoringStateChanged")
	case ActionRecordExposure:
		if before != StateRouteActive {
			return invalid()
		}
		return set(StateRouteActive, "ExposureRecorded")
	case ActionAskFreeQuestion:
		if before == StateFreeAnswer {
			if input.ActiveFrame == nil || input.ActiveFrame.Invalidated {
				return Transition{}, ErrFocusFrameInvalid
			}
			return set(StateFreeQuestion, "FreeQuestionAsked", "TutoringStateChanged")
		}
		if before != StateRouteActive && before != StateActivityIssued && before != StateAwaitingResponse {
			return invalid()
		}
		if input.ActiveFrame != nil && !input.ActiveFrame.Invalidated {
			return Transition{}, ErrFocusFrameExists
		}
		frame := &FocusFrame{
			ID: command.FrameID, SessionID: input.ID, SavedState: before, Context: cloneContext(input.Context),
			SavedAggregateVersion: input.AggregateVer, CreatedEventSequence: command.CreatedEventSequence,
		}
		transition.Session.ActiveFrame = frame
		transition.Intermediate = []State{StateFocusSuspended}
		return set(StateFreeQuestion, "FocusSuspended", "FreeQuestionAsked", "TutoringStateChanged")
	case ActionRecordFreeAnswer:
		if before != StateFreeQuestion {
			return invalid()
		}
		return set(StateFreeAnswer, "FreeAnswerRecorded", "ExposureRecorded", "TutoringStateChanged")
	case ActionResumeFocus:
		if before != StateFreeQuestion && before != StateFreeAnswer {
			return invalid()
		}
		frame := input.ActiveFrame
		if frame == nil || frame.Invalidated {
			return Transition{}, ErrFocusFrameInvalid
		}
		transition.Intermediate = []State{StateFocusResumed}
		transition.Session.Context = cloneContext(frame.Context)
		transition.Session.ActiveFrame = nil
		transition.After = frame.SavedState
		transition.Session.State = frame.SavedState
		transition.Events = []string{"FocusResumed", "TutoringStateChanged"}
		return transition, nil
	case ActionConvertFreeAnswerToQuiz:
		if before != StateFreeAnswer || input.ActiveFrame == nil || input.ActiveFrame.Invalidated {
			return invalid()
		}
		transition.Session.AttachedQuiz = true
		return set(StateActivityIssued, "ActivityIssued", "TutoringStateChanged")
	case ActionEndActivity:
		if before != StateActivityIssued && before != StateAwaitingResponse && before != StateEvaluating && before != StateFeedback {
			return invalid()
		}
		invalidateFrame(&transition.Session, "end_activity")
		transition.Session.AttachedQuiz = false
		clearActivity(&transition.Session.Context)
		return set(StateRouteActive, "ActivityEnded", "TutoringStateChanged")
	case ActionSwitchGoal:
		if before == StateCompleted {
			return invalid()
		}
		invalidateFrame(&transition.Session, "switch_goal")
		transition.Session.AttachedQuiz = false
		clearActivity(&transition.Session.Context)
		return set(StateGoalReady, "TutoringStateChanged")
	case ActionCompleteSession:
		if before != StateRouteActive && before != StateAdvanceOrReview {
			return invalid()
		}
		invalidateFrame(&transition.Session, "complete_session")
		return set(StateCompleted, "LearningCompleted", "TutoringStateChanged")
	default:
		return invalid()
	}
}

func cloneContext(value FocusContext) FocusContext {
	value.ActivityID = cloneString(value.ActivityID)
	value.AttemptID = cloneString(value.AttemptID)
	return value
}

func cloneString(value *string) *string {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func clearActivity(context *FocusContext) {
	context.ActivityID = nil
	context.AttemptID = nil
}

func invalidateFrame(session *Session, reason string) {
	if session.ActiveFrame == nil {
		return
	}
	copy := *session.ActiveFrame
	copy.Invalidated = true
	copy.InvalidationReason = reason
	session.ActiveFrame = &copy
}
