package tutoring

import (
	"errors"
	"reflect"
	"testing"
)

func TestTransitionMatrix(t *testing.T) {
	states := []State{
		StateIdle, StateGoalReady, StateDiagnostic, StateRouteActive, StateActivityIssued,
		StateAwaitingResponse, StateEvaluating, StateFeedback, StateAdvanceOrReview,
		StateCompleted, StateFocusSuspended, StateFreeQuestion, StateFreeAnswer, StateFocusResumed,
	}
	tests := []struct {
		action  Action
		allowed map[State]State
	}{
		{ActionCreateSession, map[State]State{StateIdle: StateGoalReady}},
		{ActionStartDiagnostic, map[State]State{StateGoalReady: StateDiagnostic}},
		{ActionApplyRoute, map[State]State{StateDiagnostic: StateRouteActive, StateRouteActive: StateRouteActive}},
		{ActionIssueActivity, map[State]State{StateRouteActive: StateActivityIssued}},
		{ActionPresentReview, map[State]State{StateRouteActive: StateActivityIssued}},
		{ActionPresentActivity, map[State]State{StateActivityIssued: StateAwaitingResponse}},
		{ActionSubmitAttempt, map[State]State{StateAwaitingResponse: StateEvaluating}},
		{ActionRecordAssessment, map[State]State{StateEvaluating: StateFeedback}},
		{ActionAcknowledgeFeedback, map[State]State{StateFeedback: StateRouteActive}},
		{ActionRecordExposure, map[State]State{StateRouteActive: StateRouteActive}},
		{ActionAskFreeQuestion, map[State]State{StateRouteActive: StateFreeQuestion, StateActivityIssued: StateFreeQuestion, StateAwaitingResponse: StateFreeQuestion, StateFreeAnswer: StateFreeQuestion}},
		{ActionRecordFreeAnswer, map[State]State{StateFreeQuestion: StateFreeAnswer}},
		{ActionResumeFocus, map[State]State{StateFreeQuestion: StateRouteActive, StateFreeAnswer: StateRouteActive}},
		{ActionConvertFreeAnswerToQuiz, map[State]State{StateFreeAnswer: StateActivityIssued}},
		{ActionEndActivity, map[State]State{StateActivityIssued: StateRouteActive, StateAwaitingResponse: StateRouteActive, StateEvaluating: StateRouteActive, StateFeedback: StateRouteActive}},
		{ActionSwitchGoal, map[State]State{
			StateIdle: StateGoalReady, StateGoalReady: StateGoalReady, StateDiagnostic: StateGoalReady,
			StateRouteActive: StateGoalReady, StateActivityIssued: StateGoalReady,
			StateAwaitingResponse: StateGoalReady, StateEvaluating: StateGoalReady,
			StateFeedback: StateGoalReady, StateAdvanceOrReview: StateGoalReady,
			StateFocusSuspended: StateGoalReady, StateFreeQuestion: StateGoalReady,
			StateFreeAnswer: StateGoalReady, StateFocusResumed: StateGoalReady,
		}},
		{ActionCompleteSession, map[State]State{StateRouteActive: StateCompleted, StateAdvanceOrReview: StateCompleted}},
	}
	for _, test := range tests {
		t.Run(string(test.action), func(t *testing.T) {
			for _, state := range states {
				session := matrixSession(state)
				result, err := Apply(session, Command{Action: test.action, SessionID: session.ID, FrameID: "frame-1", CreatedEventSequence: 10})
				want, allowed := test.allowed[state]
				if !allowed {
					if !errors.Is(err, ErrInvalidTransition) && !errors.Is(err, ErrFocusFrameInvalid) {
						t.Errorf("state %s: error=%v, want invalid transition", state, err)
					}
					continue
				}
				if err != nil {
					t.Errorf("state %s: unexpected error: %v", state, err)
					continue
				}
				if result.After != want {
					t.Errorf("state %s: after=%s, want %s", state, result.After, want)
				}
			}
		})
	}
}

func matrixSession(state State) Session {
	activity, attempt := "activity-1", "attempt-1"
	session := Session{
		ID: "session-1", State: state, AggregateVer: 7,
		Context: FocusContext{
			GoalRevisionID: "goal-rev-1", RouteRevisionID: "route-rev-1", RouteStepID: "step-1",
			KnowledgeRevisionID: "knowledge-rev-1", FocusNodeRevisionID: "node-rev-1",
			ActivityID: &activity, AttemptID: &attempt,
		},
	}
	if state == StateFreeQuestion || state == StateFreeAnswer {
		session.ActiveFrame = &FocusFrame{ID: "frame-1", SessionID: session.ID, SavedState: StateRouteActive, Context: cloneContext(session.Context)}
	}
	return session
}

func TestFocusFrameSavesAndRestoresEveryAllowedSource(t *testing.T) {
	for _, saved := range []State{StateRouteActive, StateActivityIssued, StateAwaitingResponse} {
		t.Run(string(saved), func(t *testing.T) {
			session := matrixSession(saved)
			original := cloneContext(session.Context)
			asked, err := Apply(session, Command{Action: ActionAskFreeQuestion, SessionID: session.ID, FrameID: "frame-9", CreatedEventSequence: 51})
			if err != nil {
				t.Fatal(err)
			}
			frame := asked.Session.ActiveFrame
			if frame == nil || frame.SavedState != saved || frame.SavedAggregateVersion != 7 || frame.CreatedEventSequence != 51 || !reflect.DeepEqual(frame.Context, original) {
				t.Fatalf("frame did not freeze source context: %+v", frame)
			}
			asked.Session.Context.RouteRevisionID = "changed-route"
			answered, err := Apply(asked.Session, Command{Action: ActionRecordFreeAnswer, SessionID: session.ID})
			if err != nil {
				t.Fatal(err)
			}
			resumed, err := Apply(answered.Session, Command{Action: ActionResumeFocus, SessionID: session.ID})
			if err != nil {
				t.Fatal(err)
			}
			if resumed.After != saved || resumed.Session.ActiveFrame != nil || !reflect.DeepEqual(resumed.Session.Context, original) {
				t.Fatalf("resume did not restore exact context: %+v", resumed)
			}
		})
	}
}

func TestFreeQuestionFollowUpReusesFrame(t *testing.T) {
	session := matrixSession(StateRouteActive)
	asked, err := Apply(session, Command{Action: ActionAskFreeQuestion, FrameID: "frame-1"})
	if err != nil {
		t.Fatal(err)
	}
	answered, err := Apply(asked.Session, Command{Action: ActionRecordFreeAnswer})
	if err != nil {
		t.Fatal(err)
	}
	followup, err := Apply(answered.Session, Command{Action: ActionAskFreeQuestion, FrameID: "frame-2"})
	if err != nil {
		t.Fatal(err)
	}
	if followup.Session.ActiveFrame == nil || followup.Session.ActiveFrame.ID != "frame-1" {
		t.Fatalf("follow-up nested or replaced frame: %+v", followup.Session.ActiveFrame)
	}
}

func TestAttachedQuizReturnsToFreeAnswerUntilExplicitResume(t *testing.T) {
	session := matrixSession(StateFreeAnswer)
	converted, err := Apply(session, Command{Action: ActionConvertFreeAnswerToQuiz})
	if err != nil {
		t.Fatal(err)
	}
	current := converted.Session
	for _, action := range []Action{ActionPresentActivity, ActionSubmitAttempt, ActionRecordAssessment, ActionAcknowledgeFeedback} {
		result, err := Apply(current, Command{Action: action})
		if err != nil {
			t.Fatalf("%s: %v", action, err)
		}
		current = result.Session
	}
	if current.State != StateFreeAnswer || current.ActiveFrame == nil {
		t.Fatalf("attached quiz did not return to active free-answer frame: %+v", current)
	}
}

func TestFocusFrameInvalidationIsPermanent(t *testing.T) {
	for _, action := range []Action{ActionEndActivity, ActionSwitchGoal} {
		t.Run(string(action), func(t *testing.T) {
			session := matrixSession(StateActivityIssued)
			session.ActiveFrame = &FocusFrame{ID: "frame-1", SavedState: StateActivityIssued, Context: cloneContext(session.Context)}
			result, err := Apply(session, Command{Action: action})
			if err != nil {
				t.Fatal(err)
			}
			if result.Session.ActiveFrame == nil || !result.Session.ActiveFrame.Invalidated {
				t.Fatalf("frame was not invalidated: %+v", result.Session.ActiveFrame)
			}
			result.Session.State = StateFreeAnswer
			if _, err := Apply(result.Session, Command{Action: ActionResumeFocus}); !errors.Is(err, ErrFocusFrameInvalid) {
				t.Fatalf("invalidated frame resumed: %v", err)
			}
		})
	}
}

func TestCompletedSessionIsTerminal(t *testing.T) {
	for _, action := range []Action{ActionCreateSession, ActionSwitchGoal, ActionCompleteSession, ActionAskFreeQuestion, ActionRecordExposure} {
		if _, err := Apply(matrixSession(StateCompleted), Command{Action: action}); !errors.Is(err, ErrInvalidTransition) {
			t.Errorf("%s from completed: %v", action, err)
		}
	}
}
