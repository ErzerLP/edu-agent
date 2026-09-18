package learning

import (
	"context"
	"reflect"
	"testing"

	"github.com/edu-agent/edu-agent/server/internal/tutoring"
)

func TestHistoricalAssessmentDecisionPreservesCurrentFocus(t *testing.T) {
	a, attempt, assessment := assessmentFixture()
	a.SessionID = "session-1"
	attempt.SessionID = a.SessionID
	assessment.SessionID = a.SessionID
	current := assessmentFeedbackSession(a, attempt, 9)
	current.State = tutoring.StateRouteActive
	current.Context.ActivityID = nil
	current.Context.AttemptID = nil
	store := &proposalTestStore{session: current, activity: a, attempt: attempt, assessment: assessment,
		decision: AssessmentDecision{ID: "original", AssessmentID: assessment.ID, Version: 1, Disposition: DispositionAccepted}}
	s := newProposalTestService(t, store, &proposalTestRepository{}, nil)
	_, err := s.Decide(context.Background(), "75900000-0000-4000-8000-000000000099", assessment.ID, AssessmentDecisionCommand{
		Operation: coordinatorOperation("75900000-0000-4000-8000-000000000011", current.ID, 9),
		Kind:      "void", Reason: "复核发现原结论有误", ExpectedDispositionVersion: 1,
	})
	if err != nil {
		t.Fatalf("已离开反馈的原评估应可追加作废：%v", err)
	}
	if !reflect.DeepEqual(store.lastCommit.Batch.Session.Context, current.Context) || store.lastCommit.Batch.Session.State != current.State {
		t.Fatal("历史处置改变了当前焦点")
	}
}

func TestFeedbackDecisionsKeepExistingEvidencePolicy(t *testing.T) {
	for _, scenario := range []struct {
		name   string
		change func(*Activity, *Attempt, *AssessmentArtifact, *AssessmentDecision)
		want   []string
	}{
		{"低确定性允许人工核对", func(_ *Activity, _ *Attempt, a *AssessmentArtifact, _ *AssessmentDecision) { a.Confidence = 700 }, []string{"confirm", "override", "void"}},
		{"来源支持不足不可直接接纳", func(_ *Activity, _ *Attempt, a *AssessmentArtifact, _ *AssessmentDecision) {
			a.RiskFlags = []RiskFlag{RiskInsufficientKnowledgeSupport}
		}, []string{"override", "void"}},
		{"证据冲突保留既有人工复核政策", func(_ *Activity, _ *Attempt, a *AssessmentArtifact, _ *AssessmentDecision) {
			a.RiskFlags = []RiskFlag{RiskConflictingEvidence}
		}, []string{"confirm", "override", "void"}},
		{"答案揭示不可制造证据", func(_ *Activity, a *Attempt, _ *AssessmentArtifact, _ *AssessmentDecision) {
			a.Help = HelpAnswerRevealed
		}, []string{"void"}},
		{"不具证据资格", func(_ *Activity, a *Attempt, _ *AssessmentArtifact, _ *AssessmentDecision) {
			a.EvidenceIneligibleReason = "不满足原合同"
		}, []string{"void"}},
		{"已作废不可再次处置", func(_ *Activity, _ *Attempt, _ *AssessmentArtifact, d *AssessmentDecision) {
			d.Disposition = DispositionVoided
		}, []string{}},
		{"客观字符串规则不可人工覆盖", func(a *Activity, _ *Attempt, _ *AssessmentArtifact, _ *AssessmentDecision) {
			a.Type = ActivityObjective
		}, []string{"void"}},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			a, attempt, assessment := assessmentFixture()
			attempt.EvidenceEligibility = true
			decision := AssessmentDecision{Disposition: DispositionProvisional}
			scenario.change(&a, &attempt, &assessment, &decision)
			if got := FeedbackDecisions(a, attempt, assessment, decision); !reflect.DeepEqual(got, scenario.want) {
				t.Fatalf("操作提示放宽或隐藏真实政策：got=%v want=%v", got, scenario.want)
			}
		})
	}
}
