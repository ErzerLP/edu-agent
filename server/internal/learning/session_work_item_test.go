package learning

import (
	"reflect"
	"testing"

	"github.com/edu-agent/edu-agent/server/internal/tutoring"
)

func TestWorkItemActionsCanonicalMatrix(t *testing.T) {
	tests := []struct {
		name      string
		state     tutoring.State
		context   WorkItemActionContext
		actions   []tutoring.Action
		decisions []string
	}{
		{"goal ready", tutoring.StateGoalReady, WorkItemActionContext{}, []tutoring.Action{tutoring.ActionStartDiagnostic, tutoring.ActionSwitchGoal}, []string{}},
		{"diagnostic", tutoring.StateDiagnostic, WorkItemActionContext{}, []tutoring.Action{tutoring.ActionApplyRoute, tutoring.ActionSwitchGoal}, []string{}},
		{"route active", tutoring.StateRouteActive, WorkItemActionContext{}, []tutoring.Action{tutoring.ActionApplyRoute, tutoring.ActionIssueActivity, tutoring.ActionRecordExposure, tutoring.ActionAskFreeQuestion, tutoring.ActionCompleteSession, tutoring.ActionSwitchGoal}, []string{}},
		{"route active due review", tutoring.StateRouteActive, WorkItemActionContext{DueReview: true}, []tutoring.Action{tutoring.ActionApplyRoute, tutoring.ActionIssueActivity, tutoring.ActionPresentReview, tutoring.ActionRecordExposure, tutoring.ActionAskFreeQuestion, tutoring.ActionCompleteSession, tutoring.ActionSwitchGoal}, []string{}},
		{"activity issued", tutoring.StateActivityIssued, WorkItemActionContext{}, []tutoring.Action{tutoring.ActionPresentActivity, tutoring.ActionAskFreeQuestion, tutoring.ActionEndActivity, tutoring.ActionSwitchGoal}, []string{}},
		{"awaiting response", tutoring.StateAwaitingResponse, WorkItemActionContext{}, []tutoring.Action{tutoring.ActionSubmitAttempt, tutoring.ActionAskFreeQuestion, tutoring.ActionEndActivity, tutoring.ActionSwitchGoal}, []string{}},
		{"evaluating", tutoring.StateEvaluating, WorkItemActionContext{}, []tutoring.Action{tutoring.ActionRecordAssessment, tutoring.ActionEndActivity, tutoring.ActionSwitchGoal}, []string{}},
		{"provisional confirmable", tutoring.StateFeedback, WorkItemActionContext{AssessmentDisposition: DispositionProvisional, AssessmentConfirmable: true}, []tutoring.Action{}, []string{"confirm", "override", "void"}},
		{"provisional not confirmable", tutoring.StateFeedback, WorkItemActionContext{AssessmentDisposition: DispositionProvisional}, []tutoring.Action{}, []string{"override", "void"}},
		{"accepted", tutoring.StateFeedback, WorkItemActionContext{AssessmentDisposition: DispositionAccepted}, []tutoring.Action{tutoring.ActionAcknowledgeFeedback, tutoring.ActionEndActivity, tutoring.ActionSwitchGoal}, []string{"override", "void"}},
		{"overridden", tutoring.StateFeedback, WorkItemActionContext{AssessmentDisposition: DispositionOverridden}, []tutoring.Action{tutoring.ActionAcknowledgeFeedback, tutoring.ActionEndActivity, tutoring.ActionSwitchGoal}, []string{"override", "void"}},
		{"voided", tutoring.StateFeedback, WorkItemActionContext{AssessmentDisposition: DispositionVoided}, []tutoring.Action{tutoring.ActionAcknowledgeFeedback, tutoring.ActionEndActivity, tutoring.ActionSwitchGoal}, []string{}},
		{"free question", tutoring.StateFreeQuestion, WorkItemActionContext{}, []tutoring.Action{tutoring.ActionRecordFreeAnswer, tutoring.ActionResumeFocus, tutoring.ActionSwitchGoal}, []string{}},
		{"free answer", tutoring.StateFreeAnswer, WorkItemActionContext{}, []tutoring.Action{tutoring.ActionAskFreeQuestion, tutoring.ActionConvertFreeAnswerToQuiz, tutoring.ActionResumeFocus, tutoring.ActionSwitchGoal}, []string{}},
		{"completed", tutoring.StateCompleted, WorkItemActionContext{}, []tutoring.Action{}, []string{}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			actions, decisions, err := WorkItemActions(test.state, test.context)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(actions, test.actions) || !reflect.DeepEqual(decisions, test.decisions) {
				t.Fatalf("actions=%v decisions=%v want actions=%v decisions=%v", actions, decisions, test.actions, test.decisions)
			}
		})
	}
}

func TestWorkItemActionsRejectsUnstableAndIncompleteFeedback(t *testing.T) {
	for _, state := range []tutoring.State{tutoring.StateIdle, tutoring.StateAdvanceOrReview, tutoring.StateFocusSuspended, tutoring.StateFocusResumed, "Unknown"} {
		if _, _, err := WorkItemActions(state, WorkItemActionContext{}); err == nil {
			t.Fatalf("state %s was accepted", state)
		}
	}
	if _, _, err := WorkItemActions(tutoring.StateFeedback, WorkItemActionContext{}); err == nil {
		t.Fatal("feedback without a current disposition was accepted")
	}
}
