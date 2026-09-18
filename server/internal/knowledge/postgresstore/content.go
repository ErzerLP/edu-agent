package postgresstore

import (
	"context"
	"errors"
	"unicode/utf8"

	"github.com/edu-agent/edu-agent/server/internal/knowledge"
	"github.com/edu-agent/edu-agent/server/internal/learning"
	"github.com/edu-agent/edu-agent/server/internal/learningcontent"
	"github.com/edu-agent/edu-agent/server/internal/learningspace"
	"github.com/edu-agent/edu-agent/server/internal/pdfsource"
	"github.com/edu-agent/edu-agent/server/internal/privacy"
	"github.com/jackc/pgx/v5"
)

// ContentCitationTx 重新读取正规资料，旧副本不能绕过清除或撤销集合关联。
func (s *Store) ContentCitationTx(ctx context.Context, tx pgx.Tx, ref learning.KnowledgeReference) (learningcontent.Citation, error) {
	if _, err := privacy.LockOwnerRead(ctx, tx, privacy.OwnerKnowledge); err != nil {
		return learningcontent.Citation{}, err
	}
	var collection *string
	err := tx.QueryRow(ctx, `SELECT collection_id FROM knowledge_revisions WHERE id=$1 AND redacted_at IS NULL`, ref.KnowledgeRevisionID).Scan(&collection)
	if errors.Is(err, pgx.ErrNoRows) {
		return learningcontent.Citation{}, learningcontent.ErrNotFound
	}
	if err != nil {
		return learningcontent.Citation{}, err
	}
	collections := []string{}
	if collection != nil {
		collections = append(collections, *collection)
		ctx, err = knowledge.WithCollection(ctx, *collection)
		if err != nil {
			return learningcontent.Citation{}, err
		}
	} else {
		scope, e := readScope(ctx, tx, ref.KnowledgeRevisionID)
		if e != nil {
			return learningcontent.Citation{}, learningcontent.ErrNotFound
		}
		for _, entry := range scope.Entries {
			collections = append(collections, entry.CollectionID)
		}
	}
	for _, id := range collections {
		var linked string
		if err = tx.QueryRow(ctx, `SELECT collection_id FROM knowledge_collection_links WHERE space_id=$1 AND collection_id=$2 FOR SHARE`, learningspace.Scope(ctx), id).Scan(&linked); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return learningcontent.Citation{}, learningcontent.ErrForbidden
			}
			return learningcontent.Citation{}, err
		}
	}
	revision, err := loadScopedRevision(ctx, tx, ref.KnowledgeRevisionID)
	if err != nil {
		return learningcontent.Citation{}, err
	}
	if revision.Redacted {
		return learningcontent.Citation{}, learningcontent.ErrNotFound
	}
	for _, doc := range revision.Documents {
		if ref.DocumentRevisionID != "" && ref.DocumentRevisionID != doc.Revision.ID {
			continue
		}
		for _, node := range doc.Revision.Nodes {
			if node.ID != ref.NodeRevisionID || node.NodeID != ref.NodeID {
				continue
			}
			start, end := ref.Range.Start, ref.Range.End
			text := doc.Revision.CanonicalMarkdown
			if start < node.SectionRange.Start || end > node.SectionRange.End || start < 0 || end < start || end > len(text) {
				return learningcontent.Citation{}, learningcontent.ErrInvalid
			}
			if doc.SelectedRange != nil && (start < doc.SelectedRange.Start || end > doc.SelectedRange.End) {
				return learningcontent.Citation{}, learningcontent.ErrForbidden
			}
			result := learningcontent.Citation{Reference: ref, Title: node.Title, Status: "available", Parser: revision.ParserVersion, Historical: true, Coverage: "旧资料未记录解析覆盖范围", Locator: doc.Path}
			var parser, coverage, locator string
			err = tx.QueryRow(ctx, `SELECT metadata->>'parser',metadata->>'coverage',metadata->>'locator' FROM knowledge_source_revisions WHERE document_revision_id=$1 AND space_id=$2`, doc.Revision.ID, learningspace.Scope(ctx)).Scan(&parser, &coverage, &locator)
			if err == nil {
				result.Parser = parser
				result.Coverage = coverage
				result.Locator = locator
			} else if !errors.Is(err, pgx.ErrNoRows) {
				return learningcontent.Citation{}, err
			}
			if result.Parser == "" && doc.KnowledgeRevisionID != "" {
				if err = tx.QueryRow(ctx, `SELECT parser_version FROM knowledge_revisions WHERE id=$1`, doc.KnowledgeRevisionID).Scan(&result.Parser); err != nil {
					return learningcontent.Citation{}, err
				}
			}
			if ref.Slice == "" || start == end {
				result.Status = "missing_fragment"
				return result, nil
			}
			if text[start:end] != ref.Slice || learningcontent.TextHash(ref.Slice) != ref.SliceSHA256 {
				return learningcontent.Citation{}, learningcontent.ErrInvalid
			}
			lo, hi := max(node.SectionRange.Start, start-400), min(node.SectionRange.End, end+400)
			if doc.SelectedRange != nil {
				lo = max(lo, doc.SelectedRange.Start)
				hi = min(hi, doc.SelectedRange.End)
			}
			for lo < start && !utf8.RuneStart(text[lo]) {
				lo++
			}
			for hi < len(text) && hi > end && !utf8.RuneStart(text[hi]) {
				hi--
			}
			result.Context = text[lo:hi]
			if m := doc.Revision.PDF; m != nil {
				result.Parser, result.Coverage = m.Report.Parser, m.Report.Coverage
				if m.Locator != "" {
					result.Locator = m.Locator
				}
				result.PDF = &learningcontent.PDFCitation{Fingerprint: m.Report.Fingerprint, PageCount: m.Report.PageCount, Pages: []pdfsource.Page{}}
				if collection != nil {
					result.PDF.CollectionID = *collection
				}
				for _, p := range m.Ranges {
					if start < p.Range.End && end > p.Range.Start {
						page := m.Report.Pages[p.Number-1]
						// 章节权限仅提供其已获准的片段；完整页图另经范围校验。
						page.Text = ""
						result.PDF.Pages = append(result.PDF.Pages, page)
					}
				}
			}
			return result, nil
		}
	}
	return learningcontent.Citation{}, learningcontent.ErrNotFound
}
