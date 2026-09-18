package postgresstore_test

import (
	"testing"

	"github.com/edu-agent/edu-agent/server/internal/knowledge"
	"github.com/google/uuid"
)

func TestPostgreSQLIssue30ExplicitMergeSplitAndReject(t *testing.T) {
	ctx, pool, _, svc := newReviewerPostgresHarness(t)
	if _, err := pool.Exec(ctx, `INSERT INTO devices(id,display_name,created_at) VALUES($1,'合并拆分验收',now())`, integrationActorID); err != nil {
		t.Fatal(err)
	}
	makeNode := func(id, name string) knowledge.StructureEdit {
		return knowledge.StructureEdit{ConceptID: id, Name: name, Content: knowledge.ConceptContent{SourceStatus: "candidate"}}
	}
	create := func(kind string, edits ...knowledge.StructureEdit) knowledge.StructureProposal {
		t.Helper()
		page, e := svc.ReadStructure(ctx, knowledge.StructureQuery{})
		if e != nil {
			t.Fatal(e)
		}
		p, e := svc.CreateStructureProposal(ctx, knowledge.StructureCommand{OperationID: uuid.NewString(), BaseVersion: page.Version, Generation: page.Generation, Kind: kind, Reason: "人工明确审阅语义映射", ActorDeviceID: integrationActorID, Edits: edits})
		if e != nil {
			t.Fatal(e)
		}
		return p
	}
	decide := func(p knowledge.StructureProposal, decision string) knowledge.StructureProposal {
		t.Helper()
		v, e := svc.DecideStructureProposal(ctx, p.ID, knowledge.StructureDecision{OperationID: uuid.NewString(), Hash: p.Hash, Decision: decision, Reason: "明确审阅", ActorDeviceID: integrationActorID})
		if e != nil {
			t.Fatal(e)
		}
		return v
	}
	a, b, target := uuid.NewString(), uuid.NewString(), uuid.NewString()
	oldA, oldB := makeNode(a, "相同名称"), makeNode(b, "相同名称")
	initial := decide(create("edit", oldA, oldB), "approve")
	oldA.Content.SourceStatus = "superseded"
	oldA.Content.ReplacedBy = []string{target}
	oldB.Content = oldA.Content
	merged := create("merge", oldA, oldB, makeNode(target, "新的合并身份"))
	if n, e := svc.ReadConcept(ctx, a, ""); e != nil || n.Content.SourceStatus != "candidate" {
		t.Fatal("未批准提前合并", e)
	}
	merged = decide(merged, "approve")
	if merged.Status != "applied" {
		t.Fatal("合并未应用")
	}
	if n, e := svc.ReadConcept(ctx, a, ""); e != nil || len(n.Content.ReplacedBy) != 1 || n.Content.ReplacedBy[0] != target {
		t.Fatal("旧身份缺少显式映射", e)
	}
	if n, e := svc.ReadConcept(ctx, a, initial.After[0].RevisionID); e != nil || n.Content.SourceStatus != "candidate" {
		t.Fatal("合并修改了旧引用", e)
	}
	x, y := uuid.NewString(), uuid.NewString()
	oldTarget := makeNode(target, "新的合并身份")
	oldTarget.Content.SourceStatus = "superseded"
	oldTarget.Content.ReplacedBy = []string{x, y}
	split := decide(create("split", oldTarget, makeNode(x, "拆分一"), makeNode(y, "拆分二")), "approve")
	if split.Status != "applied" {
		t.Fatal("拆分未应用")
	}
	rejected := decide(create("edit", makeNode(x, "不应生效的改名")), "reject")
	if rejected.Status != "rejected" {
		t.Fatal("拒绝未记录")
	}
	if n, e := svc.ReadConcept(ctx, x, ""); e != nil || n.Name != "拆分一" {
		t.Fatal("拒绝仍修改概念", e)
	}
	page, e := svc.ReadStructure(ctx, knowledge.StructureQuery{})
	if e != nil {
		t.Fatal(e)
	}
	bad := makeNode(uuid.NewString(), "越权关系")
	bad.Content.Relations = []knowledge.ConceptRelation{{TargetID: uuid.NewString(), Kind: "related"}}
	if _, e = svc.CreateStructureProposal(ctx, knowledge.StructureCommand{OperationID: uuid.NewString(), BaseVersion: page.Version, Generation: page.Generation, Kind: "edit", Reason: "伪造目标", ActorDeviceID: integrationActorID, Edits: []knowledge.StructureEdit{bad}}); knowledge.ErrorCode(e) != knowledge.CodeNotFound {
		t.Fatal("接受不存在的关系目标", e)
	}
}
