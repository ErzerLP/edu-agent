package learning

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/edu-agent/edu-agent/server/internal/tutoring"
	"github.com/google/uuid"
)

const TutorPromptRevision = "tutor-proposal-v1"

type ProposalClaim struct {
	State      string
	LeaseToken string
	Artifact   *ProposalArtifact
	Category   string
}

type ProposalRepository interface {
	ClaimProposal(context.Context, string, ProposalRequest, string, time.Time) (ProposalClaim, error)
	CompleteProposal(context.Context, string, string, ProposalArtifact, time.Time) error
	FailProposal(context.Context, string, string, []string, string, time.Time) error
}

type TutorModel interface {
	Generate(context.Context, ProposalRequest) (json.RawMessage, error)
}

type ModelFailure interface {
	error
	ModelCategory() string
}

type ServiceOptions struct {
	Now             func() time.Time
	NewUUID         func() string
	Model           TutorModel
	ModelID         string
	ModelParameters map[string]any
	PromptRevision  string
}

type Service struct {
	authority       AuthorityStore
	queries         QueryStore
	proposals       ProposalRepository
	knowledge       KnowledgeReferenceResolver
	model           TutorModel
	now             func() time.Time
	newUUID         func() string
	modelID         string
	modelParameters map[string]any
	promptRevision  string
}

func NewService(store ApplicationStore, proposals ProposalRepository, knowledge KnowledgeReferenceResolver, options ServiceOptions) (*Service, error) {
	if store == nil {
		return nil, fmt.Errorf("learning store is required")
	}
	return NewServiceWithPorts(store, store, proposals, knowledge, options)
}

func NewServiceWithPorts(authority AuthorityStore, queries QueryStore, proposals ProposalRepository, knowledge KnowledgeReferenceResolver, options ServiceOptions) (*Service, error) {
	if authority == nil || queries == nil || proposals == nil || knowledge == nil {
		return nil, fmt.Errorf("learning authority, query store, proposal store, and knowledge resolver are required")
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	if options.NewUUID == nil {
		options.NewUUID = uuid.NewString
	}
	if options.PromptRevision == "" {
		options.PromptRevision = TutorPromptRevision
	}
	if options.ModelParameters == nil {
		options.ModelParameters = map[string]any{}
	}
	return &Service{authority: authority, queries: queries, proposals: proposals, knowledge: knowledge, model: options.Model, now: options.Now, newUUID: options.NewUUID, modelID: options.ModelID, modelParameters: options.ModelParameters, promptRevision: options.PromptRevision}, nil
}

func (s *Service) Propose(ctx context.Context, deviceID string, request ProposalRequest) (ProposalArtifact, error) {
	if err := validateProposalRequest(request); err != nil {
		return ProposalArtifact{}, err
	}
	requestHash, err := HashJSON(request)
	if err != nil {
		return ProposalArtifact{}, err
	}
	claim, err := s.proposals.ClaimProposal(ctx, deviceID, request, requestHash, s.now().UTC().Truncate(time.Microsecond))
	if err != nil {
		return ProposalArtifact{}, err
	}
	switch claim.State {
	case "ready":
		return *claim.Artifact, nil
	case "failed":
		return ProposalArtifact{}, proposalCategoryError(claim.Category, nil)
	case "busy":
		return ProposalArtifact{}, &Error{Code: CodeModelUnavailable, Reason: "proposal_in_progress"}
	}
	request, err = s.freezeProposalRequest(ctx, request)
	if err != nil {
		category := proposalFailureCategory(err)
		if failErr := s.persistProposalFailure(ctx, deviceID, claim.LeaseToken, []string{category}, category); failErr != nil {
			return ProposalArtifact{}, failErr
		}
		return ProposalArtifact{}, proposalCategoryError(category, err)
	}
	inputHash, err := HashJSON(request)
	if err != nil {
		return ProposalArtifact{}, err
	}
	if s.model == nil {
		if err := s.persistProposalFailure(ctx, deviceID, claim.LeaseToken, nil, "not_configured"); err != nil {
			return ProposalArtifact{}, err
		}
		return ProposalArtifact{}, &Error{Code: CodeModelUnavailable, Reason: "not_configured"}
	}
	categories := []string{}
	var artifact ProposalArtifact
	for attempt := 0; attempt < 2; attempt++ {
		raw, generateErr := s.model.Generate(ctx, request)
		if generateErr != nil {
			category := modelCategory(generateErr)
			categories = append(categories, category)
			if retryableModelCategory(category) && attempt == 0 {
				continue
			}
			if failErr := s.persistProposalFailure(ctx, deviceID, claim.LeaseToken, categories, category); failErr != nil {
				return ProposalArtifact{}, failErr
			}
			if retryableModelCategory(category) {
				return ProposalArtifact{}, &Error{Code: CodeModelUnavailable, Reason: category, Cause: generateErr}
			}
			return ProposalArtifact{}, &Error{Code: CodeProposalRejected, Reason: category, Cause: generateErr}
		}
		successCategories := append(append([]string(nil), categories...), "success")
		artifact, err = s.decodeProposal(ctx, request, inputHash, raw, successCategories)
		if err != nil {
			category := proposalFailureCategory(err)
			categories = append(categories, category)
			if failErr := s.persistProposalFailure(ctx, deviceID, claim.LeaseToken, categories, category); failErr != nil {
				return ProposalArtifact{}, failErr
			}
			return ProposalArtifact{}, proposalCategoryError(category, err)
		}
		categories = successCategories
		break
	}
	if err := s.proposals.CompleteProposal(ctx, deviceID, claim.LeaseToken, artifact, s.now().UTC()); err != nil {
		return ProposalArtifact{}, err
	}
	return artifact, nil
}

func (s *Service) persistProposalFailure(ctx context.Context, deviceID, lease string, categories []string, category string) error {
	if err := s.proposals.FailProposal(ctx, deviceID, lease, categories, category, s.now().UTC()); err != nil {
		return fmt.Errorf("persist proposal failure: %w", err)
	}
	return nil
}

func (s *Service) freezeProposalRequest(ctx context.Context, request ProposalRequest) (ProposalRequest, error) {
	if request.AggregateType == "session" {
		authority, err := s.authority.LoadSessionAuthority(ctx, request.AggregateID)
		if err != nil {
			return request, err
		}
		session := authority.Session
		if session.ID != request.AggregateID {
			return request, &Error{Code: CodeStaleProposal, Reason: "session_ownership"}
		}
		if session.AggregateVer != request.AggregateVersion {
			return request, &Error{Code: CodeStaleProposal, Reason: "aggregate_version_changed", ExpectedVersion: request.AggregateVersion, CurrentVersion: session.AggregateVer}
		}
		if err := freezeField(&request.GoalRevisionID, session.Context.GoalRevisionID); err != nil {
			return request, err
		}
		if err := freezeField(&request.RouteRevisionID, session.Context.RouteRevisionID); err != nil {
			return request, err
		}
		if err := freezeField(&request.RouteStepID, session.Context.RouteStepID); err != nil {
			return request, err
		}
		if err := freezeField(&request.FocusNodeRevisionID, session.Context.FocusNodeRevisionID); err != nil {
			return request, err
		}
		activityID, attemptID, frameID := "", "", ""
		if session.Context.ActivityID != nil {
			activityID = *session.Context.ActivityID
		}
		if session.Context.AttemptID != nil {
			attemptID = *session.Context.AttemptID
		}
		if session.ActiveFrame != nil {
			frameID = session.ActiveFrame.ID
		}
		if err := freezeField(&request.ActivityID, activityID); err != nil {
			return request, err
		}
		if err := freezeField(&request.AttemptID, attemptID); err != nil {
			return request, err
		}
		if err := freezeField(&request.FocusFrameID, frameID); err != nil {
			return request, err
		}
		state := string(session.State)
		if err := freezeField(&request.TutoringState, state); err != nil {
			return request, err
		}
		if session.Context.KnowledgeRevisionID != "" && request.KnowledgeRevisionID != session.Context.KnowledgeRevisionID {
			return request, &Error{Code: CodeStaleProposal, Reason: "knowledge_revision_changed"}
		}
		if request.Type == ProposalFreeAnswer {
			if !validActiveFocusFrame(session) {
				return request, &Error{Code: CodeStaleProposal, Reason: "active_focus_frame_invalid"}
			}
			questionID, err := s.authority.LatestFreeQuestionForFrame(ctx, session.ID, session.ActiveFrame.ID)
			if err != nil {
				return request, err
			}
			if err := freezeField(&request.FreeQuestionID, questionID); err != nil {
				return request, err
			}
			question, err := s.authority.LoadFreeQuestion(ctx, questionID)
			if err != nil {
				return request, err
			}
			if !currentFreeQuestionMatchesSession(session, question) || question.KnowledgeRevisionID != request.KnowledgeRevisionID {
				return request, &Error{Code: CodeStaleProposal, Reason: "free_question_ownership"}
			}
		}
		attachedQuizProposal := request.Type == ProposalActivity && session.State == tutoring.StateFreeAnswer ||
			request.Type == ProposalAssessment && session.AttachedQuiz
		if attachedQuizProposal {
			if request.FreeQuestionID == "" || request.FreeAnswerID == "" || !validActiveFocusFrame(session) {
				return request, &Error{Code: CodeInvalidRequest, Reason: "attached_quiz_context_required"}
			}
			latestQuestionID, err := s.authority.LatestFreeQuestionForFrame(ctx, session.ID, session.ActiveFrame.ID)
			if err != nil {
				return request, err
			}
			if latestQuestionID != request.FreeQuestionID {
				return request, &Error{Code: CodeStaleProposal, Reason: "attached_quiz_question_changed"}
			}
			question, err := s.authority.LoadFreeQuestion(ctx, request.FreeQuestionID)
			if err != nil {
				return request, err
			}
			answer, err := s.authority.LoadFreeAnswer(ctx, request.FreeAnswerID)
			if err != nil {
				return request, err
			}
			if question.ID != request.FreeQuestionID || answer.ID != request.FreeAnswerID || !currentFreeQuestionMatchesSession(session, question) || answer.SessionID != session.ID || answer.FocusFrameID != session.ActiveFrame.ID || answer.FreeQuestionID != question.ID || answer.KnowledgeRevisionID != question.KnowledgeRevisionID {
				return request, &Error{Code: CodeStaleProposal, Reason: "attached_quiz_ownership"}
			}
		}
		if err := validateProposalState(request.Type, session.State); err != nil {
			return request, err
		}
		if err := validateFrozenRequestShape(request); err != nil {
			return request, err
		}
		if err := s.validateFrozenSessionContext(ctx, request, session); err != nil {
			return request, err
		}
	} else {
		version, err := s.authority.LoadAggregateVersion(ctx, request.AggregateType, request.AggregateID)
		if err != nil {
			return request, err
		}
		if version != request.AggregateVersion {
			return request, &Error{Code: CodeStaleProposal, Reason: "aggregate_version_changed", ExpectedVersion: request.AggregateVersion, CurrentVersion: version}
		}
		goal, err := s.authority.LoadGoalRevision(ctx, request.GoalRevisionID)
		if err != nil || goal.GoalID != request.AggregateID || goal.Revision != version {
			return request, &Error{Code: CodeStaleProposal, Reason: "goal_revision_changed", Cause: err}
		}
	}
	if err := validateFrozenRequestShape(request); err != nil {
		return request, err
	}
	for _, node := range request.NodeRevisionIDs {
		if _, err := s.knowledge.Resolve(ctx, request.KnowledgeRevisionID, node); err != nil {
			return request, &Error{Code: CodeKnowledgeReferenceInvalid, Cause: err}
		}
	}
	return request, nil
}

func validateFrozenRequestShape(request ProposalRequest) error {
	if request.GoalRevisionID == "" || request.KnowledgeRevisionID == "" || len(request.NodeRevisionIDs) == 0 {
		return &Error{Code: CodeInvalidRequest, Reason: "proposal_context_incomplete"}
	}
	switch request.Type {
	case ProposalRoute:
		return nil
	case ProposalActivity:
		if request.RouteRevisionID == "" || request.RouteStepID == "" {
			return &Error{Code: CodeInvalidRequest, Reason: "route_context_required"}
		}
	case ProposalAssessment:
		if request.ActivityID == "" || request.AttemptID == "" {
			return &Error{Code: CodeInvalidRequest, Reason: "assessment_context_required"}
		}
	case ProposalFreeAnswer:
		if request.FocusFrameID == "" || request.FreeQuestionID == "" {
			return &Error{Code: CodeInvalidRequest, Reason: "free_question_context_required"}
		}
	case ProposalExplanation:
		if request.RouteRevisionID == "" || request.RouteStepID == "" {
			return &Error{Code: CodeInvalidRequest, Reason: "route_context_required"}
		}
	}
	return nil
}

func validateProposalState(kind ProposalType, state tutoring.State) error {
	valid := false
	switch kind {
	case ProposalRoute:
		valid = state == tutoring.StateDiagnostic || state == tutoring.StateRouteActive
	case ProposalActivity:
		valid = state == tutoring.StateRouteActive || state == tutoring.StateFreeAnswer
	case ProposalAssessment:
		valid = state == tutoring.StateEvaluating
	case ProposalFreeAnswer:
		valid = state == tutoring.StateFreeQuestion
	case ProposalExplanation:
		valid = state == tutoring.StateRouteActive
	}
	if !valid {
		return &Error{Code: CodeStaleProposal, Reason: "tutoring_state_changed"}
	}
	return nil
}

func (s *Service) validateFrozenSessionContext(ctx context.Context, request ProposalRequest, session tutoring.Session) error {
	if request.GoalRevisionID == "" {
		return &Error{Code: CodeStaleProposal, Reason: "goal_context_missing"}
	}
	goal, err := s.authority.LoadGoalRevision(ctx, request.GoalRevisionID)
	if err != nil {
		return err
	}
	if goal.ID != request.GoalRevisionID {
		return &Error{Code: CodeStaleProposal, Reason: "goal_context_changed"}
	}
	if request.RouteRevisionID != "" {
		route, err := s.authority.LoadRouteRevision(ctx, request.RouteRevisionID)
		if err != nil {
			return err
		}
		if route.GoalRevisionID != request.GoalRevisionID || route.KnowledgeRevisionID != request.KnowledgeRevisionID || !StableRouteSteps(route.Steps) || !routeContainsStep(route, request.RouteStepID, request.FocusNodeRevisionID) {
			return &Error{Code: CodeStaleProposal, Reason: "route_context_changed"}
		}
	}
	if request.ActivityID != "" {
		activity, err := s.authority.LoadActivity(ctx, request.ActivityID)
		if err != nil {
			return err
		}
		if activity.SessionID != session.ID || activity.GoalRevisionID != request.GoalRevisionID || activity.RouteRevisionID != request.RouteRevisionID || activity.RouteStepID != request.RouteStepID || activity.KnowledgeRevisionID != request.KnowledgeRevisionID || activity.TargetNodeRevisionID != request.FocusNodeRevisionID || !containsReferenceNode(activity.References, activity.TargetNodeRevisionID) {
			return &Error{Code: CodeStaleProposal, Reason: "activity_context_changed"}
		}
	}
	if request.AttemptID != "" {
		attempt, err := s.authority.LoadAttempt(ctx, request.AttemptID)
		if err != nil {
			return err
		}
		if attempt.SessionID != session.ID || attempt.ActivityID != request.ActivityID {
			return &Error{Code: CodeStaleProposal, Reason: "attempt_context_changed"}
		}
	}
	return nil
}

func routeContainsStep(route RouteRevision, stepID, nodeID string) bool {
	if stepID == "" {
		return false
	}
	for _, step := range route.Steps {
		if step.ID == stepID && (nodeID == "" || step.NodeRevisionID == nodeID) {
			return true
		}
	}
	return false
}

func validActiveFocusFrame(session tutoring.Session) bool {
	frame := session.ActiveFrame
	if frame == nil || frame.Invalidated || session.FocusFrameInvalidated || frame.ID == "" || frame.SessionID != session.ID || frame.SavedAggregateVersion < 1 || frame.SavedAggregateVersion >= session.AggregateVer || frame.CreatedEventSequence < 1 {
		return false
	}
	if !sameStableFocusContext(session.Context, frame.Context) || frame.Context.GoalRevisionID == "" || frame.Context.RouteRevisionID == "" || frame.Context.RouteStepID == "" || frame.Context.KnowledgeRevisionID == "" || frame.Context.FocusNodeRevisionID == "" {
		return false
	}
	switch frame.SavedState {
	case tutoring.StateRouteActive:
		if frame.Context.ActivityID != nil || frame.Context.AttemptID != nil {
			return false
		}
	case tutoring.StateActivityIssued, tutoring.StateAwaitingResponse:
		if frame.Context.ActivityID == nil || frame.Context.AttemptID != nil {
			return false
		}
	default:
		return false
	}
	if session.State == tutoring.StateFreeQuestion || session.State == tutoring.StateFreeAnswer {
		return !session.AttachedQuiz && focusContextEqual(session.Context, frame.Context)
	}
	if !session.AttachedQuiz {
		return false
	}
	switch session.State {
	case tutoring.StateActivityIssued, tutoring.StateAwaitingResponse, tutoring.StateEvaluating, tutoring.StateFeedback:
		return session.Context.ActivityID != nil
	default:
		return false
	}
}

func currentFreeQuestionMatchesSession(session tutoring.Session, question tutoring.FreeQuestion) bool {
	if !validActiveFocusFrame(session) || question.ID == "" || question.SessionID != session.ID || question.FocusFrameID != session.ActiveFrame.ID || question.KnowledgeRevisionID != session.ActiveFrame.Context.KnowledgeRevisionID || question.SessionAggregateVer <= session.ActiveFrame.SavedAggregateVersion || question.SessionAggregateVer > session.AggregateVer {
		return false
	}
	return session.State != tutoring.StateFreeQuestion || question.SessionAggregateVer == session.AggregateVer
}

func sameStableFocusContext(left, right tutoring.FocusContext) bool {
	return left.GoalRevisionID == right.GoalRevisionID &&
		left.RouteRevisionID == right.RouteRevisionID &&
		left.RouteStepID == right.RouteStepID &&
		left.KnowledgeRevisionID == right.KnowledgeRevisionID &&
		left.FocusNodeRevisionID == right.FocusNodeRevisionID
}

func focusContextEqual(left, right tutoring.FocusContext) bool {
	return sameStableFocusContext(left, right) && optionalStringEqual(left.ActivityID, right.ActivityID) && optionalStringEqual(left.AttemptID, right.AttemptID)
}

func optionalStringEqual(left, right *string) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func freezeField(target *string, current string) error {
	if *target != "" && *target != current {
		return &Error{Code: CodeStaleProposal, Reason: "frozen_context_changed"}
	}
	*target = current
	return nil
}

func proposalCategoryError(category string, cause error) error {
	switch category {
	case "not_configured", "timeout", "rate_limited", "unavailable", "upstream_error":
		return &Error{Code: CodeModelUnavailable, Reason: category, Cause: cause}
	case "stale_context":
		return &Error{Code: CodeStaleProposal, Reason: category, Cause: cause}
	case "invalid_request":
		return &Error{Code: CodeInvalidRequest, Reason: category, Cause: cause}
	default:
		return &Error{Code: CodeProposalRejected, Reason: category, Cause: cause}
	}
}

func proposalFailureCategory(err error) string {
	var domainErr *Error
	if !errors.As(err, &domainErr) {
		return "invalid_response"
	}
	if domainErr.Reason == "schema_mismatch" {
		return "schema_mismatch"
	}
	switch domainErr.Code {
	case CodeStaleProposal:
		return "stale_context"
	case CodeInvalidRequest:
		return "invalid_request"
	default:
		return "domain_invalid"
	}
}

func validateProposalRequest(request ProposalRequest) error {
	if _, err := uuid.Parse(request.RequestID); err != nil {
		return &Error{Code: CodeInvalidRequest}
	}
	if request.Type != ProposalRoute && request.Type != ProposalActivity && request.Type != ProposalAssessment && request.Type != ProposalFreeAnswer && request.Type != ProposalExplanation {
		return &Error{Code: CodeInvalidRequest}
	}
	if (request.AggregateType != "goal" && request.AggregateType != "session") || request.AggregateID == "" || request.AggregateVersion < 0 || request.KnowledgeRevisionID == "" || len(request.Input) == 0 || !json.Valid(request.Input) {
		return &Error{Code: CodeInvalidRequest}
	}
	if request.Type != ProposalRoute && request.AggregateType != "session" {
		return &Error{Code: CodeInvalidRequest}
	}
	if len(request.NodeRevisionIDs) > 100 || len(request.Input) > MaxAnswerBytes {
		return &Error{Code: CodeInvalidRequest}
	}
	seen := map[string]bool{}
	for _, id := range request.NodeRevisionIDs {
		if seen[id] {
			return &Error{Code: CodeProposalRejected, Reason: "duplicate_node_revision"}
		}
		seen[id] = true
	}
	return nil
}

func (s *Service) validateProposalForApply(ctx context.Context, proposal ProposalArtifact, kind ProposalType, session tutoring.Session) error {
	if proposal.SchemaVersion != ProposalSchemaVersion || proposal.Type != kind || proposal.FrozenRequest.RequestID == "" {
		return &Error{Code: CodeStaleProposal, Reason: "proposal_schema_or_context_missing"}
	}
	request := proposal.FrozenRequest
	if err := validateProposalRequest(request); err != nil {
		return &Error{Code: CodeStaleProposal, Reason: "invalid_frozen_request", Cause: err}
	}
	hash, err := HashJSON(request)
	if err != nil || hash != proposal.InputHash {
		return &Error{Code: CodeStaleProposal, Reason: "proposal_input_hash_mismatch", Cause: err}
	}
	if proposal.Type != request.Type || proposal.AggregateType != request.AggregateType || proposal.AggregateID != request.AggregateID || proposal.AggregateVersion != request.AggregateVersion || proposal.GoalRevisionID != request.GoalRevisionID || proposal.RouteRevisionID != request.RouteRevisionID || proposal.ActivityID != request.ActivityID || proposal.AttemptID != request.AttemptID || proposal.KnowledgeRevisionID != request.KnowledgeRevisionID {
		return &Error{Code: CodeStaleProposal, Reason: "proposal_context_mismatch"}
	}
	current, err := s.freezeProposalRequest(ctx, request)
	if err != nil {
		return err
	}
	currentHash, err := HashJSON(current)
	if err != nil || currentHash != proposal.InputHash || current.AggregateID != session.ID || current.AggregateVersion != session.AggregateVer {
		return &Error{Code: CodeStaleProposal, Reason: "proposal_context_changed", Cause: err}
	}
	switch kind {
	case ProposalRoute:
		if len(proposal.Route) == 0 || len(proposal.Route) > 100 {
			return &Error{Code: CodeProposalRejected, Reason: "route_size"}
		}
		seen := map[string]bool{}
		for _, step := range proposal.Route {
			if seen[step.NodeRevisionID] || !containsString(request.NodeRevisionIDs, step.NodeRevisionID) || !utf8.ValidString(step.TeachingIntent) || !utf8.ValidString(step.CompletionCondition) || strings.TrimSpace(step.TeachingIntent) == "" || strings.TrimSpace(step.CompletionCondition) == "" || utf8.RuneCountInString(step.TeachingIntent) > 1000 || utf8.RuneCountInString(step.CompletionCondition) > 1000 {
				return &Error{Code: CodeProposalRejected, Reason: "invalid_route_step"}
			}
			seen[step.NodeRevisionID] = true
			if _, err := s.knowledge.Resolve(ctx, request.KnowledgeRevisionID, step.NodeRevisionID); err != nil {
				return &Error{Code: CodeKnowledgeReferenceInvalid, Cause: err}
			}
		}
	case ProposalActivity:
		if proposal.Activity == nil {
			return &Error{Code: CodeProposalRejected, Reason: "activity_missing"}
		}
		copy := *proposal.Activity
		originalHash, _ := HashJSON(copy)
		if err := s.validateActivityProposal(ctx, request, &copy); err != nil {
			return err
		}
		canonicalHash, _ := HashJSON(copy)
		if originalHash != canonicalHash {
			return &Error{Code: CodeProposalRejected, Reason: "activity_not_canonical"}
		}
	case ProposalAssessment:
		if proposal.Assessment == nil || proposal.Assessment.ActivityID != request.ActivityID || proposal.Assessment.AttemptID != request.AttemptID || proposal.Assessment.ProposalInputHash != proposal.InputHash {
			return &Error{Code: CodeProposalRejected, Reason: "assessment_context_mismatch"}
		}
		activity, err := s.authority.LoadActivity(ctx, request.ActivityID)
		if err != nil {
			return err
		}
		attempt, err := s.authority.LoadAttempt(ctx, request.AttemptID)
		if err != nil {
			return err
		}
		if _, err := EvaluateAssessment(activity, attempt, *proposal.Assessment); err != nil {
			return err
		}
	case ProposalFreeAnswer, ProposalExplanation:
		if proposal.Text == nil || !utf8.ValidString(proposal.Text.Text) || strings.TrimSpace(proposal.Text.Text) == "" || utf8.RuneCountInString(proposal.Text.Text) > MaxProposalTextRunes || len(proposal.Text.References) == 0 {
			return &Error{Code: CodeProposalRejected, Reason: "text_missing"}
		}
		canonical, err := s.canonicalReferences(ctx, request.KnowledgeRevisionID, proposal.Text.References, request.NodeRevisionIDs)
		if err != nil {
			return err
		}
		originalHash, _ := HashJSON(proposal.Text.References)
		canonicalHash, _ := HashJSON(canonical)
		if originalHash != canonicalHash {
			return &Error{Code: CodeProposalRejected, Reason: "text_references_not_canonical"}
		}
	}
	return nil
}

func (s *Service) decodeProposal(ctx context.Context, request ProposalRequest, inputHash string, raw json.RawMessage, categories []string) (ProposalArtifact, error) {
	artifact := ProposalArtifact{ID: s.newUUID(), SchemaVersion: ProposalSchemaVersion, InputHash: inputHash, Type: request.Type, AggregateType: request.AggregateType, AggregateID: request.AggregateID, AggregateVersion: request.AggregateVersion, GoalRevisionID: request.GoalRevisionID, RouteRevisionID: request.RouteRevisionID, ActivityID: request.ActivityID, AttemptID: request.AttemptID, KnowledgeRevisionID: request.KnowledgeRevisionID, FrozenRequest: request, ModelID: s.modelID, ModelParameters: cloneMap(s.modelParameters), PromptRevision: s.promptRevision, AttemptCategories: append([]string(nil), categories...), CreatedAt: s.now().UTC().Truncate(time.Microsecond)}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	switch request.Type {
	case ProposalRoute:
		var output struct {
			Route []RouteProposalStep `json:"route"`
		}
		if err := decodeOne(decoder, &output); err != nil {
			return artifact, &Error{Code: CodeProposalRejected, Reason: "schema_mismatch", Cause: err}
		}
		if len(output.Route) == 0 || len(output.Route) > 100 {
			return artifact, &Error{Code: CodeProposalRejected, Reason: "route_size"}
		}
		seen := map[string]bool{}
		for _, step := range output.Route {
			if seen[step.NodeRevisionID] || !containsString(request.NodeRevisionIDs, step.NodeRevisionID) || strings.TrimSpace(step.TeachingIntent) == "" || strings.TrimSpace(step.CompletionCondition) == "" || utf8.RuneCountInString(step.TeachingIntent) > 1000 || utf8.RuneCountInString(step.CompletionCondition) > 1000 {
				return artifact, &Error{Code: CodeProposalRejected, Reason: "invalid_route_step"}
			}
			seen[step.NodeRevisionID] = true
			if _, err := s.knowledge.Resolve(ctx, request.KnowledgeRevisionID, step.NodeRevisionID); err != nil {
				return artifact, &Error{Code: CodeKnowledgeReferenceInvalid, Cause: err}
			}
		}
		artifact.Route = output.Route
	case ProposalActivity:
		var output struct {
			Activity ActivityProposal `json:"activity"`
		}
		if err := decodeOne(decoder, &output); err != nil {
			return artifact, &Error{Code: CodeProposalRejected, Reason: "schema_mismatch", Cause: err}
		}
		if err := s.validateActivityProposal(ctx, request, &output.Activity); err != nil {
			return artifact, err
		}
		artifact.Activity = &output.Activity
	case ProposalAssessment:
		var output struct {
			Assessment struct {
				Items          []AssessmentItem `json:"items"`
				RubricComplete bool             `json:"rubric_complete"`
				Confidence     int              `json:"confidence"`
				RiskFlags      []RiskFlag       `json:"risk_flags"`
			} `json:"assessment"`
		}
		if err := decodeOne(decoder, &output); err != nil {
			return artifact, &Error{Code: CodeProposalRejected, Reason: "schema_mismatch", Cause: err}
		}
		if request.AttemptID == "" || request.ActivityID == "" {
			return artifact, &Error{Code: CodeInvalidRequest}
		}
		activity, err := s.authority.LoadActivity(ctx, request.ActivityID)
		if err != nil {
			return artifact, err
		}
		attempt, err := s.authority.LoadAttempt(ctx, request.AttemptID)
		if err != nil {
			return artifact, err
		}
		assessment := AssessmentArtifact{ID: s.newUUID(), SessionID: activity.SessionID, AttemptID: attempt.ID, ActivityID: activity.ID, ActivityRevision: activity.Revision, Items: output.Assessment.Items, RubricComplete: output.Assessment.RubricComplete, Confidence: output.Assessment.Confidence, RiskFlags: output.Assessment.RiskFlags, ModelID: s.modelID, ModelParameters: cloneMap(s.modelParameters), PromptRevision: s.promptRevision, ProposalInputHash: inputHash, Attempts: len(categories), AttemptCategories: append([]string(nil), categories...), CreatedAt: artifact.CreatedAt}
		for _, item := range assessment.Items {
			if item.KnowledgeReferenceID != "" && !containsString(request.NodeRevisionIDs, item.KnowledgeReferenceID) {
				return artifact, &Error{Code: CodeProposalRejected, Reason: "node_not_allowed"}
			}
		}
		if _, err := EvaluateAssessment(activity, attempt, assessment); err != nil {
			return artifact, err
		}
		artifact.Assessment = &assessment
	case ProposalFreeAnswer, ProposalExplanation:
		var output struct {
			Text TextProposal `json:"text"`
		}
		if err := decodeOne(decoder, &output); err != nil {
			return artifact, &Error{Code: CodeProposalRejected, Reason: "schema_mismatch", Cause: err}
		}
		if !utf8.ValidString(output.Text.Text) || strings.TrimSpace(output.Text.Text) == "" || utf8.RuneCountInString(output.Text.Text) > MaxProposalTextRunes || len(output.Text.References) == 0 {
			return artifact, &Error{Code: CodeProposalRejected, Reason: "invalid_text"}
		}
		refs, err := s.canonicalReferences(ctx, request.KnowledgeRevisionID, output.Text.References, request.NodeRevisionIDs)
		if err != nil {
			return artifact, err
		}
		output.Text.References = refs
		artifact.Text = &output.Text
	}
	return artifact, nil
}

func (s *Service) validateActivityProposal(ctx context.Context, request ProposalRequest, value *ActivityProposal) error {
	if !utf8.ValidString(value.Prompt) || strings.TrimSpace(value.Prompt) == "" || utf8.RuneCountInString(value.Prompt) > MaxQuestionRunes || value.Difficulty < 1 || value.Difficulty > 5 {
		return &Error{Code: CodeProposalRejected, Reason: "invalid_activity"}
	}
	if value.Type != ActivityObjective && value.Type != ActivityOpen {
		return &Error{Code: CodeProposalRejected, Reason: "invalid_activity_type"}
	}
	if strings.TrimSpace(value.Rubric.Revision) == "" || len(value.Rubric.Items) == 0 || len(value.Rubric.Items) > MaxRubricItems {
		return &Error{Code: CodeProposalRejected, Reason: "invalid_rubric"}
	}
	seen := map[string]bool{}
	for _, item := range value.Rubric.Items {
		if strings.TrimSpace(item.ID) == "" || seen[item.ID] || strings.TrimSpace(item.Criterion) == "" {
			return &Error{Code: CodeProposalRejected, Reason: "invalid_rubric"}
		}
		seen[item.ID] = true
		required := map[string]bool{}
		for _, referenceID := range item.RequiredReferenceIDs {
			if referenceID == "" || required[referenceID] {
				return &Error{Code: CodeProposalRejected, Reason: "invalid_rubric_reference"}
			}
			required[referenceID] = true
		}
	}
	if value.Type == ActivityObjective && (value.Rubric.ObjectiveRule == nil || len(value.Rubric.ObjectiveRule.AcceptedAnswers) == 0) {
		return &Error{Code: CodeProposalRejected, Reason: "objective_rule_missing"}
	}
	if value.Rubric.ObjectiveRule != nil {
		for _, answer := range value.Rubric.ObjectiveRule.AcceptedAnswers {
			if !utf8.ValidString(answer) || strings.TrimSpace(answer) == "" || utf8.RuneCountInString(answer) > MaxAnswerBytes {
				return &Error{Code: CodeProposalRejected, Reason: "invalid_objective_rule"}
			}
		}
	}
	allowed := map[HelpLevel]bool{}
	for _, help := range value.AllowedHelp {
		if help != HelpNone && help != HelpHint && help != HelpScaffold && help != HelpAnswerRevealed {
			return &Error{Code: CodeProposalRejected, Reason: "invalid_help"}
		}
		if allowed[help] {
			return &Error{Code: CodeProposalRejected, Reason: "duplicate_help"}
		}
		allowed[help] = true
	}
	if len(allowed) == 0 {
		return &Error{Code: CodeProposalRejected, Reason: "invalid_help"}
	}
	if len(value.References) == 0 {
		return &Error{Code: CodeProposalRejected, Reason: "knowledge_reference_required"}
	}
	refs, err := s.canonicalReferences(ctx, request.KnowledgeRevisionID, value.References, request.NodeRevisionIDs)
	if err != nil {
		return err
	}
	if !containsReferenceNode(refs, request.FocusNodeRevisionID) {
		return &Error{Code: CodeProposalRejected, Reason: "activity_target_reference_missing"}
	}
	value.References = refs
	referenceIDs := make(map[string]bool, len(refs))
	for _, ref := range refs {
		referenceIDs[ref.NodeRevisionID] = true
	}
	for _, item := range value.Rubric.Items {
		for _, required := range item.RequiredReferenceIDs {
			if !referenceIDs[required] {
				return &Error{Code: CodeProposalRejected, Reason: "rubric_reference_not_frozen"}
			}
		}
	}
	return nil
}
func containsReferenceNode(values []KnowledgeReference, nodeRevisionID string) bool {
	if nodeRevisionID == "" {
		return false
	}
	for _, value := range values {
		if value.NodeRevisionID == nodeRevisionID {
			return true
		}
	}
	return false
}

func (s *Service) canonicalReferences(ctx context.Context, knowledgeRevision string, input []KnowledgeReference, allowed []string) ([]KnowledgeReference, error) {
	result := make([]KnowledgeReference, 0, len(input))
	seen := map[string]bool{}
	for _, candidate := range input {
		if candidate.NodeRevisionID == "" || seen[candidate.NodeRevisionID] || (allowed != nil && !containsString(allowed, candidate.NodeRevisionID)) {
			return nil, &Error{Code: CodeProposalRejected, Reason: "duplicate_or_missing_reference"}
		}
		seen[candidate.NodeRevisionID] = true
		resolved, err := s.knowledge.Resolve(ctx, knowledgeRevision, candidate.NodeRevisionID)
		if err != nil {
			return nil, &Error{Code: CodeKnowledgeReferenceInvalid, Cause: err}
		}
		if candidate.SliceSHA256 != "" && (candidate.SliceSHA256 != resolved.SliceSHA256 || candidate.Range != resolved.Range) {
			return nil, &Error{Code: CodeProposalRejected, Reason: "reference_hash_mismatch"}
		}
		result = append(result, resolved)
	}
	return result, nil
}
func decodeOne(decoder *json.Decoder, target any) error {
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return fmt.Errorf("multiple JSON values")
	}
	return nil
}
func modelCategory(err error) string {
	var typed ModelFailure
	if errors.As(err, &typed) {
		return typed.ModelCategory()
	}
	return "upstream_error"
}
func retryableModelCategory(category string) bool {
	switch category {
	case "timeout", "rate_limited", "unavailable", "upstream_error":
		return true
	}
	return false
}
func cloneMap(input map[string]any) map[string]any {
	result := make(map[string]any, len(input))
	keys := make([]string, 0, len(input))
	for key := range input {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		result[key] = input[key]
	}
	return result
}
