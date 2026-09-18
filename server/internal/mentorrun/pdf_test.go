package mentorrun

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"

	"github.com/edu-agent/edu-agent/server/internal/knowledge"
	"github.com/edu-agent/edu-agent/server/internal/learningchange"
	"github.com/edu-agent/edu-agent/server/internal/learningspace"
	"github.com/edu-agent/edu-agent/server/internal/pdffixture"
	"github.com/edu-agent/edu-agent/server/internal/pdfsource"
	"github.com/edu-agent/edu-agent/server/internal/research"
	"github.com/google/uuid"
)

func TestPDFAttachmentsDoNotExpandModelPayloadBudget(t *testing.T) {
	a, err := newCipher(bytes.Repeat([]byte{42}, 32))
	if err != nil {
		t.Fatal(err)
	}
	body := Body{Research: &research.State{Sources: []research.Source{{ID: "pdf", PDF: &pdfsource.Report{}}}}, SourceFiles: map[string][]byte{"pdf": bytes.Repeat([]byte{'x'}, research.MaxWire)}}
	raw, err := seal(a, "pdf-run", body)
	if err != nil {
		t.Fatal("合法原件超过旧正文上限后无法暂存", err)
	}
	recovered, err := unseal(a, "pdf-run", raw)
	if err != nil || !bytes.Equal(recovered.SourceFiles["pdf"], body.SourceFiles["pdf"]) {
		t.Fatal("原件未加密恢复", err)
	}
	body.Output = strings.Repeat("x", MaxBody)
	if _, err = seal(a, "pdf-run", body); !errors.Is(err, ErrLimit) {
		t.Fatal("原件扩展放宽了模型正文上限", err)
	}
	body.Output = ""
	body.SourceFiles["pdf"] = bytes.Repeat([]byte{'x'}, research.MaxDecoded+1)
	if _, err = seal(a, "pdf-run", body); !errors.Is(err, ErrLimit) {
		t.Fatal("原件绕过抓取字节预算", err)
	}
	body.SourceFiles = map[string][]byte{"unknown": []byte("旁路附件")}
	if _, err = seal(a, "pdf-run", body); !errors.Is(err, ErrLimit) {
		t.Fatal("附件未绑定 PDF 来源", err)
	}
}

func TestPostgreSQLPDFReferenceTeachingKeepsPhysicalPage(t *testing.T) {
	f, ks, entry := referenceFixture(t)
	f.service.starter.Content.ConfigureReferences(f.service.starter.Knowledge)
	ctx := context.Background()
	cctx, _ := knowledge.WithCollection(ctx, entry.CollectionID)
	request := knowledge.ImportCommand{OperationID: uuid.NewString(), ActorDeviceID: f.actor.Device.ID, ExpectedParentProvided: true, ExpectedParentRevisionID: &entry.RevisionID, Source: "PDF 参考", Documents: []knowledge.ImportDocument{{Path: "pages.pdf.md", PDF: &knowledge.PDFImport{Data: pdffixture.Build("第一物理页不在授权范围", "Go channel synchronizes goroutines on page two."), AcceptPartial: true}}}}
	p, err := ks.PreviewImport(cctx, request)
	if err != nil {
		t.Fatal(err)
	}
	r, err := ks.ConfirmImport(cctx, knowledge.ConfirmImportCommand{Request: request, Receipt: p.Receipt})
	if err != nil {
		t.Fatal(err)
	}
	var pageNode string
	for _, doc := range r.Revision.Documents {
		if doc.Revision.PDF != nil {
			entry.RevisionID, entry.DocumentID = r.Revision.ID, doc.Revision.DocumentID
			entry.NodeID, pageNode = doc.Revision.Nodes[2].NodeID, doc.Revision.Nodes[2].ID
		}
	}
	entry.Role = "restrict"
	c := referenceRequest(f, t, entry)
	c.Selection.SessionID = ""
	preview, err := f.changes.PreviewReferences(ctx, f.actor, f.goal, c)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.changes.ConfirmReferences(ctx, f.actor, f.goal, learningchange.ReferenceConfirmation{Request: c, Receipt: preview.Receipt, ConfirmScope: true}); err != nil {
		t.Fatal(err)
	}
	create := f.create
	create.OperationID = uuid.NewString()
	run, err := f.service.Create(ctx, f.actor, learningspace.DefaultID, f.goal, create)
	if err != nil {
		t.Fatal(err)
	}
	f.work(t)
	started := f.snapshot(t, run.RunID)
	if started.Status != "succeeded" || started.StartLearning.Result == nil {
		t.Fatalf("PDF 参考开学失败：%+v", started)
	}
	source := started.Research.Sources[0]
	if source.PDF == nil || len(source.Fragments) != 1 || source.Fragments[0].Page != 2 || strings.Contains(source.Text, "第一物理页") {
		t.Fatal("模型接收了未授权页或丢失页码", source)
	}
	result := started.StartLearning.Result
	citation, err := f.service.starter.Content.Citation(ctx, f.actor, result.ArtifactID, result.ArtifactVersion, pageNode)
	if err != nil {
		t.Fatal(err)
	}
	if citation.PDF == nil || len(citation.PDF.Pages) != 1 || citation.PDF.Pages[0].Number != 2 || !strings.Contains(citation.Reference.Slice, "page two") {
		t.Fatal("教学引用没有绑定实际第二页", citation)
	}
}

func TestPostgreSQLPDFResearchExplicitPartialAdoption(t *testing.T) {
	f, _ := researchFixture(t, false)
	ctx := context.Background()
	if _, err := f.pool.Exec(ctx, `UPDATE device_tokens SET scopes=scopes||ARRAY['knowledge:write','research:adopt'] WHERE id=$1`, f.actor.TokenID); err != nil {
		t.Fatal(err)
	}
	page := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/pdf")
		_, _ = w.Write(pdffixture.Build("研究第一物理页", "第二页真实证据", ""))
	}))
	defer page.Close()
	f.service.fetcher = research.NewFetcherWithNetwork(func(context.Context, string, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
	}, func(ctx context.Context, network, address string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, page.Listener.Addr().String())
	})
	f.create.Research.AutoAdopt = true
	r, err := f.service.Create(ctx, f.actor, learningspace.DefaultID, f.goal, f.create)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.service.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	snapshot := f.snapshot(t, r.RunID)
	if snapshot.Research == nil || snapshot.Research.Sources[0].Status != "partial" {
		t.Fatal("有缺口的 PDF 被自动采纳", snapshot)
	}
	source := snapshot.Research.Sources[0]
	raw, _ := json.Marshal(snapshot)
	if bytes.Contains(raw, []byte("source_files")) || bytes.Contains(raw, []byte("pdf_original")) {
		t.Fatal("原始 PDF 进入公开运行快照")
	}
	// 加密运行正文在重启后仍可完成原文件发布，不再次请求网络。
	f.service, err = New(f.pool, f.settings, bytes.Repeat([]byte{42}, 32))
	if err != nil {
		t.Fatal(err)
	}
	c := SourceDecision{OperationID: uuid.NewString(), ExpectedVersion: snapshot.Version, Kind: "adopt"}
	if _, err = f.service.DecideSource(ctx, f.actor, learningspace.DefaultID, r.RunID, source.ID, c); !errors.Is(err, ErrInvalid) {
		t.Fatal("缺口未明确确认仍可采纳", err)
	}
	c.AcceptPartial = true
	if _, err = f.service.DecideSource(ctx, f.actor, learningspace.DefaultID, r.RunID, source.ID, c); err != nil {
		t.Fatal(err)
	}
	snapshot = f.snapshot(t, r.RunID)
	if snapshot.Research.Sources[0].Status != "adopted" || snapshot.Research.Sources[0].DocumentRevisionID == "" {
		t.Fatal("PDF 未发布为正式版本")
	}
	if _, err = f.service.Command(ctx, f.actor, learningspace.DefaultID, r.RunID, Command{OperationID: uuid.NewString(), ExpectedVersion: snapshot.Version, Kind: "clear"}); err != nil {
		t.Fatal(err)
	}
	if err = f.service.ReadSources(ctx, f.actor, learningspace.DefaultID, r.RunID, func(items []research.Source) error {
		if len(items) != 1 || items[0].Text != source.Text || items[0].PDF == nil || items[0].Fragments[1].Page != 2 {
			t.Fatal("正式 PDF 版本页码/文本无法恢复", items)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
