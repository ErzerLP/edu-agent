package mcp

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/edu-agent/edu-agent/server/internal/knowledge"
	"github.com/edu-agent/edu-agent/server/internal/learning"
	"github.com/edu-agent/edu-agent/server/internal/memory"
	"github.com/edu-agent/edu-agent/server/internal/tutoring"
	"github.com/google/uuid"
)

type operationInput struct {
	OperationID          string     `json:"operation_id"`
	PayloadSchemaVersion int        `json:"payload_schema_version"`
	AggregateType        string     `json:"aggregate_type"`
	AggregateID          string     `json:"aggregate_id"`
	ExpectedVersion      int64      `json:"expected_version"`
	OccurredAt           *time.Time `json:"occurred_at,omitempty"`
}

func (value operationInput) operation(payload any, aggregateType, aggregateID string) (learning.OperationEnvelope, error) {
	if !canonicalUUID(value.OperationID) || value.PayloadSchemaVersion != learning.EventSchemaVersion || value.AggregateType != aggregateType || !canonicalUUID(value.AggregateID) || value.ExpectedVersion < 0 || value.AggregateID != aggregateID {
		return learning.OperationEnvelope{}, invalidLearningInput()
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return learning.OperationEnvelope{}, invalidLearningInput()
	}
	return learning.OperationEnvelope{
		OperationID: value.OperationID, PayloadSchemaVersion: value.PayloadSchemaVersion,
		AggregateType: value.AggregateType, AggregateID: value.AggregateID,
		ExpectedVersion: value.ExpectedVersion, OccurredAt: value.OccurredAt, Payload: raw,
	}, nil
}

type knowledgeRetrieveInput struct {
	Query                     string                    `json:"query"`
	KnowledgeRevisionID       *string                   `json:"knowledge_revision_id,omitempty"`
	QueryContextSchemaVersion string                    `json:"query_context_schema_version,omitempty"`
	Context                   map[string]any            `json:"context,omitempty"`
	Limits                    knowledge.RetrievalLimits `json:"limits,omitempty"`
}

type pageInput struct {
	Cursor string `json:"cursor,omitempty"`
	Limit  int    `json:"limit,omitempty"`
}

func (value pageInput) learningPage() (learning.CursorPageRequest, error) {
	limit := value.Limit
	if limit == 0 {
		limit = 50
	}
	if limit < 1 || limit > 200 {
		return learning.CursorPageRequest{}, invalidLearningInput()
	}
	return learning.CursorPageRequest{Cursor: strings.TrimSpace(value.Cursor), Limit: limit}, nil
}

func (value pageInput) memoryPage() (memory.PageRequest, error) {
	limit := value.Limit
	if limit == 0 {
		limit = 50
	}
	if limit < 1 || limit > 200 {
		return memory.PageRequest{}, &memory.Error{Code: memory.CodeInvalidRequest}
	}
	return memory.PageRequest{Cursor: strings.TrimSpace(value.Cursor), Limit: limit}, nil
}

type timelineInput struct {
	pageInput
	SessionID string `json:"session_id,omitempty"`
}

type routesInput struct {
	pageInput
	CurrentOnly bool `json:"current_only,omitempty"`
}

type evidenceInput struct {
	pageInput
	NodeRevisionID string `json:"node_revision_id,omitempty"`
}

type reviewsInput struct {
	pageInput
	DueBefore *time.Time `json:"due_before,omitempty"`
}

type createGoalInput struct {
	operationInput
	GoalID             string  `json:"goal_id,omitempty"`
	Text               string  `json:"text"`
	Source             string  `json:"source"`
	PreviousRevisionID *string `json:"previous_revision_id,omitempty"`
}

func (value createGoalInput) command() (learning.GoalCommand, error) {
	if value.GoalID != "" && !canonicalUUID(value.GoalID) || value.PreviousRevisionID != nil && !canonicalUUID(*value.PreviousRevisionID) || learning.ValidateGoal(value.Text) != nil || learning.ValidateGoalSource(value.Source) != nil {
		return learning.GoalCommand{}, invalidLearningInput()
	}
	operation, err := value.operation(value, "goal", value.AggregateID)
	if err != nil {
		return learning.GoalCommand{}, err
	}
	return learning.GoalCommand{Operation: operation, GoalID: value.GoalID, Text: value.Text, Source: value.Source, PreviousRevisionID: value.PreviousRevisionID}, nil
}

type createSessionInput struct {
	operationInput
	GoalRevisionID string `json:"goal_revision_id"`
}

func (value createSessionInput) command() (learning.SessionCommand, error) {
	if !canonicalUUID(value.GoalRevisionID) || value.ExpectedVersion != 0 {
		return learning.SessionCommand{}, invalidLearningInput()
	}
	operation, err := value.operation(value, "session", value.AggregateID)
	if err != nil {
		return learning.SessionCommand{}, err
	}
	return learning.SessionCommand{Operation: operation, GoalRevisionID: value.GoalRevisionID}, nil
}

type proposeInput struct {
	RequestID           string                `json:"request_id"`
	Type                learning.ProposalType `json:"proposal_type"`
	AggregateType       string                `json:"aggregate_type"`
	AggregateID         string                `json:"aggregate_id"`
	AggregateVersion    int64                 `json:"aggregate_version"`
	GoalRevisionID      string                `json:"goal_revision_id,omitempty"`
	RouteRevisionID     string                `json:"route_revision_id,omitempty"`
	RouteStepID         string                `json:"route_step_id,omitempty"`
	FocusNodeRevisionID string                `json:"focus_node_revision_id,omitempty"`
	ActivityID          string                `json:"activity_id,omitempty"`
	AttemptID           string                `json:"attempt_id,omitempty"`
	FreeQuestionID      string                `json:"free_question_id,omitempty"`
	FreeAnswerID        string                `json:"free_answer_id,omitempty"`
	FocusFrameID        string                `json:"focus_frame_id,omitempty"`
	TutoringState       string                `json:"tutoring_state,omitempty"`
	KnowledgeRevisionID string                `json:"knowledge_revision_id"`
	NodeRevisionIDs     []string              `json:"node_revision_ids"`
	Input               json.RawMessage       `json:"input"`
}

func (value proposeInput) request() (learning.ProposalRequest, error) {
	if !canonicalUUID(value.RequestID) || !canonicalUUID(value.AggregateID) || !canonicalUUID(value.KnowledgeRevisionID) || value.AggregateVersion < 0 || (value.AggregateType != "goal" && value.AggregateType != "session") || !validProposalType(value.Type) || len(value.NodeRevisionIDs) == 0 || len(value.NodeRevisionIDs) > 100 || len(value.Input) == 0 || len(value.Input) > learning.MaxAnswerBytes || !json.Valid(value.Input) {
		return learning.ProposalRequest{}, invalidLearningInput()
	}
	var object map[string]any
	if json.Unmarshal(value.Input, &object) != nil || object == nil {
		return learning.ProposalRequest{}, invalidLearningInput()
	}
	seen := map[string]bool{}
	for _, id := range value.NodeRevisionIDs {
		if !canonicalUUID(id) || seen[id] {
			return learning.ProposalRequest{}, invalidLearningInput()
		}
		seen[id] = true
	}
	for _, id := range []string{value.GoalRevisionID, value.RouteRevisionID, value.RouteStepID, value.FocusNodeRevisionID, value.ActivityID, value.AttemptID, value.FreeQuestionID, value.FreeAnswerID, value.FocusFrameID} {
		if id != "" && !canonicalUUID(id) {
			return learning.ProposalRequest{}, invalidLearningInput()
		}
	}
	return learning.ProposalRequest{
		RequestID: value.RequestID, Type: value.Type, AggregateType: value.AggregateType,
		AggregateID: value.AggregateID, AggregateVersion: value.AggregateVersion,
		GoalRevisionID: value.GoalRevisionID, RouteRevisionID: value.RouteRevisionID,
		RouteStepID: value.RouteStepID, FocusNodeRevisionID: value.FocusNodeRevisionID,
		ActivityID: value.ActivityID, AttemptID: value.AttemptID, FreeQuestionID: value.FreeQuestionID,
		FreeAnswerID: value.FreeAnswerID, FocusFrameID: value.FocusFrameID, TutoringState: value.TutoringState,
		KnowledgeRevisionID: value.KnowledgeRevisionID, NodeRevisionIDs: append([]string(nil), value.NodeRevisionIDs...),
		Input: append(json.RawMessage(nil), value.Input...),
	}, nil
}

type applyActionInput struct {
	operationInput
	SessionID           string                         `json:"session_id"`
	Action              tutoring.Action                `json:"action"`
	ProposalID          *string                        `json:"proposal_id,omitempty"`
	Question            *string                        `json:"question,omitempty"`
	Answer              *string                        `json:"answer,omitempty"`
	Help                *learning.HelpLevel            `json:"help,omitempty"`
	GoalRevisionID      *string                        `json:"goal_revision_id,omitempty"`
	ExposureKind        *string                        `json:"exposure_kind,omitempty"`
	ExposureText        *string                        `json:"exposure_text,omitempty"`
	KnowledgeReferences *[]learning.KnowledgeReference `json:"knowledge_references,omitempty"`
	QuestionID          *string                        `json:"question_id,omitempty"`
	AnswerID            *string                        `json:"answer_id,omitempty"`
}

func (value applyActionInput) command() (learning.ActionCommand, error) {
	if !canonicalUUID(value.SessionID) || value.AggregateID != value.SessionID {
		return learning.ActionCommand{}, invalidLearningInput()
	}
	operation, err := value.operation(value, "session", value.SessionID)
	if err != nil {
		return learning.ActionCommand{}, err
	}
	command := learning.ActionCommand{Operation: operation, Action: value.Action}
	switch value.Action {
	case tutoring.ActionStartDiagnostic, tutoring.ActionPresentActivity, tutoring.ActionAcknowledgeFeedback,
		tutoring.ActionResumeFocus, tutoring.ActionEndActivity, tutoring.ActionCompleteSession:
		if value.hasActionFieldsExcept() {
			return learning.ActionCommand{}, invalidLearningInput()
		}
	case tutoring.ActionApplyRoute, tutoring.ActionIssueActivity, tutoring.ActionPresentReview, tutoring.ActionRecordFreeAnswer:
		if value.ProposalID == nil || !canonicalUUID(*value.ProposalID) || value.hasActionFieldsExcept("proposal_id") {
			return learning.ActionCommand{}, invalidLearningInput()
		}
		command.ProposalID = *value.ProposalID
	case tutoring.ActionRecordAssessment:
		if value.ProposalID != nil && !canonicalUUID(*value.ProposalID) || value.hasActionFieldsExcept("proposal_id") {
			return learning.ActionCommand{}, invalidLearningInput()
		}
		if value.ProposalID != nil {
			command.ProposalID = *value.ProposalID
		}
	case tutoring.ActionSubmitAttempt:
		if value.Answer == nil || value.Help == nil || !utf8.ValidString(*value.Answer) || len(*value.Answer) > learning.MaxAnswerBytes || !validHelp(*value.Help) || value.hasActionFieldsExcept("answer", "help") {
			return learning.ActionCommand{}, invalidLearningInput()
		}
		command.Answer, command.Help = *value.Answer, *value.Help
	case tutoring.ActionAskFreeQuestion:
		if value.Question == nil || !utf8.ValidString(*value.Question) || strings.TrimSpace(*value.Question) == "" || utf8.RuneCountInString(*value.Question) > learning.MaxQuestionRunes || value.hasActionFieldsExcept("question") {
			return learning.ActionCommand{}, invalidLearningInput()
		}
		command.Question = *value.Question
	case tutoring.ActionRecordExposure:
		if value.hasActionFieldsExcept("proposal_id", "exposure_kind", "exposure_text", "knowledge_references") {
			return learning.ActionCommand{}, invalidLearningInput()
		}
		if value.ProposalID != nil {
			if !canonicalUUID(*value.ProposalID) || value.ExposureText != nil || value.KnowledgeReferences != nil {
				return learning.ActionCommand{}, invalidLearningInput()
			}
			command.ProposalID = *value.ProposalID
			command.ExposureKind = "explanation"
			if value.ExposureKind != nil {
				if *value.ExposureKind != "reading" && *value.ExposureKind != "explanation" {
					return learning.ActionCommand{}, invalidLearningInput()
				}
				command.ExposureKind = *value.ExposureKind
			}
		} else {
			if value.ExposureKind == nil || value.ExposureText == nil || (*value.ExposureKind != "reading" && *value.ExposureKind != "explanation") || !utf8.ValidString(*value.ExposureText) || strings.TrimSpace(*value.ExposureText) == "" || utf8.RuneCountInString(*value.ExposureText) > learning.MaxProposalTextRunes {
				return learning.ActionCommand{}, invalidLearningInput()
			}
			var references []learning.KnowledgeReference
			if value.KnowledgeReferences != nil {
				references = *value.KnowledgeReferences
			}
			if len(references) > 100 {
				return learning.ActionCommand{}, invalidLearningInput()
			}
			for _, reference := range references {
				if !validKnowledgeReference(reference) {
					return learning.ActionCommand{}, invalidLearningInput()
				}
			}
			command.ExposureKind, command.ExposureText = *value.ExposureKind, *value.ExposureText
			command.References = append([]learning.KnowledgeReference(nil), references...)
		}
	case tutoring.ActionConvertFreeAnswerToQuiz:
		if value.ProposalID == nil || value.QuestionID == nil || value.AnswerID == nil || !canonicalUUID(*value.ProposalID) || !canonicalUUID(*value.QuestionID) || !canonicalUUID(*value.AnswerID) || value.hasActionFieldsExcept("proposal_id", "question_id", "answer_id") {
			return learning.ActionCommand{}, invalidLearningInput()
		}
		command.ProposalID, command.Question, command.Answer = *value.ProposalID, *value.QuestionID, *value.AnswerID
	case tutoring.ActionSwitchGoal:
		if value.GoalRevisionID == nil || !canonicalUUID(*value.GoalRevisionID) || value.hasActionFieldsExcept("goal_revision_id") {
			return learning.ActionCommand{}, invalidLearningInput()
		}
		command.GoalRevisionID = *value.GoalRevisionID
	default:
		return learning.ActionCommand{}, invalidLearningInput()
	}
	return command, nil
}

func (value applyActionInput) hasActionFieldsExcept(allowed ...string) bool {
	accepted := make(map[string]bool, len(allowed))
	for _, field := range allowed {
		accepted[field] = true
	}
	present := map[string]bool{
		"proposal_id": value.ProposalID != nil, "question": value.Question != nil,
		"answer": value.Answer != nil, "help": value.Help != nil,
		"goal_revision_id": value.GoalRevisionID != nil, "exposure_kind": value.ExposureKind != nil,
		"exposure_text": value.ExposureText != nil, "knowledge_references": value.KnowledgeReferences != nil,
		"question_id": value.QuestionID != nil, "answer_id": value.AnswerID != nil,
	}
	for field, exists := range present {
		if exists && !accepted[field] {
			return true
		}
	}
	return false
}

func decodeArguments(raw json.RawMessage, target any) error {
	if len(raw) == 0 {
		raw = []byte("{}")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("arguments must contain one JSON value")
	}
	return nil
}

func canonicalUUID(value string) bool {
	parsed, err := uuid.Parse(value)
	return err == nil && parsed.String() == value
}

func validProposalType(value learning.ProposalType) bool {
	switch value {
	case learning.ProposalRoute, learning.ProposalActivity, learning.ProposalAssessment, learning.ProposalFreeAnswer, learning.ProposalExplanation:
		return true
	default:
		return false
	}
}

func validHelp(value learning.HelpLevel) bool {
	switch value {
	case learning.HelpNone, learning.HelpHint, learning.HelpScaffold, learning.HelpAnswerRevealed:
		return true
	default:
		return false
	}
}

func validKnowledgeReference(value learning.KnowledgeReference) bool {
	if !canonicalUUID(value.NodeRevisionID) || value.KnowledgeRevisionID != "" && !canonicalUUID(value.KnowledgeRevisionID) || value.NodeID != "" && !canonicalUUID(value.NodeID) || value.DocumentRevisionID != "" && !canonicalUUID(value.DocumentRevisionID) {
		return false
	}
	if value.Range.Start < 0 || value.Range.End < value.Range.Start || !utf8.ValidString(value.Slice) {
		return false
	}
	return value.SliceSHA256 == "" || validSHA256(value.SliceSHA256)
}

func validSHA256(value string) bool {
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

func invalidLearningInput() error { return &learning.Error{Code: learning.CodeInvalidRequest} }
