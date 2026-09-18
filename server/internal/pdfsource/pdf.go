// Package pdfsource 只提取 PDF 原始文本；不运行脚本、表单或外部链接。
package pdfsource

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"image/png"
	"io"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/klippa-app/go-pdfium"
	"github.com/klippa-app/go-pdfium/enums"
	pdferrors "github.com/klippa-app/go-pdfium/errors"
	"github.com/klippa-app/go-pdfium/references"
	"github.com/klippa-app/go-pdfium/requests"
	"github.com/klippa-app/go-pdfium/webassembly"
	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/experimental"
)

const (
	Parser    = "pdfium-wasm-go-pdfium-1.20.0/text-v1"
	MaxBytes  = 4 << 20
	MaxPages  = 100
	MaxText   = 16000
	MemoryMiB = 128
	Timeout   = 10 * time.Second
)

type Page struct {
	Number      int      `json:"number"`
	Status      string   `json:"status"`
	Gaps        []string `json:"gaps"`
	Text        string   `json:"text"`
	Fingerprint string   `json:"fingerprint"`
	DuplicateOf int      `json:"duplicate_of,omitempty"`
	Start       int      `json:"start"`
	End         int      `json:"end"`
}

type Report struct {
	Parser      string `json:"parser"`
	Fingerprint string `json:"fingerprint"`
	PageCount   int    `json:"page_count"`
	Coverage    string `json:"coverage"`
	Pages       []Page `json:"pages"`
}

func (r Report) Text() string {
	var out strings.Builder
	for _, p := range r.Pages {
		if p.Text != "" {
			if out.Len() > 0 {
				out.WriteString("\n\n")
			}
			out.WriteString(p.Text)
		}
	}
	return out.String()
}

type Error struct{ Code string }

func (e *Error) Error() string { return e.Code }
func Code(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return "pdf_parse_failed"
}

var slots = make(chan struct{}, 2)

// 仅缓存可信解析器机器码；每个文件使用新的内存实例，不缓存文件或解压正文。
var compilationCache = wazero.NewCompilationCache()

func sandbox(ctx context.Context, data []byte, fn func(context.Context, pdfium.Pdfium, references.FPDF_DOCUMENT, int) error) (err error) {
	if len(data) > MaxBytes {
		return &Error{"pdf_file_limit"}
	}
	if !bytes.HasPrefix(data, []byte("%PDF-")) || !bytes.Contains(data[max(0, len(data)-1024):], []byte("%%EOF")) {
		return &Error{"pdf_invalid"}
	}
	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()
	select {
	case slots <- struct{}{}:
		defer func() { <-slots }()
	case <-ctx.Done():
		return &Error{"pdf_timeout"}
	}
	defer func() {
		if recover() != nil {
			err = &Error{"pdf_resource_limit"}
		}
		if ctx.Err() != nil {
			err = &Error{"pdf_timeout"}
		}
	}()
	pool, err := webassembly.Init(webassembly.Config{
		Context: ctx, MaxTotal: 1, FSConfig: wazero.NewFSConfig(), Stdout: io.Discard, Stderr: io.Discard,
		RuntimeConfig: wazero.NewRuntimeConfig().WithCoreFeatures(api.CoreFeaturesV2 | experimental.CoreFeaturesExceptionHandling).
			WithMemoryLimitPages(MemoryMiB * 16).WithCloseOnContextDone(true).WithCompilationCache(compilationCache),
	})
	if err != nil {
		return &Error{"pdf_resource_limit"}
	}
	defer pool.Close()
	instance, err := pool.GetInstanceWithContext(ctx)
	if err != nil {
		return &Error{"pdf_resource_limit"}
	}
	defer instance.Close()
	doc, err := instance.OpenDocument(&requests.OpenDocument{File: &data})
	if err != nil {
		if errors.Is(err, pdferrors.ErrPassword) || errors.Is(err, pdferrors.ErrSecurity) {
			return &Error{"pdf_encrypted"}
		}
		return &Error{"pdf_invalid_or_resource_limit"}
	}
	security, err := instance.FPDF_GetSecurityHandlerRevision(&requests.FPDF_GetSecurityHandlerRevision{Document: doc.Document})
	if err != nil {
		return &Error{"pdf_parse_failed"}
	}
	if security.SecurityHandlerRevision != -1 {
		return &Error{"pdf_encrypted"}
	}
	xref, err := instance.FPDF_DocumentHasValidCrossReferenceTable(&requests.FPDF_DocumentHasValidCrossReferenceTable{Document: doc.Document})
	if err != nil || !xref.DocumentHasValidCrossReferenceTable {
		return &Error{"pdf_invalid"}
	}
	count, err := instance.FPDF_GetPageCount(&requests.FPDF_GetPageCount{Document: doc.Document})
	if err != nil || count.PageCount < 1 {
		return &Error{"pdf_invalid"}
	}
	if count.PageCount > MaxPages {
		return &Error{"pdf_page_limit"}
	}
	return fn(ctx, instance, doc.Document, count.PageCount)
}

func Parse(ctx context.Context, data []byte) (Report, error) {
	r := Report{Parser: Parser, Fingerprint: fingerprint(data), Coverage: "complete_text", Pages: []Page{}}
	err := sandbox(ctx, data, func(ctx context.Context, instance pdfium.Pdfium, doc references.FPDF_DOCUMENT, count int) error {
		r.PageCount = count
		used := 0
		seen := map[string]int{}
		for n := 1; n <= count; n++ {
			if ctx.Err() != nil {
				return &Error{"pdf_timeout"}
			}
			p := parsePage(instance, requests.Page{ByIndex: &requests.PageByIndex{Document: doc, Index: n - 1}}, n)
			separator := 0
			if used > 0 {
				separator = 2
			}
			if len(p.Text)+used+separator > MaxText {
				p.Text = p.Text[:max(0, MaxText-used-separator)]
				for !utf8.ValidString(p.Text) {
					p.Text = p.Text[:len(p.Text)-1]
				}
				p.Status = "partial"
				p.Gaps = append(p.Gaps, "text_limit")
			}
			if p.Text != "" {
				p.Start = used + separator
				p.End = p.Start + len(p.Text)
				used = p.End
				p.Fingerprint = fingerprint([]byte(p.Text))
				p.DuplicateOf = seen[p.Fingerprint]
				if p.DuplicateOf == 0 {
					seen[p.Fingerprint] = n
				}
			}
			if p.Status != "text" {
				r.Coverage = "partial_pdf"
			}
			r.Pages = append(r.Pages, p)
		}
		return nil
	})
	if err != nil {
		return Report{}, err
	}
	return r, nil
}

func parsePage(instance pdfium.Pdfium, page requests.Page, number int) Page {
	p := Page{Number: number, Status: "failed", Gaps: []string{}, Text: ""}
	textPage, err := instance.FPDFText_LoadPage(&requests.FPDFText_LoadPage{Page: page})
	if err != nil {
		p.Gaps = append(p.Gaps, "page_parse_failed")
		return p
	}
	defer instance.FPDFText_ClosePage(&requests.FPDFText_ClosePage{TextPage: textPage.TextPage})
	count, err := instance.FPDFText_CountChars(&requests.FPDFText_CountChars{TextPage: textPage.TextPage})
	if err != nil || count.Count < 0 {
		p.Gaps = append(p.Gaps, "page_parse_failed")
		return p
	}
	if count.Count == 0 {
		p.Status = "no_text"
		p.Gaps = append(p.Gaps, "no_text_layer_or_blank")
		return p
	}
	p.Status = "text"
	if count.Count > MaxText {
		p.Gaps = append(p.Gaps, "text_limit")
	}
	text, err := instance.FPDFText_GetText(&requests.FPDFText_GetText{TextPage: textPage.TextPage, Count: min(count.Count, MaxText)})
	if err != nil {
		p.Status = "failed"
		p.Gaps = append(p.Gaps, "text_decode_failed")
		return p
	}
	p.Text = strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(text.Text, "\r\n", "\n"), "\r", "\n"))
	if strings.ContainsAny(p.Text, "\x00\ufffd") {
		p.Gaps = append(p.Gaps, "unicode_mapping")
		p.Text = strings.ReplaceAll(p.Text, "\x00", "�")
	}
	// 字符映射缺失不能用空字符充当已正确读取的正文。
	for i := 0; i < min(count.Count, MaxText); i++ {
		c, e := instance.FPDFText_GetUnicode(&requests.FPDFText_GetUnicode{TextPage: textPage.TextPage, Index: i})
		if e != nil || c.Unicode == 0 {
			p.Gaps = append(p.Gaps, "unicode_mapping")
			break
		}
	}
	objects, err := instance.FPDFPage_CountObjects(&requests.FPDFPage_CountObjects{Page: page})
	if err != nil || objects.Count > 10000 {
		p.Gaps = append(p.Gaps, "layout_not_verified")
	} else {
		for i := 0; i < objects.Count; i++ {
			o, e := instance.FPDFPage_GetObject(&requests.FPDFPage_GetObject{Page: page, Index: i})
			if e != nil {
				p.Gaps = append(p.Gaps, "layout_not_verified")
				break
			}
			kind, e := instance.FPDFPageObj_GetType(&requests.FPDFPageObj_GetType{PageObject: o.PageObject})
			if e != nil || kind.Type != enums.FPDF_PAGEOBJ_TEXT {
				p.Gaps = append(p.Gaps, "images_tables_formulas_or_layout")
				break
			}
			font, e := instance.FPDFTextObj_GetFont(&requests.FPDFTextObj_GetFont{PageObject: o.PageObject})
			if e != nil {
				p.Gaps = append(p.Gaps, "font_not_embedded")
				break
			}
			embedded, e := instance.FPDFFont_GetIsEmbedded(&requests.FPDFFont_GetIsEmbedded{Font: font.Font})
			if e != nil || !embedded.IsEmbedded {
				// 沙箱不读取宿主字体，替代字体可能缺字；原文提取不能冒充准确页图。
				p.Gaps = append(p.Gaps, "font_not_embedded")
				break
			}
		}
	}
	if p.Text == "" {
		p.Status = "no_text"
		p.Gaps = append(p.Gaps, "no_extractable_text")
	} else if len(p.Gaps) > 0 {
		p.Status = "partial"
	}
	return p
}

func Render(ctx context.Context, data []byte, number int) ([]byte, error) {
	var out bytes.Buffer
	err := sandbox(ctx, data, func(_ context.Context, instance pdfium.Pdfium, doc references.FPDF_DOCUMENT, count int) error {
		if number < 1 || number > count {
			return &Error{"pdf_page_not_found"}
		}
		result, err := instance.RenderPageInPixels(&requests.RenderPageInPixels{Page: requests.Page{ByIndex: &requests.PageByIndex{Document: doc, Index: number - 1}}, Width: 1000, Height: 1400})
		if err != nil {
			return &Error{"pdf_render_failed"}
		}
		defer result.Cleanup()
		return png.Encode(&out, result.Result.RenderedImage)
	})
	return out.Bytes(), err
}

func fingerprint(data []byte) string { sum := sha256.Sum256(data); return hex.EncodeToString(sum[:]) }
