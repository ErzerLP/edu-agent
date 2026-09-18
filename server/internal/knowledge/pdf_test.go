package knowledge

import (
	"strings"
	"testing"

	"github.com/edu-agent/edu-agent/server/internal/pdffixture"
	"github.com/google/uuid"
)

func TestPDFPreviewRequiresRealBytesAndPartialConsent(t *testing.T) {
	store := &previewGenerationStore{memoryCatalogStore: newMemoryCatalogStore(), generation: 1}
	s, _ := NewService(store, NewCanonicalizer(), ServiceOptions{})
	raw := pdffixture.Build("第一页真实正文", "", "第三页中文 English")
	c := ImportCommand{OperationID: uuid.NewString(), ExpectedParentProvided: true, ActorDeviceID: uuid.NewString(), Source: "PDF 测试", Documents: []ImportDocument{{Path: "资料.pdf.md", PDF: &PDFImport{Data: raw}}}}
	if _, err := s.PreviewImport(t.Context(), c); ErrorCode(err) != "pdf_partial_confirmation_required" {
		t.Fatal("缺口没有要求明确确认", err)
	}
	c.Documents[0].PDF.AcceptPartial = true
	p, err := s.PreviewImport(t.Context(), c)
	if err != nil || p.Status != "ready" || len(store.revisions) != 0 {
		t.Fatal("预览错误或发生发布", p, err)
	}
	changed := c
	changed.Documents = []ImportDocument{{Path: c.Documents[0].Path, PDF: &PDFImport{Data: pdffixture.Build("伪造的不同文件"), AcceptPartial: true}}}
	if _, err = s.ConfirmImport(t.Context(), ConfirmImportCommand{Request: changed, Receipt: p.Receipt}); ErrorCode(err) != CodeImportPreviewStale {
		t.Fatal("替换文件复用了批准", err)
	}
	result, err := s.ConfirmImport(t.Context(), ConfirmImportCommand{Request: c, Receipt: p.Receipt})
	if err != nil {
		t.Fatal(err)
	}
	doc := result.Revision.Documents[0].Revision
	if doc.PDF.Report.PageCount != 3 || len(doc.PDF.Ranges) != 2 || doc.PDF.Ranges[1].Number != 3 || !strings.Contains(doc.CanonicalMarkdown, "第三页中文 English") {
		t.Fatal("原文或物理页码丢失", doc.PDF)
	}
	c.OperationID = uuid.NewString()
	c.ExpectedParentRevisionID = &result.Revision.ID
	c.Documents[0].PDF.Locator = "https://forged.example/a.pdf"
	if _, err = s.PreviewImport(t.Context(), c); ErrorCode(err) != CodeInvalidRequest {
		t.Fatal("伪网页来源未拒绝", err)
	}
	c.Documents[0].PDF.SourceReceipt = PDFSourceReceipt(raw, c.Documents[0].PDF.Locator)
	c.Documents[0].Markdown = "模型编造的第 9 页"
	if _, err = s.PreviewImport(t.Context(), c); ErrorCode(err) != CodeInvalidRequest {
		t.Fatal("客户端文本覆盖解析结果", err)
	}
}

func TestPDFCompressedAbuseCannotBeAdopted(t *testing.T) {
	_, _, err := preparePDF(t.Context(), PDFImport{Data: pdffixture.CompressedPadding(160 << 20), AcceptPartial: true})
	if err == nil {
		t.Fatal("解压滥用被当作可用文本发布")
	}
	t.Logf("解压攻击明确拒绝：%v", err)
}
