package api

import (
	"encoding/json"
	"errors"
	"strings"
	"time"
	"unicode/utf8"
)

func validateKnowledgeRetrievalRequest(value KnowledgeRetrievalRequest) error {
	if strings.TrimSpace(value.Query) == "" || !utf8.ValidString(value.Query) {
		return errors.New("retrieval query is invalid")
	}
	if value.KnowledgeRevisionID != "" && !validLearningUUID(value.KnowledgeRevisionID) {
		return errors.New("retrieval knowledge revision is invalid")
	}
	if value.QueryContextSchemaVersion != "" && value.QueryContextSchemaVersion != "query-context-v1" {
		return errors.New("retrieval context version is invalid")
	}
	if value.Context != nil {
		if _, err := json.Marshal(value.Context); err != nil {
			return errors.New("retrieval context is not JSON encodable")
		}
	}
	if limits := value.Limits; limits != nil {
		if !optionalRange(limits.MaxDepth, 1, 8) || !optionalRange(limits.CandidatesPerLayer, 1, 20) || !optionalRange(limits.MaxHits, 1, 10) || !optionalRange(limits.TotalCandidates, 1, 200) {
			return errors.New("retrieval limits are invalid")
		}
	}
	return nil
}

func validateGoalRequest(value LearningGoalRequest) error {
	if !validLearningUUID(value.OperationID) || value.PayloadSchemaVersion != 1 || value.AggregateType != "goal" || !validLearningUUID(value.AggregateID) || value.ExpectedVersion < 0 || strings.TrimSpace(value.Text) == "" || len([]rune(value.Text)) > 4000 || !utf8.ValidString(value.Text) || strings.TrimSpace(value.Source) == "" || len(value.Source) > 200 || !utf8.ValidString(value.Source) {
		return errors.New("goal request is invalid")
	}
	if value.GoalID != "" && !validLearningUUID(value.GoalID) {
		return errors.New("goal ID is invalid")
	}
	if value.PreviousRevisionID != "" && !validLearningUUID(value.PreviousRevisionID) {
		return errors.New("previous goal revision is invalid")
	}
	return nil
}

func validateSessionRequest(value TutoringSessionRequest) error {
	if !validLearningUUID(value.OperationID) || value.PayloadSchemaVersion != 1 || value.AggregateType != "session" || !validLearningUUID(value.AggregateID) || value.ExpectedVersion != 0 || !validLearningUUID(value.GoalRevisionID) {
		return errors.New("session request is invalid")
	}
	return nil
}

func validateProposalRequest(value TutoringProposalRequest) error {
	if !validLearningUUID(value.RequestID) || !validProposalType(value.ProposalType) || (value.AggregateType != "goal" && value.AggregateType != "session") || !validLearningUUID(value.AggregateID) || value.AggregateVersion < 0 || !validLearningUUID(value.KnowledgeRevisionID) || len(value.NodeRevisionIDs) == 0 || len(value.NodeRevisionIDs) > 100 || value.Input == nil {
		return errors.New("proposal request is invalid")
	}
	for _, identifier := range []string{value.GoalRevisionID, value.RouteRevisionID, value.RouteStepID, value.FocusNodeRevisionID, value.ActivityID, value.AttemptID, value.FreeQuestionID, value.FreeAnswerID, value.FocusFrameID} {
		if identifier != "" && !validLearningUUID(identifier) {
			return errors.New("proposal context identifier is invalid")
		}
	}
	if value.TutoringState != "" && !validTutoringState(value.TutoringState) {
		return errors.New("proposal tutoring state is invalid")
	}
	seen := map[string]bool{}
	for _, nodeID := range value.NodeRevisionIDs {
		if !validLearningUUID(nodeID) || seen[nodeID] {
			return errors.New("proposal node IDs are invalid")
		}
		seen[nodeID] = true
	}
	if err := validateProposalRequestShape(value); err != nil {
		return err
	}
	encoded, err := json.Marshal(value.Input)
	if err != nil {
		return errors.New("proposal input is not JSON encodable")
	}
	if value.AggregateType == "goal" {
		return nil
	}
	var proposalContext ProposalContext
	if err := decodeStrict(encoded, &proposalContext); err != nil {
		return errors.New("proposal input context is invalid")
	}
	return validateProposalContext(value, proposalContext)
}

func validateProposalRequestShape(value TutoringProposalRequest) error {
	if value.GoalRevisionID == "" {
		return errors.New("proposal goal context is required")
	}
	if value.AggregateType == "goal" {
		if value.ProposalType != "route" || value.TutoringState != "" || value.RouteRevisionID != "" || value.RouteStepID != "" || value.FocusNodeRevisionID != "" || value.ActivityID != "" || value.AttemptID != "" || value.FreeQuestionID != "" || value.FreeAnswerID != "" || value.FocusFrameID != "" {
			return errors.New("goal proposal contains session-only context")
		}
		return nil
	}
	if value.TutoringState == "" {
		return errors.New("session proposal tutoring state is required")
	}
	switch value.ProposalType {
	case "route":
		if value.TutoringState != "Diagnostic" && value.TutoringState != "RouteActive" {
			return errors.New("route proposal state is invalid")
		}
	case "activity":
		if value.RouteRevisionID == "" || value.RouteStepID == "" || (value.TutoringState != "RouteActive" && value.TutoringState != "FreeAnswer") {
			return errors.New("activity proposal context is incomplete")
		}
		if value.TutoringState == "FreeAnswer" && (value.FreeQuestionID == "" || value.FreeAnswerID == "" || value.FocusFrameID == "") {
			return errors.New("attached quiz proposal context is incomplete")
		}
	case "assessment":
		if value.ActivityID == "" || value.AttemptID == "" || value.TutoringState != "Evaluating" {
			return errors.New("assessment proposal context is incomplete")
		}
	case "free_answer":
		if value.FreeQuestionID == "" || value.FocusFrameID == "" || value.TutoringState != "FreeQuestion" {
			return errors.New("free-answer proposal context is incomplete")
		}
	case "explanation":
		if value.RouteRevisionID == "" || value.RouteStepID == "" || value.TutoringState != "RouteActive" {
			return errors.New("explanation proposal context is incomplete")
		}
	}
	return nil
}

func validateProposalContext(request TutoringProposalRequest, value ProposalContext) error {
	if value.SchemaVersion != ProposalContextSchemaVersion || value.Retrieval.KnowledgeRevisionID != request.KnowledgeRevisionID || value.WorkItem.GoalRevision == nil || value.WorkItem.GoalRevision.GoalRevisionID != request.GoalRevisionID || value.WorkItem.AllowedActions == nil || value.WorkItem.AllowedAssessmentDecisions == nil {
		return errors.New("proposal frozen context is inconsistent")
	}
	if request.RouteRevisionID != "" && request.TutoringState != "FreeQuestion" && request.TutoringState != "FreeAnswer" && (value.WorkItem.RouteRevision == nil || value.WorkItem.RouteRevision.RouteRevisionID != request.RouteRevisionID) {
		return errors.New("proposal route context is inconsistent")
	}
	if request.ActivityID != "" && (value.WorkItem.Activity == nil || value.WorkItem.Activity.ActivityID != request.ActivityID) {
		return errors.New("proposal activity context is inconsistent")
	}
	if request.AttemptID != "" && (value.WorkItem.Attempt == nil || value.WorkItem.Attempt.AttemptID != request.AttemptID) {
		return errors.New("proposal attempt context is inconsistent")
	}
	if request.FreeQuestionID != "" && (value.WorkItem.FreeQuestion == nil || value.WorkItem.FreeQuestion.FreeQuestionID != request.FreeQuestionID) {
		return errors.New("proposal free-question context is inconsistent")
	}
	if request.FreeAnswerID != "" && (value.WorkItem.FreeAnswer == nil || value.WorkItem.FreeAnswer.FreeAnswerID != request.FreeAnswerID) {
		return errors.New("proposal free-answer context is inconsistent")
	}
	hitIDs := make(map[string]bool, len(value.Retrieval.Hits))
	for _, hit := range value.Retrieval.Hits {
		if hit.KnowledgeRevisionID != request.KnowledgeRevisionID || !validLearningUUID(hit.DocumentRevisionID) || !validLearningUUID(hit.NodeID) || !validLearningUUID(hit.NodeRevisionID) || hit.Range.Start < 0 || hit.Range.End < hit.Range.Start || !validSHA256(hit.SliceSHA256) || !utf8.ValidString(hit.Slice) || hitIDs[hit.NodeRevisionID] {
			return errors.New("proposal retrieval reference is invalid")
		}
		hitIDs[hit.NodeRevisionID] = true
	}
	if len(hitIDs) != len(request.NodeRevisionIDs) {
		return errors.New("proposal retrieval nodes are inconsistent")
	}
	for _, nodeID := range request.NodeRevisionIDs {
		if !hitIDs[nodeID] {
			return errors.New("proposal retrieval nodes are inconsistent")
		}
	}
	return nil
}

func validateTutoringActionRequest(sessionID string, value TutoringAction) error {
	if !validLearningUUID(sessionID) {
		return errors.New("session path ID is invalid")
	}
	operation, ok := tutoringActionOperation(value)
	if !ok || operation.AggregateID != sessionID {
		return errors.New("session action path and body ownership differ")
	}
	switch request := value.(type) {
	case ActionNoFieldsRequest:
		if err := validateSessionOperation(request.SessionOperation); err != nil || !allowedNoFieldAction(request.Action) {
			return errors.New("no-fields action is invalid")
		}
	case ActionProposalRequest:
		if err := validateSessionOperation(request.SessionOperation); err != nil || !allowedProposalAction(request.Action) || !validLearningUUID(request.ProposalID) {
			return errors.New("proposal action is invalid")
		}
	case ActionAssessmentRequest:
		if err := validateSessionOperation(request.SessionOperation); err != nil || request.Action != "record_assessment" || (request.ProposalID != "" && !validLearningUUID(request.ProposalID)) {
			return errors.New("assessment action is invalid")
		}
	case ActionAttemptRequest:
		if err := validateSessionOperation(request.SessionOperation); err != nil || request.Action != "submit_attempt" || strings.TrimSpace(request.Answer) == "" || len(request.Answer) > 262144 || !utf8.ValidString(request.Answer) || !validHelp(request.Help) {
			return errors.New("attempt action is invalid")
		}
	case ActionQuestionRequest:
		if err := validateSessionOperation(request.SessionOperation); err != nil || request.Action != "ask_free_question" || strings.TrimSpace(request.Question) == "" || len([]rune(request.Question)) > 8000 || !utf8.ValidString(request.Question) {
			return errors.New("question action is invalid")
		}
	case ActionDirectExposureRequest:
		if err := validateSessionOperation(request.SessionOperation); err != nil || request.Action != "record_exposure" || !validExposureKind(request.ExposureKind) || strings.TrimSpace(request.ExposureText) == "" || len([]rune(request.ExposureText)) > 32000 || !utf8.ValidString(request.ExposureText) || len(request.KnowledgeReferences) > 100 {
			return errors.New("direct exposure action is invalid")
		}
		for _, reference := range request.KnowledgeReferences {
			if !validKnowledgeReferenceInput(reference) {
				return errors.New("direct exposure reference is invalid")
			}
		}
	case ActionProposalExposureRequest:
		if err := validateSessionOperation(request.SessionOperation); err != nil || request.Action != "record_exposure" || !validLearningUUID(request.ProposalID) || (request.ExposureKind != "" && !validExposureKind(request.ExposureKind)) {
			return errors.New("proposal exposure action is invalid")
		}
	case ActionAttachedQuizRequest:
		if err := validateSessionOperation(request.SessionOperation); err != nil || request.Action != "convert_free_answer_to_quiz" || !validLearningUUID(request.ProposalID) || !validLearningUUID(request.Question) || !validLearningUUID(request.Answer) {
			return errors.New("attached quiz action is invalid")
		}
	case ActionSwitchGoalRequest:
		if err := validateSessionOperation(request.SessionOperation); err != nil || request.Action != "switch_goal" || !validLearningUUID(request.GoalRevisionID) {
			return errors.New("switch goal action is invalid")
		}
	default:
		return errors.New("unknown tutoring action type")
	}
	return nil
}

func tutoringActionOperation(value TutoringAction) (SessionOperation, bool) {
	switch request := value.(type) {
	case ActionNoFieldsRequest:
		return request.SessionOperation, true
	case ActionProposalRequest:
		return request.SessionOperation, true
	case ActionAssessmentRequest:
		return request.SessionOperation, true
	case ActionAttemptRequest:
		return request.SessionOperation, true
	case ActionQuestionRequest:
		return request.SessionOperation, true
	case ActionDirectExposureRequest:
		return request.SessionOperation, true
	case ActionProposalExposureRequest:
		return request.SessionOperation, true
	case ActionAttachedQuizRequest:
		return request.SessionOperation, true
	case ActionSwitchGoalRequest:
		return request.SessionOperation, true
	default:
		return SessionOperation{}, false
	}
}

func validateAssessmentDecisionRequest(assessmentID string, value AssessmentDecisionRequest) error {
	if !validLearningUUID(assessmentID) {
		return errors.New("assessment path ID is invalid")
	}
	switch request := value.(type) {
	case AssessmentConfirmRequest:
		if err := validateSessionOperation(request.SessionOperation); err != nil || request.Kind != "confirm" || request.ExpectedDispositionVersion < 1 {
			return errors.New("assessment confirm is invalid")
		}
	case AssessmentOverrideRequest:
		if err := validateSessionOperation(request.SessionOperation); err != nil || request.Kind != "override" || request.ExpectedDispositionVersion < 1 || strings.TrimSpace(request.Reason) == "" || len(request.Items) == 0 || len(request.Items) > 64 {
			return errors.New("assessment override is invalid")
		}
		for _, item := range request.Items {
			if !validAssessmentItem(item) || item.Conclusion == "unassessed" {
				return errors.New("assessment override item is invalid")
			}
		}
	case AssessmentVoidRequest:
		if err := validateSessionOperation(request.SessionOperation); err != nil || request.Kind != "void" || request.ExpectedDispositionVersion < 1 || strings.TrimSpace(request.Reason) == "" {
			return errors.New("assessment void is invalid")
		}
	default:
		return errors.New("unknown assessment decision type")
	}
	return nil
}

func validateSessionOperation(value SessionOperation) error {
	if !validLearningUUID(value.OperationID) || value.PayloadSchemaVersion != 1 || value.AggregateType != "session" || !validLearningUUID(value.AggregateID) || value.ExpectedVersion < 0 {
		return errors.New("session operation is invalid")
	}
	return nil
}

func allowedNoFieldAction(value string) bool {
	switch value {
	case "start_diagnostic", "present_activity", "acknowledge_feedback", "resume_focus", "end_activity", "complete_session":
		return true
	default:
		return false
	}
}

func allowedProposalAction(value string) bool {
	return value == "apply_route" || value == "issue_activity" || value == "present_review" || value == "record_free_answer"
}

func validHelp(value string) bool {
	return value == "none" || value == "hint" || value == "scaffold" || value == "answer_revealed"
}

func validExposureKind(value string) bool {
	return value == "reading" || value == "explanation"
}

func optionalRange(value, minimum, maximum int) bool {
	return value == 0 || (value >= minimum && value <= maximum)
}

func validatePageRequest(cursor string, limit int) error {
	if limit != 0 && (limit < 1 || limit > 200) {
		return errors.New("page limit is invalid")
	}
	if !validOpaqueCursor(cursor) {
		return errors.New("page cursor is invalid")
	}
	return nil
}

func validateTimelineRequest(cursor string, limit int, sessionID string) error {
	if err := validatePageRequest(cursor, limit); err != nil {
		return err
	}
	if sessionID != "" && !validLearningUUID(sessionID) {
		return errors.New("timeline session filter is invalid")
	}
	return nil
}

func validateEvidenceRequest(cursor string, limit int, nodeRevisionID string) error {
	if err := validatePageRequest(cursor, limit); err != nil {
		return err
	}
	if nodeRevisionID != "" && !validLearningUUID(nodeRevisionID) {
		return errors.New("evidence node filter is invalid")
	}
	return nil
}

func validateReviewsRequest(cursor string, limit int, dueBefore *time.Time) error {
	if err := validatePageRequest(cursor, limit); err != nil {
		return err
	}
	if dueBefore != nil && dueBefore.IsZero() {
		return errors.New("review due-before filter is invalid")
	}
	return nil
}

func validKnowledgeReferenceInput(value KnowledgeReferenceInput) bool {
	if !validLearningUUID(value.NodeRevisionID) || (value.KnowledgeRevisionID != "" && !validLearningUUID(value.KnowledgeRevisionID)) || (value.NodeID != "" && !validLearningUUID(value.NodeID)) || (value.DocumentRevisionID != "" && !validLearningUUID(value.DocumentRevisionID)) {
		return false
	}
	if value.Range != nil && (value.Range.Start < 0 || value.Range.End < value.Range.Start) {
		return false
	}
	return value.SliceSHA256 == "" || validSHA256(value.SliceSHA256)
}

func validAssessmentItem(value AssessmentItem) bool {
	if strings.TrimSpace(value.RubricItemID) == "" || !validConclusion(value.Conclusion) || !utf8.ValidString(value.AnswerQuote) || !utf8.ValidString(value.KnowledgeQuote) || !validSHA256(value.AnswerQuoteSHA256) || !validSHA256(value.KnowledgeQuoteSHA256) || value.AnswerRange.Start < 0 || value.AnswerRange.End < value.AnswerRange.Start || value.KnowledgeRange.Start < 0 || value.KnowledgeRange.End < value.KnowledgeRange.Start {
		return false
	}
	return value.KnowledgeReferenceID == "" || validLearningUUID(value.KnowledgeReferenceID)
}

func validConclusion(value string) bool {
	return value == "pass" || value == "partial" || value == "fail" || value == "unassessed"
}
