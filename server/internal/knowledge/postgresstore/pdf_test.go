package postgresstore_test

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/edu-agent/edu-agent/server/internal/knowledge"
	"github.com/edu-agent/edu-agent/server/internal/learning"
	"github.com/edu-agent/edu-agent/server/internal/learningcontent"
	"github.com/edu-agent/edu-agent/server/internal/pdffixture"
	"github.com/google/uuid"
)

func TestPostgreSQLPDFVersionsCitationsAndScope(t *testing.T) {
	ctx, pool, store, service := newReviewerPostgresHarness(t)
	raw := pdffixture.Build("第一页 original", "第二页需要核对的证据", "")
	c := knowledge.ImportCommand{OperationID: uuid.NewString(), ExpectedParentProvided: true, ActorDeviceID: integrationActorID, Source: "真实 PDF 版本验收", Documents: []knowledge.ImportDocument{{Path: "课程.pdf.md", PDF: &knowledge.PDFImport{Data: raw, AcceptPartial: true}}}}
	p, err := service.PreviewImport(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	first, err := service.ConfirmImport(ctx, knowledge.ConfirmImportCommand{Request: c, Receipt: p.Receipt})
	if err != nil {
		t.Fatal(err)
	}
	tree, err := service.Tree(ctx, first.Revision.ID)
	if err != nil {
		t.Fatal(err)
	}
	doc := tree.Revision.Documents[0].Revision
	if doc.PDF == nil || doc.PDF.Report.Pages[2].Status != "no_text" {
		t.Fatal("数据库丢失覆盖报告")
	}
	if got, err := store.PDFOriginal(ctx, first.Revision.ID, doc.ID, 2); err != nil || !bytes.Equal(got, raw) {
		t.Fatal("不可变原件丢失", err)
	}
	if _, err = pool.Exec(ctx, `UPDATE knowledge_document_payloads SET pdf_original=NULL,pdf_metadata=NULL WHERE document_revision_id=$1`, doc.ID); err == nil {
		t.Fatal("PDF 原件允许绕过不可变触发器")
	}
	quote := "第二页需要核对的证据"
	start := strings.Index(doc.CanonicalMarkdown, quote)
	ref := learning.KnowledgeReference{KnowledgeRevisionID: first.Revision.ID, DocumentRevisionID: doc.ID, NodeID: doc.Nodes[2].NodeID, NodeRevisionID: doc.Nodes[2].ID, Range: learning.SourceRange{Start: start, End: start + len(quote)}, Slice: quote, SliceSHA256: learningcontent.TextHash(quote)}
	checkCitation := func() {
		t.Helper()
		tx, e := pool.Begin(ctx)
		if e != nil {
			t.Fatal(e)
		}
		defer tx.Rollback(context.Background())
		citation, e := store.ContentCitationTx(ctx, tx, ref)
		if e != nil || citation.PDF == nil || len(citation.PDF.Pages) != 1 || citation.PDF.Pages[0].Number != 2 || citation.Reference.Slice != quote {
			t.Fatalf("页码/版本引用不准确：%+v %v", citation, e)
		}
		fake := ref
		fake.Slice = "不存在的模型引用"
		if _, e = store.ContentCitationTx(ctx, tx, fake); e == nil {
			t.Fatal("伪造片段获得页码")
		}
	}
	checkCitation()
	scope, err := service.FreezeScope(ctx, knowledge.ScopeSnapshot{ID: uuid.NewString(), Entries: []knowledge.ScopeEntry{{CollectionID: knowledge.DefaultCollectionID, RevisionID: first.Revision.ID, DocumentID: doc.DocumentID, NodeID: doc.Nodes[2].NodeID}}})
	if err != nil {
		t.Fatal(err)
	}
	limited, err := service.Tree(ctx, scope.ID)
	if err != nil || limited.Revision.Documents[0].Revision.PDF.Report.Pages[0].Text != "" {
		t.Fatal("冻结章节泄露其他页文本", err)
	}
	if _, err = store.PDFOriginal(ctx, scope.ID, doc.ID, 1); err == nil {
		t.Fatal("冻结章节读取了范围外原页")
	}
	if _, err = store.PDFOriginal(ctx, scope.ID, doc.ID, 2); err != nil {
		t.Fatal(err)
	}
	// 仅原文件改变也必须生成新版本，不能因提取文本相同而重用旧原件。
	newRaw := pdffixture.BuildWithOptions(false, "/Lang (zh-CN)", "第一页 original", "第二页需要核对的证据", "")
	c.OperationID = uuid.NewString()
	c.ExpectedParentRevisionID = &first.Revision.ID
	c.Documents[0].PDF = &knowledge.PDFImport{Data: newRaw, AcceptPartial: true}
	sawReview := false
	for i := 0; i < 4; i++ {
		p, err = service.PreviewImport(ctx, c)
		if err != nil {
			t.Fatal(err)
		}
		if p.Status == "ready" {
			break
		}
		sawReview = true
		c.IdentityReviewBasisHash, c.IdentityReviewOperationID, c.IdentityReviewReceipt = p.Review.BasisHash, p.Review.OperationID, p.Review.Receipt
		c.OperationID = uuid.NewString()
		for _, d := range p.Review.Documents {
			c.DocumentResolutions = append(c.DocumentResolutions, knowledge.DocumentResolution{Locator: d.Locator, Action: "preserve", DocumentID: doc.DocumentID, Reason: "用户确认同名更新"})
		}
		for _, n := range p.Review.Nodes {
			c.NodeResolutions = append(c.NodeResolutions, knowledge.NodeResolution{Locator: n.Locator, Action: "new", Reason: "用户确认新页节点"})
		}
	}
	if !sawReview || p.Status != "ready" {
		t.Fatal("同名文件未走身份预览", p)
	}
	second, err := service.ConfirmImport(ctx, knowledge.ConfirmImportCommand{Request: c, Receipt: p.Receipt})
	if err != nil {
		t.Fatal(err)
	}
	next := second.Revision.Documents[0].Revision
	if next.DocumentID != doc.DocumentID || next.ID == doc.ID || next.PDF.Report.Fingerprint == doc.PDF.Report.Fingerprint {
		t.Fatal("原文件变化没有生成独立版本")
	}
	checkCitation()
	if image, err := service.PDFPage(ctx, first.Revision.ID, doc.ID, 2); err != nil || !bytes.HasPrefix(image, []byte("\x89PNG")) {
		t.Fatal("历史原页无法查看", err)
	}
	// 后续导入普通资料时，旧 PDF 仅复用已存原件，不复制或覆盖它。
	_, err = service.Import(ctx, knowledge.ImportCommand{OperationID: uuid.NewString(), ExpectedParentProvided: true, ExpectedParentRevisionID: &second.Revision.ID, ActorDeviceID: integrationActorID, Source: "后续普通导入", Documents: []knowledge.ImportDocument{{Path: "extra.md", Markdown: "# 独立笔记\n兼容普通文本"}}})
	if err != nil {
		t.Fatal("后续普通导入损坏 PDF", err)
	}
}
