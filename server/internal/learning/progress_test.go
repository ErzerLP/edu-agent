package learning

import (
	"reflect"
	"testing"
	"time"
)

func TestGoalProgressSeparatesSharedNodesAndPreservesEvidence(t *testing.T) {
	a := GoalRevision{ID: "revision-a", GoalID: "goal-a"}
	b := GoalRevision{ID: "revision-b", GoalID: "goal-b"}
	revisions := map[string]GoalRevision{a.ID: a, b.ID: b}
	p := EmptyProjection("g")
	t0 := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	p.Evidence["a"] = AcceptedEvidence{ID: "a", GoalRevisionID: a.ID, NodeRevisionID: "shared", ActivityID: "activity-a", Kind: EvidencePracticeRecall, Outcome: OutcomePass, Help: HelpNone, ReceivedAt: t0}
	p.Evidence["b"] = AcceptedEvidence{ID: "b", GoalRevisionID: b.ID, NodeRevisionID: "shared", ActivityID: "activity-b", Kind: EvidencePracticeRecall, Outcome: OutcomeFail, Help: HelpNone, ReceivedAt: t0.Add(48 * time.Hour)}
	p.Evidence["void"] = AcceptedEvidence{ID: "void", GoalRevisionID: a.ID, NodeRevisionID: "shared", Kind: EvidencePracticeRecall, ReceivedAt: t0.Add(96 * time.Hour)}
	p.Invalidated["void"] = true
	p.Pending["pending"] = PendingAssessment{AssessmentID: "pending", NodeRevisionID: "shared"}
	p.Routes = []RouteProjection{{Route: RouteRevision{ID: "route", GoalRevisionID: a.ID, Steps: []RouteStep{{ID: "step1", NodeRevisionID: "shared"}, {ID: "step2", NodeRevisionID: "shared"}}}}}
	before, _ := ProjectionFingerprint(p)
	pa := BuildGoalProgress(a, revisions, p, map[string]Activity{"activity-a": {SessionID: "original-a"}}, map[string]string{"pending": b.ID}, map[string]map[string]bool{"route": {"step1": true}})
	pb := BuildGoalProgress(b, revisions, p, nil, map[string]string{"pending": b.ID}, nil)
	if pa.EvidenceCount != 1 || pb.EvidenceCount != 1 || len(pa.Pending) != 0 || len(pb.Pending) != 1 {
		t.Fatalf("目标统计混用：%+v %+v", pa, pb)
	}
	if len(pa.Reviews) != 1 || len(pb.Reviews) != 1 || pa.Reviews[0].TaskID == pb.Reviews[0].TaskID || pa.Reviews[0].SessionID != "original-a" {
		t.Fatalf("共享资料复习身份错误：%+v %+v", pa.Reviews, pb.Reviews)
	}
	if !pa.Reviews[0].DueAt.Equal(t0.Add(24*time.Hour)) || !pb.Reviews[0].DueAt.Equal(t0.Add(72*time.Hour)) {
		t.Fatal("不同目标互相改变到期时间")
	}
	if pa.Routes[0].Percent == nil || *pa.Routes[0].Percent != 50 || len(pb.Routes) != 0 {
		t.Fatal("路线分母或完成依据错误")
	}
	a.Management = &GoalManagement{Status: "completed", Completion: &GoalCompletion{Kind: "manual"}}
	completed := BuildGoalProgress(a, revisions, p, nil, nil, nil)
	if completed.Routes[0].Numerator != 0 {
		t.Fatal("手动完成错误继承路线完成")
	}
	after, _ := ProjectionFingerprint(p)
	if before != after {
		t.Fatal("查询生成了新学习事实")
	}
	replayed := BuildGoalProgress(b, revisions, p, nil, map[string]string{"pending": b.ID}, nil)
	if !reflect.DeepEqual(pb, replayed) {
		t.Fatal("相同权威输入结果不确定")
	}
}

func TestProgressExplainsRouteVersionsWithoutInventingMastery(t *testing.T) {
	goal := GoalRevision{ID: "goal-v1", GoalID: "goal"}
	first := RouteRevision{ID: "route-v1", RouteID: "route", Revision: 1, GoalRevisionID: goal.ID, Steps: []RouteStep{{ID: "one", NodeRevisionID: "node-v1", TeachingIntent: "回忆"}}}
	second := first
	second.ID, second.Revision = "route-v2", 2
	second.Steps = []RouteStep{{ID: "one-new-id", NodeRevisionID: "node-v1", TeachingIntent: "回忆"}, {ID: "two", NodeRevisionID: "node-v2", TeachingIntent: "运用"}}
	p := Projection{Routes: []RouteProjection{{Route: second}, {Route: first}}}
	item := BuildGoalProgress(goal, map[string]GoalRevision{goal.ID: goal}, p, nil, nil, map[string]map[string]bool{first.ID: {"one": true}})
	if len(item.Routes) != 2 || item.Routes[1].PreviousRevisionID != first.ID || !reflect.DeepEqual(item.Routes[1].AddedSteps, []string{"two"}) || len(item.Routes[1].RemovedSteps) != 0 {
		t.Fatalf("动态版本依据错误：%+v", item.Routes)
	}
	if item.Routes[0].Numerator != 1 || item.Routes[1].Numerator != 0 || item.Routes[1].Denominator != 2 || item.EvidenceCount != 0 {
		t.Fatalf("将旧版完成或动态分母误计为掌握：%+v", item)
	}
	for _, node := range item.Nodes {
		if node.Mastery.State != MasteryUnseen {
			t.Fatal("新节点凭空掌握")
		}
	}
	unknown := BuildGoalProgress(goal, map[string]GoalRevision{goal.ID: goal}, Projection{}, nil, nil, nil)
	if unknown.Goal.SpaceID == "" || unknown.Goal.Management == nil || unknown.Recent == nil {
		t.Fatal("旧目标缺少明确归属或合法空列表")
	}
	if len(unknown.Routes) != 0 || unknown.EvidenceCount != 0 {
		t.Fatal("空目标不应产生百分比")
	}
}
