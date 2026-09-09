package postgresstore_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/edu-agent/edu-agent/server/internal/knowledge"
	space "github.com/edu-agent/edu-agent/server/internal/learningspace"
	"github.com/google/uuid"
)

func TestPostgreSQLImportPreviewConfirmAndReplay(t *testing.T) {
	ctx, pool, store, s := newReviewerPostgresHarness(t)
	spaceID, collection := uuid.NewString(), uuid.NewString()
	if _, err := pool.Exec(ctx, `INSERT INTO learning_spaces(id,name,status,version) VALUES($1,'导入区','active',1)`, spaceID); err != nil {
		t.Fatal(err)
	}
	ctx, _ = space.WithScope(ctx, spaceID)
	if _, err := s.ChangeCollection(ctx, knowledge.CollectionCommand{ID: collection, Action: "create", Name: "资料", Source: "local"}); err != nil {
		t.Fatal(err)
	}
	ctx, _ = knowledge.WithCollection(ctx, collection)
	c := knowledge.ImportCommand{OperationID: uuid.NewString(), ExpectedParentProvided: true, Source: "测试", ActorDeviceID: integrationActorID, Documents: []knowledge.ImportDocument{{Path: "a.md", Markdown: "# Go\nchannels preserve the original text and identity\n"}, {Path: "b.md", Markdown: "# B\nsecond document\n"}}}
	p, err := s.PreviewImport(ctx, c)
	if err != nil || p.Status != "ready" || p.Summary.Added != 2 {
		t.Fatalf("预览：%+v %v", p, err)
	}
	if head, err := store.Head(ctx); err != nil || head != nil {
		t.Fatalf("预览写入了 head：%+v %v", head, err)
	}
	if _, exists, err := store.LookupImportOperation(ctx, c.OperationID); err != nil || exists {
		t.Fatalf("预览写入了 operation：%v %v", exists, err)
	}
	changed := c
	changed.Documents = append([]knowledge.ImportDocument(nil), c.Documents...)
	changed.Documents[0].Markdown = "changed"
	if _, err := s.ConfirmImport(ctx, knowledge.ConfirmImportCommand{Request: changed, Receipt: p.Receipt}); knowledge.ErrorCode(err) != knowledge.CodeImportPreviewStale {
		t.Fatalf("修改正文复用确认：%v", err)
	}
	other := c
	other.ActorDeviceID = uuid.NewString()
	if _, err := s.ConfirmImport(ctx, knowledge.ConfirmImportCommand{Request: other, Receipt: p.Receipt}); knowledge.ErrorCode(err) != knowledge.CodeImportPreviewStale {
		t.Fatalf("跨设备复用确认：%v", err)
	}
	confirm := knowledge.ConfirmImportCommand{Request: c, Receipt: p.Receipt}
	result, err := s.ConfirmImport(ctx, confirm)
	if err != nil || result.Summary == nil || result.Summary.Added != 2 {
		t.Fatalf("提交：%+v %v", result, err)
	}
	restarted, err := knowledge.NewService(store, knowledge.NewCanonicalizer(), knowledge.ServiceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	replay, err := restarted.ConfirmImport(ctx, confirm)
	if err != nil || !replay.Replayed || replay.Revision.ID != result.Revision.ID || !reflect.DeepEqual(replay.Summary, result.Summary) {
		t.Fatalf("重启后回执不一致：%+v %v", replay, err)
	}
	queried, err := restarted.ImportOperation(ctx, c.OperationID, c.ActorDeviceID)
	if err != nil || queried.Revision.ID != result.Revision.ID {
		t.Fatalf("核对原操作：%+v %v", queried, err)
	}
	if _, err := restarted.ImportOperation(ctx, c.OperationID, other.ActorDeviceID); knowledge.ErrorCode(err) != knowledge.CodeNotFound {
		t.Fatalf("跨设备查询：%v", err)
	}
	exported, err := s.Export(ctx, result.Revision.ID)
	if err != nil {
		t.Fatal(err)
	}
	next := c
	next.OperationID = uuid.NewString()
	next.ExpectedParentRevisionID = &result.Revision.ID
	next.Documents = nil
	for _, d := range exported.Documents {
		next.Documents = append(next.Documents, knowledge.ImportDocument{Path: d.Path, Markdown: d.Markdown})
	}
	p, err = s.PreviewImport(ctx, next)
	if err != nil || p.Summary.Unchanged != 2 {
		t.Fatalf("未变化预览：%+v %v", p, err)
	}
	unchanged, err := s.ConfirmImport(ctx, knowledge.ConfirmImportCommand{Request: next, Receipt: p.Receipt})
	if err != nil || !unchanged.Unchanged || unchanged.Revision.ID != result.Revision.ID {
		t.Fatalf("未变化提交：%+v %v", unchanged, err)
	}
	moved := next
	moved.OperationID = uuid.NewString()
	moved.Documents = append([]knowledge.ImportDocument(nil), next.Documents...)
	moved.Documents[0].Path = "moved.md"
	movePreview, err := s.PreviewImport(ctx, moved)
	if err != nil || movePreview.Summary.Updated != 1 || movePreview.Summary.Unchanged != 1 {
		t.Fatalf("保留正文版本的移动应计为更新：%+v %v", movePreview, err)
	}
	next.OperationID = uuid.NewString()
	next.Documents = append([]knowledge.ImportDocument(nil), next.Documents...)
	next.Documents[0].Markdown = strings.Replace(next.Documents[0].Markdown, "original text", "updated original text", 1)
	p, err = s.PreviewImport(ctx, next)
	if err != nil || p.Review == nil || len(p.Review.Nodes) == 0 {
		t.Fatalf("正文改写没有要求章节身份决定：%+v %v", p, err)
	}
	next.IdentityReviewBasisHash, next.IdentityReviewOperationID, next.IdentityReviewReceipt = p.Review.BasisHash, p.Review.OperationID, p.Review.Receipt
	next.OperationID = uuid.NewString()
	for _, n := range p.Review.Nodes {
		next.NodeResolutions = append(next.NodeResolutions, knowledge.NodeResolution{Locator: n.Locator, Action: "rewrite", SourceNodeRevisionIDs: []string{n.Candidates[0].RevisionID}, Reason: "明确承接原章节"})
	}
	p, err = s.PreviewImport(ctx, next)
	if err != nil || p.Summary.Updated != 1 || p.Summary.Unchanged != 1 || len(p.Diff) != 1 {
		t.Fatalf("更新预览：%+v %v", p, err)
	}
	// 同一父版本上的另一批次推进 head 后，旧确认必须失败。
	parallel := next
	parallel.IdentityReviewBasisHash, parallel.IdentityReviewOperationID, parallel.IdentityReviewReceipt = "", "", ""
	parallel.NodeResolutions = nil
	parallel.OperationID = uuid.NewString()
	parallel.Documents = []knowledge.ImportDocument{{Path: "other.md", Markdown: "# Other\nother material\n"}}
	pp, err := s.PreviewImport(ctx, parallel)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.ConfirmImport(ctx, knowledge.ConfirmImportCommand{Request: parallel, Receipt: pp.Receipt}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ConfirmImport(ctx, knowledge.ConfirmImportCommand{Request: next, Receipt: p.Receipt}); knowledge.ErrorCode(err) != knowledge.CodeRevisionConflict {
		t.Fatalf("过期父版本仍可提交：%v", err)
	}
}

func TestPostgreSQLImportConfirmRollsBackWholeBatch(t *testing.T) {
	ctx, pool, store, s := newReviewerPostgresHarness(t)
	generation, err := store.ImportGeneration(ctx)
	if err != nil {
		t.Fatal(err)
	}
	wrong := generation + 1
	if _, err := store.CommitImport(ctx, knowledge.PreparedCommit{OperationID: uuid.NewString(), ExpectedGeneration: &wrong}); knowledge.ErrorCode(err) != knowledge.CodeImportPreviewStale {
		t.Fatalf("事务内未检查 generation：%v", err)
	}
	c := knowledge.ImportCommand{OperationID: uuid.NewString(), ExpectedParentProvided: true, Source: "测试回滚", ActorDeviceID: integrationActorID, Documents: []knowledge.ImportDocument{{Path: "a.md", Markdown: "# A\n"}, {Path: "b.md", Markdown: "# B\n"}}}
	p, err := s.PreviewImport(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	// 在事务最后的回执写入注入故障，检验正文和 head 不会部分提交。
	if _, err = pool.Exec(ctx, `CREATE FUNCTION reject_import_receipt() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION '测试回滚'; END $$; CREATE TRIGGER reject_receipt BEFORE INSERT ON knowledge_import_operations FOR EACH ROW EXECUTE FUNCTION reject_import_receipt()`); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ConfirmImport(ctx, knowledge.ConfirmImportCommand{Request: c, Receipt: p.Receipt}); err == nil {
		t.Fatal("注入故障没有失败")
	}
	if head, err := store.Head(ctx); err != nil || head != nil {
		t.Fatalf("失败保留了 head：%+v %v", head, err)
	}
	var count int
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM knowledge_document_payloads`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("失败残留正文：%d %v", count, err)
	}
	if _, err = pool.Exec(ctx, `DROP TRIGGER reject_receipt ON knowledge_import_operations`); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ConfirmImport(ctx, knowledge.ConfirmImportCommand{Request: c, Receipt: p.Receipt}); err != nil {
		t.Fatalf("同请求重试失败：%v", err)
	}
}
