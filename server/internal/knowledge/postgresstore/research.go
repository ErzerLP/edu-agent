package postgresstore

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"strings"

	"github.com/edu-agent/edu-agent/server/internal/knowledge"
	"github.com/edu-agent/edu-agent/server/internal/learningspace"
	"github.com/edu-agent/edu-agent/server/internal/pdfsource"
	"github.com/edu-agent/edu-agent/server/internal/privacy"
	"github.com/edu-agent/edu-agent/server/internal/research"
	"github.com/jackc/pgx/v5"
)

// AdoptSourceTx 在调用方已锁定运行授权的同一事务执行 knowledge 正规写入与操作审计。
// 每个来源使用全新私有集合；没有旧身份、共享或 NoteSync 发布入口。
func (s *Store) AdoptSourceTx(ctx context.Context, tx pgx.Tx, c knowledge.SourceImport) (knowledge.ImportResult, error) {
	if _, err := privacy.LockOwnerRead(ctx, tx, privacy.OwnerKnowledge); err != nil {
		return knowledge.ImportResult{}, err
	}
	if err := lockSpaceWrite(ctx, tx); err != nil {
		return knowledge.ImportResult{}, err
	}
	prepared, err := knowledge.PrepareSourceImport(c)
	if err != nil {
		return knowledge.ImportResult{}, err
	}
	if c.Metadata.SpaceID != learningspace.Scope(ctx) {
		return knowledge.ImportResult{}, scopeMissing()
	}
	ctx, err = knowledge.WithCollection(ctx, c.SourceID)
	if err != nil {
		return knowledge.ImportResult{}, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO knowledge_collections(id,name,source,shared,owner_space_id) VALUES($1,'目标研究参考',$2,FALSE,$3)`, c.SourceID, "goal_research:"+c.GoalID, learningspace.Scope(ctx)); err != nil {
		return knowledge.ImportResult{}, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO knowledge_collection_links(space_id,collection_id) VALUES($1,$2)`, learningspace.Scope(ctx), c.SourceID); err != nil {
		return knowledge.ImportResult{}, err
	}
	if err = lockCollectionWrite(ctx, tx); err != nil {
		return knowledge.ImportResult{}, err
	}
	if err = insertRevision(ctx, tx, prepared.Revision, nil); err != nil {
		return knowledge.ImportResult{}, err
	}
	if _, err = tx.Exec(ctx, `UPDATE knowledge_collections SET head_revision_id=$2 WHERE id=$1`, c.SourceID, prepared.Revision.ID); err != nil {
		return knowledge.ImportResult{}, err
	}
	hash, _ := hex.DecodeString(prepared.RequestHash)
	if _, err = tx.Exec(ctx, `INSERT INTO knowledge_import_operations(operation_id,request_hash,result_revision_id,unchanged,completed_at) VALUES($1,$2,$3,FALSE,$4)`, c.OperationID, hash, prepared.Revision.ID, prepared.Revision.CreatedAt); err != nil {
		return knowledge.ImportResult{}, err
	}
	document := prepared.Revision.Documents[0].Revision
	encoded := "    " + strings.ReplaceAll(c.Text, "\n", "\n    ") + "\n"
	if document.PDF == nil && !strings.HasSuffix(document.CanonicalMarkdown, encoded) {
		return knowledge.ImportResult{}, &knowledge.Error{Code: knowledge.CodeInvalidMarkdown}
	}
	metadata := c.Metadata
	metadata.Status = "adopted"
	metadata.CollectionID = c.SourceID
	metadata.KnowledgeRevisionID = prepared.Revision.ID
	metadata.DocumentRevisionID = document.ID
	metadata.Text = ""
	if metadata.PDF != nil {
		report := *metadata.PDF
		report.Pages = append([]pdfsource.Page{}, report.Pages...)
		for i := range report.Pages {
			report.Pages[i].Text = ""
		}
		metadata.PDF = &report
	}
	metadata.Fragments = append([]research.Fragment{}, metadata.Fragments...)
	for i := range metadata.Fragments {
		metadata.Fragments[i].Text = ""
	}
	raw, err := json.Marshal(metadata)
	if err != nil {
		return knowledge.ImportResult{}, err
	}
	start, end := len(document.CanonicalMarkdown)-len(encoded), len(document.CanonicalMarkdown)-1
	if document.PDF != nil {
		start, end = 0, 0
	}
	if _, err = tx.Exec(ctx, `INSERT INTO knowledge_source_revisions(source_id,source_revision_id,run_id,space_id,goal_id,knowledge_revision_id,document_revision_id,text_start,text_end,metadata) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`, c.SourceID, c.SourceRevisionID, c.RunID, learningspace.Scope(ctx), c.GoalID, prepared.Revision.ID, document.ID, start, end, raw); err != nil {
		return knowledge.ImportResult{}, err
	}
	return knowledge.ImportResult{Revision: prepared.Revision}, nil
}

// ResearchSourcesTx 的正文来自正式知识文档；撤销范围与隐私检查在检索之前执行。
func (s *Store) ResearchSourcesTx(ctx context.Context, tx pgx.Tx, run, goal string) ([]research.Source, error) {
	if _, err := privacy.LockOwnerRead(ctx, tx, privacy.OwnerKnowledge); err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, `SELECT s.metadata,p.canonical_markdown,s.text_start,s.text_end,p.pdf_metadata FROM knowledge_source_revisions s JOIN knowledge_document_payloads p ON p.document_revision_id=s.document_revision_id JOIN knowledge_revisions r ON r.id=s.knowledge_revision_id WHERE s.space_id=$1 AND s.goal_id=$2 AND s.run_id=$3 AND r.redacted_at IS NULL AND EXISTS(SELECT 1 FROM knowledge_collection_links l WHERE l.space_id=s.space_id AND l.collection_id=s.source_id) ORDER BY s.source_id`, learningspace.Scope(ctx), goal, run)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []research.Source{}
	for rows.Next() {
		var raw, pdfRaw []byte
		var markdown string
		var start, end int
		var source research.Source
		if err = rows.Scan(&raw, &markdown, &start, &end, &pdfRaw); err != nil {
			return nil, err
		}
		if json.Unmarshal(raw, &source) != nil || start < 0 || end > len(markdown) || end < start {
			return nil, &knowledge.Error{Code: knowledge.CodeInvalidMarkdown}
		}
		source.Text = strings.ReplaceAll(strings.TrimPrefix(markdown[start:end], "    "), "\n    ", "\n")
		if len(pdfRaw) > 0 {
			var m knowledge.PDFMetadata
			if err = json.Unmarshal(pdfRaw, &m); err != nil {
				return nil, err
			}
			source.PDF, source.Text = &m.Report, m.Report.Text()
		}
		for i := range source.Fragments {
			f := &source.Fragments[i]
			if f.Start < 0 || f.End > len(source.Text) || f.End < f.Start {
				return nil, &knowledge.Error{Code: knowledge.CodeInvalidMarkdown}
			}
			f.Text = source.Text[f.Start:f.End]
		}
		result = append(result, source)
	}
	return result, rows.Err()
}
