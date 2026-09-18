package postgresstore_test

import (
	"context"
	"sync"
	"testing"

	"github.com/edu-agent/edu-agent/server/internal/knowledge"
	learningdb "github.com/edu-agent/edu-agent/server/internal/learning/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/learningspace"
	spacedb "github.com/edu-agent/edu-agent/server/internal/learningspace/postgresstore"
	tutoringdb "github.com/edu-agent/edu-agent/server/internal/tutoring/postgresstore"
	"github.com/google/uuid"
)

func TestPostgreSQLIssue30StructureReviewIsolationAndConcurrency(t *testing.T) {
	ctx, pool, store, service := newReviewerPostgresHarness(t)
	if _, err := pool.Exec(ctx, `INSERT INTO devices(id,display_name,created_at) VALUES($1,'知识结构验收',clock_timestamp())`, integrationActorID); err != nil {
		t.Fatal(err)
	}
	store.SetStructureLearningReader(learningdb.New(pool, tutoringdb.New(pool), store))
	base, err := service.Import(ctx, knowledge.ImportCommand{OperationID: uuid.NewString(), ExpectedParentProvided: true, Source: "知识结构来源", ActorDeviceID: integrationActorID, Documents: []knowledge.ImportDocument{{Path: "a.md", Markdown: "# 同名\n来源一的真实主张\n"}, {Path: "b.md", Markdown: "# 同名\n来源二的相反主张\n"}}})
	if err != nil {
		t.Fatal(err)
	}
	sources := []knowledge.ConceptSource{}
	for i, d := range base.Revision.Documents {
		quote := []string{"来源一的真实主张", "来源二的相反主张"}[i]
		sources = append(sources, knowledge.ConceptSource{CollectionID: knowledge.DefaultCollectionID, RevisionID: base.Revision.ID, DocumentID: d.Revision.DocumentID, NodeID: d.Revision.Nodes[0].NodeID, Quote: quote})
	}
	a, b := uuid.NewString(), uuid.NewString()
	content := knowledge.ConceptContent{SourceStatus: "conflict", Sources: sources, Claims: []knowledge.ConceptClaim{{Text: "主张一", Conditions: "常温", Sources: []int{0}}, {Text: "主张二", Conditions: "高温", Sources: []int{1}, Gap: "尚未核对实验条件"}}, Relations: []knowledge.ConceptRelation{{TargetID: b, Kind: "contrast", Suggested: true, Sources: []int{0, 1}}}}
	command := knowledge.StructureCommand{OperationID: uuid.NewString(), Generation: 1, Kind: "edit", Reason: "明确保存同名不同概念", ActorDeviceID: integrationActorID, Edits: []knowledge.StructureEdit{{ConceptID: a, Name: "同名", Content: content}, {ConceptID: b, Name: "同名", Content: knowledge.ConceptContent{SourceStatus: "candidate", Relations: []knowledge.ConceptRelation{{TargetID: a, Kind: "related", Suggested: true}}}}}}
	p, err := service.CreateStructureProposal(ctx, command)
	if err != nil {
		t.Fatal(err)
	}
	page, err := service.ReadStructure(ctx, knowledge.StructureQuery{})
	if err != nil || len(page.Items) != 0 {
		t.Fatalf("未批准已经写入节点: %+v %v", page, err)
	}
	decide := func(p knowledge.StructureProposal) knowledge.StructureProposal {
		t.Helper()
		v, e := service.DecideStructureProposal(ctx, p.ID, knowledge.StructureDecision{OperationID: uuid.NewString(), Hash: p.Hash, Decision: "approve", Reason: "已核对原文和身份", ActorDeviceID: integrationActorID})
		if e != nil {
			t.Fatal(e)
		}
		return v
	}
	p = decide(p)
	if p.Status != "applied" {
		t.Fatalf("审批未应用: %+v", p)
	}
	page, err = service.ReadStructure(ctx, knowledge.StructureQuery{})
	if err != nil || len(page.Items) != 2 || len(page.Edges) != 2 {
		t.Fatalf("同名概念或局部图不完整: %+v %v", page, err)
	}
	for _, n := range page.Items {
		if n.LearningState != "unseen" {
			t.Fatalf("来源采纳被当作掌握: %+v", n)
		}
	}
	if proposals, e := service.ListStructureProposals(ctx, "", 20); e != nil || len(proposals.Items) != 1 {
		t.Fatalf("授权提案列表不完整: %+v %v", proposals, e)
	}
	badContent := content
	badContent.Sources = append([]knowledge.ConceptSource{}, content.Sources...)
	badContent.Sources[0].Quote = "AI 凭空编造的原文"
	if _, e := service.CreateStructureProposal(ctx, knowledge.StructureCommand{OperationID: uuid.NewString(), Generation: 1, BaseVersion: page.Version, Kind: "edit", Reason: "核对伪造片段", ActorDeviceID: integrationActorID, Edits: []knowledge.StructureEdit{{ConceptID: a, Name: "同名", Content: badContent}}}); knowledge.ErrorCode(e) != knowledge.CodeInvalidRequest {
		t.Fatalf("伪造出处被接受: %v", e)
	}
	first, err := service.ReadStructure(ctx, knowledge.StructureQuery{Limit: 1})
	if err != nil || first.NextCursor == "" || !first.Partial || len(first.Edges) != 0 {
		t.Fatalf("分页未裁剪边: %+v %v", first, err)
	}
	second, err := service.ReadStructure(ctx, knowledge.StructureQuery{Limit: 1, Cursor: first.NextCursor})
	if err != nil || len(second.Items) != 1 || second.Items[0].ConceptID == first.Items[0].ConceptID {
		t.Fatal("分页重复或遗漏", err)
	}
	other, err := spacedb.New(pool).Mutate(ctx, integrationActorID, "", learningspace.Command{OperationID: uuid.NewString(), Name: "其他学习区", Status: "active"})
	if err != nil {
		t.Fatal(err)
	}
	otherCtx, err := learningspace.WithScope(ctx, other.ID)
	if err != nil {
		t.Fatal(err)
	}
	if hidden, e := service.ReadStructure(otherCtx, knowledge.StructureQuery{}); e != nil || len(hidden.Items) != 0 {
		t.Fatal("跨区泄露", e)
	}
	if _, e := service.ReadConcept(otherCtx, a, ""); knowledge.ErrorCode(e) != knowledge.CodeNotFound {
		t.Fatalf("跨区详情可读: %v", e)
	}
	if _, e := service.ReadStructureProposal(otherCtx, p.ID); knowledge.ErrorCode(e) != knowledge.CodeNotFound {
		t.Fatalf("跨区提案可读: %v", e)
	}
	replay, e := service.CreateStructureProposal(ctx, command)
	if e != nil || !replay.Replayed || replay.ID != p.ID {
		t.Fatal("重试产生额外提案", e)
	}
	command.Reason = "篡改重试"
	if _, e = service.CreateStructureProposal(ctx, command); knowledge.ErrorCode(e) != knowledge.CodeIdempotencyConflict {
		t.Fatalf("冲突重试未拒绝: %v", e)
	}
	current, e := service.ReadConcept(ctx, a, "")
	if e != nil {
		t.Fatal(e)
	}
	makeProposal := func(name string) knowledge.StructureProposal {
		t.Helper()
		v, e := service.CreateStructureProposal(ctx, knowledge.StructureCommand{OperationID: uuid.NewString(), Generation: 1, BaseVersion: page.Version, Kind: "edit", Reason: "并发改名", ActorDeviceID: integrationActorID, Edits: []knowledge.StructureEdit{{ConceptID: a, Name: name, Content: current.Content}}})
		if e != nil {
			t.Fatal(e)
		}
		return v
	}
	p1, p2 := makeProposal("并发一"), makeProposal("并发二")
	type outcome struct {
		p knowledge.StructureProposal
		e error
	}
	results := make(chan outcome, 2)
	var ready sync.WaitGroup
	ready.Add(2)
	start := make(chan struct{})
	for _, v := range []knowledge.StructureProposal{p1, p2} {
		go func() {
			ready.Done()
			<-start
			r, e := service.DecideStructureProposal(ctx, v.ID, knowledge.StructureDecision{OperationID: uuid.NewString(), Hash: v.Hash, Decision: "approve", Reason: "并发验收", ActorDeviceID: integrationActorID})
			results <- outcome{r, e}
		}()
	}
	ready.Wait()
	close(start)
	winners, stale := 0, 0
	var applied knowledge.StructureProposal
	for range 2 {
		r := <-results
		if r.e != nil {
			t.Fatal(r.e)
		}
		if r.p.Status == "applied" {
			winners++
			applied = r.p
		}
		if r.p.Status == "stale" {
			stale++
		}
	}
	if winners != 1 || stale != 1 {
		t.Fatalf("并发没有唯一赢家: applied=%d stale=%d", winners, stale)
	}
	if _, e = service.ReadStructure(ctx, knowledge.StructureQuery{Limit: 1, Cursor: first.NextCursor}); knowledge.ErrorCode(e) != knowledge.CodeProposalStale {
		t.Fatalf("旧分页游标未失效: %v", e)
	}
	historic, e := service.ReadConcept(ctx, a, current.RevisionID)
	if e != nil || historic.Name != "同名" {
		t.Fatal("旧修订被覆盖", e)
	}
	page, e = service.ReadStructure(ctx, knowledge.StructureQuery{})
	if e != nil {
		t.Fatal(e)
	}
	rollback, e := service.CreateStructureProposal(ctx, knowledge.StructureCommand{OperationID: uuid.NewString(), Generation: 1, BaseVersion: page.Version, Kind: "compensate", Compensates: applied.ID, Reason: "补偿恢复旧名称", ActorDeviceID: integrationActorID})
	if e != nil {
		t.Fatal(e)
	}
	rollback = decide(rollback)
	restored, e := service.ReadConcept(ctx, a, "")
	if e != nil || restored.Name != "同名" || restored.RevisionID == current.RevisionID || rollback.Status != "applied" {
		t.Fatalf("补偿没有生成新版本: %+v %v", restored, e)
	}
	// 解除引用后，列表、详情、旧提案和幂等回放都不能泄露已失去授权的片段。
	if _, e = service.ChangeCollection(ctx, knowledge.CollectionCommand{ID: knowledge.DefaultCollectionID, Action: "unlink"}); e != nil {
		t.Fatal(e)
	}
	if _, e = service.ReadConcept(ctx, a, ""); knowledge.ErrorCode(e) != knowledge.CodeNotFound {
		t.Fatalf("解除引用后仍可读取正文: %v", e)
	}
	if _, e = service.ReadStructureProposal(ctx, p.ID); knowledge.ErrorCode(e) != knowledge.CodeNotFound {
		t.Fatalf("旧提案泄露正文: %v", e)
	}
	command.Reason = "明确保存同名不同概念"
	if _, e = service.CreateStructureProposal(ctx, command); knowledge.ErrorCode(e) != knowledge.CodeNotFound {
		t.Fatalf("原操作回放泄露已解除引用正文: %v", e)
	}
	if proposals, e := service.ListStructureProposals(ctx, "", 20); e != nil || len(proposals.Items) != 0 {
		t.Fatalf("提案分页在授权前裁剪或泄露正文: %+v %v", proposals, e)
	}
	var evidence int
	if e = pool.QueryRow(context.Background(), `SELECT count(*) FROM learning_evidence`).Scan(&evidence); e != nil || evidence != 0 {
		t.Fatal("结构维护生成了 Evidence", e)
	}
}
