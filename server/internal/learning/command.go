package learning

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/edu-agent/edu-agent/server/internal/tutoring"
	"github.com/google/uuid"
)

type GoalCommand struct {
	Operation          OperationEnvelope `json:"operation"`
	GoalID             string            `json:"goal_id,omitempty"`
	Text               string            `json:"text"`
	Source             string            `json:"source"`
	PreviousRevisionID *string           `json:"previous_revision_id,omitempty"`
}
type SessionCommand struct {
	Operation      OperationEnvelope `json:"operation"`
	GoalRevisionID string            `json:"goal_revision_id"`
}
type ActionCommand struct {
	Operation      OperationEnvelope    `json:"operation"`
	Action         tutoring.Action      `json:"action"`
	ProposalID     string               `json:"proposal_id,omitempty"`
	Question       string               `json:"question,omitempty"`
	Answer         string               `json:"answer,omitempty"`
	Help           HelpLevel            `json:"help,omitempty"`
	Complete       bool                 `json:"complete,omitempty"`
	GoalRevisionID string               `json:"goal_revision_id,omitempty"`
	ExposureKind   string               `json:"exposure_kind,omitempty"`
	ExposureText   string               `json:"exposure_text,omitempty"`
	References     []KnowledgeReference `json:"knowledge_references,omitempty"`
}
type AssessmentDecisionCommand struct {
	Operation                  OperationEnvelope `json:"operation"`
	Kind                       string            `json:"kind"`
	ExpectedDispositionVersion int64             `json:"expected_disposition_version"`
	Reason                     string            `json:"reason,omitempty"`
	Items                      []AssessmentItem  `json:"items,omitempty"`
}

func (s *Service) CurrentSession(ctx context.Context) (SessionView, error) {
	return s.queries.CurrentSession(ctx)
}
func (s *Service) Session(ctx context.Context, id string) (SessionView, error) {
	return s.queries.Session(ctx, id)
}
func (s *Service) Timeline(ctx context.Context, query TimelineQuery) (TimelinePage, error) {
	return s.queries.Timeline(ctx, query)
}
func (s *Service) Routes(ctx context.Context, page CursorPageRequest) (RoutesPage, error) {
	return s.queries.Routes(ctx, page)
}
func (s *Service) Node(ctx context.Context, id string) (NodeView, error) {
	return s.queries.Node(ctx, id)
}
func (s *Service) Evidence(ctx context.Context, query EvidenceQuery) (EvidencePage, error) {
	return s.queries.EvidenceList(ctx, query)
}
func (s *Service) Reviews(ctx context.Context, query ReviewQuery) (ReviewsPage, error) {
	return s.queries.Reviews(ctx, query)
}
func (s *Service) ProjectionStatus(ctx context.Context) (ProjectionStatus, error) {
	return s.queries.ProjectionStatus(ctx)
}
func (s *Service) RebuildProjection(ctx context.Context) (ProjectionStatus, error) {
	return s.queries.Rebuild(ctx)
}

func (s *Service) CreateGoal(ctx context.Context, deviceID string, command GoalCommand) (OperationResult, error) {
	return s.authorityOperation(ctx, deviceID, command.Operation, command, func() (OperationResult, error) {
		return s.createGoal(ctx, deviceID, command)
	})
}

func (s *Service) CreateSession(ctx context.Context, deviceID string, command SessionCommand) (OperationResult, error) {
	return s.authorityOperation(ctx, deviceID, command.Operation, command, func() (OperationResult, error) {
		return s.createSession(ctx, deviceID, command)
	})
}

func (s *Service) ApplyAction(ctx context.Context, deviceID, sessionID string, command ActionCommand) (OperationResult, error) {
	return s.authorityOperation(ctx, deviceID, command.Operation, command, func() (OperationResult, error) {
		return s.applyAction(ctx, deviceID, sessionID, command)
	})
}

func (s *Service) Decide(ctx context.Context, deviceID, assessmentID string, command AssessmentDecisionCommand) (OperationResult, error) {
	return s.authorityOperation(ctx, deviceID, command.Operation, command, func() (OperationResult, error) {
		return s.decide(ctx, deviceID, assessmentID, command)
	})
}

func (s *Service) authorityOperation(ctx context.Context, deviceID string, operation OperationEnvelope, command any, execute func() (OperationResult, error)) (OperationResult, error) {
	requestHash, err := HashJSON(command)
	if err != nil {
		return OperationResult{}, err
	}
	lookup := OperationLookup{DeviceID: deviceID, OperationID: operation.OperationID, RequestHash: requestHash}
	if replay, replayErr, found := s.authority.LookupOperation(ctx, lookup); found || replayErr != nil {
		return replay, replayErr
	}
	result, err := execute()
	if err == nil || !archivableRejection(err) {
		return result, err
	}
	var domainErr *Error
	if !errors.As(err, &domainErr) {
		return result, err
	}
	archived := *domainErr
	archived.Cause = nil
	fence := AggregateExpectation{Type: operation.AggregateType, ID: operation.AggregateID, ExpectedVersion: operation.ExpectedVersion}
	expectations := []AggregateExpectation{fence}
	if archived.Code == CodeVersionConflict && archived.AggregateType != "" && archived.AggregateID != "" {
		conflictFence := AggregateExpectation{Type: archived.AggregateType, ID: archived.AggregateID, ExpectedVersion: archived.ExpectedVersion}
		if conflictFence.Type != fence.Type || conflictFence.ID != fence.ID {
			expectations = append(expectations, conflictFence)
		}
	}
	return s.authority.ArchiveRejection(ctx, OperationRejection{
		Lookup: lookup, AggregateType: operation.AggregateType, AggregateID: operation.AggregateID,
		Expectations: expectations, Error: archived, CompletedAt: s.now().UTC().Truncate(time.Microsecond),
	})
}

func archivableRejection(err error) bool {
	switch ErrorCode(err) {
	case CodeInvalidRequest, CodeNotFound, CodeVersionConflict, CodeInvalidTransition,
		CodeActivityStateConflict, CodeKnowledgeReferenceInvalid, CodeStaleProposal,
		CodeProposalRejected, CodeAssessmentDispositionConflict, CodeFocusFrameInvalidated:
		return true
	default:
		return false
	}
}

func (s *Service) createGoal(ctx context.Context, deviceID string, command GoalCommand) (OperationResult, error) {
	if err := ValidateOperation(command.Operation); err != nil {
		return OperationResult{}, err
	}
	if command.Operation.AggregateType != "goal" || command.Operation.AggregateID == "" || ValidateGoal(command.Text) != nil || ValidateGoalSource(command.Source) != nil {
		return OperationResult{}, &Error{Code: CodeInvalidRequest}
	}
	if command.GoalID == "" {
		command.GoalID = command.Operation.AggregateID
	}
	if command.GoalID != command.Operation.AggregateID {
		return OperationResult{}, &Error{Code: CodeInvalidRequest}
	}
	if command.Operation.ExpectedVersion == 0 {
		if command.PreviousRevisionID != nil {
			return OperationResult{}, &Error{Code: CodeInvalidRequest, Reason: "first_goal_revision_has_previous"}
		}
	} else {
		if command.PreviousRevisionID == nil || *command.PreviousRevisionID == "" {
			return OperationResult{}, &Error{Code: CodeInvalidRequest, Reason: "previous_goal_revision_required"}
		}
		previous, err := s.authority.LoadGoalRevision(ctx, *command.PreviousRevisionID)
		if err != nil {
			return OperationResult{}, err
		}
		if previous.ID != *command.PreviousRevisionID || previous.GoalID != command.GoalID || previous.Revision != command.Operation.ExpectedVersion {
			return OperationResult{}, &Error{Code: CodeInvalidRequest, Reason: "goal_revision_lineage"}
		}
	}
	now := s.now().UTC().Truncate(time.Microsecond)
	value := GoalRevision{ID: s.newUUID(), GoalID: command.GoalID, Revision: command.Operation.ExpectedVersion + 1, Text: strings.TrimSpace(command.Text), Source: strings.TrimSpace(command.Source), ActorDeviceID: deviceID, CreatedAt: now, PreviousRevisionID: command.PreviousRevisionID}
	batch := CommandBatch{GoalRevision: &value, Events: []EventDraft{draft(EventGoalRevisionCreated, "goal", value.GoalID, value)}, TypedResult: mustJSON(value)}
	return s.commit(ctx, deviceID, command.Operation, []AggregateExpectation{{Type: "goal", ID: value.GoalID, ExpectedVersion: command.Operation.ExpectedVersion}}, batch, command, now)
}
func (s *Service) createSession(ctx context.Context, deviceID string, command SessionCommand) (OperationResult, error) {
	if err := ValidateOperation(command.Operation); err != nil {
		return OperationResult{}, err
	}
	if command.Operation.AggregateType != "session" || command.GoalRevisionID == "" || command.Operation.ExpectedVersion != 0 {
		return OperationResult{}, &Error{Code: CodeInvalidRequest}
	}
	if _, err := s.authority.LoadGoalRevision(ctx, command.GoalRevisionID); err != nil {
		return OperationResult{}, err
	}
	now := s.now().UTC().Truncate(time.Microsecond)
	session := tutoring.Session{ID: command.Operation.AggregateID, State: tutoring.StateIdle, Context: tutoring.FocusContext{GoalRevisionID: command.GoalRevisionID}}
	transition, err := tutoring.Apply(session, tutoring.Command{Action: tutoring.ActionCreateSession, SessionID: session.ID})
	if err != nil {
		return OperationResult{}, mapTransitionError(err)
	}
	session = transition.Session
	batch := CommandBatch{Session: &session, TutoringState: string(session.State), ResultSession: true}
	batch.Events = transitionDrafts(session.ID, transition, nil)
	return s.commit(ctx, deviceID, command.Operation, []AggregateExpectation{{Type: "session", ID: session.ID, ExpectedVersion: 0}}, batch, command, now)
}

func (s *Service) applyAction(ctx context.Context, deviceID, sessionID string, command ActionCommand) (OperationResult, error) {
	if err := ValidateOperation(command.Operation); err != nil {
		return OperationResult{}, err
	}
	if command.Operation.AggregateType != "session" || command.Operation.AggregateID != sessionID {
		return OperationResult{}, &Error{Code: CodeInvalidRequest}
	}
	authority, err := s.authority.LoadSessionAuthority(ctx, sessionID)
	if err != nil {
		return OperationResult{}, err
	}
	session := authority.Session
	if session.AggregateVer != command.Operation.ExpectedVersion {
		return OperationResult{}, &Error{Code: CodeVersionConflict, AggregateType: "session", AggregateID: sessionID, ExpectedVersion: command.Operation.ExpectedVersion, CurrentVersion: session.AggregateVer, AsOfEventSequence: authority.AsOfEventSequence}
	}
	if err := s.guardFeedbackExit(ctx, session, command.Action); err != nil {
		return OperationResult{}, err
	}
	now := s.now().UTC().Truncate(time.Microsecond)
	batch := CommandBatch{}
	expectations := []AggregateExpectation{{Type: "session", ID: session.ID, ExpectedVersion: command.Operation.ExpectedVersion}}
	tcommand := tutoring.Command{Action: command.Action, SessionID: sessionID, Complete: command.Complete}
	var payloads = map[EventType][]json.RawMessage{}
	switch command.Action {
	case tutoring.ActionApplyRoute:
		proposal, err := s.requireProposal(ctx, command.ProposalID, ProposalRoute, session)
		if err != nil {
			return OperationResult{}, err
		}
		routeID := s.newUUID()
		revision := int64(1)
		if session.Context.RouteRevisionID != "" {
			current, err := s.authority.LoadRouteRevision(ctx, session.Context.RouteRevisionID)
			if err != nil {
				return OperationResult{}, err
			}
			if current.GoalRevisionID != session.Context.GoalRevisionID {
				return OperationResult{}, &Error{Code: CodeStaleProposal, Reason: "route_goal_changed"}
			}
			routeID, revision = current.RouteID, current.Revision+1
		}
		route := RouteRevision{ID: s.newUUID(), RouteID: routeID, Revision: revision, GoalRevisionID: session.Context.GoalRevisionID, KnowledgeRevisionID: proposal.KnowledgeRevisionID, PolicyVersion: RoutePolicyVersion, SourceProposalID: proposal.ID, CreatedAt: now}
		batch.Authority.RouteSteps = make(map[string]KnowledgeOwner, len(proposal.Route))
		for index, item := range proposal.Route {
			resolved, err := s.knowledge.Resolve(ctx, proposal.KnowledgeRevisionID, item.NodeRevisionID)
			if err != nil || resolved.KnowledgeRevisionID != proposal.KnowledgeRevisionID || resolved.NodeID == "" || resolved.NodeRevisionID != item.NodeRevisionID || resolved.DocumentRevisionID == "" {
				return OperationResult{}, &Error{Code: CodeKnowledgeReferenceInvalid, Cause: err}
			}
			step := RouteStep{ID: s.newUUID(), Ordinal: index, NodeID: resolved.NodeID, NodeRevisionID: resolved.NodeRevisionID, TeachingIntent: item.TeachingIntent, CompletionCondition: item.CompletionCondition}
			route.Steps = append(route.Steps, step)
			batch.Authority.RouteSteps[step.ID] = knowledgeOwner(resolved)
		}
		if !StableRouteSteps(route.Steps) {
			return OperationResult{}, &Error{Code: CodeProposalRejected}
		}
		session.Context.RouteRevisionID = route.ID
		session.Context.KnowledgeRevisionID = route.KnowledgeRevisionID
		session.Context.RouteStepID = route.Steps[0].ID
		session.Context.FocusNodeRevisionID = route.Steps[0].NodeRevisionID
		session.CompletedRoute = false
		tcommand.Context = &session.Context
		batch.RouteRevision = &route
		payloads[EventRouteRevisionCreated] = []json.RawMessage{mustJSON(route)}
	case tutoring.ActionIssueActivity, tutoring.ActionPresentReview, tutoring.ActionConvertFreeAnswerToQuiz:
		proposal, err := s.requireProposal(ctx, command.ProposalID, ProposalActivity, session)
		if err != nil {
			return OperationResult{}, err
		}
		activity, err := s.materializeActivity(ctx, session, proposal, now, command.Action == tutoring.ActionPresentReview)
		if err != nil {
			return OperationResult{}, err
		}
		if command.Action == tutoring.ActionConvertFreeAnswerToQuiz {
			if !validActiveFocusFrame(session) {
				return OperationResult{}, &Error{Code: CodeFocusFrameInvalidated}
			}
			if command.Question == "" || command.Answer == "" || command.Question != proposal.FrozenRequest.FreeQuestionID || command.Answer != proposal.FrozenRequest.FreeAnswerID {
				return OperationResult{}, &Error{Code: CodeStaleProposal, Reason: "attached_quiz_context_changed"}
			}
			question, err := s.authority.LoadFreeQuestion(ctx, command.Question)
			if err != nil {
				return OperationResult{}, err
			}
			answer, err := s.authority.LoadFreeAnswer(ctx, command.Answer)
			if err != nil {
				return OperationResult{}, err
			}
			latestQuestionID, err := s.authority.LatestFreeQuestionForFrame(ctx, session.ID, session.ActiveFrame.ID)
			if err != nil {
				return OperationResult{}, err
			}
			if latestQuestionID != question.ID || !currentFreeQuestionMatchesSession(session, question) || answer.SessionID != session.ID || answer.FocusFrameID != session.ActiveFrame.ID || answer.FreeQuestionID != question.ID || answer.KnowledgeRevisionID != question.KnowledgeRevisionID {
				return OperationResult{}, &Error{Code: CodeFocusFrameInvalidated, Reason: "attached_quiz_ownership"}
			}
			activity.AttachedFreeQuestionID = question.ID
			activity.AttachedFreeAnswerID = answer.ID
		}
		session.Context.ActivityID = &activity.ID
		session.Context.AttemptID = nil
		tcommand.Context = &session.Context
		batch.Activity = &activity
		if command.Action == tutoring.ActionPresentReview {
			payloads[EventReviewPresented] = []json.RawMessage{mustJSON(activity)}
		}
		payloads[EventActivityIssued] = []json.RawMessage{mustJSON(activity)}
	case tutoring.ActionPresentActivity:
		if session.Context.ActivityID == nil {
			return OperationResult{}, &Error{Code: CodeActivityStateConflict}
		}
		activity, err := s.authority.LoadActivity(ctx, *session.Context.ActivityID)
		if err != nil {
			return OperationResult{}, err
		}
		payloads[EventActivityPresented] = []json.RawMessage{mustJSON(activity)}
	case tutoring.ActionSubmitAttempt:
		if session.Context.ActivityID == nil || !utf8.ValidString(command.Answer) || len([]byte(command.Answer)) > MaxAnswerBytes {
			return OperationResult{}, &Error{Code: CodeInvalidRequest}
		}
		activity, err := s.authority.LoadActivity(ctx, *session.Context.ActivityID)
		if err != nil {
			return OperationResult{}, err
		}
		if !validHelp(command.Help) || !containsHelp(activity.AllowedHelp, command.Help) {
			return OperationResult{}, &Error{Code: CodeInvalidRequest, Reason: "help_not_allowed"}
		}
		attempt := Attempt{ID: s.newUUID(), SessionID: session.ID, ActivityID: activity.ID, ActivityRevision: activity.Revision, AnswerPayloadID: s.newUUID(), Answer: command.Answer, AnswerSHA256: SHA256([]byte(command.Answer)), Help: command.Help, ActorDeviceID: deviceID, OccurredAt: command.Operation.OccurredAt, ReceivedAt: now}
		session.Context.AttemptID = &attempt.ID
		tcommand.Context = &session.Context
		batch.Attempt = &attempt
		payloads[EventAttemptSubmitted] = []json.RawMessage{mustJSON(attempt)}
		if command.Help == HelpAnswerRevealed {
			exposure := Exposure{ID: s.newUUID(), SessionID: session.ID, Kind: "answer_revealed", Text: command.Answer, References: activity.References, ReceivedAt: now}
			batch.Exposures = append(batch.Exposures, exposure)
			payloads[EventExposureRecorded] = append(payloads[EventExposureRecorded], mustJSON(exposure))
		}
	case tutoring.ActionRecordAssessment:
		if session.Context.ActivityID == nil || session.Context.AttemptID == nil {
			return OperationResult{}, &Error{Code: CodeActivityStateConflict}
		}
		activity, err := s.authority.LoadActivity(ctx, *session.Context.ActivityID)
		if err != nil {
			return OperationResult{}, err
		}
		attempt, err := s.authority.LoadAttempt(ctx, *session.Context.AttemptID)
		if err != nil {
			return OperationResult{}, err
		}
		var artifact AssessmentArtifact
		if activity.Type == ActivityObjective {
			inputHash, hashErr := HashJSON(struct {
				Activity Activity `json:"activity"`
				Attempt  Attempt  `json:"attempt"`
			}{Activity: activity, Attempt: attempt})
			if hashErr != nil {
				return OperationResult{}, hashErr
			}
			artifact = AssessmentArtifact{ID: s.newUUID(), SessionID: session.ID, AttemptID: attempt.ID, ActivityID: activity.ID, ActivityRevision: activity.Revision, RubricComplete: true, Confidence: 1000, ModelID: "deterministic-objective", ModelParameters: map[string]any{}, PromptRevision: "objective-rule-v1", ProposalInputHash: inputHash, Attempts: 1, AttemptCategories: []string{"deterministic"}, CreatedAt: now}
		} else {
			proposal, err := s.requireProposal(ctx, command.ProposalID, ProposalAssessment, session)
			if err != nil {
				return OperationResult{}, err
			}
			if proposal.Assessment == nil || proposal.Assessment.AttemptID != attempt.ID || proposal.Assessment.ActivityID != activity.ID {
				return OperationResult{}, &Error{Code: CodeStaleProposal}
			}
			artifact = *proposal.Assessment
		}
		acceptance, err := EvaluateAssessment(activity, attempt, artifact)
		if err != nil {
			return OperationResult{}, err
		}
		assessmentOwners, err := assessmentAuthority(activity, artifact)
		if err != nil {
			return OperationResult{}, err
		}
		decision := AssessmentDecision{ID: s.newUUID(), AssessmentID: artifact.ID, Version: 1, Disposition: acceptance.Disposition, Items: artifact.Items, ActorDeviceID: deviceID, CreatedAt: now}
		batch.Assessment = &artifact
		batch.Authority.AssessmentItems = assessmentOwners
		batch.Decisions = []AssessmentDecision{decision}
		batch.Disposition = acceptance.Disposition
		payloads[EventAssessmentRecorded] = []json.RawMessage{mustJSON(artifact)}
		if acceptance.Disposition == DispositionProvisional {
			payloads[EventAssessmentMarkedProvisional] = []json.RawMessage{mustJSON(AssessmentProjectionEvent{AssessmentID: artifact.ID, NodeRevisionID: activity.TargetNodeRevisionID, Reasons: acceptance.Reasons, Decision: decision})}
		} else {
			if attempt.Help == HelpAnswerRevealed {
				return OperationResult{}, &Error{Code: CodeAssessmentDispositionConflict, Reason: "answer_revealed"}
			}
			evidence := s.makeEvidence(activity, attempt, artifact, decision, artifact.Items, acceptance.Outcome, now)
			decision.ProducedEvidenceID = &evidence.ID
			batch.Decisions[0] = decision
			batch.Evidence = []AcceptedEvidence{evidence}
			owner, err := evidenceAuthority(activity, evidence)
			if err != nil {
				return OperationResult{}, err
			}
			batch.Authority.Evidence = map[string]EvidenceOwner{evidence.ID: owner}
			payloads[EventAssessmentAccepted] = []json.RawMessage{mustJSON(AssessmentProjectionEvent{AssessmentID: artifact.ID, NodeRevisionID: evidence.NodeRevisionID, Decision: decision})}
			payloads[EventEvidenceAccepted] = []json.RawMessage{mustJSON(evidence)}
			batch.Misconceptions, err = s.recomputeMisconceptions(ctx, evidence.NodeRevisionID, "", &evidence)
			if err != nil {
				return OperationResult{}, err
			}
			for _, item := range batch.Misconceptions {
				payloads[EventMisconceptionHypothesisRevised] = append(payloads[EventMisconceptionHypothesisRevised], mustJSON(item))
			}
		}
	case tutoring.ActionRecordExposure:
		exposure := Exposure{ID: s.newUUID(), SessionID: session.ID, Kind: command.ExposureKind, Text: command.ExposureText, References: command.References, ReceivedAt: now}
		if command.ProposalID != "" {
			proposal, err := s.requireProposal(ctx, command.ProposalID, ProposalExplanation, session)
			if err != nil {
				return OperationResult{}, err
			}
			exposure.Text = proposal.Text.Text
			exposure.References = proposal.Text.References
			exposure.SourceProposalID = proposal.ID
		} else if len(exposure.References) > 0 {
			if session.Context.KnowledgeRevisionID == "" {
				return OperationResult{}, &Error{Code: CodeKnowledgeReferenceInvalid}
			}
			references, err := s.canonicalReferences(ctx, session.Context.KnowledgeRevisionID, exposure.References, nil)
			if err != nil {
				return OperationResult{}, err
			}
			exposure.References = references
		}
		if command.ProposalID != "" && exposure.Kind == "" {
			exposure.Kind = "explanation"
		}
		if exposure.Kind != "reading" && exposure.Kind != "explanation" {
			return OperationResult{}, &Error{Code: CodeInvalidRequest, Reason: "invalid_exposure_kind"}
		}
		if strings.TrimSpace(exposure.Text) == "" {
			return OperationResult{}, &Error{Code: CodeInvalidRequest}
		}
		batch.Exposures = []Exposure{exposure}
		payloads[EventExposureRecorded] = []json.RawMessage{mustJSON(exposure)}
	case tutoring.ActionAskFreeQuestion:
		if strings.TrimSpace(command.Question) == "" || utf8.RuneCountInString(command.Question) > MaxQuestionRunes {
			return OperationResult{}, &Error{Code: CodeInvalidRequest}
		}
		frameID := s.newUUID()
		if session.ActiveFrame != nil {
			frameID = session.ActiveFrame.ID
		}
		tcommand.FrameID = frameID
		question := tutoring.FreeQuestion{ID: s.newUUID(), SessionID: session.ID, FocusFrameID: frameID, Text: command.Question, KnowledgeRevisionID: session.Context.KnowledgeRevisionID, ActorDeviceID: deviceID, OccurredAt: command.Operation.OccurredAt, ReceivedAt: now}
		batch.FreeQuestion = &question
		payloads[EventFreeQuestionAsked] = []json.RawMessage{mustJSON(question)}
	case tutoring.ActionRecordFreeAnswer:
		proposal, err := s.requireProposal(ctx, command.ProposalID, ProposalFreeAnswer, session)
		if err != nil {
			return OperationResult{}, err
		}
		if !validActiveFocusFrame(session) || proposal.Text == nil {
			return OperationResult{}, &Error{Code: CodeFocusFrameInvalidated}
		}
		questionID, err := s.authority.LatestFreeQuestionForFrame(ctx, session.ID, session.ActiveFrame.ID)
		if err != nil {
			return OperationResult{}, err
		}
		question, err := s.authority.LoadFreeQuestion(ctx, questionID)
		if err != nil {
			return OperationResult{}, err
		}
		if questionID != proposal.FrozenRequest.FreeQuestionID || !currentFreeQuestionMatchesSession(session, question) || proposal.KnowledgeRevisionID != question.KnowledgeRevisionID {
			return OperationResult{}, &Error{Code: CodeStaleProposal, Reason: "free_question_context_changed"}
		}
		answer := tutoring.FreeAnswer{ID: s.newUUID(), SessionID: session.ID, FocusFrameID: session.ActiveFrame.ID, FreeQuestionID: questionID, Text: proposal.Text.Text, KnowledgeRevisionID: proposal.KnowledgeRevisionID, References: toFrozen(proposal.Text.References), SourceProposalID: proposal.ID, ReceivedAt: now}
		batch.FreeAnswer = &answer
		exposure := Exposure{ID: s.newUUID(), SessionID: session.ID, Kind: "free_answer", Text: answer.Text, References: proposal.Text.References, SourceProposalID: proposal.ID, ReceivedAt: now}
		batch.Exposures = []Exposure{exposure}
		payloads[EventFreeAnswerRecorded] = []json.RawMessage{mustJSON(answer)}
		payloads[EventExposureRecorded] = []json.RawMessage{mustJSON(exposure)}
	case tutoring.ActionSwitchGoal:
		if command.GoalRevisionID == "" {
			return OperationResult{}, &Error{Code: CodeInvalidRequest}
		}
		goal, err := s.authority.LoadGoalRevision(ctx, command.GoalRevisionID)
		if err != nil {
			return OperationResult{}, err
		}
		session.Context = tutoring.FocusContext{GoalRevisionID: command.GoalRevisionID}
		tcommand.Context = &session.Context
		batch.InvalidateFrame = true
		expectations = append(expectations, AggregateExpectation{Type: "goal", ID: goal.GoalID, ExpectedVersion: goal.Revision})
	case tutoring.ActionAcknowledgeFeedback:
		if !session.AttachedQuiz {
			if session.Context.RouteRevisionID == "" || session.Context.RouteStepID == "" {
				return OperationResult{}, &Error{Code: CodeInvalidTransition, Reason: "route_context_missing"}
			}
			route, err := s.authority.LoadRouteRevision(ctx, session.Context.RouteRevisionID)
			if err != nil {
				return OperationResult{}, err
			}
			if route.ID != session.Context.RouteRevisionID || route.GoalRevisionID != session.Context.GoalRevisionID || route.KnowledgeRevisionID != session.Context.KnowledgeRevisionID {
				return OperationResult{}, &Error{Code: CodeInvalidTransition, Reason: "route_ownership"}
			}
			position := -1
			for index, step := range route.Steps {
				if step.ID == session.Context.RouteStepID {
					position = index
					break
				}
			}
			if position < 0 {
				return OperationResult{}, &Error{Code: CodeInvalidTransition, Reason: "route_step_missing"}
			}
			if position+1 >= len(route.Steps) {
				session.Context.ActivityID = nil
				session.Context.AttemptID = nil
				tcommand.Context = &session.Context
				tcommand.Complete = true
				session.CompletedRoute = true
			} else {
				next := route.Steps[position+1]
				session.Context.RouteStepID = next.ID
				session.Context.FocusNodeRevisionID = next.NodeRevisionID
				session.Context.ActivityID = nil
				session.Context.AttemptID = nil
				tcommand.Context = &session.Context
			}
		}
	case tutoring.ActionResumeFocus:
		batch.ResumeFrame = true
	case tutoring.ActionEndActivity, tutoring.ActionCompleteSession:
		batch.InvalidateFrame = true
	case tutoring.ActionStartDiagnostic:
	default:
		return OperationResult{}, &Error{Code: CodeInvalidRequest}
	}
	transition, err := tutoring.Apply(session, tcommand)
	if err != nil {
		return OperationResult{}, mapTransitionError(err)
	}
	session = transition.Session
	batch.Session = &session
	if session.ActiveFrame != nil {
		batch.FocusFrame = session.ActiveFrame
	}
	batch.TutoringState = string(session.State)
	batch.ResultSession = true
	batch.Events = transitionDrafts(session.ID, transition, payloads)
	return s.commit(ctx, deviceID, command.Operation, expectations, batch, command, now)
}

func (s *Service) guardFeedbackExit(ctx context.Context, session tutoring.Session, action tutoring.Action) error {
	if session.State != tutoring.StateFeedback || (action != tutoring.ActionAcknowledgeFeedback && action != tutoring.ActionEndActivity && action != tutoring.ActionSwitchGoal) {
		return nil
	}
	if session.Context.AttemptID == nil {
		return &Error{Code: CodeActivityStateConflict, Reason: "feedback_attempt_missing"}
	}
	artifact, decision, err := s.authority.LoadAssessmentForAttempt(ctx, *session.Context.AttemptID)
	if err != nil {
		return err
	}
	if artifact.SessionID != session.ID || artifact.AttemptID != *session.Context.AttemptID || decision.AssessmentID != artifact.ID {
		return &Error{Code: CodeActivityStateConflict, Reason: "feedback_assessment_ownership"}
	}
	if decision.Disposition == DispositionProvisional {
		return &Error{Code: CodeAssessmentDispositionConflict, CurrentDisposition: string(decision.Disposition), Reason: "provisional_feedback_unresolved"}
	}
	return nil
}

func (s *Service) decide(ctx context.Context, deviceID, assessmentID string, command AssessmentDecisionCommand) (OperationResult, error) {
	if err := ValidateOperation(command.Operation); err != nil {
		return OperationResult{}, err
	}
	if command.Operation.AggregateType != "session" || command.Operation.AggregateID == "" || assessmentID == "" {
		return OperationResult{}, &Error{Code: CodeInvalidRequest}
	}
	authority, err := s.authority.LoadSessionAuthority(ctx, command.Operation.AggregateID)
	if err != nil {
		return OperationResult{}, err
	}
	session := authority.Session
	if session.AggregateVer != command.Operation.ExpectedVersion {
		return OperationResult{}, &Error{Code: CodeVersionConflict, AggregateType: "session", AggregateID: session.ID, ExpectedVersion: command.Operation.ExpectedVersion, CurrentVersion: session.AggregateVer, AsOfEventSequence: authority.AsOfEventSequence}
	}
	if session.State != tutoring.StateFeedback || session.Context.ActivityID == nil || session.Context.AttemptID == nil {
		return OperationResult{}, &Error{Code: CodeActivityStateConflict, Reason: "assessment_decision_requires_current_feedback"}
	}
	artifact, current, err := s.authority.LoadAssessmentForAttempt(ctx, *session.Context.AttemptID)
	if err != nil {
		return OperationResult{}, err
	}
	if artifact.ID != assessmentID || artifact.SessionID != session.ID || artifact.ActivityID != *session.Context.ActivityID || artifact.AttemptID != *session.Context.AttemptID || current.AssessmentID != artifact.ID {
		return OperationResult{}, &Error{Code: CodeActivityStateConflict, Reason: "assessment_decision_not_current"}
	}
	activity, err := s.authority.LoadActivity(ctx, *session.Context.ActivityID)
	if err != nil {
		return OperationResult{}, err
	}
	attempt, err := s.authority.LoadAttempt(ctx, *session.Context.AttemptID)
	if err != nil {
		return OperationResult{}, err
	}
	if !activityOwnsCurrentAssessment(session, activity, attempt, artifact) {
		return OperationResult{}, &Error{Code: CodeActivityStateConflict, Reason: "assessment_decision_chain_mismatch"}
	}
	effect, err := DecideAssessment(current, artifact, DecisionCommand{Kind: command.Kind, ExpectedVersion: command.ExpectedDispositionVersion, Reason: command.Reason, Items: command.Items}, ConfirmableAssessment(activity, attempt, artifact))
	if err != nil {
		return OperationResult{}, err
	}
	if effect.CreateEvidence && attempt.Help == HelpAnswerRevealed {
		return OperationResult{}, &Error{Code: CodeAssessmentDispositionConflict, CurrentDisposition: string(current.Disposition), Reason: "answer_revealed"}
	}
	now := s.now().UTC().Truncate(time.Microsecond)
	decision := AssessmentDecision{ID: s.newUUID(), AssessmentID: assessmentID, Version: current.Version + 1, Disposition: effect.Disposition, Items: effect.Items, Reason: command.Reason, ActorDeviceID: deviceID, CreatedAt: now, ReplacesDecisionID: &current.ID}
	batch := CommandBatch{Session: &session, Decisions: []AssessmentDecision{decision}, Disposition: effect.Disposition, TutoringState: string(session.State)}
	node := activity.TargetNodeRevisionID
	invalidatedEvidenceID := ""
	if effect.InvalidateEvidence && current.ProducedEvidenceID != nil {
		invalidatedEvidenceID = *current.ProducedEvidenceID
		batch.Invalidations = []EvidenceInvalidation{{ID: s.newUUID(), EvidenceID: invalidatedEvidenceID, DecisionID: &decision.ID, Reason: command.Kind}}
	}
	var acceptedEvidence *AcceptedEvidence
	if effect.CreateEvidence {
		outcome, err := ValidateAssessmentReplacement(activity, attempt, artifact, effect.Items)
		if err != nil {
			return OperationResult{}, err
		}
		evidence := s.makeEvidence(activity, attempt, artifact, decision, effect.Items, outcome, now)
		decision.ProducedEvidenceID = &evidence.ID
		batch.Decisions[0] = decision
		batch.Evidence = []AcceptedEvidence{evidence}
		owner, err := evidenceAuthority(activity, evidence)
		if err != nil {
			return OperationResult{}, err
		}
		batch.Authority.Evidence = map[string]EvidenceOwner{evidence.ID: owner}
		acceptedEvidence = &evidence
	}
	if invalidatedEvidenceID != "" || acceptedEvidence != nil {
		batch.Misconceptions, err = s.recomputeMisconceptions(ctx, node, invalidatedEvidenceID, acceptedEvidence)
		if err != nil {
			return OperationResult{}, err
		}
	}
	eventType := EventAssessmentOverridden
	if effect.Disposition == DispositionAccepted {
		eventType = EventAssessmentAccepted
	} else if effect.Disposition == DispositionVoided {
		eventType = EventAssessmentVoided
	}
	events := []EventDraft{draft(eventType, "session", artifact.SessionID, AssessmentProjectionEvent{AssessmentID: artifact.ID, NodeRevisionID: node, Decision: decision})}
	if invalidatedEvidenceID != "" {
		events = append(events, draft(EventEvidenceInvalidated, "session", artifact.SessionID, map[string]any{"evidence_id": invalidatedEvidenceID}))
	}
	if acceptedEvidence != nil {
		events = append(events, draft(EventEvidenceAccepted, "session", artifact.SessionID, *acceptedEvidence))
	}
	for _, item := range batch.Misconceptions {
		events = append(events, draft(EventMisconceptionHypothesisRevised, "session", artifact.SessionID, item))
	}
	events = append(events, draft(EventTutoringStateChanged, "session", artifact.SessionID, SessionProjection{Session: session}))
	batch.Events = events
	batch.TypedResult = mustJSON(decision)
	return s.commit(ctx, deviceID, command.Operation, []AggregateExpectation{{Type: "session", ID: artifact.SessionID, ExpectedVersion: command.Operation.ExpectedVersion}}, batch, command, now)
}

func activityOwnsCurrentAssessment(session tutoring.Session, activity Activity, attempt Attempt, artifact AssessmentArtifact) bool {
	return activity.ID == *session.Context.ActivityID &&
		activity.SessionID == session.ID &&
		activity.GoalRevisionID == session.Context.GoalRevisionID &&
		activity.RouteRevisionID == session.Context.RouteRevisionID &&
		activity.RouteStepID == session.Context.RouteStepID &&
		activity.KnowledgeRevisionID == session.Context.KnowledgeRevisionID &&
		activity.TargetNodeRevisionID == session.Context.FocusNodeRevisionID &&
		attempt.ID == *session.Context.AttemptID &&
		attempt.SessionID == session.ID &&
		attempt.ActivityID == activity.ID &&
		attempt.ActivityRevision == activity.Revision &&
		artifact.SessionID == session.ID &&
		artifact.ActivityID == activity.ID &&
		artifact.ActivityRevision == activity.Revision &&
		artifact.AttemptID == attempt.ID
}

func (s *Service) commit(ctx context.Context, deviceID string, operation OperationEnvelope, expectations []AggregateExpectation, batch CommandBatch, hashValue any, now time.Time) (OperationResult, error) {
	hash, err := HashJSON(hashValue)
	if err != nil {
		return OperationResult{}, err
	}
	return s.authority.Commit(ctx, CommitRequest{DeviceID: deviceID, Operation: operation, RequestHash: hash, Expectations: expectations, Batch: batch, ReceivedAt: now})
}
func (s *Service) requireProposal(ctx context.Context, id string, kind ProposalType, session tutoring.Session) (ProposalArtifact, error) {
	if id == "" {
		return ProposalArtifact{}, &Error{Code: CodeInvalidRequest}
	}
	proposal, err := s.authority.LoadProposal(ctx, id)
	if err != nil {
		return proposal, err
	}
	if proposal.Type != kind || proposal.AggregateType != "session" || proposal.AggregateID != session.ID || proposal.AggregateVersion != session.AggregateVer {
		return proposal, &Error{Code: CodeStaleProposal}
	}
	if err := s.validateProposalForApply(ctx, proposal, kind, session); err != nil {
		return proposal, err
	}
	return proposal, nil
}
func (s *Service) materializeActivity(ctx context.Context, session tutoring.Session, proposal ProposalArtifact, now time.Time, review bool) (Activity, error) {
	if proposal.Activity == nil || session.Context.GoalRevisionID == "" || session.Context.RouteRevisionID == "" || session.Context.RouteStepID == "" || session.Context.FocusNodeRevisionID == "" {
		return Activity{}, &Error{Code: CodeStaleProposal}
	}
	route, err := s.authority.LoadRouteRevision(ctx, session.Context.RouteRevisionID)
	if err != nil {
		return Activity{}, err
	}
	if route.ID != session.Context.RouteRevisionID || route.GoalRevisionID != session.Context.GoalRevisionID || route.KnowledgeRevisionID != proposal.KnowledgeRevisionID {
		return Activity{}, &Error{Code: CodeStaleProposal, Reason: "activity_route_changed"}
	}
	var target *RouteStep
	for index := range route.Steps {
		if route.Steps[index].ID == session.Context.RouteStepID {
			target = &route.Steps[index]
			break
		}
	}
	if target == nil || target.NodeID == "" || target.NodeRevisionID != session.Context.FocusNodeRevisionID {
		return Activity{}, &Error{Code: CodeStaleProposal, Reason: "activity_target_changed"}
	}
	canonicalTarget := false
	for _, reference := range proposal.Activity.References {
		if reference.NodeRevisionID == target.NodeRevisionID && reference.NodeID == target.NodeID && reference.KnowledgeRevisionID == route.KnowledgeRevisionID {
			canonicalTarget = true
			break
		}
	}
	if !canonicalTarget {
		return Activity{}, &Error{Code: CodeProposalRejected, Reason: "activity_target_reference_missing"}
	}
	value := Activity{ID: s.newUUID(), Revision: 1, SessionID: session.ID, GoalRevisionID: session.Context.GoalRevisionID, RouteRevisionID: route.ID, RouteStepID: target.ID, KnowledgeRevisionID: route.KnowledgeRevisionID, TargetNodeID: target.NodeID, TargetNodeRevisionID: target.NodeRevisionID, Prompt: proposal.Activity.Prompt, Type: proposal.Activity.Type, Rubric: proposal.Activity.Rubric, Difficulty: proposal.Activity.Difficulty, AllowedHelp: proposal.Activity.AllowedHelp, References: proposal.Activity.References, ActivityPolicyVersion: ActivityPolicyVersion, AssessmentPolicyVersion: AssessmentPolicyVersion, ReviewPolicyVersion: ReviewPolicyVersion, SourceProposalID: proposal.ID, AttachedFreeQuestionID: proposal.FrozenRequest.FreeQuestionID, AttachedFreeAnswerID: proposal.FrozenRequest.FreeAnswerID, Review: review, CreatedAt: now}
	return CloneActivity(value), nil
}
func (s *Service) makeEvidence(activity Activity, attempt Attempt, artifact AssessmentArtifact, decision AssessmentDecision, items []AssessmentItem, outcome Outcome, now time.Time) AcceptedEvidence {
	kind := EvidencePracticeRecall
	if activity.Review {
		kind = EvidenceReviewRecall
	}
	var misconceptions []MisconceptionCandidate
	var rubricOutcomes []RubricOutcome
	for _, item := range items {
		rubricOutcomes = append(rubricOutcomes, RubricOutcome{RubricItemID: item.RubricItemID, Conclusion: item.Conclusion})
		if item.MisconceptionCandidate != "" && (item.Conclusion == ConclusionFail || item.Conclusion == ConclusionPartial) {
			misconceptions = append(misconceptions, MisconceptionCandidate{RubricItemID: item.RubricItemID, Text: item.MisconceptionCandidate})
		}
	}
	return AcceptedEvidence{ID: s.newUUID(), DispositionDecisionID: decision.ID, AssessmentID: artifact.ID, AttemptID: attempt.ID, ActivityID: activity.ID, ActivityRevision: activity.Revision, GoalRevisionID: activity.GoalRevisionID, RouteRevisionID: activity.RouteRevisionID, KnowledgeRevisionID: activity.KnowledgeRevisionID, NodeRevisionID: activity.TargetNodeRevisionID, RubricRevision: activity.Rubric.Revision, Kind: kind, ActivityType: activity.Type, Outcome: outcome, Help: attempt.Help, ReceivedAt: now, AcceptancePolicyVersion: AssessmentPolicyVersion, ReducerPolicyVersion: MasteryReducerVersion, ReviewPolicyVersion: ReviewPolicyVersion, Misconceptions: misconceptions, RubricOutcomes: rubricOutcomes}
}

func knowledgeOwner(reference KnowledgeReference) KnowledgeOwner {
	return KnowledgeOwner{
		KnowledgeRevisionID: reference.KnowledgeRevisionID,
		NodeID:              reference.NodeID, NodeRevisionID: reference.NodeRevisionID,
		DocumentRevisionID: reference.DocumentRevisionID,
	}
}

func assessmentAuthority(activity Activity, artifact AssessmentArtifact) ([]KnowledgeOwner, error) {
	owners := make([]KnowledgeOwner, len(artifact.Items))
	refs := make(map[string]KnowledgeReference, len(activity.References))
	for _, reference := range activity.References {
		refs[reference.NodeRevisionID] = reference
	}
	for index, item := range artifact.Items {
		if item.KnowledgeReferenceID == "" {
			continue
		}
		reference, ok := refs[item.KnowledgeReferenceID]
		if !ok || reference.KnowledgeRevisionID != activity.KnowledgeRevisionID || reference.NodeID == "" || reference.DocumentRevisionID == "" {
			return nil, &Error{Code: CodeKnowledgeReferenceInvalid, Reason: "assessment_reference_owner_missing"}
		}
		owners[index] = knowledgeOwner(reference)
	}
	return owners, nil
}

func evidenceAuthority(activity Activity, evidence AcceptedEvidence) (EvidenceOwner, error) {
	if evidence.ActivityID != activity.ID || evidence.ActivityRevision != activity.Revision || evidence.KnowledgeRevisionID != activity.KnowledgeRevisionID || evidence.NodeRevisionID != activity.TargetNodeRevisionID {
		return EvidenceOwner{}, &Error{Code: CodeKnowledgeReferenceInvalid, Reason: "evidence_activity_owner_mismatch"}
	}
	for _, reference := range activity.References {
		if reference.KnowledgeRevisionID == activity.KnowledgeRevisionID && reference.NodeID == activity.TargetNodeID && reference.NodeRevisionID == activity.TargetNodeRevisionID && reference.DocumentRevisionID != "" {
			return EvidenceOwner{SessionID: activity.SessionID, KnowledgeOwner: knowledgeOwner(reference)}, nil
		}
	}
	return EvidenceOwner{}, &Error{Code: CodeKnowledgeReferenceInvalid, Reason: "evidence_reference_owner_missing"}
}

func (s *Service) recomputeMisconceptions(ctx context.Context, nodeRevisionID, invalidatedEvidenceID string, replacement *AcceptedEvidence) ([]MisconceptionHypothesis, error) {
	values, err := s.authority.LoadValidEvidence(ctx, nodeRevisionID)
	if err != nil {
		return nil, err
	}
	filtered := values[:0]
	for _, value := range values {
		if value.ID != invalidatedEvidenceID {
			filtered = append(filtered, value)
		}
	}
	values = filtered
	if replacement != nil {
		values = append(values, *replacement)
	}
	current := ReduceNode(nodeRevisionID, values, map[string]bool{}, nil).Misconceptions
	previous, err := s.authority.LoadMisconceptions(ctx, nodeRevisionID)
	if err != nil {
		return nil, err
	}
	previousByID := make(map[string]MisconceptionHypothesis, len(previous))
	for _, value := range previous {
		previousByID[value.ID] = value
	}
	seen := map[string]bool{}
	for index := range current {
		seen[current[index].ID] = true
		if old, ok := previousByID[current[index].ID]; ok {
			current[index].Revision = old.Revision + 1
		} else {
			current[index].Revision = 1
		}
	}
	for _, old := range previous {
		if seen[old.ID] {
			continue
		}
		old.Revision++
		old.Status = MisconceptionResolved
		if replacement != nil {
			old.CounterEvidenceIDs = append(old.CounterEvidenceIDs, replacement.ID)
			old.CausedByEvidenceID = replacement.ID
		}
		current = append(current, old)
	}
	sort.Slice(current, func(i, j int) bool { return current[i].ID < current[j].ID })
	return current, nil
}

var supplementalEventOrder = []EventType{
	EventGoalRevisionCreated, EventRouteRevisionCreated, EventLearningSessionStarted,
	EventActivityIssued, EventReviewPresented, EventActivityPresented, EventAttemptSubmitted,
	EventAssessmentRecorded, EventAssessmentMarkedProvisional, EventAssessmentAccepted,
	EventAssessmentOverridden, EventAssessmentVoided, EventEvidenceInvalidated,
	EventEvidenceAccepted, EventExposureRecorded, EventFocusSuspended, EventFreeQuestionAsked,
	EventFreeAnswerRecorded, EventFocusResumed, EventRouteAdvanced, EventLearningCompleted,
	EventMisconceptionHypothesisRevised, EventRedacted, EventTutoringStateChanged,
}

func transitionDrafts(sessionID string, transition tutoring.Transition, payloads map[EventType][]json.RawMessage) []EventDraft {
	queues := make(map[EventType][]json.RawMessage, len(payloads))
	for kind, values := range payloads {
		queues[kind] = append([]json.RawMessage(nil), values...)
	}
	stateChanges := 0
	lastStateChange := -1
	for index, name := range transition.Events {
		if EventType(name) == EventTutoringStateChanged {
			stateChanges++
			lastStateChange = index
		}
	}
	stateSequence := make([]tutoring.State, stateChanges)
	for index := range stateSequence {
		stateSequence[index] = transition.After
	}
	if stateChanges == len(transition.Intermediate)+1 {
		copy(stateSequence, transition.Intermediate)
		stateSequence[len(stateSequence)-1] = transition.After
	}
	stateIndex := 0
	result := make([]EventDraft, 0, len(transition.Events)+len(queues))
	appendEvent := func(kind EventType, state tutoring.State) {
		var payload json.RawMessage
		if sessionSnapshotEvent(kind) {
			snapshot := transition.Session
			snapshot.ID = sessionID
			snapshot.State = state
			payload = mustJSON(SessionProjection{Session: snapshot})
		} else if values := queues[kind]; len(values) > 0 {
			payload = append(json.RawMessage(nil), values[0]...)
			queues[kind] = values[1:]
		} else {
			snapshot := transition.Session
			snapshot.ID = sessionID
			snapshot.State = state
			payload = mustJSON(SessionProjection{Session: snapshot})
		}
		result = append(result, EventDraft{Type: kind, AggregateType: "session", AggregateID: sessionID, Payload: payload})
	}
	appendRemaining := func() {
		for _, kind := range supplementalEventOrder {
			for len(queues[kind]) > 0 {
				appendEvent(kind, transition.After)
			}
		}
		var unknown []string
		for kind, values := range queues {
			if len(values) > 0 {
				unknown = append(unknown, string(kind))
			}
		}
		sort.Strings(unknown)
		for _, name := range unknown {
			kind := EventType(name)
			for len(queues[kind]) > 0 {
				appendEvent(kind, transition.After)
			}
		}
	}
	for index, name := range transition.Events {
		if index == lastStateChange {
			appendRemaining()
		}
		kind := EventType(name)
		state := transition.After
		switch kind {
		case EventTutoringStateChanged:
			state = stateSequence[stateIndex]
			stateIndex++
		case EventFocusSuspended:
			state = tutoring.StateFocusSuspended
		case EventFocusResumed:
			state = tutoring.StateFocusResumed
		case EventRouteAdvanced:
			if len(transition.Intermediate) > 0 {
				state = transition.Intermediate[len(transition.Intermediate)-1]
			}
		case EventLearningCompleted:
			state = tutoring.StateCompleted
		}
		appendEvent(kind, state)
	}
	appendRemaining()
	return result
}

func sessionSnapshotEvent(kind EventType) bool {
	switch kind {
	case EventLearningSessionStarted, EventTutoringStateChanged, EventFocusSuspended,
		EventFocusResumed, EventRouteAdvanced, EventLearningCompleted:
		return true
	default:
		return false
	}
}
func draft(kind EventType, aggregateType, aggregateID string, value any) EventDraft {
	return EventDraft{Type: kind, AggregateType: aggregateType, AggregateID: aggregateID, Payload: mustJSON(value)}
}
func mustJSON(value any) json.RawMessage {
	encoded, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return encoded
}
func validHelp(value HelpLevel) bool {
	return value == HelpNone || value == HelpHint || value == HelpScaffold || value == HelpAnswerRevealed
}
func containsHelp(values []HelpLevel, target HelpLevel) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
func combineDecisionOutcomes(items []AssessmentItem) Outcome {
	values := make([]Outcome, 0, len(items))
	for _, item := range items {
		switch item.Conclusion {
		case ConclusionPass:
			values = append(values, OutcomePass)
		case ConclusionPartial:
			values = append(values, OutcomePartial)
		case ConclusionFail:
			values = append(values, OutcomeFail)
		}
	}
	return combineOutcomes(values)
}
func mapTransitionError(err error) error {
	if err == nil {
		return nil
	}
	if strings.Contains(err.Error(), tutoring.ErrFocusFrameInvalid.Error()) {
		return &Error{Code: CodeFocusFrameInvalidated, Cause: err}
	}
	return &Error{Code: CodeInvalidTransition, Cause: err}
}
func toFrozen(values []KnowledgeReference) []tutoring.FrozenReference {
	result := make([]tutoring.FrozenReference, len(values))
	for index, value := range values {
		result[index] = tutoring.FrozenReference{KnowledgeRevisionID: value.KnowledgeRevisionID, NodeID: value.NodeID, NodeRevisionID: value.NodeRevisionID, DocumentRevisionID: value.DocumentRevisionID, Start: value.Range.Start, End: value.Range.End, Slice: value.Slice, SliceSHA256: value.SliceSHA256}
	}
	return result
}
func validUUID(value string) bool { _, err := uuid.Parse(value); return err == nil }
