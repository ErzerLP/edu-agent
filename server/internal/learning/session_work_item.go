package learning

import (
	"fmt"

	"github.com/edu-agent/edu-agent/server/internal/tutoring"
)

type WorkItemActionContext struct {
	DueReview             bool
	AssessmentDisposition Disposition
	AssessmentConfirmable bool
}

func WorkItemActions(state tutoring.State, context WorkItemActionContext) ([]tutoring.Action, []string, error) {
	actions := []tutoring.Action{}
	decisions := []string{}
	switch state {
	case tutoring.StateGoalReady:
		actions = append(actions, tutoring.ActionStartDiagnostic, tutoring.ActionSwitchGoal)
	case tutoring.StateDiagnostic:
		actions = append(actions, tutoring.ActionApplyRoute, tutoring.ActionSwitchGoal)
	case tutoring.StateRouteActive:
		actions = append(actions, tutoring.ActionApplyRoute, tutoring.ActionIssueActivity)
		if context.DueReview {
			actions = append(actions, tutoring.ActionPresentReview)
		}
		actions = append(actions, tutoring.ActionRecordExposure, tutoring.ActionAskFreeQuestion, tutoring.ActionCompleteSession, tutoring.ActionSwitchGoal)
	case tutoring.StateActivityIssued:
		actions = append(actions, tutoring.ActionPresentActivity, tutoring.ActionAskFreeQuestion, tutoring.ActionEndActivity, tutoring.ActionSwitchGoal)
	case tutoring.StateAwaitingResponse:
		actions = append(actions, tutoring.ActionSubmitAttempt, tutoring.ActionAskFreeQuestion, tutoring.ActionEndActivity, tutoring.ActionSwitchGoal)
	case tutoring.StateEvaluating:
		actions = append(actions, tutoring.ActionRecordAssessment, tutoring.ActionEndActivity, tutoring.ActionSwitchGoal)
	case tutoring.StateFeedback:
		switch context.AssessmentDisposition {
		case DispositionProvisional:
			if context.AssessmentConfirmable {
				decisions = append(decisions, "confirm")
			}
			decisions = append(decisions, "override", "void")
		case DispositionAccepted, DispositionOverridden:
			actions = append(actions, tutoring.ActionAcknowledgeFeedback, tutoring.ActionEndActivity, tutoring.ActionSwitchGoal)
			decisions = append(decisions, "override", "void")
		case DispositionVoided:
			actions = append(actions, tutoring.ActionAcknowledgeFeedback, tutoring.ActionEndActivity, tutoring.ActionSwitchGoal)
		default:
			return nil, nil, fmt.Errorf("feedback assessment disposition is required")
		}
	case tutoring.StateFreeQuestion:
		actions = append(actions, tutoring.ActionRecordFreeAnswer, tutoring.ActionResumeFocus, tutoring.ActionSwitchGoal)
	case tutoring.StateFreeAnswer:
		actions = append(actions, tutoring.ActionAskFreeQuestion, tutoring.ActionConvertFreeAnswerToQuiz, tutoring.ActionResumeFocus, tutoring.ActionSwitchGoal)
	case tutoring.StateCompleted:
	case tutoring.StateIdle, tutoring.StateAdvanceOrReview, tutoring.StateFocusSuspended, tutoring.StateFocusResumed:
		return nil, nil, fmt.Errorf("state %s is not a stable session view state", state)
	default:
		return nil, nil, fmt.Errorf("unknown tutoring state %s", state)
	}
	return actions, decisions, nil
}
