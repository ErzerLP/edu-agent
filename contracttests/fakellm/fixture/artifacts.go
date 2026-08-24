package fixture

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

type sourceRange struct {
	Start int `json:"start"`
	End   int `json:"end"`
}

type proposalRequest struct {
	RequestID           string          `json:"request_id"`
	ProposalType        RequestKind     `json:"proposal_type"`
	AggregateType       string          `json:"aggregate_type"`
	AggregateID         string          `json:"aggregate_id"`
	AggregateVersion    int64           `json:"aggregate_version"`
	GoalRevisionID      string          `json:"goal_revision_id,omitempty"`
	RouteRevisionID     string          `json:"route_revision_id,omitempty"`
	RouteStepID         string          `json:"route_step_id,omitempty"`
	FocusNodeRevisionID string          `json:"focus_node_revision_id,omitempty"`
	ActivityID          string          `json:"activity_id,omitempty"`
	AttemptID           string          `json:"attempt_id,omitempty"`
	FreeQuestionID      string          `json:"free_question_id,omitempty"`
	FreeAnswerID        string          `json:"free_answer_id,omitempty"`
	FocusFrameID        string          `json:"focus_frame_id,omitempty"`
	TutoringState       string          `json:"tutoring_state,omitempty"`
	KnowledgeRevisionID string          `json:"knowledge_revision_id"`
	NodeRevisionIDs     []string        `json:"node_revision_ids"`
	Input               json.RawMessage `json:"input"`
}

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

type proposalContext struct {
	SchemaVersion string `json:"schema_version"`
	WorkItem      struct {
		Activity *struct {
			Rubric struct {
				Items []contextRubricItem `json:"items"`
			} `json:"rubric"`
			KnowledgeReferences []contextReference `json:"knowledge_references"`
		} `json:"activity,omitempty"`
		Attempt *struct {
			Answer string `json:"answer"`
		} `json:"attempt,omitempty"`
	} `json:"work_item"`
	Retrieval struct {
		Hits []contextReference `json:"hits"`
	} `json:"retrieval"`
}

type modelReference struct {
	NodeRevisionID string `json:"node_revision_id"`
}

type routeStep struct {
	NodeRevisionID      string `json:"node_revision_id"`
	TeachingIntent      string `json:"teaching_intent"`
	CompletionCondition string `json:"completion_condition"`
}

type rubricItem struct {
	RubricItemID         string   `json:"rubric_item_id"`
	Criterion            string   `json:"criterion"`
	RequiredReferenceIDs []string `json:"required_reference_ids,omitempty"`
}

type objectiveRule struct {
	AcceptedAnswers []string `json:"accepted_answers"`
	CaseSensitive   bool     `json:"case_sensitive"`
	TrimSpace       bool     `json:"trim_space"`
}

type rubric struct {
	RubricRevision string         `json:"rubric_revision"`
	Items          []rubricItem   `json:"items"`
	ObjectiveRule  *objectiveRule `json:"objective_rule,omitempty"`
}

type activity struct {
	Prompt              string           `json:"prompt"`
	Type                string           `json:"type"`
	Rubric              rubric           `json:"rubric"`
	Difficulty          int              `json:"difficulty"`
	AllowedHelp         []string         `json:"allowed_help"`
	KnowledgeReferences []modelReference `json:"knowledge_references"`
}

type assessmentItem struct {
	RubricItemID           string      `json:"rubric_item_id"`
	Conclusion             string      `json:"conclusion"`
	AnswerQuote            string      `json:"answer_quote"`
	AnswerRange            sourceRange `json:"answer_range"`
	AnswerQuoteSHA256      string      `json:"answer_quote_sha256"`
	KnowledgeReferenceID   string      `json:"knowledge_reference_id"`
	KnowledgeQuote         string      `json:"knowledge_quote"`
	KnowledgeRange         sourceRange `json:"knowledge_range"`
	KnowledgeQuoteSHA256   string      `json:"knowledge_quote_sha256"`
	MisconceptionCandidate string      `json:"misconception_candidate,omitempty"`
}

func renderArtifact(kind RequestKind, request proposalRequest, scenario Scenario) ([]byte, error) {
	switch kind {
	case KindRoute:
		steps := make([]routeStep, 0, len(request.NodeRevisionIDs))
		for index, nodeID := range request.NodeRevisionIDs {
			steps = append(steps, routeStep{
				NodeRevisionID:      nodeID,
				TeachingIntent:      fmt.Sprintf("Teach canonical node %d", index+1),
				CompletionCondition: fmt.Sprintf("Complete canonical node %d activity", index+1),
			})
		}
		return json.Marshal(struct {
			Route []routeStep `json:"route"`
		}{Route: steps})
	case KindActivity:
		return renderActivity(request, scenario)
	case KindAssessment:
		return renderAssessment(request, scenario)
	case KindFreeAnswer, KindExplanation:
		text := "Strict fake answer grounded in canonical knowledge."
		if kind == KindExplanation {
			text = "Strict fake explanation grounded in canonical knowledge."
		}
		return json.Marshal(struct {
			Text struct {
				Text                string           `json:"text"`
				KnowledgeReferences []modelReference `json:"knowledge_references"`
			} `json:"text"`
		}{Text: struct {
			Text                string           `json:"text"`
			KnowledgeReferences []modelReference `json:"knowledge_references"`
		}{Text: text, KnowledgeReferences: referencesFor(request)}})
	default:
		return nil, fmt.Errorf("unsupported proposal kind %q", kind)
	}
}

func renderActivity(request proposalRequest, scenario Scenario) ([]byte, error) {
	activityType := scenario.ActivityType
	if activityType == "" {
		activityType = "open"
	}
	allowedHelp := append([]string(nil), scenario.AllowedHelp...)
	if len(allowedHelp) == 0 {
		allowedHelp = []string{"none", "hint"}
	}
	target := request.FocusNodeRevisionID
	if target == "" && len(request.NodeRevisionIDs) > 0 {
		target = request.NodeRevisionIDs[0]
	}
	item := rubricItem{RubricItemID: "strict-rubric-item-1", Criterion: "Answer is supported by canonical knowledge"}
	if target != "" {
		item.RequiredReferenceIDs = []string{target}
	}
	value := activity{
		Prompt:              "Answer using the referenced canonical knowledge.",
		Type:                activityType,
		Rubric:              rubric{RubricRevision: "strict-rubric-v1", Items: []rubricItem{item}},
		Difficulty:          2,
		AllowedHelp:         allowedHelp,
		KnowledgeReferences: referencesFor(request),
	}
	if activityType == "objective" {
		value.Rubric.ObjectiveRule = &objectiveRule{AcceptedAnswers: []string{"expected"}, TrimSpace: true}
	}
	return json.Marshal(struct {
		Activity activity `json:"activity"`
	}{Activity: value})
}

func renderAssessment(request proposalRequest, scenario Scenario) ([]byte, error) {
	var contextValue proposalContext
	if err := json.Unmarshal(request.Input, &contextValue); err != nil {
		return nil, fmt.Errorf("decode proposal context: %w", err)
	}
	answer := "strict fake answer"
	items := []contextRubricItem{{RubricItemID: "strict-rubric-item-1", Criterion: "Answer is supported"}}
	references := contextValue.Retrieval.Hits
	if contextValue.WorkItem.Activity != nil {
		if len(contextValue.WorkItem.Activity.Rubric.Items) > 0 {
			items = contextValue.WorkItem.Activity.Rubric.Items
		}
		if len(contextValue.WorkItem.Activity.KnowledgeReferences) > 0 {
			references = contextValue.WorkItem.Activity.KnowledgeReferences
		}
	}
	if contextValue.WorkItem.Attempt != nil {
		answer = contextValue.WorkItem.Attempt.Answer
	}
	conclusion := scenario.AssessmentConclusion
	if conclusion == "" {
		conclusion = "pass"
	}
	outputItems := make([]assessmentItem, 0, len(items))
	for _, inputItem := range items {
		reference := chooseReference(inputItem.RequiredReferenceIDs, references, request.NodeRevisionIDs)
		outputItems = append(outputItems, assessmentItem{
			RubricItemID:         inputItem.RubricItemID,
			Conclusion:           conclusion,
			AnswerQuote:          answer,
			AnswerRange:          sourceRange{End: len(answer)},
			AnswerQuoteSHA256:    sha256Hex(answer),
			KnowledgeReferenceID: reference.NodeRevisionID,
			KnowledgeQuote:       reference.Slice,
			KnowledgeRange:       sourceRange{End: len(reference.Slice)},
			KnowledgeQuoteSHA256: sha256Hex(reference.Slice),
		})
	}
	confidence := 950
	risks := []string{}
	if scenario.Kind == ScenarioProvisional {
		confidence = 800
	}
	if scenario.Kind == ScenarioRisk {
		risks = []string{scenario.RiskFlag}
	}
	return json.Marshal(struct {
		Assessment struct {
			Items          []assessmentItem `json:"items"`
			RubricComplete bool             `json:"rubric_complete"`
			Confidence     int              `json:"confidence"`
			RiskFlags      []string         `json:"risk_flags"`
		} `json:"assessment"`
	}{Assessment: struct {
		Items          []assessmentItem `json:"items"`
		RubricComplete bool             `json:"rubric_complete"`
		Confidence     int              `json:"confidence"`
		RiskFlags      []string         `json:"risk_flags"`
	}{Items: outputItems, RubricComplete: true, Confidence: confidence, RiskFlags: risks}})
}

func referencesFor(request proposalRequest) []modelReference {
	result := make([]modelReference, 0, len(request.NodeRevisionIDs))
	for _, nodeID := range request.NodeRevisionIDs {
		result = append(result, modelReference{NodeRevisionID: nodeID})
	}
	return result
}

func chooseReference(required []string, references []contextReference, nodeIDs []string) contextReference {
	for _, requiredID := range required {
		for _, reference := range references {
			if reference.NodeRevisionID == requiredID {
				return reference
			}
		}
	}
	if len(references) > 0 {
		return references[0]
	}
	if len(required) > 0 {
		return contextReference{NodeRevisionID: required[0]}
	}
	if len(nodeIDs) > 0 {
		return contextReference{NodeRevisionID: nodeIDs[0]}
	}
	return contextReference{}
}

func sha256Hex(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}
