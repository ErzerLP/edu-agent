package knowledge

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/edu-agent/edu-agent/server/internal/pdfsource"
)

type PDFImport struct {
	Data          []byte `json:"data"`
	AcceptPartial bool   `json:"accept_partial"`
	Locator       string `json:"locator,omitempty"`
	SourceReceipt string `json:"source_receipt,omitempty"`
}

type PDFPageRange struct {
	Number int         `json:"number"`
	Range  SourceRange `json:"range"`
}

type PDFMetadata struct {
	Kind    string           `json:"kind"`
	Locator string           `json:"locator"`
	Report  pdfsource.Report `json:"report"`
	Ranges  []PDFPageRange   `json:"ranges"`
}

// 网页标识只能来自已执行网络与留存检查的抓取器，普通上传不能伪称网页获取。
func PDFSourceReceipt(data []byte, locator string) string {
	sum := sha256.Sum256(data)
	mac := hmac.New(sha256.New, identityReviewKey)
	mac.Write([]byte("pdf-source-v1\n" + locator + "\n" + hex.EncodeToString(sum[:])))
	return hex.EncodeToString(mac.Sum(nil))
}

func preparePDF(ctx context.Context, input PDFImport) (string, *PDFMetadata, error) {
	if input.Locator != "" && !hmac.Equal([]byte(input.SourceReceipt), []byte(PDFSourceReceipt(input.Data, input.Locator))) {
		return "", nil, &Error{Code: CodeInvalidRequest}
	}
	r, err := pdfsource.Parse(ctx, input.Data)
	if err != nil {
		return "", nil, &Error{Code: pdfsource.Code(err)}
	}
	if r.Text() == "" {
		return "", nil, &Error{Code: "pdf_no_usable_text"}
	}
	if r.Coverage != "complete_text" && !input.AcceptPartial {
		return "", nil, &Error{Code: "pdf_partial_confirmation_required"}
	}
	m := &PDFMetadata{Kind: "uploaded_pdf", Locator: input.Locator, Report: r, Ranges: []PDFPageRange{}}
	if input.Locator != "" {
		m.Kind = "web_pdf"
	}
	return PDFMarkdown(*m), m, nil
}

// 规范正文是原文派生；页号和指纹由服务端生成，文本不能注入标题或身份标记。
func PDFMarkdown(m PDFMetadata) string {
	metadata, _ := json.Marshal(struct {
		Provenance, Kind, Locator, Fingerprint, Parser, Coverage string
	}{"pdf_extracted_original", m.Kind, m.Locator, m.Report.Fingerprint, m.Report.Parser, m.Report.Coverage})
	var out strings.Builder
	out.WriteString("PDF 文本层提取；物理页码从 1 开始。仅文本，不保证表格、公式、图片及阅读顺序的语义。\n\n    " + string(metadata) + "\n\n")
	for _, p := range m.Report.Pages {
		fmt.Fprintf(&out, "## 第 %d 页\n\n解析状态：%s；缺口：%s\n\n", p.Number, p.Status, strings.Join(p.Gaps, ", "))
		if p.Text != "" {
			out.WriteString("    " + strings.ReplaceAll(p.Text, "\n", "\n    ") + "\n\n")
		}
	}
	return out.String()
}

func attachPDF(document *DocumentRevision, metadata *PDFMetadata, raw []byte) {
	if metadata == nil {
		return
	}
	m := *metadata
	m.Ranges = []PDFPageRange{}
	for i, p := range m.Report.Pages {
		if p.Text != "" {
			m.Ranges = append(m.Ranges, PDFPageRange{Number: p.Number, Range: document.Nodes[i+1].LocalBodyRange})
		}
	}
	document.PDF, document.PDFOriginal = &m, raw
}

type pdfReader interface {
	PDFOriginal(context.Context, string, string, int) ([]byte, error)
}

func (s *Service) PDFPage(ctx context.Context, revision, document string, number int) ([]byte, error) {
	if !validUUID(revision) || !validUUID(document) || number < 1 || number > pdfsource.MaxPages {
		return nil, &Error{Code: CodeInvalidRequest}
	}
	reader, ok := s.store.(pdfReader)
	if !ok {
		return nil, &Error{Code: CodeNotFound}
	}
	raw, err := reader.PDFOriginal(ctx, revision, document, number)
	if err != nil {
		return nil, err
	}
	image, err := pdfsource.Render(ctx, raw, number)
	if err != nil {
		return nil, &Error{Code: pdfsource.Code(err)}
	}
	return image, nil
}
