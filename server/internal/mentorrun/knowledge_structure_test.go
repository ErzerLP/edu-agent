package mentorrun

import (
	"context"
	"reflect"
	"testing"

	"github.com/edu-agent/edu-agent/server/internal/knowledge"
	"github.com/edu-agent/edu-agent/server/internal/learningchange"
	"github.com/edu-agent/edu-agent/server/internal/tutoring"
	"github.com/google/uuid"
)

func TestPostgreSQLIssue30MaintenanceUsesAdaptiveBoundary(t *testing.T) {
	f := adaptiveFixture(t)
	ctx := context.Background()
	owner := f.service.starter.Knowledge
	owner.SetStructureLearningReader(f.service.starter.Learning)
	svc, err := knowledge.NewService(owner, knowledge.NewCanonicalizer(), knowledge.ServiceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	f.act(t, tutoring.ActionPresentActivity, "")
	before := f.context(t)
	page, err := svc.ReadStructure(ctx, knowledge.StructureQuery{GoalID: f.goal})
	if err != nil || len(page.Items) == 0 {
		t.Fatalf("开学概念未出现在结构中: %+v %v", page, err)
	}
	n := page.Items[0]
	n.Content.Description = "审阅后纠正解释的适用条件"
	p, err := svc.CreateStructureProposal(ctx, knowledge.StructureCommand{OperationID: uuid.NewString(), BaseVersion: page.Version, Generation: page.Generation, Kind: "edit", Reason: "用户核对来源后纠正概念", ActorDeviceID: f.actor.Device.ID, Edits: []knowledge.StructureEdit{{ConceptID: n.ConceptID, GoalID: n.GoalID, Name: n.Name + "（已纠正）", Content: n.Content}}})
	if err != nil {
		t.Fatal(err)
	}
	if p.Impact.Activities == 0 || p.Impact.Contexts == 0 || p.Impact.Contents == 0 {
		t.Fatalf("教学影响缺失: %+v", p.Impact)
	}
	applied, err := svc.DecideStructureProposal(ctx, p.ID, knowledge.StructureDecision{OperationID: uuid.NewString(), Hash: p.Hash, Decision: "approve", Reason: "批准纠正", ActorDeviceID: f.actor.Device.ID})
	if err != nil || applied.Status != "applied" {
		t.Fatal("维护未应用", err)
	}
	if current := f.context(t); !reflect.DeepEqual(current.Base, before.Base) || !reflect.DeepEqual(current.Content.Body, before.Content.Body) {
		t.Fatal("知识审批改写了当前题目或正文")
	}
	c := f.propose(t, "route")
	if c.Status != "queued_for_boundary" || c.Candidate.ContextID == before.Base.ContextID {
		t.Fatalf("维护没有经正式候选排队: %+v", c)
	}
	if current := f.context(t); current.Base.ContextID != before.Base.ContextID || current.Base.ActivityID != before.Base.ActivityID {
		t.Fatal("排队提前切换了冻结上下文或活动")
	}
	f.act(t, tutoring.ActionEndActivity, "")
	if _, err = f.changes.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	read, err := f.changes.Read(ctx, f.actor, f.goal, c.ID, 0)
	if err != nil || read.Status != "applied" {
		t.Fatalf("安全边界未接入维护: %+v %v", read, err)
	}
	after := f.context(t)
	if after.Base.ContextID != c.Candidate.ContextID {
		t.Fatal("后续现场未绑定新知识上下文")
	}
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	old, e := owner.ContextTx(ctx, tx, before.Base.ContextID)
	if e != nil {
		t.Fatal(e)
	}
	next, e := owner.ContextTx(ctx, tx, after.Base.ContextID)
	if e != nil {
		t.Fatal(e)
	}
	if old.Concepts[0].Name == next.Concepts[0].Name || old.Concepts[0].RevisionID == next.Concepts[0].RevisionID {
		t.Fatal("新旧知识修订未分离")
	}
	var evidence int
	if e = tx.QueryRow(ctx, `SELECT count(*) FROM learning_evidence`).Scan(&evidence); e != nil || evidence != 0 {
		t.Fatal("知识维护或教学接入生成了额外 Evidence", e)
	}
	// 保留旧评估语义；候选仍是 learningchange 的正规路线变更。
	if read.Candidate.Kind != "route" || read.Policy != "boundary" {
		t.Fatalf("绕过既有教学 owner: %+v", read)
	}
}

func TestPostgreSQLIssue30NewDependencyMakesMaintenanceStale(t *testing.T) {
	f := adaptiveFixture(t)
	ctx := context.Background()
	owner := f.service.starter.Knowledge
	owner.SetStructureLearningReader(f.service.starter.Learning)
	svc, err := knowledge.NewService(owner, knowledge.NewCanonicalizer(), knowledge.ServiceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	page, err := svc.ReadStructure(ctx, knowledge.StructureQuery{GoalID: f.goal})
	if err != nil || len(page.Items) == 0 {
		t.Fatal("缺少概念", err)
	}
	n := page.Items[0]
	p, err := svc.CreateStructureProposal(ctx, knowledge.StructureCommand{OperationID: uuid.NewString(), BaseVersion: page.Version, Generation: page.Generation, Kind: "edit", Reason: "新增依赖前提出", ActorDeviceID: f.actor.Device.ID, Edits: []knowledge.StructureEdit{{ConceptID: n.ConceptID, GoalID: n.GoalID, Name: "新名称", Content: n.Content}}})
	if err != nil {
		t.Fatal(err)
	}
	// 正规教学变化追加正文版本，使原维护影响失效，但没有新的概念版本。
	before := f.context(t)
	_, err = f.changes.Change(ctx, f.actor, f.goal, f.session, uuid.NewString(), learningchange.Command{OperationID: uuid.NewString(), Action: "propose", Base: &before.Base, Candidate: &learningchange.Candidate{Kind: "explanation", Trigger: "user_request", Reason: "新增教学解释", Explanation: "依赖更新后应重新审阅维护影响"}})
	if err != nil {
		t.Fatal(err)
	}
	latest, err := svc.ReadStructureProposal(ctx, p.ID)
	if err != nil || latest.Impact.Fingerprint == latest.CurrentImpact.Fingerprint {
		t.Fatal("未显示后续内容依赖变化", err)
	}
	result, err := svc.DecideStructureProposal(ctx, p.ID, knowledge.StructureDecision{OperationID: uuid.NewString(), Hash: p.Hash, Decision: "approve", Reason: "旧基础审批", ActorDeviceID: f.actor.Device.ID})
	if err != nil || result.Status != "stale" {
		t.Fatalf("新依赖未使提案失效: %+v %v", result, err)
	}
}
