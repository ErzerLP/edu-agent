package learning

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

type planningTestModel struct {
	raw   json.RawMessage
	err   error
	calls int
}

func (m *planningTestModel) Generate(context.Context, ProposalRequest) (json.RawMessage, error) {
	m.calls++
	return m.raw, m.err
}
func TestPlanningModelFailuresKeepManualContent(t *testing.T) {
	base := PlanningContent{Details: GoalDetails{Name: "并发", Priority: "normal", SelfAssessment: "已学语法"}, Steps: []PlanningStep{}, Questions: []PlanningQuestion{}, Gaps: []string{}}
	for _, raw := range []string{`{"mastery":"retained"}`, `not json`, `{"details":{"name":"并发"},"steps":[{"name":"步骤","content":"内容","completion":"说明","minutes":20,"node_revision_id":"10000000-0000-4000-8000-000000000001"}]}`} {
		m := &planningTestModel{raw: json.RawMessage(raw)}
		s := &Service{model: m}
		d := PlanningDraft{Content: base}
		s.generatePlanning(context.Background(), &d)
		if d.LastError == "" || d.Suggestion != nil || d.Content.Details.SelfAssessment != "已学语法" || m.calls != 1 {
			t.Fatalf("错误未保留：%+v", d)
		}
	}
	d := PlanningDraft{Content: base}
	(&Service{}).generatePlanning(context.Background(), &d)
	if !strings.Contains(d.LastError, "未配置") {
		t.Fatal("没有手动降级提示")
	}
	suggestion := base
	suggestion.Questions = []PlanningQuestion{{Field: "self_assessment", Question: "你的基础？"}, {Field: "purpose", Question: "用途？"}}
	raw, _ := json.Marshal(suggestion)
	d = PlanningDraft{Content: base}
	s := &Service{model: &planningTestModel{raw: raw}}
	s.generatePlanning(context.Background(), &d)
	if d.Suggestion == nil || len(d.Suggestion.Questions) != 1 || d.Suggestion.Questions[0].Field != "purpose" {
		t.Fatalf("重复询问已知信息：%+v", d)
	}
}
func TestPlanningRejectsInvalidPrerequisites(t *testing.T) {
	c := PlanningContent{Details: GoalDetails{Name: "计划"}, Steps: []PlanningStep{{Name: "A", Content: "内容", Completion: "独立解释", Minutes: 20, Prerequisites: []int{0}}}}
	if ValidatePlanningContent(c) == nil {
		t.Fatal("自引用未拒绝")
	}
	c.Steps[0].Prerequisites = nil
	if err := ValidatePlanningContent(c); err != nil {
		t.Fatal(err)
	}
	if validatePlanningReferences(c, nil, true) == nil {
		t.Fatal("无引用大纲可执行")
	}
	if err := validatePlanningReferences(c, nil, false); err != nil {
		t.Fatal("无法保存大纲")
	}
}

type planningTimeout struct{}

func (planningTimeout) Error() string         { return "模拟超时" }
func (planningTimeout) ModelCategory() string { return "timeout" }
func TestPlanningTimeoutAndCancellationKeepDraft(t *testing.T) {
	for _, cancelled := range []bool{false, true} {
		ctx, cancel := context.WithCancel(context.Background())
		if cancelled {
			cancel()
		}
		m := &planningTestModel{err: planningTimeout{}}
		d := PlanningDraft{Content: PlanningContent{Details: GoalDetails{Name: "手动草稿"}}}
		(&Service{model: m}).generatePlanning(ctx, &d)
		cancel()
		want := 2
		if cancelled {
			want = 1
		}
		if m.calls != want || d.LastError == "" || d.Content.Details.Name != "手动草稿" {
			t.Fatal("超时或取消破坏已有草稿")
		}
	}
}
