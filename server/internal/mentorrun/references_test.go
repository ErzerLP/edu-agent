package mentorrun

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/edu-agent/edu-agent/packages/agentcore/modelclient"
	identitydb "github.com/edu-agent/edu-agent/server/internal/identity/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/integrations/websearch"
	"github.com/edu-agent/edu-agent/server/internal/knowledge"
	"github.com/edu-agent/edu-agent/server/internal/learningchange"
	"github.com/edu-agent/edu-agent/server/internal/learningspace"
	memorydb "github.com/edu-agent/edu-agent/server/internal/memory/postgresstore"
	outboxdb "github.com/edu-agent/edu-agent/server/internal/platform/outbox/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/privacy"
	privacydb "github.com/edu-agent/edu-agent/server/internal/privacy/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/tutoring"
	tutoringdb "github.com/edu-agent/edu-agent/server/internal/tutoring/postgresstore"
	"github.com/google/uuid"
)

func referenceFixture(t *testing.T) (*changeFixture, *knowledge.Service, knowledge.ReferenceEntry) {
	t.Helper()
	f := adaptiveFixture(t)
	ctx := context.Background()
	if _, err := f.pool.Exec(ctx, `UPDATE device_tokens SET scopes=scopes||ARRAY['references:manage','knowledge:approve']::text[] WHERE id=$1`, f.actor.TokenID); err != nil {
		t.Fatal(err)
	}
	s, err := knowledge.NewService(f.service.starter.Knowledge, knowledge.NewCanonicalizer(), knowledge.ServiceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	collection := uuid.NewString()
	if _, err = s.ChangeCollection(ctx, knowledge.CollectionCommand{ID: collection, Action: "create", Name: "可选参考", Source: "真实导入测试"}); err != nil {
		t.Fatal(err)
	}
	cctx, _ := knowledge.WithCollection(ctx, collection)
	request := knowledge.ImportCommand{OperationID: uuid.NewString(), ActorDeviceID: f.actor.Device.ID, ExpectedParentProvided: true, Source: "用户文件", Documents: []knowledge.ImportDocument{{Path: "README.md", Markdown: "# Go channel\n\nGo channel synchronizes goroutines.\n\n## 不在范围内\n\nprivate omitted section\n"}}}
	p, err := s.PreviewImport(cctx, request)
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.ConfirmImport(cctx, knowledge.ConfirmImportCommand{Request: request, Receipt: p.Receipt})
	if err != nil {
		t.Fatal(err)
	}
	d := r.Revision.Documents[0]
	return f, s, knowledge.ReferenceEntry{ScopeEntry: knowledge.ScopeEntry{CollectionID: collection, RevisionID: r.Revision.ID, DocumentID: d.Revision.DocumentID}, Role: "supplement"}
}

func TestPostgreSQLReferencesStartWithoutSearchAndKeepHistory(t *testing.T) {
	f, _, entry := referenceFixture(t)
	ctx := context.Background()
	old := f.context(t)
	entry.Role = "restrict"
	c := referenceRequest(f, t, entry)
	c.Selection.SessionID = ""
	p, err := f.changes.PreviewReferences(ctx, f.actor, f.goal, c)
	if err != nil {
		t.Fatal(err)
	}
	adopted, err := f.changes.ConfirmReferences(ctx, f.actor, f.goal, learningchange.ReferenceConfirmation{Request: c, Receipt: p.Receipt, ConfirmScope: true})
	if err != nil {
		t.Fatal(err)
	}
	f.service.search = func(func(http.RoundTripper) http.RoundTripper) (websearch.Adapter, string, error) {
		t.Error("已采用参考开学仍触发搜索")
		return nil, "", ErrModel
	}
	create := f.create
	create.OperationID = uuid.NewString()
	r, err := f.service.Create(ctx, f.actor, learningspace.DefaultID, f.goal, create)
	if err != nil {
		t.Fatal(err)
	}
	f.work(t)
	started := f.snapshot(t, r.RunID)
	if started.Status != "succeeded" || started.StartLearning.Result == nil {
		t.Fatalf("参考开学未完成：%+v", started)
	}
	result := started.StartLearning.Result
	if result.Context.ScopeSnapshotID != adopted.ScopeSnapshotID || result.Context.References == nil || len(result.Context.Concepts) == 0 || result.SessionID == f.session {
		t.Fatal("未保留固定范围或未通过正规教学服务新建现场", result)
	}
	if len(started.Research.Sources) != 1 || started.Research.Sources[0].CollectionID != entry.CollectionID {
		t.Fatal("限制范围混入外部来源")
	}
	if after := f.context(t); !reflect.DeepEqual(old.Base, after.Base) || !reflect.DeepEqual(old.Content, after.Content) {
		t.Fatal("新课堂覆盖旧题目")
	}
}

func TestPostgreSQLReferencesUpdatedDocumentReplacesOnlyEffectiveVersion(t *testing.T) {
	f, ks, entry := referenceFixture(t)
	ctx, _ := knowledge.WithCollection(context.Background(), entry.CollectionID)
	old, err := ks.Export(ctx, entry.RevisionID)
	if err != nil {
		t.Fatal(err)
	}
	request := knowledge.ImportCommand{OperationID: uuid.NewString(), ActorDeviceID: f.actor.Device.ID, ExpectedParentProvided: true, ExpectedParentRevisionID: &entry.RevisionID, Source: "更新已知文档", Documents: []knowledge.ImportDocument{{Path: old.Documents[0].Path, Markdown: strings.Replace(old.Documents[0].Markdown, "synchronizes goroutines", "coordinates concurrent goroutines", 1)}}}
	p, err := ks.PreviewImport(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if p.Review != nil {
		request.OperationID = uuid.NewString()
		request.IdentityReviewBasisHash, request.IdentityReviewOperationID, request.IdentityReviewReceipt = p.Review.BasisHash, p.Review.OperationID, p.Review.Receipt
		for _, n := range p.Review.Nodes {
			request.NodeResolutions = append(request.NodeResolutions, knowledge.NodeResolution{Locator: n.Locator, Action: "rewrite", SourceNodeRevisionIDs: []string{n.Candidates[0].RevisionID}, Reason: "用户确认原章节更新"})
		}
		p, err = ks.PreviewImport(ctx, request)
		if err != nil {
			t.Fatal(err)
		}
	}
	if p.Status != "ready" {
		t.Fatalf("更新预览需要审阅：%+v", p.Review)
	}
	updated, err := ks.ConfirmImport(ctx, knowledge.ConfirmImportCommand{Request: request, Receipt: p.Receipt})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Revision.Documents[0].Revision.DocumentID != entry.DocumentID {
		t.Fatal("更新丢失原文档身份")
	}
	tx, err := f.pool.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	base := []knowledge.ScopeEntry{{CollectionID: entry.CollectionID, RevisionID: entry.RevisionID}}
	entry.RevisionID = updated.Revision.ID
	effective, err := f.service.starter.Knowledge.ReferenceScopeTx(context.Background(), tx, base, []knowledge.ReferenceEntry{entry})
	if err != nil || len(effective) != 1 || effective[0].RevisionID != updated.Revision.ID {
		t.Fatal("新旧文档版本同时进入有效范围", effective, err)
	}
	if _, err = ks.Export(ctx, *request.ExpectedParentRevisionID); err != nil {
		t.Fatal("旧版被删除", err)
	}
}

func TestPostgreSQLReferencesPrivacyScrubsMetadataAndPreventsReceiptRestore(t *testing.T) {
	f, ks, entry := referenceFixture(t)
	ctx := context.Background()
	c := referenceRequest(f, t, entry)
	preview, err := f.changes.PreviewReferences(ctx, f.actor, f.goal, c)
	if err != nil {
		t.Fatal(err)
	}
	confirm := learningchange.ReferenceConfirmation{Request: c, Receipt: preview.Receipt}
	if _, err = f.changes.ConfirmReferences(ctx, f.actor, f.goal, confirm); err != nil {
		t.Fatal(err)
	}
	p := privacydb.New(f.pool, privacydb.WithReadPermits(privacy.NewReadPermitManager()), privacydb.WithLocalOwner(identitydb.New(f.pool)), privacydb.WithLocalOwner(f.service.starter.Knowledge), privacydb.WithLocalOwner(f.service.starter.Learning), privacydb.WithLocalOwner(tutoringdb.New(f.pool)), privacydb.WithLocalOwner(memorydb.New(f.pool)), privacydb.WithLocalOwner(outboxdb.New(f.pool)))
	grants, err := privacy.NewErasureGrantService(privacydb.NewGrantStore(f.pool), privacy.ErasureGrantOptions{})
	if err != nil {
		t.Fatal(err)
	}
	grant, err := grants.Issue(ctx, f.actor.Device.ID, "用户参考清除验收")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	barrier, err := p.CommitBarrierAuthorized(ctx, privacy.ErasureRequest{DeviceID: f.actor.Device.ID, OperationID: uuid.NewString(), ActorDeviceID: f.actor.Device.ID, ReasonCode: string(privacy.ReasonLearnerRequest), RequestedAt: now, ManagedBackupUnrecoverableAfter: now.Add(24 * time.Hour), ExpectedCurrentLearnerGeneration: 1}, privacy.NewErasureGrantAuthorization(f.actor.Device.ID, grant.Token))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = p.RunLocalScrub(ctx, barrier.ErasureID); err != nil {
		t.Fatal(err)
	}
	var count int
	if err = f.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM knowledge_reference_heads)+(SELECT count(*) FROM knowledge_reference_operations)+(SELECT count(*) FROM knowledge_context_revisions)`).Scan(&count); err != nil || count != 0 {
		t.Fatal("参考元数据和回执残留", count, err)
	}
	if _, err = f.changes.ConfirmReferences(ctx, f.actor, f.goal, confirm); err == nil {
		t.Fatal("旧批准恢复了清除数据")
	}
	cctx, _ := knowledge.WithCollection(ctx, entry.CollectionID)
	if _, err = ks.Export(cctx, entry.RevisionID); err == nil {
		t.Fatal("清除后仍能恢复原文")
	}
}

func TestPostgreSQLReferencesAsNewDoesNotInheritIdenticalExport(t *testing.T) {
	f, ks, entry := referenceFixture(t)
	ctx, _ := knowledge.WithCollection(context.Background(), entry.CollectionID)
	export, err := ks.Export(ctx, entry.RevisionID)
	if err != nil {
		t.Fatal(err)
	}
	request := knowledge.ImportCommand{OperationID: uuid.NewString(), ActorDeviceID: f.actor.Device.ID, ExpectedParentProvided: true, ExpectedParentRevisionID: &entry.RevisionID, Source: "明确新建", Documents: []knowledge.ImportDocument{{Path: "独立副本.md", Markdown: export.Documents[0].Markdown, AsNew: true}}}
	p, err := ks.PreviewImport(ctx, request)
	if err != nil || p.Status != "ready" || p.Summary.Added != 1 {
		t.Fatal("新建仍继承了原文档身份", p, err)
	}
	changed := request
	changed.Documents = []knowledge.ImportDocument{{Path: request.Documents[0].Path, Markdown: request.Documents[0].Markdown}}
	if _, err = ks.ConfirmImport(ctx, knowledge.ConfirmImportCommand{Request: changed, Receipt: p.Receipt}); knowledge.ErrorCode(err) != knowledge.CodeImportPreviewStale {
		t.Fatal("新建决定变化后旧批准仍有效", err)
	}
	r, err := ks.ConfirmImport(ctx, knowledge.ConfirmImportCommand{Request: request, Receipt: p.Receipt})
	if err != nil || len(r.Revision.Documents) != 2 || len(r.Revision.Lineages) != 0 {
		t.Fatal("独立副本覆盖或继承旧 lineage", err)
	}
	seen := map[string]bool{}
	for _, d := range r.Revision.Documents {
		for _, n := range d.Revision.Nodes {
			if seen[n.NodeID] {
				t.Fatal("新资料继承了节点身份")
			}
			seen[n.NodeID] = true
		}
	}
}

func TestPostgreSQLReferencesMentorOpensReviewWithoutPublishing(t *testing.T) {
	f, _, entry := referenceFixture(t)
	ctx := context.Background()
	c := referenceRequest(f, t, entry)
	p, err := f.changes.PreviewReferences(ctx, f.actor, f.goal, c)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.changes.ConfirmReferences(ctx, f.actor, f.goal, learningchange.ReferenceConfirmation{Request: c, Receipt: p.Receipt}); err != nil {
		t.Fatal(err)
	}
	base := int(f.calls.Load())
	f.modelReply = func(w http.ResponseWriter, r *http.Request, n int) bool {
		if n-base == 1 {
			stream(w, modelclient.Message{Role: "assistant", ToolCalls: []modelclient.ToolCall{{ID: "read-reference", Type: "function", Function: modelclient.ToolFunction{Name: "read_references", Arguments: `{}`}}}})
		} else {
			var request modelclient.Request
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Error(err)
			}
			found := false
			for _, m := range request.Messages {
				found = found || m.Role == "tool" && strings.Contains(m.Content, "Go channel synchronizes")
			}
			if !found {
				t.Error("导师未读取已采用参考")
			}
			stream(w, modelclient.Message{Role: "assistant", ToolCalls: []modelclient.ToolCall{{ID: "open-reference", Type: "function", Function: modelclient.ToolFunction{Name: "open_references", Arguments: `{"reason":"可选补充参考，请自行选择并审阅"}`}}}})
		}
		return true
	}
	f.create = Create{OperationID: uuid.NewString(), SessionID: uuid.NewString(), TeachingSessionID: f.session, ExpectedVersion: f.context(t).Goal.Revision, Prompt: "请帮我整理参考", Save: true, RequestBudget: 3, TokenBudget: 50000}
	r := f.accept(t)
	f.work(t)
	s := f.snapshot(t, r.RunID)
	if s.Interaction == nil || !s.Interaction.ReferenceSelection {
		t.Fatalf("未打开真实审阅交互：%+v", s)
	}
	head, err := f.changes.ReadReferences(ctx, f.actor, f.goal, f.session)
	if err != nil || head.Version != 1 {
		t.Fatal("模型打开审阅擅自发布了政策", head, err)
	}
}

func TestPostgreSQLReferencesSessionOverrideMustConfirmInheritedRestriction(t *testing.T) {
	f, _, entry := referenceFixture(t)
	ctx := context.Background()
	entry.Role = "restrict"
	goalRequest := referenceRequest(f, t, entry)
	goalRequest.Selection.SessionID = ""
	p, err := f.changes.PreviewReferences(ctx, f.actor, f.goal, goalRequest)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.changes.ConfirmReferences(ctx, f.actor, f.goal, learningchange.ReferenceConfirmation{Request: goalRequest, Receipt: p.Receipt, ConfirmScope: true}); err != nil {
		t.Fatal(err)
	}
	entry.Role = "supplement"
	local := referenceRequest(f, t, entry)
	p, err = f.changes.PreviewReferences(ctx, f.actor, f.goal, local)
	if err != nil || !p.RequiresScopeConfirmation || p.Before.Selection.Entries[0].Role != "restrict" {
		t.Fatal("会话覆盖未显示继承限制", p, err)
	}
	confirm := learningchange.ReferenceConfirmation{Request: local, Receipt: p.Receipt}
	if _, err = f.changes.ConfirmReferences(ctx, f.actor, f.goal, confirm); !errors.Is(err, learningchange.ErrConflict) {
		t.Fatal("会话覆盖绕过限制批准", err)
	}
	confirm.ConfirmScope = true
	if _, err = f.changes.ConfirmReferences(ctx, f.actor, f.goal, confirm); err != nil {
		t.Fatal(err)
	}
	goalState, err := f.changes.ReadReferences(ctx, f.actor, f.goal, "")
	if err != nil || goalState.Selection.Entries[0].Role != "restrict" {
		t.Fatal("会话选择污染目标政策", err)
	}
}

func referenceRequest(f *changeFixture, t *testing.T, entry knowledge.ReferenceEntry) learningchange.ReferenceRequest {
	return learningchange.ReferenceRequest{OperationID: uuid.NewString(), ExpectedGoalVersion: f.context(t).Base.GoalVersion, Selection: knowledge.ReferenceSelection{SessionID: f.session, Entries: []knowledge.ReferenceEntry{entry}}}
}

func TestPostgreSQLReferencesPreviewConfirmReplayAndFutureContext(t *testing.T) {
	f, ks, entry := referenceFixture(t)
	ctx := context.Background()
	f.act(t, tutoring.ActionPresentActivity, "")
	before := f.context(t)
	c := referenceRequest(f, t, entry)
	var count int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM knowledge_context_revisions`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	p, err := f.changes.PreviewReferences(ctx, f.actor, f.goal, c)
	if err != nil || p.RequiresScopeConfirmation {
		t.Fatal("补充参考预览", p, err)
	}
	var afterPreview int
	if err = f.pool.QueryRow(ctx, `SELECT count(*) FROM knowledge_context_revisions`).Scan(&afterPreview); err != nil || afterPreview != count {
		t.Fatal("预览发布了上下文", err)
	}
	changed := c
	changed.Selection.Entries = []knowledge.ReferenceEntry{entry}
	changed.Selection.Entries[0].Role = "restrict"
	if _, err = f.changes.ConfirmReferences(ctx, f.actor, f.goal, learningchange.ReferenceConfirmation{Request: changed, Receipt: p.Receipt, ConfirmScope: true}); !errors.Is(err, learningchange.ErrConflict) {
		t.Fatal("修改角色复用旧批准", err)
	}
	confirm := learningchange.ReferenceConfirmation{Request: c, Receipt: p.Receipt}
	results := make(chan knowledge.ReferenceState, 6)
	failures := make(chan error, 6)
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, e := f.changes.ConfirmReferences(ctx, f.actor, f.goal, confirm)
			results <- r
			failures <- e
		}()
	}
	wg.Wait()
	close(results)
	close(failures)
	for e := range failures {
		if e != nil {
			t.Fatal("并发同操作重试", e)
		}
	}
	var adopted knowledge.ReferenceState
	for r := range results {
		if adopted.Version != 0 && !reflect.DeepEqual(adopted, r) {
			t.Fatal("并发回执不一致")
		}
		adopted = r
	}
	if adopted.Version != 1 {
		t.Fatal("重复发布", adopted)
	}
	read, err := f.changes.ReferenceOperation(ctx, f.actor, f.goal, c.OperationID)
	if err != nil || !reflect.DeepEqual(read, adopted) {
		t.Fatal("原操作查询不一致", err)
	}
	after := f.context(t)
	if !reflect.DeepEqual(after.Base, before.Base) || !reflect.DeepEqual(after.Content, before.Content) || after.AvailableContextID != adopted.ContextID {
		t.Fatal("采用改写了原题或没有更新后续上下文")
	}
	if _, err = ks.Export(ctx, before.Session.Context.KnowledgeRevisionID); err != nil {
		t.Fatal("旧来源不能读取", err)
	}
	next := f.propose(t, "route")
	if next.Candidate.ContextID != adopted.ContextID || next.Status != "queued_for_boundary" {
		t.Fatal("后续建议未接入新上下文", next)
	}
	f.act(t, tutoring.ActionEndActivity, "")
	if _, err = f.changes.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if got := f.context(t); got.Base.ContextID != adopted.ContextID || got.Base.ActivityID == before.Base.ActivityID {
		t.Fatal("安全边界没有接入后续新题")
	}
}

func TestPostgreSQLReferencesRestrictionIsolationParentAndRollback(t *testing.T) {
	f, ks, entry := referenceFixture(t)
	ctx := context.Background()
	entry.Role = "restrict"
	c := referenceRequest(f, t, entry)
	p, err := f.changes.PreviewReferences(ctx, f.actor, f.goal, c)
	if err != nil || !p.RequiresScopeConfirmation {
		t.Fatal("限制未要求具体确认", err)
	}
	if _, err = f.changes.ConfirmReferences(ctx, f.actor, f.goal, learningchange.ReferenceConfirmation{Request: c, Receipt: p.Receipt}); !errors.Is(err, learningchange.ErrConflict) {
		t.Fatal("无范围确认仍通过", err)
	}
	other, _ := learningspace.WithScope(ctx, uuid.NewString())
	if _, err = f.changes.ConfirmReferences(other, f.actor, f.goal, learningchange.ReferenceConfirmation{Request: c, Receipt: p.Receipt, ConfirmScope: true}); err == nil {
		t.Fatal("跨区采用")
	}
	if _, err = f.pool.Exec(ctx, `CREATE FUNCTION fail_reference_receipt() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION '测试回执写入失败'; END $$; CREATE TRIGGER fail_reference_receipt BEFORE INSERT ON knowledge_reference_operations FOR EACH ROW EXECUTE FUNCTION fail_reference_receipt()`); err != nil {
		t.Fatal(err)
	}
	confirm := learningchange.ReferenceConfirmation{Request: c, Receipt: p.Receipt, ConfirmScope: true}
	if _, err = f.changes.ConfirmReferences(ctx, f.actor, f.goal, confirm); err == nil {
		t.Fatal("回执故障未传播")
	}
	var n int
	if err = f.pool.QueryRow(ctx, `SELECT count(*) FROM knowledge_reference_heads`).Scan(&n); err != nil || n != 0 {
		t.Fatal("回执失败仍推进上下文", n, err)
	}
	if _, err = f.pool.Exec(ctx, `DROP TRIGGER fail_reference_receipt ON knowledge_reference_operations`); err != nil {
		t.Fatal(err)
	}
	adopted, err := f.changes.ConfirmReferences(ctx, f.actor, f.goal, confirm)
	if err != nil {
		t.Fatal(err)
	}
	tree, err := ks.Tree(ctx, adopted.ScopeSnapshotID)
	if err != nil || len(tree.Revision.Documents) != 1 || tree.Revision.Documents[0].Revision.DocumentID != entry.DocumentID {
		t.Fatal("限制泄露基础来源", err)
	}
	goalRefs, err := f.changes.ReadReferences(ctx, f.actor, f.goal, "")
	if err != nil || goalRefs.Version != 0 {
		t.Fatal("本次限制污染目标政策", err)
	}
	c.OperationID = uuid.NewString()
	c.Selection.Entries[0].Role = "prefer"
	p, err = f.changes.PreviewReferences(ctx, f.actor, f.goal, c)
	if err != nil || !p.RequiresScopeConfirmation {
		t.Fatal("移除限制未要求确认", err)
	}
	cctx, _ := knowledge.WithCollection(ctx, entry.CollectionID)
	_, err = ks.Import(cctx, knowledge.ImportCommand{OperationID: uuid.NewString(), ActorDeviceID: f.actor.Device.ID, ExpectedParentProvided: true, ExpectedParentRevisionID: &entry.RevisionID, Source: "并发新增", Documents: []knowledge.ImportDocument{{Path: "new.md", Markdown: "# 新资料\n新增正文"}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.changes.ConfirmReferences(ctx, f.actor, f.goal, learningchange.ReferenceConfirmation{Request: c, Receipt: p.Receipt, ConfirmScope: true}); !errors.Is(err, learningchange.ErrConflict) {
		t.Fatal("父版本变化复用旧批准", err)
	}
}

func TestPostgreSQLReferencesPreferenceBeforeShortlistAndConcurrentPolicies(t *testing.T) {
	f, ks, entry := referenceFixture(t)
	ctx := context.Background()
	entry.Role = "prefer"
	c := referenceRequest(f, t, entry)
	p, err := f.changes.PreviewReferences(ctx, f.actor, f.goal, c)
	if err != nil {
		t.Fatal(err)
	}
	second := c
	second.OperationID = uuid.NewString()
	p2, err := f.changes.PreviewReferences(ctx, f.actor, f.goal, second)
	if err != nil {
		t.Fatal(err)
	}
	r, err := f.changes.ConfirmReferences(ctx, f.actor, f.goal, learningchange.ReferenceConfirmation{Request: c, Receipt: p.Receipt})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.changes.ConfirmReferences(ctx, f.actor, f.goal, learningchange.ReferenceConfirmation{Request: second, Receipt: p2.Receipt}); !errors.Is(err, learningchange.ErrConflict) {
		t.Fatal("覆盖了已变化的参考父政策", err)
	}
	retrieval, err := ks.Retrieve(ctx, knowledge.RetrievalCommand{ScopeSnapshotID: &r.ScopeSnapshotID, Query: "Go channel"})
	if err != nil || len(retrieval.DocumentShortlist) == 0 || retrieval.DocumentShortlist[0] != "README.md" {
		t.Fatal("优先依据未进入首选候选", retrieval.DocumentShortlist, err)
	}
	if _, err = f.pool.Exec(ctx, `UPDATE device_tokens SET scopes=array_remove(scopes,'references:manage') WHERE id=$1`, f.actor.TokenID); err != nil {
		t.Fatal(err)
	}
	if _, err = f.changes.ConfirmReferences(ctx, f.actor, f.goal, learningchange.ReferenceConfirmation{Request: c, Receipt: p.Receipt}); !errors.Is(err, learningchange.ErrForbidden) {
		t.Fatal("权限撤销仍可重放正文", err)
	}
}
