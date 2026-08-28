package learning

import (
	"fmt"
	"sort"
	"strings"
)

type Acceptance struct {
	Disposition Disposition `json:"disposition"`
	Outcome     Outcome     `json:"outcome,omitempty"`
	Reasons     []string    `json:"reasons,omitempty"`
}

func EvaluateAssessment(activity Activity, attempt Attempt, artifact AssessmentArtifact) (Acceptance, error) {
	if activity.TargetNodeID == "" || activity.TargetNodeRevisionID == "" {
		return Acceptance{}, &Error{Code: CodeProposalRejected, Reason: "activity_target_missing"}
	}
	targetReference := false
	for _, reference := range activity.References {
		if reference.KnowledgeRevisionID == activity.KnowledgeRevisionID && reference.NodeID == activity.TargetNodeID && reference.NodeRevisionID == activity.TargetNodeRevisionID {
			targetReference = true
			break
		}
	}
	if !targetReference {
		return Acceptance{}, &Error{Code: CodeProposalRejected, Reason: "activity_target_reference_missing"}
	}
	if attempt.ActivityID != activity.ID || attempt.ActivityRevision != activity.Revision || artifact.AttemptID != attempt.ID || artifact.ActivityID != activity.ID || artifact.ActivityRevision != activity.Revision {
		return Acceptance{}, &Error{Code: CodeProposalRejected, Reason: "immutable_reference_mismatch"}
	}
	if attempt.Help == HelpAnswerRevealed {
		return Acceptance{Disposition: DispositionProvisional, Reasons: []string{"answer_revealed"}}, nil
	}
	if activity.Type == ActivityObjective {
		if activity.Rubric.ObjectiveRule == nil || len(activity.Rubric.ObjectiveRule.AcceptedAnswers) == 0 {
			return Acceptance{}, &Error{Code: CodeProposalRejected, Reason: "objective_rule_missing"}
		}
		actual := attempt.Answer
		if activity.Rubric.ObjectiveRule.TrimSpace {
			actual = strings.TrimSpace(actual)
		}
		for _, expected := range activity.Rubric.ObjectiveRule.AcceptedAnswers {
			if activity.Rubric.ObjectiveRule.TrimSpace {
				expected = strings.TrimSpace(expected)
			}
			if (!activity.Rubric.ObjectiveRule.CaseSensitive && strings.EqualFold(actual, expected)) || actual == expected {
				return Acceptance{Disposition: DispositionAccepted, Outcome: OutcomePass}, nil
			}
		}
		return Acceptance{Disposition: DispositionAccepted, Outcome: OutcomeFail}, nil
	}
	if activity.Type != ActivityOpen {
		return Acceptance{}, &Error{Code: CodeProposalRejected, Reason: "activity_not_assessable"}
	}
	if artifact.Confidence < 0 || artifact.Confidence > 1000 {
		return Acceptance{}, &Error{Code: CodeProposalRejected, Reason: "confidence_out_of_range"}
	}
	knownRisks := map[RiskFlag]bool{
		RiskIncompleteRubric: true, RiskInsufficientAnswerEvidence: true, RiskInsufficientKnowledgeSupport: true,
		RiskConflictingEvidence: true, RiskAmbiguousRubric: true, RiskUnsafeContent: true,
		RiskSchemaRepaired: true, RiskStaleContext: true, RiskRetryExhausted: true,
	}
	for _, risk := range artifact.RiskFlags {
		if !knownRisks[risk] {
			return Acceptance{}, &Error{Code: CodeProposalRejected, Reason: "unknown_risk_flag"}
		}
	}
	items := make(map[string]RubricItem, len(activity.Rubric.Items))
	provisional := []string{}
	if len(activity.Rubric.Items) == 0 {
		provisional = append(provisional, "incomplete_rubric")
	}
	for _, item := range activity.Rubric.Items {
		if item.ID == "" || items[item.ID].ID != "" {
			return Acceptance{}, &Error{Code: CodeProposalRejected, Reason: "invalid_frozen_rubric"}
		}
		items[item.ID] = item
	}
	refs := make(map[string]KnowledgeReference, len(activity.References))
	for _, ref := range activity.References {
		if ref.NodeRevisionID == "" || ref.KnowledgeRevisionID != activity.KnowledgeRevisionID || refs[ref.NodeRevisionID].NodeRevisionID != "" {
			return Acceptance{}, &Error{Code: CodeProposalRejected, Reason: "invalid_frozen_reference"}
		}
		refs[ref.NodeRevisionID] = ref
	}
	seen := map[string]bool{}
	outcomes := make([]Outcome, 0, len(artifact.Items))
	for _, item := range artifact.Items {
		frozen, exists := items[item.RubricItemID]
		if !exists || seen[item.RubricItemID] {
			return Acceptance{}, &Error{Code: CodeProposalRejected, Reason: "unknown_or_duplicate_rubric_item"}
		}
		seen[item.RubricItemID] = true
		scorable := true
		switch item.Conclusion {
		case ConclusionPass:
			outcomes = append(outcomes, OutcomePass)
		case ConclusionPartial:
			outcomes = append(outcomes, OutcomePartial)
		case ConclusionFail:
			outcomes = append(outcomes, OutcomeFail)
		case ConclusionUnassessed:
			scorable = false
			provisional = append(provisional, "unassessed")
		default:
			return Acceptance{}, &Error{Code: CodeProposalRejected, Reason: "unknown_conclusion"}
		}
		if item.MisconceptionCandidate != "" && item.Conclusion != ConclusionFail && item.Conclusion != ConclusionPartial {
			return Acceptance{}, &Error{Code: CodeProposalRejected, Reason: "misconception_without_failure"}
		}
		if assessmentSupportMissing(item.AnswerRange, item.AnswerQuote, item.AnswerQuoteSHA256) {
			if scorable {
				provisional = append(provisional, "insufficient_answer_evidence")
			}
		} else if err := validateSlice(attempt.Answer, item.AnswerRange, item.AnswerQuote, item.AnswerQuoteSHA256); err != nil {
			return Acceptance{}, &Error{Code: CodeProposalRejected, Reason: "invalid_answer_quote", Cause: err}
		}

		if item.KnowledgeReferenceID == "" {
			if !assessmentSupportMissing(item.KnowledgeRange, item.KnowledgeQuote, item.KnowledgeQuoteSHA256) {
				return Acceptance{}, &Error{Code: CodeProposalRejected, Reason: "knowledge_reference_mismatch"}
			}
			if scorable {
				provisional = append(provisional, "insufficient_knowledge_support")
			}
			continue
		}
		ref, ok := refs[item.KnowledgeReferenceID]
		if !ok || ref.KnowledgeRevisionID != activity.KnowledgeRevisionID || (len(frozen.RequiredReferenceIDs) > 0 && !containsString(frozen.RequiredReferenceIDs, item.KnowledgeReferenceID)) {
			return Acceptance{}, &Error{Code: CodeProposalRejected, Reason: "knowledge_reference_mismatch"}
		}
		if assessmentSupportMissing(item.KnowledgeRange, item.KnowledgeQuote, item.KnowledgeQuoteSHA256) {
			if scorable {
				provisional = append(provisional, "insufficient_knowledge_support")
			}
		} else if err := validateSlice(ref.Slice, item.KnowledgeRange, item.KnowledgeQuote, item.KnowledgeQuoteSHA256); err != nil {
			return Acceptance{}, &Error{Code: CodeProposalRejected, Reason: "invalid_knowledge_quote", Cause: err}
		}
	}
	if len(seen) != len(items) || !artifact.RubricComplete {
		provisional = append(provisional, "incomplete_rubric")
	}
	if len(outcomes) == 0 {
		provisional = append(provisional, "no_scorable_items")
	}
	if artifact.Confidence < 850 {
		provisional = append(provisional, "low_confidence")
	}
	for _, risk := range artifact.RiskFlags {
		provisional = append(provisional, string(risk))
	}
	if len(provisional) > 0 {
		sort.Strings(provisional)
		return Acceptance{Disposition: DispositionProvisional, Reasons: unique(provisional)}, nil
	}
	outcome := combineOutcomes(outcomes)
	if outcome == "" {
		return Acceptance{Disposition: DispositionProvisional, Reasons: []string{"no_scorable_items"}}, nil
	}
	return Acceptance{Disposition: DispositionAccepted, Outcome: outcome}, nil
}

func validateSlice(source string, byteRange SourceRange, quote, expectedHash string) error {
	if byteRange.Start < 0 || byteRange.End <= byteRange.Start || byteRange.End > len(source) {
		return fmt.Errorf("range out of bounds")
	}
	actual := source[byteRange.Start:byteRange.End]
	if actual != quote || SHA256([]byte(actual)) != expectedHash {
		return fmt.Errorf("quote or hash mismatch")
	}
	return nil
}

func assessmentSupportMissing(byteRange SourceRange, quote, expectedHash string) bool {
	return byteRange.Start == 0 && byteRange.End == 0 && quote == "" && (expectedHash == "" || expectedHash == SHA256(nil))
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func combineOutcomes(values []Outcome) Outcome {
	if len(values) == 0 {
		return ""
	}
	result := OutcomePass
	for _, value := range values {
		if value == OutcomeFail {
			return OutcomeFail
		}
		if value == OutcomePartial {
			result = OutcomePartial
		}
	}
	return result
}

func unique(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	result := values[:1]
	for _, value := range values[1:] {
		if value != result[len(result)-1] {
			result = append(result, value)
		}
	}
	return result
}

type DecisionCommand struct {
	Kind            string
	ExpectedVersion int64
	Reason          string
	Items           []AssessmentItem
}

type DecisionEffect struct {
	Disposition        Disposition
	Items              []AssessmentItem
	InvalidateEvidence bool
	CreateEvidence     bool
}

func DecideAssessment(current AssessmentDecision, artifact AssessmentArtifact, command DecisionCommand, confirmable ...bool) (DecisionEffect, error) {
	if command.ExpectedVersion != current.Version {
		return DecisionEffect{}, &Error{Code: CodeAssessmentDispositionConflict, CurrentDisposition: string(current.Disposition)}
	}
	invalidate := current.ProducedEvidenceID != nil
	switch command.Kind {
	case "confirm":
		if current.Disposition != DispositionProvisional || len(confirmable) == 0 || !confirmable[0] {
			return DecisionEffect{}, &Error{Code: CodeAssessmentDispositionConflict, CurrentDisposition: string(current.Disposition)}
		}
		return DecisionEffect{Disposition: DispositionAccepted, Items: append([]AssessmentItem(nil), artifact.Items...), CreateEvidence: true}, nil
	case "override":
		if current.Disposition == DispositionVoided || strings.TrimSpace(command.Reason) == "" || len(command.Items) == 0 {
			return DecisionEffect{}, &Error{Code: CodeAssessmentDispositionConflict, CurrentDisposition: string(current.Disposition)}
		}
		if err := completeReplacement(artifact, command.Items); err != nil {
			return DecisionEffect{}, &Error{Code: CodeAssessmentDispositionConflict, CurrentDisposition: string(current.Disposition), Cause: err}
		}
		return DecisionEffect{Disposition: DispositionOverridden, Items: append([]AssessmentItem(nil), command.Items...), InvalidateEvidence: invalidate, CreateEvidence: true}, nil
	case "void":
		if current.Disposition == DispositionVoided {
			return DecisionEffect{}, &Error{Code: CodeAssessmentDispositionConflict, CurrentDisposition: string(current.Disposition)}
		}
		return DecisionEffect{Disposition: DispositionVoided, Items: append([]AssessmentItem(nil), current.Items...), InvalidateEvidence: invalidate}, nil
	default:
		return DecisionEffect{}, &Error{Code: CodeInvalidRequest}
	}
}

func ConfirmableAssessment(activity Activity, attempt Attempt, artifact AssessmentArtifact) bool {
	if attempt.Help == HelpAnswerRevealed || !artifact.RubricComplete || len(activity.Rubric.Items) == 0 || len(artifact.Items) != len(activity.Rubric.Items) {
		return false
	}
	acceptance, err := EvaluateAssessment(activity, attempt, artifact)
	if err != nil || acceptance.Disposition != DispositionProvisional {
		return false
	}
	for _, reason := range acceptance.Reasons {
		switch reason {
		case "low_confidence", string(RiskConflictingEvidence), string(RiskAmbiguousRubric):
		default:
			return false
		}
	}
	return len(acceptance.Reasons) > 0
}

func ValidateAssessmentReplacement(activity Activity, attempt Attempt, artifact AssessmentArtifact, items []AssessmentItem) (Outcome, error) {
	if activity.Type == ActivityObjective {
		return "", &Error{Code: CodeAssessmentDispositionConflict, Reason: "objective_assessment_is_deterministic"}
	}
	replacement := artifact
	replacement.Items = append([]AssessmentItem(nil), items...)
	replacement.RubricComplete = true
	replacement.Confidence = 1000
	replacement.RiskFlags = nil
	acceptance, err := EvaluateAssessment(activity, attempt, replacement)
	if err != nil || acceptance.Disposition != DispositionAccepted || acceptance.Outcome == "" {
		if err != nil {
			return "", err
		}
		return "", &Error{Code: CodeAssessmentDispositionConflict}
	}
	return acceptance.Outcome, nil
}

func completeReplacement(_ AssessmentArtifact, items []AssessmentItem) error {
	if len(items) == 0 {
		return &Error{Code: CodeAssessmentDispositionConflict}
	}
	seen := map[string]bool{}
	for _, item := range items {
		if item.RubricItemID == "" || seen[item.RubricItemID] || item.Conclusion == ConclusionUnassessed || (item.Conclusion != ConclusionPass && item.Conclusion != ConclusionPartial && item.Conclusion != ConclusionFail) {
			return &Error{Code: CodeAssessmentDispositionConflict}
		}
		seen[item.RubricItemID] = true
	}
	return nil
}
