package postgresstore

import (
	"context"

	"github.com/edu-agent/edu-agent/server/internal/knowledge"
)

// PDFOriginal 先按既有集合/冻结范围和隐私代次核对权限，再读取原件。
func (s *Store) PDFOriginal(ctx context.Context, revision, document string, page int) ([]byte, error) {
	tx, err := s.beginPrivacyRead(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(context.Background())
	r, err := loadScopedRevision(ctx, tx, revision)
	if err != nil {
		return nil, err
	}
	if r.Redacted {
		return nil, &knowledge.Error{Code: knowledge.CodeContentRedacted}
	}
	for _, d := range r.Documents {
		if d.Revision.ID != document || d.Revision.PDF == nil || page > d.Revision.PDF.Report.PageCount {
			continue
		}
		if d.CollectionID != "" {
			if err = checkCollection(ctx, tx, d.CollectionID); err != nil {
				return nil, err
			}
		}
		if d.SelectedRange != nil {
			allowed := false
			for _, p := range d.Revision.PDF.Ranges {
				if p.Number == page && p.Range.Start >= d.SelectedRange.Start && p.Range.End <= d.SelectedRange.End {
					allowed = true
				}
			}
			if !allowed {
				return nil, &knowledge.Error{Code: knowledge.CodeNotFound}
			}
		}
		var raw []byte
		if err = tx.QueryRow(ctx, `SELECT pdf_original FROM knowledge_document_payloads WHERE document_revision_id=$1`, document).Scan(&raw); err != nil {
			return nil, err
		}
		if len(raw) == 0 {
			return nil, &knowledge.Error{Code: knowledge.CodeNotFound}
		}
		return raw, tx.Commit(ctx)
	}
	return nil, &knowledge.Error{Code: knowledge.CodeNotFound}
}
