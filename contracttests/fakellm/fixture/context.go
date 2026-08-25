package fixture

import (
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

const proposalContextSchemaVersion = "go-cli-context-v1"

var fixtureUUID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

type contextReference struct {
	KnowledgeRevisionID string      `json:"knowledge_revision_id"`
	DocumentRevisionID  string      `json:"document_revision_id"`
	NodeID              string      `json:"node_id"`
	NodeRevisionID      string      `json:"node_revision_id"`
	Range               sourceRange `json:"range"`
	Slice               string      `json:"slice"`
	SliceSHA256         string      `json:"slice_sha256"`
}

type contextRubricItem struct {
	RubricItemID         string   `json:"rubric_item_id"`
	Criterion            string   `json:"criterion"`
	RequiredReferenceIDs []string `json:"required_reference_ids,omitempty"`
}

type contextObjectiveRule struct {
	AcceptedAnswers []string `json:"accepted_answers"`
	CaseSensitive   bool     `json:"case_sensitive"`
	TrimSpace       bool     `json:"trim_space"`
}

type contextRubric struct {
	RubricRevision string                `json:"rubric_revision"`
	Items          []contextRubricItem   `json:"items"`
	ObjectiveRule  *contextObjectiveRule `json:"objective_rule,omitempty"`
}

type contextGoalRevision struct {
	GoalRevisionID     string  `json:"goal_revision_id"`
	GoalID             string  `json:"goal_id"`
	Revision           int64   `json:"revision"`
	Text               string  `json:"text"`
	Source             string  `json:"source"`
	ActorDeviceID      string  `json:"actor_device_id"`
	CreatedAt          string  `json:"created_at"`
	PreviousRevisionID *string `json:"previous_revision_id,omitempty"`
}

type contextRouteStep struct {
	RouteStepID         string `json:"route_step_id"`
	Ordinal             int    `json:"ordinal"`
	NodeID              string `json:"node_id"`
	NodeRevisionID      string `json:"node_revision_id"`
	TeachingIntent      string `json:"teaching_intent"`
	CompletionCondition string `json:"completion_condition"`
}

type contextRouteRevision struct {
	RouteRevisionID     string             `json:"route_revision_id"`
	RouteID             string             `json:"route_id"`
	Revision            int64              `json:"revision"`
	GoalRevisionID      string             `json:"goal_revision_id"`
	KnowledgeRevisionID string             `json:"knowledge_revision_id"`
	RoutePolicyVersion  string             `json:"route_policy_version"`
	SourceProposalID    string             `json:"source_proposal_id"`
	Steps               []contextRouteStep `json:"steps"`
	CreatedAt           string             `json:"created_at"`
}

type contextActivity struct {
	ActivityID              string             `json:"activity_id"`
	Revision                int64              `json:"revision"`
	SessionID               string             `json:"session_id"`
	GoalRevisionID          string             `json:"goal_revision_id"`
	RouteRevisionID         string             `json:"route_revision_id"`
	RouteStepID             string             `json:"route_step_id"`
	KnowledgeRevisionID     string             `json:"knowledge_revision_id"`
	TargetNodeID            string             `json:"target_node_id"`
	TargetNodeRevisionID    string             `json:"target_node_revision_id"`
	KnowledgeReferences     []contextReference `json:"knowledge_references"`
	Prompt                  string             `json:"prompt"`
	Type                    string             `json:"type"`
	Rubric                  contextRubric      `json:"rubric"`
	Difficulty              int                `json:"difficulty"`
	AllowedHelp             []string           `json:"allowed_help"`
	ActivityPolicyVersion   string             `json:"activity_policy_version"`
	AssessmentPolicyVersion string             `json:"assessment_policy_version"`
	ReviewPolicyVersion     string             `json:"review_policy_version"`
	SourceProposalID        string             `json:"source_proposal_id,omitempty"`
	AttachedFreeQuestionID  string             `json:"attached_free_question_id,omitempty"`
	AttachedFreeAnswerID    string             `json:"attached_free_answer_id,omitempty"`
	Review                  bool               `json:"review"`
	CreatedAt               string             `json:"created_at"`
}

type contextAttempt struct {
	AttemptID        string  `json:"attempt_id"`
	SessionID        string  `json:"session_id"`
	ActivityID       string  `json:"activity_id"`
	ActivityRevision int64   `json:"activity_revision"`
	AnswerPayloadID  string  `json:"answer_payload_id"`
	Answer           string  `json:"answer"`
	AnswerSHA256     string  `json:"answer_sha256"`
	Help             string  `json:"help"`
	ActorDeviceID    string  `json:"actor_device_id"`
	OccurredAt       *string `json:"occurred_at,omitempty"`
	ReceivedAt       string  `json:"received_at"`
}

type contextAssessment struct {
	AssessmentID      string           `json:"assessment_id"`
	SessionID         string           `json:"session_id"`
	AttemptID         string           `json:"attempt_id"`
	ActivityID        string           `json:"activity_id"`
	ActivityRevision  int64            `json:"activity_revision"`
	Items             []assessmentItem `json:"items"`
	RubricComplete    bool             `json:"rubric_complete"`
	Confidence        int              `json:"confidence"`
	RiskFlags         []string         `json:"risk_flags"`
	ModelID           string           `json:"model_id"`
	ModelParameters   map[string]any   `json:"model_parameters"`
	PromptRevision    string           `json:"prompt_revision"`
	ProposalInputHash string           `json:"proposal_input_hash"`
	Attempts          int              `json:"attempts"`
	AttemptCategories []string         `json:"attempt_categories"`
	CreatedAt         string           `json:"created_at"`
}

type contextAssessmentDecision struct {
	DecisionID         string           `json:"decision_id"`
	AssessmentID       string           `json:"assessment_id"`
	Version            int64            `json:"version"`
	Disposition        string           `json:"disposition"`
	Items              []assessmentItem `json:"items"`
	Reason             string           `json:"reason,omitempty"`
	ActorDeviceID      string           `json:"actor_device_id"`
	CreatedAt          string           `json:"created_at"`
	ReplacesDecisionID string           `json:"replaces_decision_id,omitempty"`
	ProducedEvidenceID string           `json:"produced_evidence_id,omitempty"`
}

type contextFrozenReference struct {
	KnowledgeRevisionID string `json:"knowledge_revision_id"`
	NodeID              string `json:"node_id"`
	NodeRevisionID      string `json:"node_revision_id"`
	DocumentRevisionID  string `json:"document_revision_id"`
	Start               int    `json:"start"`
	End                 int    `json:"end"`
	Slice               string `json:"slice"`
	SliceSHA256         string `json:"slice_sha256"`
}

type contextFreeQuestion struct {
	FreeQuestionID          string                   `json:"free_question_id"`
	SessionID               string                   `json:"session_id"`
	FocusFrameID            string                   `json:"focus_frame_id"`
	SessionAggregateVersion int64                    `json:"session_aggregate_version"`
	Text                    string                   `json:"text"`
	KnowledgeRevisionID     string                   `json:"knowledge_revision_id"`
	References              []contextFrozenReference `json:"references"`
	ActorDeviceID           string                   `json:"actor_device_id"`
	OccurredAt              *string                  `json:"occurred_at,omitempty"`
	ReceivedAt              string                   `json:"received_at"`
}

type contextFreeAnswer struct {
	FreeAnswerID        string                   `json:"free_answer_id"`
	SessionID           string                   `json:"session_id"`
	FocusFrameID        string                   `json:"focus_frame_id"`
	FreeQuestionID      string                   `json:"free_question_id"`
	Text                string                   `json:"text"`
	KnowledgeRevisionID string                   `json:"knowledge_revision_id"`
	References          []contextFrozenReference `json:"references"`
	SourceProposalID    string                   `json:"source_proposal_id,omitempty"`
	ReceivedAt          string                   `json:"received_at"`
}

type contextWorkItem struct {
	AllowedActions             []string                   `json:"allowed_actions"`
	AllowedAssessmentDecisions []string                   `json:"allowed_assessment_decisions"`
	GoalRevision               *contextGoalRevision       `json:"goal_revision,omitempty"`
	RouteRevision              *contextRouteRevision      `json:"route_revision,omitempty"`
	Activity                   *contextActivity           `json:"activity,omitempty"`
	Attempt                    *contextAttempt            `json:"attempt,omitempty"`
	Assessment                 *contextAssessment         `json:"assessment,omitempty"`
	AssessmentDecision         *contextAssessmentDecision `json:"assessment_decision,omitempty"`
	FreeQuestion               *contextFreeQuestion       `json:"free_question,omitempty"`
	FreeAnswer                 *contextFreeAnswer         `json:"free_answer,omitempty"`
}

type proposalContext struct {
	SchemaVersion string          `json:"schema_version"`
	WorkItem      contextWorkItem `json:"work_item"`
	Retrieval     struct {
		KnowledgeRevisionID string             `json:"knowledge_revision_id"`
		Hits                []contextReference `json:"hits"`
	} `json:"retrieval"`
}

// validatedProposalContext contains only references that passed the fixture's independent authority checks.
type validatedProposalContext struct {
	value      proposalContext
	references []contextReference
	byNodeID   map[string]contextReference
}

func validateProposalFixtureContext(request proposalRequest, kind RequestKind) (validatedProposalContext, error) {
	if err := validateProposalFixtureRequest(request, kind); err != nil {
		return validatedProposalContext{}, err
	}
	var contextValue proposalContext
	if err := decodeStrict(request.Input, &contextValue); err != nil {
		return validatedProposalContext{}, fmt.Errorf("decode proposal context: %w", err)
	}
	if contextValue.SchemaVersion != proposalContextSchemaVersion {
		return validatedProposalContext{}, fmt.Errorf("proposal context schema version %q is unsupported", contextValue.SchemaVersion)
	}
	if contextValue.WorkItem.AllowedActions == nil || contextValue.WorkItem.AllowedAssessmentDecisions == nil {
		return validatedProposalContext{}, fmt.Errorf("proposal work item action authority is missing")
	}
	if contextValue.Retrieval.KnowledgeRevisionID != request.KnowledgeRevisionID {
		return validatedProposalContext{}, fmt.Errorf("retrieval knowledge revision does not match proposal")
	}
	if err := validateWorkItemAuthority(request, kind, contextValue.WorkItem); err != nil {
		return validatedProposalContext{}, err
	}

	byNodeID := make(map[string]contextReference, len(contextValue.Retrieval.Hits))
	seenAuthorityNodeIDs := make(map[string]bool, len(contextValue.Retrieval.Hits))
	for _, reference := range contextValue.Retrieval.Hits {
		if err := validateContextReference(reference, request.KnowledgeRevisionID); err != nil {
			return validatedProposalContext{}, err
		}
		if _, exists := byNodeID[reference.NodeRevisionID]; exists || seenAuthorityNodeIDs[reference.NodeID] {
			return validatedProposalContext{}, fmt.Errorf("proposal retrieval contains duplicate node authority")
		}
		byNodeID[reference.NodeRevisionID] = reference
		seenAuthorityNodeIDs[reference.NodeID] = true
	}
	if len(byNodeID) != len(request.NodeRevisionIDs) {
		return validatedProposalContext{}, fmt.Errorf("proposal retrieval node revision set is incomplete")
	}
	ordered := make([]contextReference, 0, len(request.NodeRevisionIDs))
	for _, nodeRevisionID := range request.NodeRevisionIDs {
		reference, ok := byNodeID[nodeRevisionID]
		if !ok {
			return validatedProposalContext{}, fmt.Errorf("proposal retrieval is missing node revision %q", nodeRevisionID)
		}
		ordered = append(ordered, reference)
	}
	validated := validatedProposalContext{value: contextValue, references: ordered, byNodeID: byNodeID}
	if kind == KindAssessment {
		if err := validateAssessmentReferences(validated); err != nil {
			return validatedProposalContext{}, err
		}
	}
	return validated, nil
}

func validateProposalFixtureRequest(request proposalRequest, kind RequestKind) error {
	if request.ProposalType != kind || kind == KindCapabilityProbe || !fixtureUUID.MatchString(request.RequestID) || request.AggregateType != "session" || !fixtureUUID.MatchString(request.AggregateID) || request.AggregateVersion < 0 || !fixtureUUID.MatchString(request.GoalRevisionID) || !fixtureUUID.MatchString(request.KnowledgeRevisionID) || len(request.NodeRevisionIDs) == 0 || len(request.NodeRevisionIDs) > 100 {
		return fmt.Errorf("invalid proposal request schema")
	}
	for _, identifier := range []string{
		request.RouteRevisionID, request.RouteStepID, request.FocusNodeRevisionID, request.ActivityID,
		request.AttemptID, request.FreeQuestionID, request.FreeAnswerID, request.FocusFrameID,
	} {
		if identifier != "" && !fixtureUUID.MatchString(identifier) {
			return fmt.Errorf("proposal context identifier is invalid")
		}
	}
	seen := make(map[string]bool, len(request.NodeRevisionIDs))
	for _, nodeRevisionID := range request.NodeRevisionIDs {
		if !fixtureUUID.MatchString(nodeRevisionID) || seen[nodeRevisionID] {
			return fmt.Errorf("proposal node revision IDs are invalid")
		}
		seen[nodeRevisionID] = true
	}
	requireRoute := func() bool {
		return request.RouteRevisionID != "" && request.RouteStepID != "" && request.FocusNodeRevisionID != ""
	}
	switch kind {
	case KindRoute:
		if request.TutoringState != "Diagnostic" && request.TutoringState != "RouteActive" {
			return fmt.Errorf("route proposal state is invalid")
		}
	case KindActivity:
		if !requireRoute() || (request.TutoringState != "RouteActive" && request.TutoringState != "FreeAnswer") {
			return fmt.Errorf("activity proposal context is incomplete")
		}
		if request.TutoringState == "FreeAnswer" && (request.FreeQuestionID == "" || request.FreeAnswerID == "" || request.FocusFrameID == "") {
			return fmt.Errorf("attached activity proposal context is incomplete")
		}
	case KindAssessment:
		if !requireRoute() || request.ActivityID == "" || request.AttemptID == "" || request.TutoringState != "Evaluating" {
			return fmt.Errorf("assessment proposal context is incomplete")
		}
	case KindFreeAnswer:
		if !requireRoute() || request.FreeQuestionID == "" || request.FocusFrameID == "" || request.TutoringState != "FreeQuestion" {
			return fmt.Errorf("free-answer proposal context is incomplete")
		}
	case KindExplanation:
		if !requireRoute() || request.TutoringState != "RouteActive" {
			return fmt.Errorf("explanation proposal context is incomplete")
		}
	default:
		return fmt.Errorf("unsupported proposal kind %q", kind)
	}
	return nil
}

func validateWorkItemAuthority(request proposalRequest, kind RequestKind, workItem contextWorkItem) error {
	goal := workItem.GoalRevision
	if goal == nil || goal.GoalRevisionID != request.GoalRevisionID || !fixtureUUID.MatchString(goal.GoalRevisionID) {
		return fmt.Errorf("work-item goal revision authority does not match proposal")
	}
	if request.RouteRevisionID != "" {
		route := workItem.RouteRevision
		if route == nil || route.RouteRevisionID != request.RouteRevisionID || route.GoalRevisionID != request.GoalRevisionID || route.KnowledgeRevisionID != request.KnowledgeRevisionID {
			return fmt.Errorf("work-item route authority does not match proposal")
		}
		matchedStep := false
		for _, step := range route.Steps {
			if !fixtureUUID.MatchString(step.RouteStepID) || !fixtureUUID.MatchString(step.NodeID) || !fixtureUUID.MatchString(step.NodeRevisionID) {
				return fmt.Errorf("work-item route step authority ID is invalid")
			}
			if step.RouteStepID == request.RouteStepID {
				if request.FocusNodeRevisionID != "" && step.NodeRevisionID != request.FocusNodeRevisionID {
					return fmt.Errorf("work-item route step node authority does not match proposal")
				}
				matchedStep = true
				break
			}
		}
		if request.RouteStepID != "" && !matchedStep {
			return fmt.Errorf("work-item route step authority is missing")
		}
	}
	if request.ActivityID != "" {
		activity := workItem.Activity
		if activity == nil || activity.ActivityID != request.ActivityID || activity.SessionID != request.AggregateID || activity.GoalRevisionID != request.GoalRevisionID || activity.RouteRevisionID != request.RouteRevisionID || activity.RouteStepID != request.RouteStepID || activity.KnowledgeRevisionID != request.KnowledgeRevisionID || !fixtureUUID.MatchString(activity.TargetNodeID) || activity.TargetNodeRevisionID != request.FocusNodeRevisionID {
			return fmt.Errorf("work-item activity authority does not match proposal")
		}
		if request.FreeQuestionID != "" && activity.AttachedFreeQuestionID != "" && activity.AttachedFreeQuestionID != request.FreeQuestionID {
			return fmt.Errorf("work-item attached question authority does not match proposal")
		}
		if request.FreeAnswerID != "" && activity.AttachedFreeAnswerID != "" && activity.AttachedFreeAnswerID != request.FreeAnswerID {
			return fmt.Errorf("work-item attached answer authority does not match proposal")
		}
	}
	if request.AttemptID != "" {
		attempt := workItem.Attempt
		if attempt == nil || attempt.AttemptID != request.AttemptID || attempt.SessionID != request.AggregateID || attempt.ActivityID != request.ActivityID || !utf8.ValidString(attempt.Answer) || attempt.Answer == "" {
			return fmt.Errorf("work-item attempt authority does not match proposal")
		}
	}
	if request.FreeQuestionID != "" {
		question := workItem.FreeQuestion
		if question == nil || question.FreeQuestionID != request.FreeQuestionID || question.SessionID != request.AggregateID || question.FocusFrameID != request.FocusFrameID || question.KnowledgeRevisionID != request.KnowledgeRevisionID {
			return fmt.Errorf("work-item free-question authority does not match proposal")
		}
	}
	if request.FreeAnswerID != "" {
		answer := workItem.FreeAnswer
		if answer == nil || answer.FreeAnswerID != request.FreeAnswerID || answer.SessionID != request.AggregateID || answer.FocusFrameID != request.FocusFrameID || answer.FreeQuestionID != request.FreeQuestionID || answer.KnowledgeRevisionID != request.KnowledgeRevisionID {
			return fmt.Errorf("work-item free-answer authority does not match proposal")
		}
	}
	if (kind == KindActivity || kind == KindAssessment) && request.FocusNodeRevisionID != "" {
		if workItem.RouteRevision == nil {
			return fmt.Errorf("work-item focus route authority is missing")
		}
	}
	return nil
}

func validateContextReference(reference contextReference, knowledgeRevisionID string) error {
	if reference.KnowledgeRevisionID != knowledgeRevisionID || !fixtureUUID.MatchString(reference.KnowledgeRevisionID) || !fixtureUUID.MatchString(reference.DocumentRevisionID) || !fixtureUUID.MatchString(reference.NodeID) || !fixtureUUID.MatchString(reference.NodeRevisionID) {
		return fmt.Errorf("proposal retrieval reference authority is invalid")
	}
	if !utf8.ValidString(reference.Slice) || reference.Slice == "" || strings.ContainsRune(reference.Slice, '\x00') {
		return fmt.Errorf("proposal retrieval canonical slice is invalid UTF-8")
	}
	if reference.Range.Start < 0 || reference.Range.End <= reference.Range.Start || reference.Range.End-reference.Range.Start != len(reference.Slice) {
		return fmt.Errorf("proposal retrieval canonical slice range is invalid")
	}
	if reference.SliceSHA256 != sha256Hex(reference.Slice) {
		return fmt.Errorf("proposal retrieval canonical slice hash is invalid")
	}
	return nil
}

func validateAssessmentReferences(contextValue validatedProposalContext) error {
	activity := contextValue.value.WorkItem.Activity
	if activity == nil || len(activity.KnowledgeReferences) != len(contextValue.references) || len(activity.Rubric.Items) == 0 {
		return fmt.Errorf("assessment work-item references or rubric are incomplete")
	}
	target, ok := contextValue.byNodeID[activity.TargetNodeRevisionID]
	if !ok || target.NodeID != activity.TargetNodeID {
		return fmt.Errorf("assessment activity target authority is stale")
	}
	seenReferences := make(map[string]bool, len(activity.KnowledgeReferences))
	for _, reference := range activity.KnowledgeReferences {
		canonical, ok := contextValue.byNodeID[reference.NodeRevisionID]
		if !ok || seenReferences[reference.NodeRevisionID] || reference != canonical {
			return fmt.Errorf("assessment work-item reference is stale or non-canonical")
		}
		seenReferences[reference.NodeRevisionID] = true
	}
	seenItems := make(map[string]bool, len(activity.Rubric.Items))
	for _, item := range activity.Rubric.Items {
		if strings.TrimSpace(item.RubricItemID) == "" || strings.TrimSpace(item.Criterion) == "" || seenItems[item.RubricItemID] {
			return fmt.Errorf("assessment work-item rubric is invalid")
		}
		seenItems[item.RubricItemID] = true
		seenRequired := map[string]bool{}
		for _, required := range item.RequiredReferenceIDs {
			if _, ok := contextValue.byNodeID[required]; !ok || seenRequired[required] {
				return fmt.Errorf("assessment rubric reference authority is invalid")
			}
			seenRequired[required] = true
		}
	}
	return nil
}
