package api

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"reflect"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

var (
	learningUUIDPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	sha256Pattern       = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

func validateRetrieval(value KnowledgeRetrievalResult) error {
	if !validLearningUUID(value.KnowledgeRevisionID) || value.RetrieverVersion != "retriever-v1" || value.SelectorVersion != "selector-v1" || value.QueryContextSchemaVersion != "query-context-v1" {
		return errors.New("knowledge retrieval metadata is invalid")
	}
	if value.SummarySnapshot == nil || value.DocumentShortlist == nil || value.Trace == nil || value.Hits == nil || len(value.Hits) > 10 {
		return errors.New("knowledge retrieval collections are incomplete")
	}
	if len(value.DocumentShortlist) > 8 {
		return errors.New("knowledge retrieval shortlist is too large")
	}
	for _, trace := range value.Trace {
		if trace.Index < 0 || trace.Depth < 0 || (trace.ParentNodeRevisionID != "" && !validLearningUUID(trace.ParentNodeRevisionID)) || trace.Candidates == nil || trace.Decisions == nil || !validSHA256(trace.CandidateSetHash) {
			return errors.New("knowledge retrieval trace is invalid")
		}
		for _, candidate := range trace.Candidates {
			if candidate.Ordinal < 0 || !validLearningUUID(candidate.NodeRevisionID) || candidate.Score < 0 || !validSHA256(candidate.TitleSHA256) || !utf8.ValidString(candidate.Title) || (candidate.SummaryArtifactID != "" && !validLearningUUID(candidate.SummaryArtifactID)) || candidate.LocalBodyScore < 0 {
				return errors.New("knowledge retrieval candidate is invalid")
			}
		}
	}
	for _, hit := range value.Hits {
		if !validLearningUUID(hit.DocumentID) || !validLearningUUID(hit.DocumentRevisionID) || !validLearningUUID(hit.NodeID) || !validLearningUUID(hit.NodeRevisionID) || hit.Path == "" || !utf8.ValidString(hit.Path) || hit.Provenance != "canonical_markdown" || !validSHA256(hit.SliceSHA256) || !utf8.ValidString(hit.CanonicalSlice) || !validSourceRange(hit.HeadingRange) || !validSourceRange(hit.LocalBodyRange) || !validSourceRange(hit.SectionRange) || hit.TraceIndex < 0 || hit.Depth < 0 {
			return errors.New("knowledge retrieval hit is invalid")
		}
	}
	return nil
}

func validateLearningOperationResult(status, aggregateType, aggregateID string, aggregateVersion, firstEventSeq, lastEventSeq, projectionAsOfEventSeq int64, tutoringState, evidenceDisposition string) error {
	if status != "succeeded" || !validLearningUUID(aggregateID) || aggregateVersion < 1 || firstEventSeq < 1 || lastEventSeq < firstEventSeq || projectionAsOfEventSeq < lastEventSeq {
		return errors.New("learning operation result is incomplete")
	}
	if aggregateType != "goal" && aggregateType != "session" {
		return errors.New("learning aggregate type is invalid")
	}
	if tutoringState != "" && !validTutoringState(tutoringState) {
		return errors.New("learning operation tutoring state is invalid")
	}
	if evidenceDisposition != "" && !validAssessmentDisposition(evidenceDisposition) {
		return errors.New("learning operation evidence disposition is invalid")
	}
	return nil
}

func validateGoalOperationResult(value GoalOperationResult) error {
	if err := validateLearningOperationResult(value.Status, value.AggregateType, value.AggregateID, value.AggregateVersion, value.FirstEventSeq, value.LastEventSeq, value.ProjectionAsOfEventSeq, value.TutoringState, value.EvidenceDisposition); err != nil || value.AggregateType != "goal" {
		return errors.New("goal operation result is invalid")
	}
	if !validGoalRevision(value.Result) || value.Result.GoalID != value.AggregateID || value.Result.Revision != value.AggregateVersion || value.TutoringState != "" || value.EvidenceDisposition != "" {
		return errors.New("goal operation payload is inconsistent")
	}
	return nil
}

func validateSessionOperationResult(value SessionOperationResult) error {
	if err := validateLearningOperationResult(value.Status, value.AggregateType, value.AggregateID, value.AggregateVersion, value.FirstEventSeq, value.LastEventSeq, value.ProjectionAsOfEventSeq, value.TutoringState, value.EvidenceDisposition); err != nil || value.AggregateType != "session" {
		return errors.New("session operation result is invalid")
	}
	if err := validateTutoringSession(value.Result); err != nil || value.Result.SessionID != value.AggregateID || value.Result.AggregateVersion != value.AggregateVersion {
		return errors.New("session operation payload is inconsistent")
	}
	if value.TutoringState != "" && value.TutoringState != value.Result.State {
		return errors.New("session operation state is inconsistent")
	}
	return nil
}

func validateAssessmentDecisionOperationResult(value AssessmentDecisionOperationResult) error {
	if err := validateLearningOperationResult(value.Status, value.AggregateType, value.AggregateID, value.AggregateVersion, value.FirstEventSeq, value.LastEventSeq, value.ProjectionAsOfEventSeq, value.TutoringState, value.EvidenceDisposition); err != nil || value.AggregateType != "session" {
		return errors.New("assessment decision operation result is invalid")
	}
	if !validAssessmentDecision(value.Result) {
		return errors.New("assessment decision operation payload is invalid")
	}
	if value.EvidenceDisposition != "" && value.EvidenceDisposition != value.Result.Disposition {
		return errors.New("assessment decision disposition is inconsistent")
	}
	return nil
}

func validateGoalOperationBinding(value GoalOperationResult, request LearningGoalRequest) error {
	if value.AggregateID != request.AggregateID || value.Result.GoalID != request.AggregateID || value.Result.Revision != request.ExpectedVersion+1 {
		return errors.New("goal operation result does not belong to the request")
	}
	return nil
}

func validateSessionCreateBinding(value SessionOperationResult, request TutoringSessionRequest) error {
	if err := validateSessionOperationBinding(value, request.AggregateID); err != nil {
		return err
	}
	if value.Result.Focus.GoalRevisionID != request.GoalRevisionID {
		return errors.New("created session result uses another goal revision")
	}
	return nil
}

func validateSessionOperationBinding(value SessionOperationResult, aggregateID string) error {
	if value.AggregateID != aggregateID || value.Result.SessionID != aggregateID {
		return errors.New("session operation result does not belong to the request")
	}
	return nil
}

func validateAssessmentDecisionOperationBinding(value AssessmentDecisionOperationResult, assessmentID, aggregateID string) error {
	if value.AggregateID != aggregateID || value.Result.AssessmentID != assessmentID {
		return errors.New("assessment decision result does not belong to the request")
	}
	return nil
}

func assessmentDecisionAggregateID(value AssessmentDecisionRequest) string {
	switch request := value.(type) {
	case AssessmentConfirmRequest:
		return request.AggregateID
	case AssessmentOverrideRequest:
		return request.AggregateID
	case AssessmentVoidRequest:
		return request.AggregateID
	default:
		return ""
	}
}

func validateProposalBinding(value TutoringProposal, request TutoringProposalRequest) error {
	expectedHash, err := hashJSON(request)
	if err != nil {
		return err
	}
	frozenHash, err := hashJSON(value.FrozenRequest)
	if err != nil {
		return err
	}
	if value.InputHash != expectedHash || value.InputHash != frozenHash ||
		value.ProposalType != request.ProposalType || value.AggregateType != request.AggregateType ||
		value.AggregateID != request.AggregateID || value.AggregateVersion != request.AggregateVersion ||
		value.GoalRevisionID != request.GoalRevisionID || value.RouteRevisionID != request.RouteRevisionID ||
		value.ActivityID != request.ActivityID || value.AttemptID != request.AttemptID ||
		value.KnowledgeRevisionID != request.KnowledgeRevisionID {
		return errors.New("tutoring proposal does not match its frozen request")
	}
	return nil
}

func hashJSON(value any) (string, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	var normalized any
	if err := json.Unmarshal(encoded, &normalized); err != nil {
		return "", err
	}
	encoded, err = json.Marshal(normalized)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}

func validateTutoringProposal(value TutoringProposal) error {
	if !validLearningUUID(value.ProposalID) || value.SchemaVersion != 1 || !validSHA256(value.InputHash) || !validProposalType(value.ProposalType) || !validLearningUUID(value.AggregateID) || !validLearningUUID(value.KnowledgeRevisionID) || value.AggregateVersion < 0 || value.ModelID == "" || value.ModelParameters == nil || value.PromptRevision == "" || len(value.AttemptCategories) < 1 || len(value.AttemptCategories) > 2 || value.CreatedAt.IsZero() {
		return errors.New("tutoring proposal is incomplete")
	}
	if value.AggregateType != "goal" && value.AggregateType != "session" {
		return errors.New("tutoring proposal aggregate type is invalid")
	}
	if err := validateProposalRequest(value.FrozenRequest); err != nil {
		return errors.New("tutoring proposal frozen request is invalid")
	}
	frozenHash, err := hashJSON(value.FrozenRequest)
	if err != nil || frozenHash != value.InputHash || value.ProposalType != value.FrozenRequest.ProposalType || value.AggregateType != value.FrozenRequest.AggregateType || value.AggregateID != value.FrozenRequest.AggregateID || value.AggregateVersion != value.FrozenRequest.AggregateVersion || value.GoalRevisionID != value.FrozenRequest.GoalRevisionID || value.RouteRevisionID != value.FrozenRequest.RouteRevisionID || value.ActivityID != value.FrozenRequest.ActivityID || value.AttemptID != value.FrozenRequest.AttemptID || value.KnowledgeRevisionID != value.FrozenRequest.KnowledgeRevisionID {
		return errors.New("tutoring proposal frozen context is inconsistent")
	}
	switch value.ProposalType {
	case "route":
		if len(value.Route) < 1 || len(value.Route) > 100 || value.Activity != nil || value.Assessment != nil || value.Text != nil {
			return errors.New("route proposal union is invalid")
		}
		seen := map[string]bool{}
		for _, step := range value.Route {
			if !validLearningUUID(step.NodeRevisionID) || seen[step.NodeRevisionID] || !containsString(value.FrozenRequest.NodeRevisionIDs, step.NodeRevisionID) || strings.TrimSpace(step.TeachingIntent) == "" || strings.TrimSpace(step.CompletionCondition) == "" || !utf8.ValidString(step.TeachingIntent) || !utf8.ValidString(step.CompletionCondition) || len([]rune(step.TeachingIntent)) > 1000 || len([]rune(step.CompletionCondition)) > 1000 {
				return errors.New("route proposal step is invalid")
			}
			seen[step.NodeRevisionID] = true
		}
	case "activity":
		if len(value.Route) != 0 || value.Activity == nil || value.Assessment != nil || value.Text != nil || !validActivityProposal(*value.Activity, value.KnowledgeRevisionID, value.FrozenRequest.NodeRevisionIDs) {
			return errors.New("activity proposal union is invalid")
		}
	case "assessment":
		if len(value.Route) != 0 || value.Activity != nil || value.Assessment == nil || value.Text != nil || !validAssessmentProposalArtifact(*value.Assessment) || value.Assessment.SessionID != value.AggregateID || value.Assessment.ActivityID != value.ActivityID || value.Assessment.AttemptID != value.AttemptID || value.Assessment.ProposalInputHash != value.InputHash {
			return errors.New("assessment proposal union is invalid")
		}
	case "free_answer", "explanation":
		if len(value.Route) != 0 || value.Activity != nil || value.Assessment != nil || value.Text == nil || strings.TrimSpace(value.Text.Text) == "" || !utf8.ValidString(value.Text.Text) || len([]rune(value.Text.Text)) > 32000 || !validKnowledgeReferences(value.Text.KnowledgeReferences, value.KnowledgeRevisionID, value.FrozenRequest.NodeRevisionIDs) {
			return errors.New("text proposal union is invalid")
		}
	}
	return nil
}

func validateSessionView(value SessionView) error {
	if err := validateProjectionMetadata(value.Metadata); err != nil {
		return err
	}
	if err := validateTutoringSession(value.Session); err != nil || value.EstimatedActiveTime.DurationSeconds < 0 || value.EstimatedActiveTime.AlgorithmVersion == "" || value.EstimatedActiveTime.SampleCount < 0 {
		return errors.New("session view is incomplete")
	}
	if value.Session.State == "Completed" {
		if value.WorkItem != nil {
			return errors.New("completed session includes a work item")
		}
		return nil
	}
	if invalidCommittedState(value.Session.State) || value.WorkItem == nil {
		return errors.New("session work item is unavailable")
	}
	item := value.WorkItem
	if item.AllowedActions == nil || item.AllowedAssessmentDecisions == nil || !uniqueAllowed(item.AllowedActions, validAllowedAction) || !uniqueAllowed(item.AllowedAssessmentDecisions, validAssessmentDecisionKind) {
		return errors.New("session allowed operations are invalid")
	}
	if item.GoalRevision == nil || !validGoalRevision(*item.GoalRevision) || value.Session.Focus.GoalRevisionID != item.GoalRevision.GoalRevisionID {
		return errors.New("session goal is missing or inconsistent")
	}
	if item.RouteRevision != nil && !validRouteRevision(*item.RouteRevision) {
		return errors.New("session route is invalid")
	}
	if item.Activity != nil && !validActivity(*item.Activity) {
		return errors.New("session activity is invalid")
	}
	if item.Attempt != nil && !validAttempt(*item.Attempt) {
		return errors.New("session attempt is invalid")
	}
	if item.Assessment != nil && !validAssessmentArtifact(*item.Assessment) {
		return errors.New("session assessment artifact is invalid")
	}
	if item.AssessmentDecision != nil && !validAssessmentDecision(*item.AssessmentDecision) {
		return errors.New("session assessment decision is invalid")
	}
	if item.FreeQuestion != nil && !validFreeQuestion(*item.FreeQuestion) {
		return errors.New("session free question is invalid")
	}
	if item.FreeAnswer != nil && !validFreeAnswer(*item.FreeAnswer) {
		return errors.New("session free answer is invalid")
	}
	switch value.Session.State {
	case "GoalReady", "Diagnostic":
		return nil
	case "RouteActive":
		if item.RouteRevision == nil {
			return errors.New("session route is missing")
		}
	case "ActivityIssued", "AwaitingResponse":
		if item.RouteRevision == nil || item.Activity == nil {
			return errors.New("session activity is missing")
		}
	case "Evaluating":
		if item.RouteRevision == nil || item.Activity == nil || item.Attempt == nil {
			return errors.New("session attempt is missing")
		}
	case "Feedback":
		if item.RouteRevision == nil || item.Activity == nil || item.Attempt == nil || item.Assessment == nil || item.AssessmentDecision == nil {
			return errors.New("session assessment is missing")
		}
		if item.Assessment.AttemptID != item.Attempt.AttemptID || item.Assessment.ActivityID != item.Activity.ActivityID || item.Assessment.ActivityRevision != item.Activity.Revision || item.Assessment.SessionID != value.Session.SessionID || item.AssessmentDecision.AssessmentID != item.Assessment.AssessmentID || !assessmentDecisionMatchesArtifact(*item.Assessment, *item.AssessmentDecision) {
			return errors.New("session assessment ownership is inconsistent")
		}
		if item.AssessmentDecision.Disposition == "provisional" && len(item.AllowedActions) != 0 {
			return errors.New("provisional feedback exposes a tutoring action")
		}
	case "FreeQuestion":
		if item.FreeQuestion == nil || item.FreeAnswer != nil || item.FreeQuestion.SessionID != value.Session.SessionID {
			return errors.New("free question context is invalid")
		}
	case "FreeAnswer":
		if item.FreeQuestion == nil || item.FreeAnswer == nil || item.FreeQuestion.SessionID != value.Session.SessionID || item.FreeAnswer.SessionID != value.Session.SessionID || item.FreeAnswer.FreeQuestionID != item.FreeQuestion.FreeQuestionID || item.FreeAnswer.FocusFrameID != item.FreeQuestion.FocusFrameID {
			return errors.New("free answer context is invalid")
		}
	default:
		return errors.New("unsupported committed tutoring state")
	}
	if item.RouteRevision != nil && item.RouteRevision.GoalRevisionID != item.GoalRevision.GoalRevisionID {
		return errors.New("session route goal is inconsistent")
	}
	if item.Activity != nil {
		if item.Activity.SessionID != value.Session.SessionID || item.RouteRevision == nil || item.Activity.RouteRevisionID != item.RouteRevision.RouteRevisionID || item.Activity.GoalRevisionID != item.GoalRevision.GoalRevisionID {
			return errors.New("session activity ownership is inconsistent")
		}
		if item.Attempt != nil && (item.Attempt.SessionID != value.Session.SessionID || item.Attempt.ActivityID != item.Activity.ActivityID || item.Attempt.ActivityRevision != item.Activity.Revision) {
			return errors.New("session attempt ownership is inconsistent")
		}
		if value.Session.AttachedQuiz && (item.Activity.AttachedFreeQuestionID == "" || item.Activity.AttachedFreeAnswerID == "") {
			return errors.New("attached quiz identifiers are missing")
		}
	}
	return nil
}

func validateProjectionMetadata(value ProjectionMetadata) error {
	if value.AsOfEventSeq < 0 || value.ProjectionVersion == "" || value.MasteryReducerVersion == "" || value.AssessmentPolicyVersion == "" || value.ReviewPolicyVersion == "" || !validLearningUUID(value.Generation) || value.ReasonCodes == nil {
		return errors.New("projection metadata is incomplete")
	}
	if value.KnowledgeRevisionID != "" && !validLearningUUID(value.KnowledgeRevisionID) {
		return errors.New("projection knowledge revision is invalid")
	}
	return nil
}

func validateProjectionPage[T any](metadata ProjectionMetadata, items []T) error {
	if err := validateProjectionMetadata(metadata); err != nil {
		return err
	}
	if reflect.ValueOf(items).IsNil() {
		return errors.New("projection page items are required")
	}
	return nil
}

func validateProjectionPageCursor[T any](metadata ProjectionMetadata, items []T, cursor string) error {
	if err := validateProjectionPage(metadata, items); err != nil {
		return err
	}
	if !validOpaqueCursor(cursor) {
		return errors.New("projection page cursor is invalid")
	}
	return nil
}

func validOpaqueCursor(value string) bool {
	if !utf8.ValidString(value) {
		return false
	}
	for _, character := range value {
		if unicode.IsControl(character) {
			return false
		}
	}
	return true
}

func validLearningUUID(value string) bool { return learningUUIDPattern.MatchString(value) }
func validSHA256(value string) bool       { return sha256Pattern.MatchString(value) }

func validSourceRange(value SourceRange) bool {
	return value.Start >= 0 && value.End >= value.Start && value.StartLine >= 1 && value.EndLine >= value.StartLine
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func validActivityProposal(value ActivityProposal, knowledgeRevisionID string, allowedNodeIDs []string) bool {
	if strings.TrimSpace(value.Prompt) == "" || !utf8.ValidString(value.Prompt) || (value.Type != "objective" && value.Type != "open") || value.Difficulty < 1 || value.Difficulty > 5 || value.AllowedHelp == nil || !uniqueAllowed(value.AllowedHelp, validHelp) || !validRubric(value.Rubric) {
		return false
	}
	return validKnowledgeReferences(value.KnowledgeReferences, knowledgeRevisionID, allowedNodeIDs)
}

func validRubric(value Rubric) bool {
	if value.RubricRevision == "" || len(value.Items) < 1 || len(value.Items) > 64 {
		return false
	}
	seen := map[string]bool{}
	for _, item := range value.Items {
		if strings.TrimSpace(item.RubricItemID) == "" || strings.TrimSpace(item.Criterion) == "" || !utf8.ValidString(item.Criterion) || seen[item.RubricItemID] {
			return false
		}
		seen[item.RubricItemID] = true
		for _, referenceID := range item.RequiredReferenceIDs {
			if !validLearningUUID(referenceID) {
				return false
			}
		}
	}
	if rule := value.ObjectiveRule; rule != nil {
		if rule.AcceptedAnswers == nil {
			return false
		}
		for _, answer := range rule.AcceptedAnswers {
			if !utf8.ValidString(answer) {
				return false
			}
		}
	}
	return true
}

func validKnowledgeReferences(values []KnowledgeReference, knowledgeRevisionID string, allowedNodeIDs []string) bool {
	if len(values) < 1 || len(values) > 100 {
		return false
	}
	for _, reference := range values {
		if reference.KnowledgeRevisionID != knowledgeRevisionID || !validLearningUUID(reference.NodeID) || !validLearningUUID(reference.NodeRevisionID) || (reference.DocumentRevisionID != "" && !validLearningUUID(reference.DocumentRevisionID)) || reference.Range.Start < 0 || reference.Range.End < reference.Range.Start || !validSHA256(reference.SliceSHA256) || !utf8.ValidString(reference.Slice) || (len(allowedNodeIDs) > 0 && !containsString(allowedNodeIDs, reference.NodeRevisionID)) {
			return false
		}
	}
	return true
}

func validRouteRevision(value RouteRevision) bool {
	if !validLearningUUID(value.RouteRevisionID) || !validLearningUUID(value.RouteID) || value.Revision < 1 || !validLearningUUID(value.GoalRevisionID) || !validLearningUUID(value.KnowledgeRevisionID) || value.RoutePolicyVersion == "" || !validLearningUUID(value.SourceProposalID) || len(value.Steps) < 1 || len(value.Steps) > 100 || value.CreatedAt.IsZero() {
		return false
	}
	seen := map[string]bool{}
	for index, step := range value.Steps {
		if !validLearningUUID(step.RouteStepID) || step.Ordinal != index || !validLearningUUID(step.NodeID) || !validLearningUUID(step.NodeRevisionID) || strings.TrimSpace(step.TeachingIntent) == "" || strings.TrimSpace(step.CompletionCondition) == "" || seen[step.RouteStepID] {
			return false
		}
		seen[step.RouteStepID] = true
	}
	return true
}

func validActivity(value Activity) bool {
	if !validLearningUUID(value.ActivityID) || value.Revision < 1 || !validLearningUUID(value.SessionID) || !validLearningUUID(value.GoalRevisionID) || !validLearningUUID(value.RouteRevisionID) || !validLearningUUID(value.RouteStepID) || !validLearningUUID(value.KnowledgeRevisionID) || !validLearningUUID(value.TargetNodeID) || !validLearningUUID(value.TargetNodeRevisionID) || strings.TrimSpace(value.Prompt) == "" || (value.Type != "objective" && value.Type != "open") || !validRubric(value.Rubric) || value.Difficulty < 1 || value.Difficulty > 5 || value.AllowedHelp == nil || !uniqueAllowed(value.AllowedHelp, validHelp) || value.ActivityPolicyVersion == "" || value.AssessmentPolicyVersion == "" || value.ReviewPolicyVersion == "" || value.CreatedAt.IsZero() {
		return false
	}
	if value.SourceProposalID != "" && !validLearningUUID(value.SourceProposalID) {
		return false
	}
	if value.AttachedFreeQuestionID != "" && !validLearningUUID(value.AttachedFreeQuestionID) {
		return false
	}
	if value.AttachedFreeAnswerID != "" && !validLearningUUID(value.AttachedFreeAnswerID) {
		return false
	}
	if !validKnowledgeReferences(value.KnowledgeReferences, value.KnowledgeRevisionID, nil) {
		return false
	}
	for _, reference := range value.KnowledgeReferences {
		if reference.NodeRevisionID == value.TargetNodeRevisionID {
			return true
		}
	}
	return false
}

func validAttempt(value Attempt) bool {
	if !validLearningUUID(value.AttemptID) || !validLearningUUID(value.SessionID) || !validLearningUUID(value.ActivityID) || value.ActivityRevision < 1 || !validLearningUUID(value.AnswerPayloadID) || !utf8.ValidString(value.Answer) || !validSHA256(value.AnswerSHA256) || !validHelp(value.Help) || !validLearningUUID(value.ActorDeviceID) || value.ReceivedAt.IsZero() {
		return false
	}
	if value.EvidenceEligibility != (value.EvidenceIneligibleReason == "") || (value.EvidenceIneligibleReason != "" && !validEvidenceIneligibleReason(value.EvidenceIneligibleReason)) {
		return false
	}
	if value.ArchiveDisposition != "online" && value.ArchiveDisposition != "offline_succeeded" {
		return false
	}
	return value.OfflineSubmissionID == "" || validLearningUUID(value.OfflineSubmissionID)
}

func validEvidenceIneligibleReason(value string) bool {
	switch OfflineReasonCode(value) {
	case OfflineReasonDuplicateActivity, OfflineReasonStaleKnowledge, OfflineReasonExpiredActivity, OfflineReasonStaleContext, OfflineReasonStalePolicy, OfflineReasonAnswerRevealed:
		return true
	default:
		return false
	}
}

func validFrozenReferences(values []FrozenReference, knowledgeRevisionID string) bool {
	if values == nil || len(values) > 100 {
		return false
	}
	for _, reference := range values {
		if reference.KnowledgeRevisionID != knowledgeRevisionID || !validLearningUUID(reference.NodeID) || !validLearningUUID(reference.NodeRevisionID) || !validLearningUUID(reference.DocumentRevisionID) || reference.Start < 0 || reference.End < reference.Start || !validSHA256(reference.SliceSHA256) || !utf8.ValidString(reference.Slice) {
			return false
		}
	}
	return true
}

func validFreeQuestion(value FreeQuestion) bool {
	return validLearningUUID(value.FreeQuestionID) && validLearningUUID(value.SessionID) && validLearningUUID(value.FocusFrameID) && value.SessionAggregateVersion >= 1 && strings.TrimSpace(value.Text) != "" && utf8.ValidString(value.Text) && validLearningUUID(value.KnowledgeRevisionID) && validFrozenReferences(value.References, value.KnowledgeRevisionID) && validLearningUUID(value.ActorDeviceID) && !value.ReceivedAt.IsZero()
}

func validFreeAnswer(value FreeAnswer) bool {
	return validLearningUUID(value.FreeAnswerID) && validLearningUUID(value.SessionID) && validLearningUUID(value.FocusFrameID) && validLearningUUID(value.FreeQuestionID) && strings.TrimSpace(value.Text) != "" && utf8.ValidString(value.Text) && validLearningUUID(value.KnowledgeRevisionID) && validFrozenReferences(value.References, value.KnowledgeRevisionID) && (value.SourceProposalID == "" || validLearningUUID(value.SourceProposalID)) && !value.ReceivedAt.IsZero()
}

func validAssessmentProposalArtifact(value AssessmentArtifact) bool {
	// The proposal is created before the store arbitrates the activity evidence
	// claim. Preserve strict validation of every other field while accepting that
	// single unresolved eligibility state; committed session views must still
	// carry either eligibility=true or a stable ineligibility reason.
	if !value.EvidenceEligibility && value.EvidenceIneligibleReason == "" {
		value.EvidenceEligibility = true
	}
	return validAssessmentArtifact(value)
}

func validAssessmentArtifact(value AssessmentArtifact) bool {
	if !validLearningUUID(value.AssessmentID) || !validLearningUUID(value.SessionID) || !validLearningUUID(value.AttemptID) || !validLearningUUID(value.ActivityID) || value.ActivityRevision < 1 || value.Items == nil || value.Confidence < 0 || value.Confidence > 1000 || value.RiskFlags == nil || value.ModelID == "" || value.ModelParameters == nil || value.PromptRevision == "" || !validSHA256(value.ProposalInputHash) || value.Attempts < 1 || len(value.AttemptCategories) < 1 || len(value.AttemptCategories) > 2 || value.CreatedAt.IsZero() {
		return false
	}
	if value.EvidenceEligibility != (value.EvidenceIneligibleReason == "") || (value.EvidenceIneligibleReason != "" && !validEvidenceIneligibleReason(value.EvidenceIneligibleReason)) {
		return false
	}
	for _, item := range value.Items {
		if !validAssessmentItem(item) {
			return false
		}
	}
	return true
}

func validProposalType(value string) bool {
	switch value {
	case "route", "activity", "assessment", "free_answer", "explanation":
		return true
	default:
		return false
	}
}

func validTutoringState(value string) bool {
	switch value {
	case "Idle", "GoalReady", "Diagnostic", "RouteActive", "ActivityIssued", "AwaitingResponse", "Evaluating", "Feedback", "AdvanceOrReview", "Completed", "FocusSuspended", "FreeQuestion", "FreeAnswer", "FocusResumed":
		return true
	default:
		return false
	}
}

func invalidCommittedState(value string) bool {
	return value == "Idle" || value == "AdvanceOrReview" || value == "FocusSuspended" || value == "FocusResumed"
}

func validAllowedAction(value string) bool {
	switch value {
	case "start_diagnostic", "apply_route", "issue_activity", "present_activity", "submit_attempt", "record_assessment", "acknowledge_feedback", "present_review", "record_exposure", "ask_free_question", "record_free_answer", "convert_free_answer_to_quiz", "resume_focus", "end_activity", "switch_goal", "complete_session":
		return true
	default:
		return false
	}
}

func validAssessmentDecisionKind(value string) bool {
	return value == "confirm" || value == "override" || value == "void"
}

func validAssessmentDisposition(value string) bool {
	return value == "provisional" || value == "accepted" || value == "overridden" || value == "voided"
}

func validGoalRevision(value GoalRevision) bool {
	return validLearningUUID(value.GoalRevisionID) && validLearningUUID(value.GoalID) && value.Revision >= 1 && value.Text != "" && value.Source != "" && validLearningUUID(value.ActorDeviceID) && !value.CreatedAt.IsZero() && (value.PreviousRevisionID == "" || validLearningUUID(value.PreviousRevisionID))
}

func validateTutoringSession(value TutoringSession) error {
	if !validLearningUUID(value.SessionID) || !validTutoringState(value.State) || value.AggregateVersion < 0 {
		return errors.New("tutoring session is invalid")
	}
	for _, identifier := range []string{value.Focus.GoalRevisionID, value.Focus.RouteRevisionID, value.Focus.RouteStepID, value.Focus.KnowledgeRevisionID, value.Focus.FocusNodeRevisionID, value.Focus.ActivityID, value.Focus.AttemptID} {
		if identifier != "" && !validLearningUUID(identifier) {
			return errors.New("tutoring session focus is invalid")
		}
	}
	if frame := value.ActiveFocusFrame; frame != nil {
		if !validLearningUUID(frame.FocusFrameID) || frame.SessionID != value.SessionID || !validTutoringState(frame.SavedState) || frame.SavedAggregateVersion < 0 || frame.CreatedEventSeq < 0 {
			return errors.New("tutoring session focus frame is invalid")
		}
	}
	return nil
}

func assessmentDecisionMatchesArtifact(artifact AssessmentArtifact, decision AssessmentDecision) bool {
	if len(artifact.Items) != len(decision.Items) {
		return false
	}
	artifactByRubric := make(map[string]AssessmentItem, len(artifact.Items))
	for _, item := range artifact.Items {
		if _, exists := artifactByRubric[item.RubricItemID]; exists {
			return false
		}
		artifactByRubric[item.RubricItemID] = item
	}
	for _, item := range decision.Items {
		source, ok := artifactByRubric[item.RubricItemID]
		if !ok || source.AnswerQuote != item.AnswerQuote || source.AnswerRange != item.AnswerRange || source.AnswerQuoteSHA256 != item.AnswerQuoteSHA256 || source.KnowledgeReferenceID != item.KnowledgeReferenceID || source.KnowledgeQuote != item.KnowledgeQuote || source.KnowledgeRange != item.KnowledgeRange || source.KnowledgeQuoteSHA256 != item.KnowledgeQuoteSHA256 {
			return false
		}
	}
	return true
}

func validAssessmentDecision(value AssessmentDecision) bool {
	if !validLearningUUID(value.DecisionID) || !validLearningUUID(value.AssessmentID) || value.Version < 1 || !validAssessmentDisposition(value.Disposition) || value.Items == nil || !validLearningUUID(value.ActorDeviceID) || value.CreatedAt.IsZero() {
		return false
	}
	if value.ReplacesDecisionID != "" && !validLearningUUID(value.ReplacesDecisionID) {
		return false
	}
	if value.ProducedEvidenceID != "" && !validLearningUUID(value.ProducedEvidenceID) {
		return false
	}
	for _, item := range value.Items {
		if !validAssessmentItem(item) {
			return false
		}
	}
	return true
}

func uniqueAllowed(values []string, valid func(string) bool) bool {
	seen := make(map[string]bool, len(values))
	for _, value := range values {
		if !valid(value) || seen[value] {
			return false
		}
		seen[value] = true
	}
	return true
}
