package httpapi

import (
	"net/http"

	"github.com/edu-agent/edu-agent/server/internal/privacy"
	"github.com/edu-agent/edu-agent/server/internal/research"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// 解析只返回候选，不保存正文；所有网络读取复用研究的逐跳地址与格式边界。
func (a *API) mountReferenceSources(r chi.Router) {
	fetcher := a.referenceFetcher
	if fetcher == nil {
		fetcher = research.NewFetcher()
	}
	r.With(a.requireScope("references:manage"), a.requireScope("knowledge:write"), a.responseReadPermit("content_redacted", privacy.OwnerKnowledge)).Post("/v1/knowledge/reference-sources", func(w http.ResponseWriter, r *http.Request) {
		var input struct {
			URL     string `json:"url"`
			Consent bool   `json:"external_consent"`
		}
		if err := decodeJSON(w, r, 16<<10, &input); err != nil || !input.Consent {
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
		writeJSON(w, 200, map[string]any{
			"locator": source.Locator, "final_url": source.FinalURL, "title": source.Title,
			"kind": source.Kind, "status": source.Status, "failure": source.Failure,
			"parser": source.Parser, "coverage": source.Coverage, "fingerprint": source.Fingerprint,
			"storage_allowed": source.StorageAllowed, "text": source.Text,
		})
	})
}
