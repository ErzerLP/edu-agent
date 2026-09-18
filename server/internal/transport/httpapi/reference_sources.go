package httpapi

import (
	"context"
	"net/http"
	"strconv"

	"github.com/edu-agent/edu-agent/server/internal/knowledge"
	"github.com/edu-agent/edu-agent/server/internal/pdfsource"
	"github.com/edu-agent/edu-agent/server/internal/privacy"
	"github.com/edu-agent/edu-agent/server/internal/research"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// 解析只返回候选，不保存正文；所有网络读取复用研究的逐跳地址与格式边界。
func (a *API) mountReferenceSources(r chi.Router) {
	r.With(a.requireScope("knowledge:read")).Get("/v1/knowledge/source-capabilities", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"formats": []string{"markdown", "utf8_text", "html", "pdf_text_layer"}, "pdf": map[string]any{
			"parser": pdfsource.Parser, "max_bytes": pdfsource.MaxBytes, "max_pages": pdfsource.MaxPages, "max_text_bytes": pdfsource.MaxText,
			"memory_mib": pdfsource.MemoryMiB, "timeout_seconds": int(pdfsource.Timeout.Seconds()), "concurrency": 2,
			"web_max_wire_bytes": research.MaxWire, "web_max_decoded_bytes": research.MaxDecoded,
			"ocr": false, "encrypted": false, "viewer": "server_png", "original_export": false,
			"limitations": []string{"仅物理页码，不使用印刷页标签", "表格、公式、图片与复杂阅读顺序需对照页图核对", "未嵌入字体会标记缺口，页图可能缺字或替代字形", "原件仅用于服务端安全页图，CLI 使用规范文本"},
		}})
	})
	if pages, ok := a.knowledge.(interface {
		PDFPage(context.Context, string, string, int) ([]byte, error)
	}); ok {
		r.With(a.requireScope("knowledge:read"), a.responseReadPermit("content_redacted", privacy.OwnerKnowledge)).Get("/v1/knowledge/revisions/{revisionID}/documents/{documentID}/pages/{page}", func(w http.ResponseWriter, r *http.Request) {
			n, err := strconv.Atoi(chi.URLParam(r, "page"))
			if err != nil {
				writeError(w, r, 400, "invalid_request", "页码无效")
				return
			}
			data, err := pages.PDFPage(r.Context(), chi.URLParam(r, "revisionID"), chi.URLParam(r, "documentID"), n)
			if err != nil {
				a.writeKnowledgeFailure(w, r, "pdf_page", err)
				return
			}
			w.Header().Set("Content-Type", "image/png")
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("X-Content-Type-Options", "nosniff")
			_, _ = w.Write(data)
		})
	}
	fetcher := a.referenceFetcher
	if fetcher == nil {
		fetcher = research.NewFetcher()
	}
	r.With(a.requireScope("references:manage"), a.requireScope("knowledge:write"), a.responseReadPermit("content_redacted", privacy.OwnerKnowledge)).Post("/v1/knowledge/reference-sources", func(w http.ResponseWriter, r *http.Request) {
		var input struct {
			URL            string `json:"url"`
			Consent        bool   `json:"external_consent"`
			PDFData        []byte `json:"pdf_data"`
			StorageConsent bool   `json:"storage_consent"`
		}
		if err := decodeJSON(w, r, 6<<20, &input); err != nil {
			importInputFailure(w, r, err)
			return
		}
		if input.PDFData != nil {
			if input.URL != "" || !input.StorageConsent {
				writeError(w, r, 400, "invalid_request", "上传 PDF 需确认有权保存原件与提取文本")
				return
			}
			report, err := pdfsource.Parse(r.Context(), input.PDFData)
			if err != nil {
				writeError(w, r, 422, pdfsource.Code(err), "PDF 未解析，请核对格式或资源限制")
				return
			}
			status := "parsed"
			if report.Coverage != "complete_text" {
				status = "partial"
			}
			writeJSON(w, 200, map[string]any{"locator": "", "final_url": "", "title": "", "kind": "application/pdf", "status": status, "failure": "", "parser": report.Parser, "coverage": report.Coverage, "fingerprint": report.Fingerprint, "storage_allowed": true, "text": report.Text(), "pdf": report})
			return
		}
		if !input.Consent {
			writeError(w, r, 400, "invalid_request", "读取网页需要明确授权")
			return
		}
		if _, err := research.ValidateURL(input.URL); err != nil {
			writeError(w, r, 400, "research_policy_rejected", "网页地址不符合公开网络边界")
			return
		}
		source, err := fetcher.Fetch(r.Context(), research.Source{ID: uuid.NewString(), Locator: input.URL}, research.Policy{Mode: "supplement", Domains: []string{}}, func() error { return nil })
		if err != nil {
			writeError(w, r, 502, "reference_source_failed", "网页解析未完成，未导入资料")
			return
		}
		result := map[string]any{
			"locator": source.Locator, "final_url": source.FinalURL, "title": source.Title,
			"kind": source.Kind, "status": source.Status, "failure": source.Failure,
			"parser": source.Parser, "coverage": source.Coverage, "fingerprint": source.Fingerprint,
			"storage_allowed": source.StorageAllowed, "text": source.Text,
		}
		if source.PDF != nil {
			result["pdf"], result["pdf_data"] = source.PDF, source.PDFOriginal
			result["source_receipt"] = knowledge.PDFSourceReceipt(source.PDFOriginal, source.FinalURL)
		}
		writeJSON(w, 200, result)
	})
}
