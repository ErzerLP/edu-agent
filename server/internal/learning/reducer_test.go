package learning

import (
	"testing"
	"time"
)

func evidence(id, activity string, at time.Time, outcome Outcome, help HelpLevel) AcceptedEvidence {
	conclusion := ConclusionPartial
	if outcome == OutcomePass {
		conclusion = ConclusionPass
	} else if outcome == OutcomeFail {
		conclusion = ConclusionFail
	}
	return AcceptedEvidence{
		ID: id, ActivityID: activity, NodeRevisionID: "node-1", Kind: EvidencePracticeRecall,
		Outcome: outcome, Help: help, ReceivedAt: at, AcceptancePolicyVersion: AssessmentPolicyVersion,
		ReducerPolicyVersion: MasteryReducerVersion, ReviewPolicyVersion: ReviewPolicyVersion,
		RubricOutcomes: []RubricOutcome{{RubricItemID: "r1", Conclusion: conclusion}},
	}
}

func TestMasteryRetainedBoundariesAndInvalidation(t *testing.T) {
	start := time.Date(2026, 8, 20, 10, 0, 0, 0, time.UTC)
	first := evidence("e1", "a1", start, OutcomePass, HelpNone)
	second := evidence("e2", "a2", start.Add(24*time.Hour), OutcomePass, HelpHint)
	reviewFirst := evidence("review-1", "review-a1", start, OutcomePass, HelpNone)
	reviewFirst.Kind = EvidenceReviewRecall
	reviewSecond := evidence("review-2", "review-a2", start.Add(24*time.Hour), OutcomePass, HelpHint)
	reviewSecond.Kind = EvidenceReviewRecall
	tests := []struct {
		name        string
		values      []AcceptedEvidence
		invalidated map[string]bool
		want        MasteryState
	}{
		{"none", nil, nil, MasteryUnseen},
		{"single", []AcceptedEvidence{first}, nil, MasteryLearning},
		{"before boundary", []AcceptedEvidence{first, evidence("e2", "a2", start.Add(24*time.Hour-time.Nanosecond), OutcomePass, HelpNone)}, nil, MasteryLearning},
		{"same activity", []AcceptedEvidence{first, evidence("e2", "a1", start.Add(24*time.Hour), OutcomePass, HelpNone)}, nil, MasteryLearning},
		{"high help", []AcceptedEvidence{first, evidence("e2", "a2", start.Add(24*time.Hour), OutcomePass, HelpScaffold)}, nil, MasteryLearning},
		{"exact boundary", []AcceptedEvidence{first, second}, nil, MasteryRetained},
		{"review recall exact boundary", []AcceptedEvidence{reviewFirst, reviewSecond}, nil, MasteryRetained},
		{"later failure", []AcceptedEvidence{first, second, evidence("e3", "a3", start.Add(49*time.Hour), OutcomeFail, HelpNone)}, nil, MasteryLearning},
		{"invalidated failure preserves retained", []AcceptedEvidence{
			first, second,
			evidence("invalid-fail", "a3", start.Add(49*time.Hour), OutcomeFail, HelpNone),
		}, map[string]bool{"invalid-fail": true}, MasteryRetained},
		{"single success after failure", []AcceptedEvidence{
			first, second,
			evidence("fail", "a3", start.Add(49*time.Hour), OutcomeFail, HelpNone),
			evidence("relearn-1", "a4", start.Add(50*time.Hour), OutcomePass, HelpNone),
		}, nil, MasteryLearning},
		{"retained after relearning", []AcceptedEvidence{
			first, second,
			evidence("fail", "a3", start.Add(49*time.Hour), OutcomeFail, HelpNone),
			evidence("relearn-1", "a4", start.Add(50*time.Hour), OutcomePass, HelpNone),
			evidence("relearn-2", "a5", start.Add(74*time.Hour), OutcomePass, HelpHint),
		}, nil, MasteryRetained},
		{"relearning requires different activities", []AcceptedEvidence{
			first, second,
			evidence("fail", "a3", start.Add(49*time.Hour), OutcomePartial, HelpNone),
			evidence("relearn-1", "a4", start.Add(50*time.Hour), OutcomePass, HelpNone),
			evidence("relearn-2", "a4", start.Add(74*time.Hour), OutcomePass, HelpNone),
		}, nil, MasteryLearning},
		{"interleaved activities can requalify", []AcceptedEvidence{
			first, second,
			evidence("fail", "a3", start.Add(49*time.Hour), OutcomeFail, HelpNone),
			evidence("relearn-a", "a4", start.Add(50*time.Hour), OutcomePass, HelpNone),
			evidence("relearn-b-early", "a5", start.Add(51*time.Hour), OutcomePass, HelpNone),
			evidence("relearn-a-late", "a4", start.Add(75*time.Hour), OutcomePass, HelpNone),
		}, nil, MasteryRetained},
		{"failure after relearning downgrades again", []AcceptedEvidence{
			first, second,
			evidence("fail", "a3", start.Add(49*time.Hour), OutcomeFail, HelpNone),
			evidence("relearn-1", "a4", start.Add(50*time.Hour), OutcomePass, HelpNone),
			evidence("relearn-2", "a5", start.Add(74*time.Hour), OutcomePass, HelpNone),
			evidence("fail-again", "a6", start.Add(75*time.Hour), OutcomePartial, HelpNone),
		}, nil, MasteryLearning},
		{"invalidated relearning success", []AcceptedEvidence{
			first, second,
			evidence("fail", "a3", start.Add(49*time.Hour), OutcomeFail, HelpNone),
			evidence("relearn-1", "a4", start.Add(50*time.Hour), OutcomePass, HelpNone),
			evidence("relearn-2", "a5", start.Add(74*time.Hour), OutcomePass, HelpNone),
		}, map[string]bool{"relearn-2": true}, MasteryLearning},
		{"invalidated second", []AcceptedEvidence{first, second}, map[string]bool{"e2": true}, MasteryLearning},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := ReduceNode("node-1", test.values, test.invalidated, nil)
			if result.Mastery.BaselineState != test.want || result.Mastery.State != test.want {
				t.Fatalf("mastery=%+v, want %s", result.Mastery, test.want)
			}
		})
	}
}

func TestProvisionalIsOverlayOnly(t *testing.T) {
	start := time.Date(2026, 8, 20, 10, 0, 0, 0, time.UTC)
	values := []AcceptedEvidence{
		evidence("e1", "a1", start, OutcomePass, HelpNone),
		evidence("e2", "a2", start.Add(24*time.Hour), OutcomePass, HelpNone),
	}
	baseline := ReduceNode("node-1", values, nil, nil)
	overlay := ReduceNode("node-1", values, nil, []PendingAssessment{{AssessmentID: "assessment-1", NodeRevisionID: "node-1", Reasons: []string{"low_confidence"}}})
	if baseline.Mastery.State != MasteryRetained || overlay.Mastery.State != MasteryProvisional || overlay.Mastery.BaselineState != MasteryRetained {
		t.Fatalf("baseline=%+v overlay=%+v", baseline.Mastery, overlay.Mastery)
	}
	if overlay.Review == nil || baseline.Review == nil || overlay.Review.Step != baseline.Review.Step || !overlay.Review.DueAt.Equal(baseline.Review.DueAt) {
		t.Fatalf("provisional changed review: baseline=%+v overlay=%+v", baseline.Review, overlay.Review)
	}
}

func TestFixedIntervalReviewLadder(t *testing.T) {
	start := time.Date(2026, 8, 20, 10, 0, 0, 0, time.UTC)
	values := []AcceptedEvidence{evidence("e0", "a0", start, OutcomePass, HelpNone)}
	result := ReduceNode("node-1", values, nil, nil)
	if result.Review == nil || result.Review.Step != 0 || !result.Review.DueAt.Equal(start.Add(24*time.Hour)) {
		t.Fatalf("first review=%+v", result.Review)
	}
	// An early pass remains evidence but does not move the schedule.
	values = append(values, evidence("early", "early", start.Add(12*time.Hour), OutcomePass, HelpNone))
	result = ReduceNode("node-1", values, nil, nil)
	if result.Review.Step != 0 || !result.Review.DueAt.Equal(start.Add(24*time.Hour)) {
		t.Fatalf("early pass advanced review=%+v", result.Review)
	}
	at := result.Review.DueAt
	for step := 1; step < len(reviewIntervals); step++ {
		values = append(values, evidence("pass"+string(rune('0'+step)), "activity"+string(rune('0'+step)), at, OutcomePass, HelpHint))
		result = ReduceNode("node-1", values, nil, nil)
		if result.Review.Step != step || !result.Review.DueAt.Equal(at.Add(reviewIntervals[step])) {
			t.Fatalf("step %d review=%+v", step, result.Review)
		}
		at = result.Review.DueAt
	}
	failureAt := at.Add(time.Hour)
	values = append(values, evidence("failure", "failure", failureAt, OutcomePartial, HelpNone))
	result = ReduceNode("node-1", values, nil, nil)
	if result.Review.Step != 0 || !result.Review.DueAt.Equal(failureAt.Add(24*time.Hour)) {
		t.Fatalf("failure did not reset review=%+v", result.Review)
	}
	highHelpAt := result.Review.DueAt
	values = append(values, evidence("help", "help", highHelpAt, OutcomePass, HelpScaffold))
	result = ReduceNode("node-1", values, nil, nil)
	if result.Review.Step != 0 || !result.Review.DueAt.Equal(highHelpAt.Add(24*time.Hour)) {
		t.Fatalf("high-help pass did not reset review=%+v", result.Review)
	}
}

func TestMisconceptionLifecycleIsEvidenceDerived(t *testing.T) {
	start := time.Date(2026, 8, 20, 10, 0, 0, 0, time.UTC)
	failed := evidence("fail-1", "a1", start, OutcomeFail, HelpNone)
	failed.Misconceptions = []MisconceptionCandidate{{RubricItemID: "r1", Text: "  Queue is LIFO "}}
	result := ReduceNode("node-1", []AcceptedEvidence{failed}, nil, nil)
	if len(result.Misconceptions) != 1 || result.Misconceptions[0].Status != MisconceptionProposed {
		t.Fatalf("proposed=%+v", result.Misconceptions)
	}
	failedAgain := evidence("fail-2", "a2", start.Add(time.Hour), OutcomePartial, HelpNone)
	failedAgain.Misconceptions = []MisconceptionCandidate{{RubricItemID: "r1", Text: "queue is lifo"}}
	values := []AcceptedEvidence{failed, failedAgain}
	result = ReduceNode("node-1", values, nil, nil)
	if result.Misconceptions[0].Status != MisconceptionSupported {
		t.Fatalf("supported=%+v", result.Misconceptions)
	}
	unrelated := evidence("unrelated", "a3", start.Add(90*time.Minute), OutcomePass, HelpNone)
	unrelated.RubricOutcomes = []RubricOutcome{{RubricItemID: "r2", Conclusion: ConclusionPass}}
	values = append(values, unrelated)
	result = ReduceNode("node-1", values, nil, nil)
	if result.Misconceptions[0].Status != MisconceptionSupported {
		t.Fatalf("unrelated rubric counter changed hypothesis=%+v", result.Misconceptions)
	}
	values = append(values, evidence("counter-1", "a3", start.Add(2*time.Hour), OutcomePass, HelpNone))
	result = ReduceNode("node-1", values, nil, nil)
	if result.Misconceptions[0].Status != MisconceptionChallenged {
		t.Fatalf("challenged=%+v", result.Misconceptions)
	}
	values = append(values, evidence("counter-2", "a4", start.Add(26*time.Hour), OutcomePass, HelpHint))
	result = ReduceNode("node-1", values, nil, nil)
	if result.Misconceptions[0].Status != MisconceptionResolved {
		t.Fatalf("resolved=%+v", result.Misconceptions)
	}
	result = ReduceNode("node-1", values, map[string]bool{"fail-2": true}, nil)
	if result.Misconceptions[0].Status == MisconceptionSupported {
		t.Fatalf("invalidated evidence survived recomputation: %+v", result.Misconceptions)
	}
}

func TestEstimatedActiveTimeUsesTrustedReceivedAtAndCapsGaps(t *testing.T) {
	start := time.Date(2026, 8, 20, 10, 0, 0, 0, time.UTC)
	result := EstimateActiveTime("s1", []InteractionSample{
		{EventSequence: 4, SessionID: "s1", ReceivedAt: start.Add(20 * time.Minute), UserInitiated: true},
		{EventSequence: 1, SessionID: "s1", ReceivedAt: start, UserInitiated: true},
		{EventSequence: 2, SessionID: "s1", ReceivedAt: start.Add(2 * time.Minute), UserInitiated: false},
		{EventSequence: 3, SessionID: "s1", ReceivedAt: start.Add(4 * time.Minute), UserInitiated: true},
		{EventSequence: 5, SessionID: "other", ReceivedAt: start.Add(21 * time.Minute), UserInitiated: true},
	})
	if !result.Estimated || result.AlgorithmVersion != ActiveTimePolicyVersion || result.SampleCount != 3 || result.DurationSeconds != int64((4*time.Minute+5*time.Minute)/time.Second) {
		t.Fatalf("estimate=%+v", result)
	}
}
