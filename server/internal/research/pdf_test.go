package research

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/edu-agent/edu-agent/server/internal/pdffixture"
)

func TestFetchPDFOriginalPagesAndRestrictions(t *testing.T) {
	for _, restricted := range []bool{false, true} {
		f, calls := fixtureFetcher(t, func(w http.ResponseWriter, r *http.Request) {
			if !strings.Contains(r.Header.Get("Accept"), "application/pdf") {
				t.Error("未声明 PDF 能力")
			}
			w.Header().Set("Content-Type", "application/pdf")
			if restricted {
				w.Header().Set("Cache-Control", "no-store")
			}
			_, _ = w.Write(pdffixture.Build("Chinese 中文 evidence", "", "第三页"))
		})
		s, err := f.Fetch(context.Background(), Source{Locator: "http://example.com/material.pdf"}, Policy{Mode: "supplement"}, func() error { return nil })
		if err != nil || *calls != 1 {
			t.Fatal(err)
		}
		if restricted {
			if s.Failure != "storage_restricted" || s.PDF != nil || len(s.PDFOriginal) != 0 || s.Text != "" {
				t.Fatal("禁止保存的 PDF 进入缓存", s)
			}
			continue
		}
		if s.Status != "partial" || s.PDF == nil || s.PDF.PageCount != 3 || len(s.PDFOriginal) == 0 || s.Fragments[0].Page != 1 || s.Fragments[len(s.Fragments)-1].Page != 3 {
			t.Fatal("PDF 来源页序丢失", s)
		}
		for _, fragment := range s.Fragments {
			if s.Text[fragment.Start:fragment.End] != fragment.Text {
				t.Fatal("网页 PDF 片段偏移错误")
			}
		}
	}
}
