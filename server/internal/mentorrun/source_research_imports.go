package mentorrun

import (
	"context"
	"errors"
	"fmt"
	"github.com/edu-agent/edu-agent/server/internal/identity"
	"github.com/edu-agent/edu-agent/server/internal/knowledge"
	knowledgepg "github.com/edu-agent/edu-agent/server/internal/knowledge/postgresstore"
	"github.com/edu-agent/edu-agent/server/internal/learningspace"
	"github.com/edu-agent/edu-agent/server/internal/research"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"strings"
	"time"
)

type SourceResearchImportState struct {
	Preview SourceResearchPreview        `json:"preview"`
	Items   []SourceResearchImportStatus `json:"items"`
}

type SourceResearchImportStatus struct {
	ItemID string                    `json:"item_id"`
	Status string                    `json:"status"`
	Source *knowledge.SourceIdentity `json:"source,omitempty"`
}

func (s *Service) markSourceResearchImportedTx(ctx context.Context, tx pgx.Tx, item row, receipt WebImportReceipt) error {
	id := uuid.NewSHA1(uuid.MustParse(item.SourceResearchJobID), []byte(receipt.RevisionID)).String()
	var raw []byte
	if err := tx.QueryRow(ctx, `SELECT ciphertext FROM learning_source_research_candidates WHERE id=$1 AND job_id=$2`, id, item.SourceResearchJobID).Scan(&raw); err != nil {
		return err
	}
	var candidate sourceResearchCandidateRecord
	if err := s.openWeb("source_research_candidates", id, item.Generation, raw, &candidate); err != nil {
		return err
	}
	candidate.Source = &receipt.Source
	raw, err := s.sealWeb("source_research_candidates", id, item.Generation, candidate)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE learning_source_research_candidates SET ciphertext=$2 WHERE id=$1`, id, raw)
	return err
}

func (s *Service) SourceResearchImports(ctx context.Context, actor identity.Credential, space, id, previewID string) (SourceResearchImportState, error) {
	if !validID(previewID) {
		return SourceResearchImportState{}, ErrInvalid
	}
	tx, g, err := s.sourceResearchTx(ctx, actor, space, id, false)
	if err != nil {
		return SourceResearchImportState{}, err
	}
	defer tx.Rollback(context.Background())
	item, _, _, _, _, err := s.sourceResearchRowTx(ctx, tx, actor, space, id, g, false)
	if err != nil {
		return SourceResearchImportState{}, err
	}
	var raw []byte
	if err = tx.QueryRow(ctx, `SELECT ciphertext FROM learning_source_research_previews WHERE id=$1 AND job_id=$2`, previewID, id).Scan(&raw); err != nil {
		return SourceResearchImportState{}, ErrNotFound
	}
	var preview SourceResearchPreview
	if err = s.openWeb("source_research_previews", previewID, g, raw, &preview); err != nil {
		return SourceResearchImportState{}, err
	}
	result := []SourceResearchImportStatus{}
	for _, entry := range preview.Items {
		status := SourceResearchImportStatus{ItemID: entry.ID, Status: "pending"}
		var receiptID string
		if entry.Kind == "document" {
			err = tx.QueryRow(ctx, `SELECT id::text,ciphertext FROM learning_web_import_receipts WHERE owner_id IN (SELECT id FROM learning_mentor_runs WHERE source_research_job_id=$1) AND revision_id=$2 ORDER BY id LIMIT 1`, item.SourceResearchJobID, entry.RevisionID).Scan(&receiptID, &raw)
		} else {
			err = tx.QueryRow(ctx, `SELECT id::text,ciphertext FROM learning_source_research_imports WHERE job_id=$1 AND report_id=$2`, id, entry.ID).Scan(&receiptID, &raw)
		}
		if err == nil {
			var receipt WebImportReceipt
			kind := "receipt"
			if entry.Kind == "report" {
				kind = "source_research_imports"
			}
			if err = s.openWeb(kind, receiptID, g, raw, &receipt); err != nil {
				return SourceResearchImportState{}, err
			}
			status.Status = "imported"
			status.Source = &receipt.Source
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return SourceResearchImportState{}, err
		}
		result = append(result, status)
	}
	return SourceResearchImportState{Preview: preview, Items: result}, nil
}

func (s *Service) PreviewSourceResearch(ctx context.Context, actor identity.Credential, space, id string, c SourceResearchPreviewRequest) (SourceResearchPreview, error) {
	result := SourceResearchPreview{}
	if !validID(c.OperationID) || !validID(c.ResultSnapshotID) || c.ExpectedSelectionVersion < 1 {
		return result, ErrInvalid
	}
	tx, g, err := s.sourceResearchTx(ctx, actor, space, id, true)
	if err != nil {
		return result, err
	}
	defer tx.Rollback(context.Background())
	item, _, selection, _, selectedReport, err := s.sourceResearchRowTx(ctx, tx, actor, space, id, g, true)
	if err != nil {
		return result, err
	}
	if err = sourceActor(ctx, tx, actor.TokenID); err != nil {
		return result, err
	}
	if err = knowledgeActor(ctx, tx, actor.Device.ID, actor.TokenID); err != nil {
		return result, err
	}
	hash := requestHash("source_research_preview", space, id, c)
	if found, e := s.webOperation(ctx, tx, actor.Device.ID, space, c.OperationID, g, hash, &result); e != nil || found {
		if found && time.Now().After(result.ExpiresAt) {
			return result, ErrConflict
		}
		if found {
			var exists bool
			if e = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM learning_source_research_previews WHERE id=$1)`, result.ID).Scan(&exists); e != nil {
				return result, e
			}
			if !exists {
				return result, ErrConflict
			}
		}
		return result, e
	}
	if sourceResearchActive(sourceResearchStatus(item)) || selection != c.ExpectedSelectionVersion || c.ResultSnapshotID != item.body.SourceResearch.ResultSnapshotID {
		return result, ErrConflict
	}
	snapshot, err := s.sourceResearchResultTx(ctx, tx, item, c.ResultSnapshotID)
	if err != nil {
		return result, err
	}
	result = SourceResearchPreview{ID: uuid.NewString(), Version: 1, ExpiresAt: time.Now().UTC().Add(15 * time.Minute), ResultSnapshotID: c.ResultSnapshotID, Items: []SourceResearchPreviewItem{}}
	if result.ExpiresAt.After(item.ExpiresAt) {
		result.ExpiresAt = item.ExpiresAt
	}
	for _, candidateID := range snapshot.CandidateIDs {
		candidate, e := s.sourceResearchCandidateTx(ctx, tx, item, candidateID)
		if e != nil {
			return result, e
		}
		if len(result.Items) >= item.body.SourceResearch.Limits.MaxSourcesPerRound {
			break
		}
		if !candidate.Selected {
			continue
		}
		if candidate.Source != nil {
			continue
		}
		d, e := s.webDocumentTx(ctx, tx, item, candidate.DocumentID)
		if e != nil {
			return result, e
		}
		preview, e := s.createWebPreviewTx(ctx, tx, item, d)
		if e != nil {
			return result, e
		}
		result.Items = append(result.Items, SourceResearchPreviewItem{ID: candidateID, Kind: "document", OperationID: uuid.NewSHA1(uuid.MustParse(result.ID), []byte(candidateID)).String(), WebPreviewID: preview.ID, RevisionID: d.RevisionID, Title: d.Title, Coverage: d.Coverage, RequiresPartialAccept: preview.Items[0].RequiresPartialAccept, AlreadyImported: candidate.Source != nil, Source: candidate.Source})
	}
	if selectedReport != "" {
		report, e := s.sourceResearchReportTx(ctx, tx, item, selectedReport, 0, 1)
		if e != nil || len(report.Sections) == 0 {
			return result, ErrConflict
		}
		result.ReportRevisionID = selectedReport
		result.Items = append(result.Items, SourceResearchPreviewItem{ID: selectedReport, Kind: "report", OperationID: uuid.NewSHA1(uuid.MustParse(result.ID), []byte(selectedReport)).String(), Title: "整理报告", Coverage: report.Status, RequiresPartialAccept: report.Status != "complete"})
	}
	if len(result.Items) == 0 {
		return result, ErrInvalid
	}
	raw, err := s.sealWeb("source_research_previews", result.ID, g, result)
	if err != nil {
		return result, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO learning_source_research_previews(id,job_id,privacy_generation,ciphertext,expires_at) VALUES($1,$2,$3,$4,$5)`, result.ID, id, g, raw, result.ExpiresAt); err != nil {
		return result, err
	}
	if err = s.saveWebOperation(ctx, tx, actor.Device.ID, space, "", item.RunID, c.OperationID, g, hash, result); err != nil {
		return result, err
	}
	return result, tx.Commit(ctx)
}

// The same store/schema is used for mentor and research previews.
func (s *Service) createWebPreviewTx(ctx context.Context, tx pgx.Tx, item row, d research.WebDocument) (WebPreview, error) {
	result := WebPreview{ID: uuid.NewString(), Version: 1, ExpiresAt: time.Now().UTC().Add(15 * time.Minute), Items: []WebPreviewItem{{RevisionID: d.RevisionID, Hash: d.SHA256, Coverage: d.Coverage, RequiresPartialAccept: d.PDF != nil && d.Coverage != "complete_text", Document: d}}}
	if result.ExpiresAt.After(item.ExpiresAt) {
		result.ExpiresAt = item.ExpiresAt
	}
	stored := result
	stored.Items = append([]WebPreviewItem{}, result.Items...)
	stored.Items[0].Document = research.WebDocument{}
	raw, err := s.sealWeb("preview", result.ID, item.Generation, stored)
	if err != nil {
		return result, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO learning_web_import_previews(id,owner_id,document_id,device_id,space_id,privacy_generation,ciphertext,expires_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, result.ID, item.RunID, d.ID, item.device, item.SpaceID, item.Generation, raw, result.ExpiresAt)
	return result, err
}
func (s *Service) sourceResearchImportGateTx(ctx context.Context, tx pgx.Tx, item row, r WebImportRequest) error {
	if item.SourceResearchJobID == "" {
		return nil
	}
	var live bool
	if err := tx.QueryRow(ctx, `SELECT NOT deleted AND expires_at>clock_timestamp() FROM learning_source_research_jobs WHERE id=$1 AND privacy_generation=$2 FOR SHARE`, item.SourceResearchJobID, item.Generation).Scan(&live); err != nil {
		return err
	}
	if !live {
		return ErrNotFound
	}
	rows, err := tx.Query(ctx, `SELECT id::text,ciphertext FROM learning_source_research_previews WHERE job_id=$1 AND expires_at>clock_timestamp()`, item.SourceResearchJobID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var raw []byte
		var preview SourceResearchPreview
		if err = rows.Scan(&id, &raw); err != nil {
			return err
		}
		if err = s.openWeb("source_research_previews", id, item.Generation, raw, &preview); err != nil {
			return err
		}
		for _, selected := range preview.Items {
			if selected.WebPreviewID == r.PreviewID && selected.OperationID == r.OperationID && selected.RevisionID == r.RevisionID {
				return nil
			}
		}
	}
	if err = rows.Err(); err != nil {
		return err
	}
	return ErrConflict
}
func (s *Service) ImportSourceResearchReport(ctx context.Context, actor identity.Credential, space, id string, c SourceResearchReportImport) (WebImportReceipt, error) {
	result := WebImportReceipt{}
	if !validID(c.OperationID) || !validID(c.PreviewID) || !validID(c.ReportRevisionID) || c.ExpectedVersion != 1 || !c.StorageConsent {
		return result, ErrInvalid
	}
	tx, g, err := s.sourceResearchTx(ctx, actor, space, id, true)
	if err != nil {
		return result, err
	}
	defer tx.Rollback(context.Background())
	item, _, _, _, _, err := s.sourceResearchRowTx(ctx, tx, actor, space, id, g, true)
	if err != nil {
		return result, err
	}
	if err = sourceActor(ctx, tx, actor.TokenID); err != nil {
		return result, err
	}
	if err = knowledgeActor(ctx, tx, actor.Device.ID, actor.TokenID); err != nil {
		return result, err
	}
	hash := requestHash("source_research_report_import", space, id, c)
	if found, e := s.webOperation(ctx, tx, actor.Device.ID, space, c.OperationID, g, hash, &result); e != nil || found {
		return result, e
	}
	var raw []byte
	if err = tx.QueryRow(ctx, `SELECT ciphertext FROM learning_source_research_previews WHERE id=$1 AND job_id=$2 AND privacy_generation=$3 AND expires_at>clock_timestamp()`, c.PreviewID, id, g).Scan(&raw); err != nil {
		return result, ErrConflict
	}
	var preview SourceResearchPreview
	if err = s.openWeb("source_research_previews", c.PreviewID, g, raw, &preview); err != nil {
		return result, err
	}
	allowed := false
	for _, entry := range preview.Items {
		if entry.Kind == "report" && entry.ID == c.ReportRevisionID && entry.OperationID == c.OperationID && (!entry.RequiresPartialAccept || c.AcceptPartial) {
			allowed = true
		}
	}
	if !allowed {
		return result, ErrConflict
	}
	var receiptID string
	err = tx.QueryRow(ctx, `SELECT id::text,ciphertext FROM learning_source_research_imports WHERE job_id=$1 AND report_id=$2`, id, c.ReportRevisionID).Scan(&receiptID, &raw)
	if err == nil {
		if err = s.openWeb("source_research_imports", receiptID, g, raw, &result); err != nil {
			return result, err
		}
	} else if errors.Is(err, pgx.ErrNoRows) {
		report, e := s.sourceResearchReportTx(ctx, tx, item, c.ReportRevisionID, 0, int(^uint(0)>>1)-1)
		if e != nil {
			return result, e
		}
		markdown := "# 整理报告\n\n由 AI 根据所列资料整理。报告只保留引用片段和原网页地址；未另行导入的网页不提供全文或 PDF 页图。\n\n"
		original, e := scan(tx.QueryRow(ctx, `SELECT `+columns+` FROM learning_mentor_runs WHERE id=(SELECT run_id FROM learning_source_research_rounds WHERE id=$1)`, report.RoundID))
		if e != nil {
			return result, e
		}
		if e = s.decode(&original); e != nil {
			return result, e
		}
		markdown += "搜集需求（用户描述）：\n\n    " + strings.ReplaceAll(original.body.SourceResearch.Request, "\n", "\n    ") + "\n\n"
		evidence := []knowledge.SourceEvidence{}
		for _, section := range report.Sections {
			markdown += "## " + section.Title + "\n\n" + section.Body + "\n\n"
			for _, citation := range section.Citations {
				d, e := s.webDocumentTx(ctx, tx, item, citation.Web.DocumentID)
				if e != nil {
					return result, e
				}
				proof, e := d.Cite(citation.Web.FragmentID, citation.Web.Quote, []string{citation.Web.FragmentID})
				if e != nil || proof.DocumentSHA256 != citation.Web.DocumentSHA256 {
					return result, errCitation
				}
				ev := knowledge.SourceEvidence{ID: citation.ID, Title: d.Title, URL: d.URL, FetchedAt: d.FetchedAt, Citation: proof}
				var receiptRaw []byte
				var rid string
				e = tx.QueryRow(ctx, `SELECT id::text,ciphertext FROM learning_web_import_receipts WHERE owner_id IN (SELECT id FROM learning_mentor_runs WHERE source_research_job_id=$1) AND revision_id=$2 ORDER BY id LIMIT 1`, item.SourceResearchJobID, d.RevisionID).Scan(&rid, &receiptRaw)
				if e == nil {
					var receipt WebImportReceipt
					if e = s.openWeb("receipt", rid, g, receiptRaw, &receipt); e != nil {
						return result, e
					}
					ev.Source = &receipt.Source
				} else if !errors.Is(e, pgx.ErrNoRows) {
					return result, e
				} else {
					scoped, _ := learningspace.WithScope(ctx, space)
					ev.Source, e = knowledgepg.New(s.pool).WebSourceIdentityTx(scoped, tx, d.Fingerprint)
					if e != nil {
						return result, e
					}
				}
				evidence = append(evidence, ev)
				markdown += fmt.Sprintf("[%s] %s — %s\n\n> %s\n\n", citation.ID, d.Title, d.URL, strings.ReplaceAll(proof.Quote, "\n", "\n> "))
			}
			for _, gap := range section.Gaps {
				markdown += "缺口：" + gap + "\n\n"
			}
		}
		for _, gap := range report.Gaps {
			markdown += "整体缺口：" + gap + "\n\n"
		}
		sourceID := uuid.NewString()
		title := "整理报告"
		source := research.Source{ID: sourceID, RevisionID: report.ID, SpaceID: space, Purpose: "source_research_report", Title: title, Kind: "text/markdown", Status: "parsed", StorageAllowed: true, Text: markdown, Coverage: report.Status, Fingerprint: research.TextHash(markdown), FetchedAt: &report.CreatedAt}
		if report.Status != "complete" {
			source.Status = "partial"
		}
		derivation := &knowledge.SourceDerivation{Kind: "source_research", JobID: id, RoundID: report.RoundID, ReportRevisionID: report.ID, GeneratedAt: report.CreatedAt, Model: report.Model, Partial: report.Status != "complete"}
		scoped, _ := learningspace.WithScope(ctx, space)
		imported, e := knowledgepg.New(s.pool).AdoptSourceTx(scoped, tx, knowledge.SourceImport{RunID: item.RunID, Metadata: source, OperationID: c.OperationID, SourceID: sourceID, SourceRevisionID: report.ID, ActorDeviceID: actor.Device.ID, Title: title, Fingerprint: source.Fingerprint, Coverage: source.Coverage, Text: markdown, FetchedAt: report.CreatedAt, Derivation: derivation, Evidence: evidence})
		if e != nil {
			return result, e
		}
		doc := imported.Revision.Documents[0].Revision
		result = WebImportReceipt{OperationID: c.OperationID, RunID: item.RunID, DocumentID: report.ID, RevisionID: report.ID, Source: knowledge.SourceIdentity{CollectionID: sourceID, RevisionID: imported.Revision.ID, DocumentID: doc.DocumentID, DocumentRevisionID: doc.ID}}
		receiptID = uuid.NewString()
		raw, e = s.sealWeb("source_research_imports", receiptID, g, result)
		if e != nil {
			return result, e
		}
		if _, e = tx.Exec(ctx, `INSERT INTO learning_source_research_imports(id,job_id,privacy_generation,report_id,ciphertext) VALUES($1,$2,$3,$4,$5)`, receiptID, id, g, report.ID, raw); e != nil {
			return result, e
		}
	} else {
		return result, err
	}
	if err = s.touchSourceResearchTx(ctx, tx, &item); err != nil {
		return result, err
	}
	if err = s.saveWebOperation(ctx, tx, actor.Device.ID, space, "", item.RunID, c.OperationID, g, hash, result); err != nil {
		return result, err
	}
	return result, tx.Commit(ctx)
}
