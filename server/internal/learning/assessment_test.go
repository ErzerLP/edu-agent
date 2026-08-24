package learning

import (
	"strings"
	"testing"
	"time"
)

func assessmentFixture() (Activity, Attempt, AssessmentArtifact) {
	answer := "A queue is FIFO because insertion and removal occur at opposite ends."
	knowledgeText := "A queue follows first-in, first-out order."
	answerQuote := "FIFO"
	knowledgeQuote := "first-in, first-out"
	activity := Activity{
		ID: "activity-1", Revision: 1, SessionID: "session-1", Type: ActivityOpen,
		KnowledgeRevisionID: "knowledge-1", TargetNodeID: "node-1", TargetNodeRevisionID: "node-rev-1", AssessmentPolicyVersion: AssessmentPolicyVersion,
		Rubric:     Rubric{Revision: "rubric-1", Items: []RubricItem{{ID: "item-1", Criterion: "identifies FIFO", RequiredReferenceIDs: []string{"node-rev-1"}}}},
		References: []KnowledgeReference{{KnowledgeRevisionID: "knowledge-1", NodeID: "node-1", NodeRevisionID: "node-rev-1", DocumentRevisionID: "document-rev-1", Slice: knowledgeText, SliceSHA256: SHA256([]byte(knowledgeText))}},
	}
	attempt := Attempt{ID: "attempt-1", ActivityID: activity.ID, ActivityRevision: 1, Answer: answer, AnswerSHA256: SHA256([]byte(answer)), Help: HelpNone, ReceivedAt: time.Date(2026, 8, 20, 10, 0, 0, 0, time.UTC)}
	artifact := AssessmentArtifact{
		ID: "assessment-1", AttemptID: attempt.ID, ActivityID: activity.ID, ActivityRevision: 1,
		RubricComplete: true, Confidence: 850,
		Items: []AssessmentItem{{
			RubricItemID: "item-1", Conclusion: ConclusionPass,
			AnswerQuote: answerQuote, AnswerRange: SourceRange{Start: strings.Index(answer, answerQuote), End: strings.Index(answer, answerQuote) + len(answerQuote)}, AnswerQuoteSHA256: SHA256([]byte(answerQuote)),
			KnowledgeReferenceID: "node-rev-1", KnowledgeQuote: knowledgeQuote,
			KnowledgeRange: SourceRange{Start: strings.Index(knowledgeText, knowledgeQuote), End: strings.Index(knowledgeText, knowledgeQuote) + len(knowledgeQuote)}, KnowledgeQuoteSHA256: SHA256([]byte(knowledgeQuote)),
		}},
	}
	return activity, attempt, artifact
}

func TestOpenAssessmentAcceptanceMatrix(t *testing.T) {
	activity, attempt, valid := assessmentFixture()
	tests := []struct {
		name        string
		mutate      func(*AssessmentArtifact)
		disposition Disposition
		hard        bool
	}{
		{"accepted threshold", func(*AssessmentArtifact) {}, DispositionAccepted, false},
		{"low confidence", func(value *AssessmentArtifact) { value.Confidence = 849 }, DispositionProvisional, false},
		{"incomplete rubric", func(value *AssessmentArtifact) { value.RubricComplete = false }, DispositionProvisional, false},
		{"unassessed", func(value *AssessmentArtifact) { value.Items[0].Conclusion = ConclusionUnassessed }, DispositionProvisional, false},
		{"missing answer support", func(value *AssessmentArtifact) {
			value.Items[0].AnswerRange = SourceRange{}
			value.Items[0].AnswerQuote = ""
			value.Items[0].AnswerQuoteSHA256 = ""
		}, DispositionProvisional, false},
		{"missing knowledge support", func(value *AssessmentArtifact) {
			value.Items[0].KnowledgeReferenceID = ""
			value.Items[0].KnowledgeRange = SourceRange{}
			value.Items[0].KnowledgeQuote = ""
			value.Items[0].KnowledgeQuoteSHA256 = ""
		}, DispositionProvisional, false},
		{"risk", func(value *AssessmentArtifact) { value.RiskFlags = []RiskFlag{RiskUnsafeContent} }, DispositionProvisional, false},
		{"unknown risk", func(value *AssessmentArtifact) { value.RiskFlags = []RiskFlag{"invented"} }, "", true},
		{"duplicate item", func(value *AssessmentArtifact) { value.Items = append(value.Items, value.Items[0]) }, "", true},
		{"unknown item", func(value *AssessmentArtifact) { value.Items[0].RubricItemID = "unknown" }, "", true},
		{"answer range", func(value *AssessmentArtifact) { value.Items[0].AnswerRange.End = len(attempt.Answer) + 1 }, "", true},
		{"answer hash", func(value *AssessmentArtifact) { value.Items[0].AnswerQuoteSHA256 = strings.Repeat("0", 64) }, "", true},
		{"knowledge reference", func(value *AssessmentArtifact) { value.Items[0].KnowledgeReferenceID = "cross-revision" }, "", true},
		{"knowledge range", func(value *AssessmentArtifact) { value.Items[0].KnowledgeRange.Start = -1 }, "", true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			artifact := valid
			artifact.Items = append([]AssessmentItem(nil), valid.Items...)
			test.mutate(&artifact)
			result, err := EvaluateAssessment(activity, attempt, artifact)
			if test.hard {
				if ErrorCode(err) != CodeProposalRejected {
					t.Fatalf("error=%v, want proposal_rejected", err)
				}
				return
			}
			if err != nil || result.Disposition != test.disposition {
				t.Fatalf("result=%+v error=%v", result, err)
			}
		})
	}
}

func TestOpenAssessmentReferenceMembershipAndOptionalRubricSupport(t *testing.T) {
	activity, attempt, artifact := assessmentFixture()
	other := activity.References[0]
	other.NodeRevisionID = "node-rev-2"
	activity.References = append(activity.References, other)
	activity.Rubric.Items[0].RequiredReferenceIDs = []string{"node-rev-2"}
	if _, err := EvaluateAssessment(activity, attempt, artifact); ErrorCode(err) != CodeProposalRejected {
		t.Fatalf("cross-item required reference error=%v", err)
	}

	activity, attempt, artifact = assessmentFixture()
	activity.Rubric.Items[0].RequiredReferenceIDs = nil
	accepted, err := EvaluateAssessment(activity, attempt, artifact)
	if err != nil || accepted.Disposition != DispositionAccepted {
		t.Fatalf("verified optional reference result=%+v err=%v", accepted, err)
	}
	artifact.Items[0].KnowledgeReferenceID = ""
	artifact.Items[0].KnowledgeRange = SourceRange{}
	artifact.Items[0].KnowledgeQuote = ""
	artifact.Items[0].KnowledgeQuoteSHA256 = ""
	provisional, err := EvaluateAssessment(activity, attempt, artifact)
	if err != nil || provisional.Disposition != DispositionProvisional {
		t.Fatalf("unverified optional knowledge result=%+v err=%v", provisional, err)
	}
}

func TestConfirmRequiresFullyScorableArtifactAndSafeRisk(t *testing.T) {
	activity, attempt, artifact := assessmentFixture()
	artifact.Confidence = 849
	if !ConfirmableAssessment(activity, attempt, artifact) {
		t.Fatal("low-confidence fully supported artifact should be confirmable")
	}
	artifact.Items[0].Conclusion = ConclusionUnassessed
	if ConfirmableAssessment(activity, attempt, artifact) {
		t.Fatal("all-unassessed artifact was confirmable")
	}
	activity, attempt, artifact = assessmentFixture()
	artifact.Confidence = 849
	artifact.RubricComplete = false
	if ConfirmableAssessment(activity, attempt, artifact) {
		t.Fatal("incomplete artifact was confirmable")
	}
	activity, attempt, artifact = assessmentFixture()
	artifact.Confidence = 849
	artifact.Items[0].KnowledgeReferenceID = ""
	artifact.Items[0].KnowledgeRange = SourceRange{}
	artifact.Items[0].KnowledgeQuote = ""
	artifact.Items[0].KnowledgeQuoteSHA256 = ""
	if ConfirmableAssessment(activity, attempt, artifact) {
		t.Fatal("artifact without knowledge support was confirmable")
	}
	_, attempt, artifact = assessmentFixture()
	activity, _, _ = assessmentFixture()
	artifact.Confidence = 849
	attempt.Help = HelpAnswerRevealed
	if ConfirmableAssessment(activity, attempt, artifact) {
		t.Fatal("answer-revealed artifact was confirmable")
	}
	if got := combineOutcomes(nil); got != "" {
		t.Fatalf("empty outcomes=%q, want empty", got)
	}
}

func TestEveryKnownRiskProducesProvisional(t *testing.T) {
	activity, attempt, artifact := assessmentFixture()
	for _, risk := range []RiskFlag{
		RiskIncompleteRubric, RiskInsufficientAnswerEvidence, RiskInsufficientKnowledgeSupport,
		RiskConflictingEvidence, RiskAmbiguousRubric, RiskUnsafeContent, RiskSchemaRepaired,
		RiskStaleContext, RiskRetryExhausted,
	} {
		t.Run(string(risk), func(t *testing.T) {
			value := artifact
			value.RiskFlags = []RiskFlag{risk}
			result, err := EvaluateAssessment(activity, attempt, value)
			if err != nil || result.Disposition != DispositionProvisional {
				t.Fatalf("result=%+v error=%v", result, err)
			}
		})
	}
}

func TestObjectiveAssessmentUsesFrozenRule(t *testing.T) {
	activity := Activity{ID: "activity-1", Revision: 1, Type: ActivityObjective, KnowledgeRevisionID: "knowledge-1", TargetNodeID: "node-1", TargetNodeRevisionID: "node-rev-1", References: []KnowledgeReference{{KnowledgeRevisionID: "knowledge-1", NodeID: "node-1", NodeRevisionID: "node-rev-1"}}, Rubric: Rubric{ObjectiveRule: &ObjectiveRule{AcceptedAnswers: []string{"Paris"}, TrimSpace: true}}}
	attempt := Attempt{ID: "attempt-1", ActivityID: activity.ID, ActivityRevision: 1, Answer: " Paris ", Help: HelpNone}
	artifact := AssessmentArtifact{AttemptID: attempt.ID, ActivityID: activity.ID, ActivityRevision: 1}
	result, err := EvaluateAssessment(activity, attempt, artifact)
	if err != nil || result.Disposition != DispositionAccepted || result.Outcome != OutcomePass {
		t.Fatalf("result=%+v error=%v", result, err)
	}
	attempt.Answer = "London"
	result, err = EvaluateAssessment(activity, attempt, artifact)
	if err != nil || result.Outcome != OutcomeFail {
		t.Fatalf("wrong answer result=%+v error=%v", result, err)
	}
	attempt.Help = HelpAnswerRevealed
	result, err = EvaluateAssessment(activity, attempt, artifact)
	if err != nil || result.Disposition != DispositionProvisional {
		t.Fatalf("revealed answer result=%+v error=%v", result, err)
	}
}

func TestAssessmentDispositionAppendOnlyEffects(t *testing.T) {
	_, _, artifact := assessmentFixture()
	evidenceID := "evidence-1"
	accepted := AssessmentDecision{ID: "decision-1", AssessmentID: artifact.ID, Version: 2, Disposition: DispositionAccepted, ProducedEvidenceID: &evidenceID}
	override, err := DecideAssessment(accepted, artifact, DecisionCommand{Kind: "override", ExpectedVersion: 2, Reason: "manual correction", Items: artifact.Items})
	if err != nil || !override.InvalidateEvidence || !override.CreateEvidence || override.Disposition != DispositionOverridden {
		t.Fatalf("override=%+v error=%v", override, err)
	}
	voided, err := DecideAssessment(accepted, artifact, DecisionCommand{Kind: "void", ExpectedVersion: 2})
	if err != nil || !voided.InvalidateEvidence || voided.CreateEvidence || voided.Disposition != DispositionVoided {
		t.Fatalf("void=%+v error=%v", voided, err)
	}
	provisional := AssessmentDecision{Version: 1, Disposition: DispositionProvisional}
	confirmed, err := DecideAssessment(provisional, artifact, DecisionCommand{Kind: "confirm", ExpectedVersion: 1}, true)
	if err != nil || confirmed.Disposition != DispositionAccepted || !confirmed.CreateEvidence {
		t.Fatalf("confirm=%+v error=%v", confirmed, err)
	}
}

func TestAssessmentDispositionRejectsStaleAndIncompleteOverride(t *testing.T) {
	_, _, artifact := assessmentFixture()
	current := AssessmentDecision{Version: 3, Disposition: DispositionProvisional}
	if _, err := DecideAssessment(current, artifact, DecisionCommand{Kind: "confirm", ExpectedVersion: 2}); ErrorCode(err) != CodeAssessmentDispositionConflict || err.(*Error).CurrentDisposition != string(current.Disposition) {
		t.Fatalf("stale decision error=%v", err)
	}
	if _, err := DecideAssessment(current, artifact, DecisionCommand{Kind: "override", ExpectedVersion: 3, Reason: "reason"}); ErrorCode(err) != CodeAssessmentDispositionConflict || err.(*Error).CurrentDisposition != string(current.Disposition) {
		t.Fatalf("incomplete override error=%v", err)
	}
	invalid := append([]AssessmentItem(nil), artifact.Items...)
	invalid[0].Conclusion = ConclusionUnassessed
	if _, err := DecideAssessment(current, artifact, DecisionCommand{Kind: "override", ExpectedVersion: 3, Reason: "reason", Items: invalid}); ErrorCode(err) != CodeAssessmentDispositionConflict || err.(*Error).CurrentDisposition != string(current.Disposition) {
		t.Fatalf("invalid replacement error=%v", err)
	}
}
