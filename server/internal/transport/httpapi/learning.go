package httpapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/edu-agent/edu-agent/server/internal/learning"
	"github.com/edu-agent/edu-agent/server/internal/tutoring"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"
)

type operationInput struct {
	OperationID          *string    `json:"operation_id"`
	PayloadSchemaVersion *int       `json:"payload_schema_version"`
	AggregateType        *string    `json:"aggregate_type"`
	AggregateID          *string    `json:"aggregate_id"`
	ExpectedVersion      *int64     `json:"expected_version"`
	OccurredAt           *time.Time `json:"occurred_at,omitempty"`
}

func (value operationInput) operation(payload any, aggregateType, pathID string) (learning.OperationEnvelope, error) {
	if value.OperationID == nil || value.PayloadSchemaVersion == nil || value.AggregateType == nil || value.AggregateID == nil || value.ExpectedVersion == nil {
		return learning.OperationEnvelope{}, invalidLearningInput()
	}
	if !validLearningUUID(*value.OperationID) || *value.PayloadSchemaVersion != learning.EventSchemaVersion || *value.AggregateType != aggregateType || !validLearningUUID(*value.AggregateID) || *value.ExpectedVersion < 0 {
		return learning.OperationEnvelope{}, invalidLearningInput()
	}
	if pathID != "" && *value.AggregateID != pathID {
		return learning.OperationEnvelope{}, invalidLearningInput()
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return learning.OperationEnvelope{}, invalidLearningInput()
	}
	return learning.OperationEnvelope{OperationID: *value.OperationID, PayloadSchemaVersion: *value.PayloadSchemaVersion, AggregateType: *value.AggregateType, AggregateID: *value.AggregateID, ExpectedVersion: *value.ExpectedVersion, OccurredAt: value.OccurredAt, Payload: raw}, nil
}

type learningGoalInput struct {
	operationInput
	GoalID             *string `json:"goal_id,omitempty"`
	Text               *string `json:"text"`
	Source             *string `json:"source"`
	PreviousRevisionID *string `json:"previous_revision_id,omitempty"`
}

type tutoringSessionInput struct {
	operationInput
	GoalRevisionID *string `json:"goal_revision_id"`
}

type tutoringProposalInput struct {
	RequestID           *string                `json:"request_id"`
	Type                *learning.ProposalType `json:"proposal_type"`
	AggregateType       *string                `json:"aggregate_type"`
	AggregateID         *string                `json:"aggregate_id"`
	AggregateVersion    *int64                 `json:"aggregate_version"`
	GoalRevisionID      *string                `json:"goal_revision_id,omitempty"`
	RouteRevisionID     *string                `json:"route_revision_id,omitempty"`
	RouteStepID         *string                `json:"route_step_id,omitempty"`
	FocusNodeRevisionID *string                `json:"focus_node_revision_id,omitempty"`
	ActivityID          *string                `json:"activity_id,omitempty"`
	AttemptID           *string                `json:"attempt_id,omitempty"`
	FreeQuestionID      *string                `json:"free_question_id,omitempty"`
	FreeAnswerID        *string                `json:"free_answer_id,omitempty"`
	FocusFrameID        *string                `json:"focus_frame_id,omitempty"`
	TutoringState       *string                `json:"tutoring_state,omitempty"`
	KnowledgeRevisionID *string                `json:"knowledge_revision_id"`
	NodeRevisionIDs     *[]string              `json:"node_revision_ids"`
	Input               *json.RawMessage       `json:"input"`
}

func (value tutoringProposalInput) proposal() (learning.ProposalRequest, error) {
	if value.RequestID == nil || value.Type == nil || value.AggregateType == nil || value.AggregateID == nil || value.AggregateVersion == nil || value.KnowledgeRevisionID == nil || value.NodeRevisionIDs == nil || value.Input == nil {
		return learning.ProposalRequest{}, invalidLearningInput()
	}
	if !validLearningUUID(*value.RequestID) || !validProposalType(*value.Type) || (*value.AggregateType != "goal" && *value.AggregateType != "session") || !validLearningUUID(*value.AggregateID) || !validLearningUUID(*value.KnowledgeRevisionID) || *value.AggregateVersion < 0 || len(*value.NodeRevisionIDs) == 0 || len(*value.NodeRevisionIDs) > 100 || len(*value.Input) > learning.MaxAnswerBytes || !json.Valid(*value.Input) {
		return learning.ProposalRequest{}, invalidLearningInput()
	}
	var inputObject map[string]any
	if json.Unmarshal(*value.Input, &inputObject) != nil || inputObject == nil {
		return learning.ProposalRequest{}, invalidLearningInput()
	}
	seenNodes := map[string]bool{}
	for _, id := range *value.NodeRevisionIDs {
		if !validLearningUUID(id) || seenNodes[id] {
			return learning.ProposalRequest{}, invalidLearningInput()
		}
		seenNodes[id] = true
	}
	for _, optional := range []*string{value.GoalRevisionID, value.RouteRevisionID, value.RouteStepID, value.FocusNodeRevisionID, value.ActivityID, value.AttemptID, value.FreeQuestionID, value.FreeAnswerID, value.FocusFrameID} {
		if optional != nil && !validLearningUUID(*optional) {
			return learning.ProposalRequest{}, invalidLearningInput()
		}
	}
	if value.TutoringState != nil && !validTutoringState(*value.TutoringState) {
		return learning.ProposalRequest{}, invalidLearningInput()
	}
	return learning.ProposalRequest{
		RequestID: *value.RequestID, Type: *value.Type, AggregateType: *value.AggregateType,
		AggregateID: *value.AggregateID, AggregateVersion: *value.AggregateVersion,
		GoalRevisionID: stringValue(value.GoalRevisionID), RouteRevisionID: stringValue(value.RouteRevisionID),
		RouteStepID: stringValue(value.RouteStepID), FocusNodeRevisionID: stringValue(value.FocusNodeRevisionID),
		ActivityID: stringValue(value.ActivityID), AttemptID: stringValue(value.AttemptID),
		FreeQuestionID: stringValue(value.FreeQuestionID), FreeAnswerID: stringValue(value.FreeAnswerID),
		FocusFrameID: stringValue(value.FocusFrameID), TutoringState: stringValue(value.TutoringState),
		KnowledgeRevisionID: *value.KnowledgeRevisionID, NodeRevisionIDs: append([]string(nil), (*value.NodeRevisionIDs)...),
		Input: append(json.RawMessage(nil), (*value.Input)...),
	}, nil
}

type actionBaseInput struct {
	operationInput
	Action *tutoring.Action `json:"action"`
}
type actionNoFieldsInput struct{ actionBaseInput }
type actionProposalInput struct {
	actionBaseInput
	ProposalID *string `json:"proposal_id"`
}
type actionAssessmentInput struct {
	actionBaseInput
	ProposalID *string `json:"proposal_id,omitempty"`
}
type actionAttemptInput struct {
	actionBaseInput
	Answer *string             `json:"answer"`
	Help   *learning.HelpLevel `json:"help"`
}
type actionQuestionInput struct {
	actionBaseInput
	Question *string `json:"question"`
}
type actionExposureInput struct {
	actionBaseInput
	ProposalID   *string                    `json:"proposal_id,omitempty"`
	ExposureKind *string                    `json:"exposure_kind,omitempty"`
	ExposureText *string                    `json:"exposure_text,omitempty"`
	References   *[]knowledgeReferenceInput `json:"knowledge_references,omitempty"`
}
type actionAttachedQuizInput struct {
	actionBaseInput
	ProposalID *string `json:"proposal_id"`
	QuestionID *string `json:"question"`
	AnswerID   *string `json:"answer"`
}
type actionSwitchGoalInput struct {
	actionBaseInput
	GoalRevisionID *string `json:"goal_revision_id"`
}

type sourceRangeInput struct {
	Start *int `json:"start"`
	End   *int `json:"end"`
}

func (value *sourceRangeInput) sourceRange(required bool) (learning.SourceRange, error) {
	if value == nil {
		if required {
			return learning.SourceRange{}, invalidLearningInput()
		}
		return learning.SourceRange{}, nil
	}
	if value.Start == nil || value.End == nil || *value.Start < 0 || *value.End <= *value.Start {
		return learning.SourceRange{}, invalidLearningInput()
	}
	return learning.SourceRange{Start: *value.Start, End: *value.End}, nil
}

type knowledgeReferenceInput struct {
	KnowledgeRevisionID *string           `json:"knowledge_revision_id,omitempty"`
	NodeID              *string           `json:"node_id,omitempty"`
	NodeRevisionID      *string           `json:"node_revision_id"`
	DocumentRevisionID  *string           `json:"document_revision_id,omitempty"`
	Range               *sourceRangeInput `json:"range,omitempty"`
	Slice               *string           `json:"slice,omitempty"`
	SliceSHA256         *string           `json:"slice_sha256,omitempty"`
}

func (value knowledgeReferenceInput) reference() (learning.KnowledgeReference, error) {
	if value.NodeRevisionID == nil || !validLearningUUID(*value.NodeRevisionID) {
		return learning.KnowledgeReference{}, invalidLearningInput()
	}
	for _, optional := range []*string{value.KnowledgeRevisionID, value.NodeID, value.DocumentRevisionID} {
		if optional != nil && !validLearningUUID(*optional) {
			return learning.KnowledgeReference{}, invalidLearningInput()
		}
	}
	rangeValue, err := value.Range.sourceRange(false)
	if err != nil {
		return learning.KnowledgeReference{}, err
	}
	if value.Slice != nil && !utf8.ValidString(*value.Slice) {
		return learning.KnowledgeReference{}, invalidLearningInput()
	}
	if value.SliceSHA256 != nil && !validLearningSHA256(*value.SliceSHA256) {
		return learning.KnowledgeReference{}, invalidLearningInput()
	}
	return learning.KnowledgeReference{
		KnowledgeRevisionID: stringValue(value.KnowledgeRevisionID), NodeID: stringValue(value.NodeID),
		NodeRevisionID: *value.NodeRevisionID, DocumentRevisionID: stringValue(value.DocumentRevisionID),
		Range: rangeValue, Slice: stringValue(value.Slice), SliceSHA256: stringValue(value.SliceSHA256),
	}, nil
}

type decisionBaseInput struct {
	operationInput
	Kind                       *string `json:"kind"`
	ExpectedDispositionVersion *int64  `json:"expected_disposition_version"`
}
type decisionConfirmInput struct{ decisionBaseInput }
type decisionVoidInput struct {
	decisionBaseInput
	Reason *string `json:"reason"`
}
type decisionOverrideInput struct {
	decisionBaseInput
	Reason *string                `json:"reason"`
	Items  *[]assessmentItemInput `json:"items"`
}
type assessmentItemInput struct {
	RubricItemID           *string              `json:"rubric_item_id"`
	Conclusion             *learning.Conclusion `json:"conclusion"`
	AnswerQuote            *string              `json:"answer_quote"`
	AnswerRange            *sourceRangeInput    `json:"answer_range"`
	AnswerQuoteSHA256      *string              `json:"answer_quote_sha256"`
	KnowledgeReferenceID   *string              `json:"knowledge_reference_id"`
	KnowledgeQuote         *string              `json:"knowledge_quote"`
	KnowledgeRange         *sourceRangeInput    `json:"knowledge_range"`
	KnowledgeQuoteSHA256   *string              `json:"knowledge_quote_sha256"`
	MisconceptionCandidate *string              `json:"misconception_candidate,omitempty"`
}

func (value assessmentItemInput) item() (learning.AssessmentItem, error) {
	if value.RubricItemID == nil || value.Conclusion == nil || value.AnswerQuote == nil || value.AnswerRange == nil || value.AnswerQuoteSHA256 == nil || value.KnowledgeReferenceID == nil || value.KnowledgeQuote == nil || value.KnowledgeRange == nil || value.KnowledgeQuoteSHA256 == nil {
		return learning.AssessmentItem{}, invalidLearningInput()
	}
	if strings.TrimSpace(*value.RubricItemID) == "" || !utf8.ValidString(*value.AnswerQuote) || !utf8.ValidString(*value.KnowledgeQuote) || (value.MisconceptionCandidate != nil && !utf8.ValidString(*value.MisconceptionCandidate)) {
		return learning.AssessmentItem{}, invalidLearningInput()
	}
	if *value.Conclusion != learning.ConclusionPass && *value.Conclusion != learning.ConclusionPartial && *value.Conclusion != learning.ConclusionFail && *value.Conclusion != learning.ConclusionUnassessed {
		return learning.AssessmentItem{}, invalidLearningInput()
	}
	if *value.KnowledgeReferenceID != "" && !validLearningUUID(*value.KnowledgeReferenceID) {
		return learning.AssessmentItem{}, invalidLearningInput()
	}
	if !validLearningSHA256(*value.AnswerQuoteSHA256) || !validLearningSHA256(*value.KnowledgeQuoteSHA256) {
		return learning.AssessmentItem{}, invalidLearningInput()
	}
	answerRange, err := value.AnswerRange.sourceRange(true)
	if err != nil {
		return learning.AssessmentItem{}, err
	}
	knowledgeRange, err := value.KnowledgeRange.sourceRange(true)
	if err != nil {
		return learning.AssessmentItem{}, err
	}
	return learning.AssessmentItem{
		RubricItemID: *value.RubricItemID, Conclusion: *value.Conclusion,
		AnswerQuote: *value.AnswerQuote, AnswerRange: answerRange, AnswerQuoteSHA256: *value.AnswerQuoteSHA256,
		KnowledgeReferenceID: *value.KnowledgeReferenceID, KnowledgeQuote: *value.KnowledgeQuote,
		KnowledgeRange: knowledgeRange, KnowledgeQuoteSHA256: *value.KnowledgeQuoteSHA256,
		MisconceptionCandidate: stringValue(value.MisconceptionCandidate),
	}, nil
}

func (a *API) handleLearningCreateGoal(w http.ResponseWriter, r *http.Request) {
	if _, ok := strictLearningQuery(w, r); !ok {
		return
	}
	var request learningGoalInput
	if !a.decodeLearning(w, r, &request) {
		return
	}
	if request.Text == nil || request.Source == nil || !validOptionalUUID(request.GoalID) || !validOptionalUUID(request.PreviousRevisionID) || learning.ValidateGoal(*request.Text) != nil || learning.ValidateGoalSource(*request.Source) != nil {
		writeLearningInvalid(w, r)
		return
	}
	operation, err := request.operation(request, "goal", "")
	if err != nil {
		writeLearningInvalid(w, r)
		return
	}
	credential, _ := credentialFromContext(r.Context())
	result, err := a.learning.CreateGoal(r.Context(), credential.Device.ID, learning.GoalCommand{Operation: operation, GoalID: stringValue(request.GoalID), Text: *request.Text, Source: *request.Source, PreviousRevisionID: request.PreviousRevisionID})
	if err != nil {
		a.writeLearningFailure(w, r, "create_goal", err)
		return
	}
	writeLearningResult(w, result)
}

func (a *API) handleLearningCreateSession(w http.ResponseWriter, r *http.Request) {
	if _, ok := strictLearningQuery(w, r); !ok {
		return
	}
	var request tutoringSessionInput
	if !a.decodeLearning(w, r, &request) {
		return
	}
	if request.GoalRevisionID == nil || !validLearningUUID(*request.GoalRevisionID) {
		writeLearningInvalid(w, r)
		return
	}
	operation, err := request.operation(request, "session", "")
	if err != nil || operation.ExpectedVersion != 0 {
		writeLearningInvalid(w, r)
		return
	}
	credential, _ := credentialFromContext(r.Context())
	result, err := a.learning.CreateSession(r.Context(), credential.Device.ID, learning.SessionCommand{Operation: operation, GoalRevisionID: *request.GoalRevisionID})
	if err != nil {
		a.writeLearningFailure(w, r, "create_session", err)
		return
	}
	writeLearningResult(w, result)
}

func (a *API) handleLearningProposal(w http.ResponseWriter, r *http.Request) {
	if _, ok := strictLearningQuery(w, r); !ok {
		return
	}
	var input tutoringProposalInput
	if !a.decodeLearning(w, r, &input) {
		return
	}
	request, err := input.proposal()
	if err != nil {
		writeLearningInvalid(w, r)
		return
	}
	credential, _ := credentialFromContext(r.Context())
	result, err := a.learning.Propose(r.Context(), credential.Device.ID, request)
	if err != nil {
		a.writeLearningFailure(w, r, "proposal", err)
		return
	}
	writeJSON(w, http.StatusCreated, result)
}

func (a *API) handleLearningAction(w http.ResponseWriter, r *http.Request) {
	if _, ok := strictLearningQuery(w, r); !ok {
		return
	}
	sessionID := chi.URLParam(r, "sessionID")
	if !validLearningUUID(sessionID) {
		writeLearningInvalid(w, r)
		return
	}
	data, ok := a.readLearning(w, r)
	if !ok {
		return
	}
	var discriminator struct {
		Action *tutoring.Action `json:"action"`
	}
	if err := json.Unmarshal(data, &discriminator); err != nil || discriminator.Action == nil {
		writeLearningInvalid(w, r)
		return
	}
	command := learning.ActionCommand{Action: *discriminator.Action}
	var base actionBaseInput
	switch command.Action {
	case tutoring.ActionStartDiagnostic, tutoring.ActionPresentActivity, tutoring.ActionAcknowledgeFeedback, tutoring.ActionResumeFocus, tutoring.ActionEndActivity, tutoring.ActionCompleteSession:
		var input actionNoFieldsInput
		if decodeLearningData(data, &input) != nil {
			writeLearningInvalid(w, r)
			return
		}
		base = input.actionBaseInput
	case tutoring.ActionApplyRoute, tutoring.ActionIssueActivity, tutoring.ActionPresentReview, tutoring.ActionRecordFreeAnswer:
		var input actionProposalInput
		if decodeLearningData(data, &input) != nil || input.ProposalID == nil || !validLearningUUID(*input.ProposalID) {
			writeLearningInvalid(w, r)
			return
		}
		base, command.ProposalID = input.actionBaseInput, *input.ProposalID
	case tutoring.ActionRecordAssessment:
		var input actionAssessmentInput
		if decodeLearningData(data, &input) != nil || !validOptionalUUID(input.ProposalID) {
			writeLearningInvalid(w, r)
			return
		}
		base, command.ProposalID = input.actionBaseInput, stringValue(input.ProposalID)
	case tutoring.ActionSubmitAttempt:
		var input actionAttemptInput
		if decodeLearningData(data, &input) != nil || input.Answer == nil || input.Help == nil || !utf8.ValidString(*input.Answer) || len(*input.Answer) > learning.MaxAnswerBytes || (*input.Help != learning.HelpNone && *input.Help != learning.HelpHint && *input.Help != learning.HelpScaffold && *input.Help != learning.HelpAnswerRevealed) {
			writeLearningInvalid(w, r)
			return
		}
		base, command.Answer, command.Help = input.actionBaseInput, *input.Answer, *input.Help
	case tutoring.ActionAskFreeQuestion:
		var input actionQuestionInput
		if decodeLearningData(data, &input) != nil || input.Question == nil || !utf8.ValidString(*input.Question) || strings.TrimSpace(*input.Question) == "" || utf8.RuneCountInString(*input.Question) > learning.MaxQuestionRunes {
			writeLearningInvalid(w, r)
			return
		}
		base, command.Question = input.actionBaseInput, *input.Question
	case tutoring.ActionRecordExposure:
		var input actionExposureInput
		if decodeLearningData(data, &input) != nil || !validOptionalUUID(input.ProposalID) {
			writeLearningInvalid(w, r)
			return
		}
		if (input.ProposalID == nil && input.ExposureText == nil) || (input.ProposalID != nil && (input.ExposureText != nil || input.References != nil)) {
			writeLearningInvalid(w, r)
			return
		}
		if input.ProposalID == nil {
			if input.ExposureKind == nil || !validExposureKind(*input.ExposureKind) {
				writeLearningInvalid(w, r)
				return
			}
		} else if input.ExposureKind != nil && !validExposureKind(*input.ExposureKind) {
			writeLearningInvalid(w, r)
			return
		}
		if input.ExposureText != nil && (!utf8.ValidString(*input.ExposureText) || strings.TrimSpace(*input.ExposureText) == "" || utf8.RuneCountInString(*input.ExposureText) > learning.MaxProposalTextRunes) {
			writeLearningInvalid(w, r)
			return
		}
		base, command.ProposalID = input.actionBaseInput, stringValue(input.ProposalID)
		command.ExposureKind = "explanation"
		if input.ExposureKind != nil {
			command.ExposureKind = *input.ExposureKind
		}
		command.ExposureText = stringValue(input.ExposureText)
		if input.References != nil {
			if len(*input.References) > 100 {
				writeLearningInvalid(w, r)
				return
			}
			for _, item := range *input.References {
				reference, err := item.reference()
				if err != nil {
					writeLearningInvalid(w, r)
					return
				}
				command.References = append(command.References, reference)
			}
		}
	case tutoring.ActionConvertFreeAnswerToQuiz:
		var input actionAttachedQuizInput
		if decodeLearningData(data, &input) != nil || input.ProposalID == nil || input.QuestionID == nil || input.AnswerID == nil || !validLearningUUID(*input.ProposalID) || !validLearningUUID(*input.QuestionID) || !validLearningUUID(*input.AnswerID) {
			writeLearningInvalid(w, r)
			return
		}
		base, command.ProposalID, command.Question, command.Answer = input.actionBaseInput, *input.ProposalID, *input.QuestionID, *input.AnswerID
	case tutoring.ActionSwitchGoal:
		var input actionSwitchGoalInput
		if decodeLearningData(data, &input) != nil || input.GoalRevisionID == nil || !validLearningUUID(*input.GoalRevisionID) {
			writeLearningInvalid(w, r)
			return
		}
		base, command.GoalRevisionID = input.actionBaseInput, *input.GoalRevisionID
	default:
		writeLearningInvalid(w, r)
		return
	}
	if base.Action == nil || *base.Action != command.Action {
		writeLearningInvalid(w, r)
		return
	}
	operation, err := base.operation(json.RawMessage(data), "session", sessionID)
	if err != nil {
		writeLearningInvalid(w, r)
		return
	}
	operation.Payload = append(json.RawMessage(nil), data...)
	command.Operation = operation
	credential, _ := credentialFromContext(r.Context())
	result, err := a.learning.ApplyAction(r.Context(), credential.Device.ID, sessionID, command)
	if err != nil {
		a.writeLearningFailure(w, r, "session_action", err)
		return
	}
	writeLearningResult(w, result)
}

func (a *API) handleLearningDecision(w http.ResponseWriter, r *http.Request) {
	if _, ok := strictLearningQuery(w, r); !ok {
		return
	}
	assessmentID := chi.URLParam(r, "assessmentID")
	if !validLearningUUID(assessmentID) {
		writeLearningInvalid(w, r)
		return
	}
	data, ok := a.readLearning(w, r)
	if !ok {
		return
	}
	var discriminator struct {
		Kind *string `json:"kind"`
	}
	if err := json.Unmarshal(data, &discriminator); err != nil || discriminator.Kind == nil {
		writeLearningInvalid(w, r)
		return
	}
	command := learning.AssessmentDecisionCommand{Kind: *discriminator.Kind}
	var base decisionBaseInput
	switch command.Kind {
	case "confirm":
		var input decisionConfirmInput
		if decodeLearningData(data, &input) != nil {
			writeLearningInvalid(w, r)
			return
		}
		base = input.decisionBaseInput
	case "void":
		var input decisionVoidInput
		if decodeLearningData(data, &input) != nil || input.Reason == nil || strings.TrimSpace(*input.Reason) == "" || !utf8.ValidString(*input.Reason) {
			writeLearningInvalid(w, r)
			return
		}
		base, command.Reason = input.decisionBaseInput, *input.Reason
	case "override":
		var input decisionOverrideInput
		if decodeLearningData(data, &input) != nil || input.Reason == nil || strings.TrimSpace(*input.Reason) == "" || !utf8.ValidString(*input.Reason) || input.Items == nil || len(*input.Items) == 0 || len(*input.Items) > learning.MaxRubricItems {
			writeLearningInvalid(w, r)
			return
		}
		base, command.Reason = input.decisionBaseInput, *input.Reason
		for _, raw := range *input.Items {
			item, err := raw.item()
			if err != nil {
				writeLearningInvalid(w, r)
				return
			}
			command.Items = append(command.Items, item)
		}
	default:
		writeLearningInvalid(w, r)
		return
	}
	if base.Kind == nil || *base.Kind != command.Kind || base.ExpectedDispositionVersion == nil || *base.ExpectedDispositionVersion < 1 {
		writeLearningInvalid(w, r)
		return
	}
	operation, err := base.operation(json.RawMessage(data), "session", "")
	if err != nil {
		writeLearningInvalid(w, r)
		return
	}
	operation.Payload = append(json.RawMessage(nil), data...)
	command.Operation, command.ExpectedDispositionVersion = operation, *base.ExpectedDispositionVersion
	credential, _ := credentialFromContext(r.Context())
	result, err := a.learning.Decide(r.Context(), credential.Device.ID, assessmentID, command)
	if err != nil {
		a.writeLearningFailure(w, r, "assessment_decision", err)
		return
	}
	writeLearningResult(w, result)
}

func (a *API) handleLearningCurrentSession(w http.ResponseWriter, r *http.Request) {
	if _, ok := strictLearningQuery(w, r); !ok {
		return
	}
	result, err := a.learning.CurrentSession(r.Context())
	if err != nil {
		a.writeLearningFailure(w, r, "current_session", err)
		return
	}
	normalizeSessionView(&result)
	writeJSON(w, http.StatusOK, result)
}
func (a *API) handleLearningSession(w http.ResponseWriter, r *http.Request) {
	if _, ok := strictLearningQuery(w, r); !ok {
		return
	}
	id := chi.URLParam(r, "sessionID")
	if !validLearningUUID(id) {
		writeLearningInvalid(w, r)
		return
	}
	result, err := a.learning.Session(r.Context(), id)
	if err != nil {
		a.writeLearningFailure(w, r, "session", err)
		return
	}
	normalizeSessionView(&result)
	writeJSON(w, http.StatusOK, result)
}
func (a *API) handleLearningTimeline(w http.ResponseWriter, r *http.Request) {
	query, ok := strictLearningQuery(w, r, "cursor", "limit", "session_id")
	if !ok {
		return
	}
	page, ok := learningPage(w, r, query)
	if !ok {
		return
	}
	sessionID := query.Get("session_id")
	if _, present := query["session_id"]; present && !validLearningUUID(sessionID) {
		writeLearningInvalid(w, r)
		return
	}
	result, err := a.learning.Timeline(r.Context(), learning.TimelineQuery{Page: page, SessionID: sessionID})
	if err != nil {
		a.writeLearningFailure(w, r, "timeline", err)
		return
	}
	normalizeProjectionMetadata(&result.Metadata)
	if result.Items == nil {
		result.Items = []learning.TimelineItem{}
	}
	writeJSON(w, http.StatusOK, result)
}
func (a *API) handleLearningRoutes(w http.ResponseWriter, r *http.Request) {
	query, ok := strictLearningQuery(w, r, "cursor", "limit")
	if !ok {
		return
	}
	page, ok := learningPage(w, r, query)
	if !ok {
		return
	}
	result, err := a.learning.Routes(r.Context(), page)
	if err != nil {
		a.writeLearningFailure(w, r, "routes", err)
		return
	}
	normalizeProjectionMetadata(&result.Metadata)
	if result.Items == nil {
		result.Items = []learning.RouteProjection{}
	}
	writeJSON(w, http.StatusOK, result)
}
func (a *API) handleLearningNode(w http.ResponseWriter, r *http.Request) {
	if _, ok := strictLearningQuery(w, r); !ok {
		return
	}
	id := chi.URLParam(r, "nodeRevisionID")
	if !validLearningUUID(id) {
		writeLearningInvalid(w, r)
		return
	}
	result, err := a.learning.Node(r.Context(), id)
	if err != nil {
		a.writeLearningFailure(w, r, "node", err)
		return
	}
	normalizeNodeView(&result)
	writeJSON(w, http.StatusOK, result)
}
func (a *API) handleLearningEvidence(w http.ResponseWriter, r *http.Request) {
	query, ok := strictLearningQuery(w, r, "cursor", "limit", "node_revision_id")
	if !ok {
		return
	}
	page, ok := learningPage(w, r, query)
	if !ok {
		return
	}
	nodeID := query.Get("node_revision_id")
	if _, present := query["node_revision_id"]; present && !validLearningUUID(nodeID) {
		writeLearningInvalid(w, r)
		return
	}
	result, err := a.learning.Evidence(r.Context(), learning.EvidenceQuery{Page: page, NodeRevisionID: nodeID})
	if err != nil {
		a.writeLearningFailure(w, r, "evidence", err)
		return
	}
	normalizeProjectionMetadata(&result.Metadata)
	if result.Items == nil {
		result.Items = []learning.AcceptedEvidence{}
	}
	writeJSON(w, http.StatusOK, result)
}
func (a *API) handleLearningReviews(w http.ResponseWriter, r *http.Request) {
	query, ok := strictLearningQuery(w, r, "cursor", "limit", "due_before")
	if !ok {
		return
	}
	page, ok := learningPage(w, r, query)
	if !ok {
		return
	}
	var due *time.Time
	if values, present := query["due_before"]; present {
		parsed, err := time.Parse(time.RFC3339, values[0])
		if err != nil {
			writeLearningInvalid(w, r)
			return
		}
		due = &parsed
	}
	result, err := a.learning.Reviews(r.Context(), learning.ReviewQuery{Page: page, DueBefore: due})
	if err != nil {
		a.writeLearningFailure(w, r, "reviews", err)
		return
	}
	normalizeProjectionMetadata(&result.Metadata)
	if result.Items == nil {
		result.Items = []learning.ReviewSchedule{}
	}
	writeJSON(w, http.StatusOK, result)
}
func (a *API) handleLearningProjectionStatus(w http.ResponseWriter, r *http.Request) {
	if _, ok := strictLearningQuery(w, r); !ok {
		return
	}
	result, err := a.learning.ProjectionStatus(r.Context())
	if err != nil {
		a.writeLearningFailure(w, r, "projection_status", err)
		return
	}
	normalizeProjectionMetadata(&result.Metadata)
	writeJSON(w, http.StatusOK, result)
}

func (a *API) readLearning(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	data, err := readJSONBody(w, r, a.maxLearningRequestBody)
	if err != nil {
		writeLearningDecodeFailure(w, r, err)
		return nil, false
	}
	return data, true
}
func (a *API) decodeLearning(w http.ResponseWriter, r *http.Request, target any) bool {
	data, err := readJSONBody(w, r, a.maxLearningRequestBody)
	if err != nil {
		writeLearningDecodeFailure(w, r, err)
		return false
	}
	if err := rejectLearningOptionalNulls(data, target); err != nil {
		writeLearningInvalid(w, r)
		return false
	}
	if err := decodeJSONData(data, target); err != nil {
		writeLearningDecodeFailure(w, r, err)
		return false
	}
	return true
}

func decodeLearningData(data []byte, target any) error {
	if err := rejectLearningOptionalNulls(data, target); err != nil {
		return err
	}
	return decodeJSONData(data, target)
}

func rejectLearningOptionalNulls(data []byte, target any) error {
	return rejectOptionalJSONNulls(json.RawMessage(data), reflect.TypeOf(target))
}

func rejectOptionalJSONNulls(raw json.RawMessage, typ reflect.Type) error {
	if typ == nil {
		return nil
	}
	for typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	if typ == reflect.TypeOf(json.RawMessage(nil)) || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil
	}
	switch typ.Kind() {
	case reflect.Struct:
		if len(bytes.TrimSpace(raw)) == 0 || bytes.TrimSpace(raw)[0] != '{' {
			return nil
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(raw, &fields); err != nil {
			return invalidLearningInput()
		}
		for index := 0; index < typ.NumField(); index++ {
			field := typ.Field(index)
			if field.PkgPath != "" && !field.Anonymous {
				continue
			}
			name, options := jsonFieldInfo(field)
			if field.Anonymous && name == "" {
				if err := rejectOptionalJSONNulls(raw, field.Type); err != nil {
					return err
				}
				continue
			}
			if name == "" || name == "-" {
				continue
			}
			child, present := fields[name]
			if !present {
				continue
			}
			if options["omitempty"] && bytes.Equal(bytes.TrimSpace(child), []byte("null")) {
				return invalidLearningInput()
			}
			if err := rejectOptionalJSONNulls(child, field.Type); err != nil {
				return err
			}
		}
	case reflect.Slice, reflect.Array:
		if len(bytes.TrimSpace(raw)) == 0 || bytes.TrimSpace(raw)[0] != '[' {
			return nil
		}
		var elements []json.RawMessage
		if err := json.Unmarshal(raw, &elements); err != nil {
			return invalidLearningInput()
		}
		for _, element := range elements {
			if err := rejectOptionalJSONNulls(element, typ.Elem()); err != nil {
				return err
			}
		}
	}
	return nil
}

func jsonFieldInfo(field reflect.StructField) (string, map[string]bool) {
	tag := field.Tag.Get("json")
	if tag == "" {
		if field.Anonymous {
			return "", nil
		}
		return field.Name, nil
	}
	parts := strings.Split(tag, ",")
	options := make(map[string]bool, len(parts)-1)
	for _, option := range parts[1:] {
		options[option] = true
	}
	return parts[0], options
}
func writeLearningDecodeFailure(w http.ResponseWriter, r *http.Request, err error) {
	var max *http.MaxBytesError
	if errors.As(err, &max) {
		writeError(w, r, http.StatusRequestEntityTooLarge, "payload_too_large", "Request body exceeds the learning limit")
	} else {
		writeLearningInvalid(w, r)
	}
}
func writeLearningInvalid(w http.ResponseWriter, r *http.Request) {
	writeError(w, r, http.StatusBadRequest, learning.CodeInvalidRequest, "Learning request is invalid")
}
func invalidLearningInput() error { return &learning.Error{Code: learning.CodeInvalidRequest} }
func validLearningUUID(value string) bool {
	parsed, err := uuid.Parse(value)
	return err == nil && parsed.String() == value
}
func validOptionalUUID(value *string) bool { return value == nil || validLearningUUID(*value) }
func validLearningSHA256(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, character := range []byte(value) {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}
func validProposalType(value learning.ProposalType) bool {
	switch value {
	case learning.ProposalRoute, learning.ProposalActivity, learning.ProposalAssessment, learning.ProposalFreeAnswer, learning.ProposalExplanation:
		return true
	default:
		return false
	}
}
func validExposureKind(value string) bool {
	switch value {
	case "reading", "explanation":
		return true
	default:
		return false
	}
}
func validTutoringState(value string) bool {
	switch tutoring.State(value) {
	case tutoring.StateIdle, tutoring.StateGoalReady, tutoring.StateDiagnostic, tutoring.StateRouteActive, tutoring.StateActivityIssued, tutoring.StateAwaitingResponse, tutoring.StateEvaluating, tutoring.StateFeedback, tutoring.StateAdvanceOrReview, tutoring.StateCompleted, tutoring.StateFocusSuspended, tutoring.StateFreeQuestion, tutoring.StateFreeAnswer, tutoring.StateFocusResumed:
		return true
	default:
		return false
	}
}
func stringValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
func strictLearningQuery(w http.ResponseWriter, r *http.Request, allowed ...string) (url.Values, bool) {
	values, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		writeLearningInvalid(w, r)
		return nil, false
	}
	set := map[string]bool{}
	for _, key := range allowed {
		set[key] = true
	}
	for key, entries := range values {
		if !set[key] || len(entries) != 1 {
			writeLearningInvalid(w, r)
			return nil, false
		}
	}
	return values, true
}
func learningPage(w http.ResponseWriter, r *http.Request, query url.Values) (learning.CursorPageRequest, bool) {
	value := learning.CursorPageRequest{Cursor: strings.TrimSpace(query.Get("cursor")), Limit: 50}
	if values, present := query["limit"]; present {
		parsed, err := strconv.Atoi(values[0])
		if err != nil || parsed < 1 || parsed > 200 {
			writeLearningInvalid(w, r)
			return value, false
		}
		value.Limit = parsed
	}
	return value, true
}
func normalizeProjectionMetadata(metadata *learning.ProjectionMetadata) {
	if metadata.ReasonCodes == nil {
		metadata.ReasonCodes = []string{}
	}
}
func normalizeSessionView(view *learning.SessionView) {
	normalizeProjectionMetadata(&view.Metadata)
}
func normalizeNodeView(view *learning.NodeView) {
	normalizeProjectionMetadata(&view.Metadata)
	if view.Evidence == nil {
		view.Evidence = []learning.AcceptedEvidence{}
	}
	if view.Node.Misconceptions == nil {
		view.Node.Misconceptions = []learning.MisconceptionHypothesis{}
	}
	if view.Node.Mastery.Kinds == nil {
		view.Node.Mastery.Kinds = map[learning.EvidenceKind]int{}
	}
	if view.Node.Mastery.Outcomes == nil {
		view.Node.Mastery.Outcomes = map[learning.Outcome]int{}
	}
	if view.Node.Mastery.Help == nil {
		view.Node.Mastery.Help = map[learning.HelpLevel]int{}
	}
	if view.Node.Mastery.UncertaintyReasons == nil {
		view.Node.Mastery.UncertaintyReasons = []string{}
	}
	for index := range view.Node.Misconceptions {
		item := &view.Node.Misconceptions[index]
		if item.SourceEvidenceIDs == nil {
			item.SourceEvidenceIDs = []string{}
		}
		if item.CounterEvidenceIDs == nil {
			item.CounterEvidenceIDs = []string{}
		}
	}
}
func writeLearningResult(w http.ResponseWriter, result learning.OperationResult) {
	status := http.StatusCreated
	if result.Replayed {
		status = http.StatusOK
	}
	writeJSON(w, status, result)
}
func (a *API) writeLearningFailure(w http.ResponseWriter, r *http.Request, operation string, err error) {
	code := learning.ErrorCode(err)
	status := http.StatusInternalServerError
	message := "Request could not be completed"
	switch code {
	case learning.CodeInvalidRequest:
		status, message = http.StatusBadRequest, "Learning request is invalid"
	case learning.CodeNotFound:
		status, message = http.StatusNotFound, "Learning resource was not found"
	case learning.CodeKnowledgeReferenceInvalid, learning.CodeProposalRejected:
		status, message = http.StatusUnprocessableEntity, "Learning proposal is invalid"
	case learning.CodeModelUnavailable:
		status, message = http.StatusServiceUnavailable, "Model is unavailable"
	case learning.CodeIdempotencyConflict, learning.CodeVersionConflict, learning.CodeInvalidTransition, learning.CodeActivityStateConflict, learning.CodeStaleProposal, learning.CodeAssessmentDispositionConflict, learning.CodeFocusFrameInvalidated, learning.CodeStaleCursor:
		status, message = http.StatusConflict, "Learning request conflicts with current state"
	case learning.CodeUnsupportedEventSchema, learning.CodeProjectionUnavailable:
		status, message = http.StatusServiceUnavailable, "Learning projection is unavailable"
	case "":
		a.logger.ErrorContext(r.Context(), "learning request failed", "request_id", middleware.GetReqID(r.Context()), "operation", operation, "error_category", "internal")
	}
	if code == "" {
		code = "internal_error"
	}
	response := map[string]any{"error": map[string]string{"code": code, "message": message, "request_id": middleware.GetReqID(r.Context())}}
	var domain *learning.Error
	if errors.As(err, &domain) {
		if domain.AggregateID != "" {
			response["conflict"] = map[string]any{"aggregate_type": domain.AggregateType, "aggregate_id": domain.AggregateID, "expected_version": domain.ExpectedVersion, "current_version": domain.CurrentVersion, "as_of_event_seq": domain.AsOfEventSequence}
		}
		if domain.CurrentDisposition != "" {
			response["current_disposition"] = domain.CurrentDisposition
		}
	}
	writeJSON(w, status, response)
}
