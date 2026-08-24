package fixture

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	maxRouteCanonicalExcerptRunes = 96
	maxRouteCanonicalExcerptBytes = 256
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

type modelReference struct {
	NodeRevisionID string      `json:"node_revision_id"`
	SliceSHA256    string      `json:"slice_sha256"`
	Range          sourceRange `json:"range"`
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
	contextValue, err := validateProposalFixtureContext(request, kind)
	if err != nil {
		return nil, err
	}
	switch kind {
	case KindRoute:
		steps, err := renderRouteSteps(request, contextValue, scenario)
		if err != nil {
			return nil, err
		}
		return json.Marshal(struct {
			Route []routeStep `json:"route"`
		}{Route: steps})
	case KindActivity:
		return renderActivity(request, contextValue, scenario)
	case KindAssessment:
		return renderAssessment(contextValue, scenario)
	case KindFreeAnswer, KindExplanation:
		excerpt, err := normalizeRouteCanonicalSlice(contextValue.references[0].Slice)
		if err != nil {
			return nil, err
		}
		text := "Strict fake answer grounded in canonical knowledge: " + excerpt
		if kind == KindExplanation {
			text = "Strict fake explanation grounded in canonical knowledge: " + excerpt
		}
		return json.Marshal(struct {
			Text struct {
				Text                string           `json:"text"`
				KnowledgeReferences []modelReference `json:"knowledge_references"`
			} `json:"text"`
		}{Text: struct {
			Text                string           `json:"text"`
			KnowledgeReferences []modelReference `json:"knowledge_references"`
		}{Text: text, KnowledgeReferences: modelReferencesFor(contextValue.references)}})
	default:
		return nil, fmt.Errorf("unsupported proposal kind %q", kind)
	}
}

func renderRouteSteps(request proposalRequest, contextValue validatedProposalContext, scenario Scenario) ([]routeStep, error) {
	references := contextValue.references
	if scenario.RouteStepLimit > 0 && scenario.RouteStepLimit < len(references) {
		references = references[:scenario.RouteStepLimit]
	}
	steps := make([]routeStep, 0, len(references))
	for _, reference := range references {
		excerpt, err := normalizeRouteCanonicalSlice(reference.Slice)
		if err != nil {
			return nil, fmt.Errorf("normalize route retrieval hit for node revision %q: %w", reference.NodeRevisionID, err)
		}
		steps = append(steps, routeStep{
			NodeRevisionID:      reference.NodeRevisionID,
			TeachingIntent:      "Teach the canonical concept: " + excerpt,
			CompletionCondition: "Complete when the learner explains the canonical concept: " + excerpt,
		})
	}
	if len(steps) == 0 || len(request.NodeRevisionIDs) == 0 {
		return nil, fmt.Errorf("route proposal has no canonical references")
	}
	return steps, nil
}

func normalizeRouteCanonicalSlice(value string) (string, error) {
	if !utf8.ValidString(value) {
		return "", fmt.Errorf("canonical slice is not valid UTF-8")
	}

	runes := make([]rune, 0, maxRouteCanonicalExcerptRunes)
	bytesUsed := 0
	pendingSpace := false
	for _, r := range value {
		if unicode.IsSpace(r) {
			if len(runes) > 0 {
				pendingSpace = true
			}
			continue
		}
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			continue
		}

		size := utf8.RuneLen(r)
		spaceRunes := 0
		spaceBytes := 0
		if pendingSpace {
			spaceRunes = 1
			spaceBytes = 1
		}
		if len(runes)+spaceRunes+1 > maxRouteCanonicalExcerptRunes || bytesUsed+spaceBytes+size > maxRouteCanonicalExcerptBytes {
			break
		}
		if pendingSpace {
			runes = append(runes, ' ')
			bytesUsed++
			pendingSpace = false
		}
		runes = append(runes, r)
		bytesUsed += size
	}

	result := strings.TrimSpace(string(runes))
	if result == "" {
		return "", fmt.Errorf("canonical slice is empty after normalization")
	}
	return result, nil
}

func renderActivity(request proposalRequest, contextValue validatedProposalContext, scenario Scenario) ([]byte, error) {
	activityType := scenario.ActivityType
	if activityType == "" {
		activityType = "open"
	}
	allowedHelp := append([]string(nil), scenario.AllowedHelp...)
	if len(allowedHelp) == 0 {
		allowedHelp = []string{"none", "hint"}
	}
	target := request.FocusNodeRevisionID
	if target == "" {
		target = contextValue.references[0].NodeRevisionID
	}
	targetReference, ok := contextValue.byNodeID[target]
	if !ok {
		return nil, fmt.Errorf("activity target canonical reference is missing")
	}
	excerpt, err := normalizeRouteCanonicalSlice(targetReference.Slice)
	if err != nil {
		return nil, err
	}
	item := rubricItem{
		RubricItemID:         "strict-rubric-item-1",
		Criterion:            "Answer is supported by the canonical concept: " + excerpt,
		RequiredReferenceIDs: []string{target},
	}
	value := activity{
		Prompt:              "Answer using the canonical concept: " + excerpt,
		Type:                activityType,
		Rubric:              rubric{RubricRevision: "strict-rubric-v1", Items: []rubricItem{item}},
		Difficulty:          2,
		AllowedHelp:         allowedHelp,
		KnowledgeReferences: modelReferencesFor(contextValue.references),
	}
	if activityType == "objective" {
		value.Rubric.ObjectiveRule = &objectiveRule{AcceptedAnswers: []string{"expected"}, TrimSpace: true}
	}
	return json.Marshal(struct {
		Activity activity `json:"activity"`
	}{Activity: value})
}

func renderAssessment(contextValue validatedProposalContext, scenario Scenario) ([]byte, error) {
	activityValue := contextValue.value.WorkItem.Activity
	attempt := contextValue.value.WorkItem.Attempt
	if activityValue == nil || attempt == nil {
		return nil, fmt.Errorf("assessment authority context is incomplete")
	}
	conclusion := scenario.AssessmentConclusion
	if conclusion == "" {
		conclusion = "pass"
	}
	outputItems := make([]assessmentItem, 0, len(activityValue.Rubric.Items))
	for _, inputItem := range activityValue.Rubric.Items {
		reference, err := chooseAssessmentReference(inputItem.RequiredReferenceIDs, activityValue.KnowledgeReferences)
		if err != nil {
			return nil, fmt.Errorf("rubric item %q: %w", inputItem.RubricItemID, err)
		}
		outputItems = append(outputItems, assessmentItem{
			RubricItemID:         inputItem.RubricItemID,
			Conclusion:           conclusion,
			AnswerQuote:          attempt.Answer,
			AnswerRange:          sourceRange{End: len(attempt.Answer)},
			AnswerQuoteSHA256:    sha256Hex(attempt.Answer),
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

func modelReferencesFor(references []contextReference) []modelReference {
	result := make([]modelReference, 0, len(references))
	for _, reference := range references {
		result = append(result, modelReference{
			NodeRevisionID: reference.NodeRevisionID,
			SliceSHA256:    reference.SliceSHA256,
			Range:          reference.Range,
		})
	}
	return result
}

func chooseAssessmentReference(required []string, references []contextReference) (contextReference, error) {
	byNodeID := make(map[string]contextReference, len(references))
	for _, reference := range references {
		byNodeID[reference.NodeRevisionID] = reference
	}
	for _, requiredID := range required {
		if reference, ok := byNodeID[requiredID]; ok {
			return reference, nil
		}
	}
	if len(required) > 0 {
		return contextReference{}, fmt.Errorf("required canonical reference is missing")
	}
	if len(references) == 0 {
		return contextReference{}, fmt.Errorf("canonical reference is missing")
	}
	return references[0], nil
}

func sha256Hex(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}
