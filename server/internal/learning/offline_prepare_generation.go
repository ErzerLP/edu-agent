package learning

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/edu-agent/edu-agent/server/internal/tutoring"
	"github.com/google/uuid"
)

func (s *Service) GenerateOfflinePrepare(ctx context.Context, request OfflinePrepareGenerationRequest) (OfflinePrepareArtifact, error) {
	if s == nil || request.Count < 1 || request.Count > OfflineMaxPackCount || uuid.Validate(request.DeviceID) != nil || uuid.Validate(request.OperationID) != nil || uuid.Validate(request.SessionID) != nil || request.ExpectedSessionVersion < 1 || request.GoalRevisionID == "" || request.Route.ID == "" || request.RouteStepID == "" || request.KnowledgeRevisionID == "" || request.Route.GoalRevisionID != request.GoalRevisionID || request.Route.KnowledgeRevisionID != request.KnowledgeRevisionID || !StableRouteSteps(request.Route.Steps) {
		return OfflinePrepareArtifact{}, &Error{Code: CodeInvalidRequest, Reason: "invalid_offline_prepare_generation"}
	}
	if request.SessionState != string(tutoring.StateRouteActive) && request.SessionState != string(tutoring.StateAwaitingResponse) {
		return OfflinePrepareArtifact{}, &Error{Code: CodeOfflinePrepareUnavailable, Reason: "offline_prepare_session_state"}
	}
	artifact := OfflinePrepareArtifact{
		ProtocolVersion: 1, SessionID: request.SessionID, SessionState: request.SessionState,
		ExpectedSessionVersion: request.ExpectedSessionVersion, GoalRevisionID: request.GoalRevisionID,
		RouteRevisionID: request.Route.ID, RouteStepID: request.RouteStepID,
		KnowledgeRevisionID: request.KnowledgeRevisionID, Activities: []Activity{},
	}
	if request.CurrentActivity != nil {
		current := CloneActivity(*request.CurrentActivity)
		if current.SessionID != request.SessionID || current.GoalRevisionID != request.GoalRevisionID || current.RouteRevisionID != request.Route.ID || current.KnowledgeRevisionID != request.KnowledgeRevisionID || current.Revision != 1 || (current.Type != ActivityObjective && current.Type != ActivityOpen) {
			return OfflinePrepareArtifact{}, &Error{Code: CodeOfflinePrepareUnavailable, Reason: "offline_current_activity_invalid"}
		}
		artifact.Activities = append(artifact.Activities, current)
		if len(artifact.Activities) == request.Count {
			return artifact, nil
		}
	}

	steps, err := offlinePrepareRouteSteps(request)
	if err != nil {
		return OfflinePrepareArtifact{}, err
	}
	if s.model == nil {
		if len(artifact.Activities) != 0 {
			artifact.ModelPartial = true
			return artifact, nil
		}
		return OfflinePrepareArtifact{}, &Error{Code: CodeModelUnavailable, Reason: "not_configured"}
	}
	for _, step := range steps {
		if len(artifact.Activities) == request.Count {
			break
		}
		proposalRequest := ProposalRequest{
			RequestID: s.newUUID(), Type: ProposalActivity, AggregateType: "session",
			AggregateID: request.SessionID, AggregateVersion: request.ExpectedSessionVersion,
			GoalRevisionID: request.GoalRevisionID, RouteRevisionID: request.Route.ID,
			RouteStepID: step.ID, FocusNodeRevisionID: step.NodeRevisionID,
			TutoringState: string(tutoring.StateRouteActive), KnowledgeRevisionID: request.KnowledgeRevisionID,
			NodeRevisionIDs: []string{step.NodeRevisionID}, Input: offlinePrepareModelInput(request, step),
		}
		inputHash, hashErr := HashJSON(proposalRequest)
		if hashErr != nil {
			return OfflinePrepareArtifact{}, hashErr
		}
		categories := []string{}
		var proposal ProposalArtifact
		var generateErr error
		modelFailure := false
		for attempt := 0; attempt < 2; attempt++ {
			var raw json.RawMessage
			raw, generateErr = s.model.Generate(ctx, proposalRequest)
			if generateErr != nil {
				modelFailure = true
				category := modelCategory(generateErr)
				categories = append(categories, category)
				if retryableModelCategory(category) && attempt == 0 {
					continue
				}
				break
			}
			categories = append(categories, "success")
			proposal, generateErr = s.decodeProposal(ctx, proposalRequest, inputHash, raw, categories)
			break
		}
		if generateErr != nil {
			if len(artifact.Activities) != 0 {
				artifact.ModelPartial = true
				return artifact, nil
			}
			if modelFailure {
				return OfflinePrepareArtifact{}, proposalCategoryError(modelCategory(generateErr), generateErr)
			}
			return OfflinePrepareArtifact{}, generateErr
		}
		activity, activityErr := offlineActivityFromProposal(s, request, step, proposal)
		if activityErr != nil {
			if len(artifact.Activities) != 0 {
				artifact.ModelPartial = true
				return artifact, nil
			}
			return OfflinePrepareArtifact{}, activityErr
		}
		artifact.Activities = append(artifact.Activities, activity)
	}
	return artifact, nil
}

func offlinePrepareRouteSteps(request OfflinePrepareGenerationRequest) ([]RouteStep, error) {
	start := -1
	for index, step := range request.Route.Steps {
		if step.ID == request.RouteStepID {
			start = index
			break
		}
	}
	if start < 0 {
		return nil, &Error{Code: CodeOfflinePrepareUnavailable, Reason: "offline_route_step_missing"}
	}
	currentStep := ""
	if request.CurrentActivity != nil {
		currentStep = request.CurrentActivity.RouteStepID
	}
	steps := make([]RouteStep, 0, len(request.Route.Steps)-start)
	for _, step := range request.Route.Steps[start:] {
		if step.ID != currentStep {
			steps = append(steps, step)
		}
	}
	return steps, nil
}

func offlinePrepareModelInput(request OfflinePrepareGenerationRequest, step RouteStep) json.RawMessage {
	value := struct {
		Mode                string    `json:"mode"`
		GoalRevisionID      string    `json:"goal_revision_id"`
		RouteRevisionID     string    `json:"route_revision_id"`
		KnowledgeRevisionID string    `json:"knowledge_revision_id"`
		RouteStep           RouteStep `json:"route_step"`
	}{
		Mode: "offline_prepare", GoalRevisionID: request.GoalRevisionID,
		RouteRevisionID: request.Route.ID, KnowledgeRevisionID: request.KnowledgeRevisionID,
		RouteStep: step,
	}
	encoded, _ := json.Marshal(value)
	return encoded
}

func offlineActivityFromProposal(s *Service, request OfflinePrepareGenerationRequest, step RouteStep, proposal ProposalArtifact) (Activity, error) {
	if proposal.Activity == nil || proposal.Type != ProposalActivity || proposal.AggregateID != request.SessionID || proposal.AggregateVersion != request.ExpectedSessionVersion || proposal.GoalRevisionID != request.GoalRevisionID || proposal.RouteRevisionID != request.Route.ID || proposal.KnowledgeRevisionID != request.KnowledgeRevisionID {
		return Activity{}, &Error{Code: CodeProposalRejected, Reason: "offline_activity_proposal_context"}
	}
	foundTarget := false
	for _, reference := range proposal.Activity.References {
		if reference.KnowledgeRevisionID == request.KnowledgeRevisionID && reference.NodeID == step.NodeID && reference.NodeRevisionID == step.NodeRevisionID && reference.DocumentRevisionID != "" {
			foundTarget = true
			break
		}
	}
	if !foundTarget {
		return Activity{}, &Error{Code: CodeKnowledgeReferenceInvalid, Reason: "offline_activity_target_reference_missing"}
	}
	createdAt := s.now().UTC().Truncate(time.Microsecond)
	if createdAt.IsZero() {
		return Activity{}, errors.New("offline activity creation time is unavailable")
	}
	activity := Activity{
		ID: s.newUUID(), Revision: 1, SessionID: request.SessionID,
		GoalRevisionID: request.GoalRevisionID, RouteRevisionID: request.Route.ID,
		RouteStepID: step.ID, KnowledgeRevisionID: request.KnowledgeRevisionID,
		TargetNodeID: step.NodeID, TargetNodeRevisionID: step.NodeRevisionID,
		References: proposal.Activity.References, Prompt: proposal.Activity.Prompt,
		Type: proposal.Activity.Type, Rubric: proposal.Activity.Rubric,
		Difficulty: proposal.Activity.Difficulty, AllowedHelp: proposal.Activity.AllowedHelp,
		ActivityPolicyVersion: ActivityPolicyVersion, AssessmentPolicyVersion: AssessmentPolicyVersion,
		ReviewPolicyVersion: ReviewPolicyVersion, SourceProposalID: proposal.ID, CreatedAt: createdAt,
	}
	if uuid.Validate(activity.ID) != nil || uuid.Validate(activity.SourceProposalID) != nil {
		return Activity{}, fmt.Errorf("offline activity generator returned a non-UUID identity")
	}
	return CloneActivity(activity), nil
}
