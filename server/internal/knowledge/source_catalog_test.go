package knowledge

import (
	space "github.com/edu-agent/edu-agent/server/internal/learningspace"
	"github.com/edu-agent/edu-agent/server/internal/research"
	"github.com/google/uuid"
	"strings"
	"testing"
)

func TestSourceReceiptBindsBodyAndSpace(t *testing.T) {
	ctx := t.Context()
	source := research.Source{Locator: "https://example.org/start", FinalURL: "https://example.org/final", Kind: "text/html", Status: "parsed", Parser: "html-v1", Coverage: "complete", Fingerprint: strings.Repeat("a", 64), Text: "真实正文", StorageAllowed: true}
	markdown, receipt := FetchedSourceReceipt(ctx, 3, source)
	if receipt == "" {
		t.Fatal("usable fetch has no receipt")
	}
	metadata, err := verifySourceReceipt(ctx, 3, markdown, receipt)
	if err != nil || metadata.FinalURL != source.FinalURL || metadata.Kind != "url" {
		t.Fatal(metadata, err)
	}
	other, _ := space.WithScope(ctx, uuid.NewString())
	for _, c := range []struct {
		body, receipt string
		generation    int64
	}{
		{markdown + "forged", receipt, 3}, {markdown, receipt + "x", 3}, {markdown, receipt, 4},
	} {
		if _, err := verifySourceReceipt(ctx, c.generation, c.body, c.receipt); ErrorCode(err) != CodeInvalidRequest {
			t.Fatal("forged receipt accepted", err)
		}
	}
	if _, err := verifySourceReceipt(other, 3, markdown, receipt); ErrorCode(err) != CodeInvalidRequest {
		t.Fatal("cross-space receipt accepted", err)
	}
	source.StorageAllowed = false
	if _, r := FetchedSourceReceipt(ctx, 3, source); r != "" {
		t.Fatal("forbidden source obtained receipt")
	}
}

func TestDiscoverySummaryCannotObtainBodyReceiptOrCitation(t *testing.T) {
	source := research.Source{ID: uuid.NewString(), Locator: "https://example.org/", Title: "摘要候选", Status: "candidate", Text: "摘要不是已读正文", StorageAllowed: true}
	if body, receipt := FetchedSourceReceipt(t.Context(), 1, source); body != "" || receipt != "" {
		t.Fatal("summary received body receipt")
	}
	synthesis := research.Synthesis{Points: []research.Point{{Text: "伪造引用", Citations: []research.Citation{{SourceID: source.ID, Quote: source.Text}}}}}
	if synthesis.Validate([]research.Source{source}) == nil {
		t.Fatal("summary became read citation")
	}
}

func TestSourceMetadataDoesNotTrustMarkdownPrefix(t *testing.T) {
	store := &previewGenerationStore{memoryCatalogStore: newMemoryCatalogStore(), generation: 1}
	s, _ := NewService(store, NewCanonicalizer(), ServiceOptions{})
	c := ImportCommand{OperationID: uuid.NewString(), ExpectedParentProvided: true, ActorDeviceID: uuid.NewString(), Source: "browser", Documents: []ImportDocument{{Path: "web.pdf.md", Markdown: "# 假 PDF\n来源：https://forged.example\n解析器：可信\n"}}}
	p, err := s.PreviewImport(t.Context(), c)
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.ConfirmImport(t.Context(), ConfirmImportCommand{Request: c, Receipt: p.Receipt})
	if err != nil {
		t.Fatal(err)
	}
	m := r.Revision.Documents[0].Revision.SourceMetadata
	if m == nil || m.Kind != "text" || m.FinalURL != "" || m.Coverage != "provided_text" {
		t.Fatalf("text impersonated external source: %+v", m)
	}
}

func TestSourceFragmentsPreserveUTF8PositionsAndPDFPage(t *testing.T) {
	text := "x\n" + strings.Repeat("界", 1500) + "\nend"
	doc := DocumentRevision{CanonicalMarkdown: text, Nodes: []NodeRevision{{ID: uuid.NewString(), NodeID: uuid.NewString(), LocalBodyRange: SourceRange{Start: 2, End: len(text), StartLine: 2, EndLine: 3}}}, PDF: &PDFMetadata{Ranges: []PDFPageRange{{Number: 2, Range: SourceRange{Start: 2, End: len(text)}}}}}
	fragments := SourceFragments(doc)
	if len(fragments) != 1 || !fragments[0].Truncated || fragments[0].Text != strings.Repeat("界", 1365) || fragments[0].Range != (SourceRange{Start: 2, End: 4097, StartLine: 2, EndLine: 2}) || fragments[0].Page != 2 {
		t.Fatal("fragment lost UTF-8 byte/line/page position", fragments)
	}
}

func TestSourceFragmentsOmitUnparsedPDFPages(t *testing.T) {
	doc := DocumentRevision{CanonicalMarkdown: "metadata\n已读正文\n未解析\n", PDF: &PDFMetadata{Ranges: []PDFPageRange{{Number: 2, Range: SourceRange{Start: 9, End: 22}}}}}
	for _, r := range []SourceRange{{Start: 0, End: 9}, {Start: 9, End: 22}, {Start: 22, End: 32}} {
		doc.Nodes = append(doc.Nodes, NodeRevision{LocalBodyRange: r})
	}
	fragments := SourceFragments(doc)
	if len(fragments) != 1 || fragments[0].Page != 2 || fragments[0].Text != "已读正文\n" {
		t.Fatal("PDF metadata or unread page was presented as source text", fragments)
	}
}
