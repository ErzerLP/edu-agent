package postgresstore

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"

	"github.com/edu-agent/edu-agent/server/internal/knowledge"
	space "github.com/edu-agent/edu-agent/server/internal/learningspace"
	"github.com/edu-agent/edu-agent/server/internal/privacy"
	"github.com/jackc/pgx/v5"
)

type sourceCursor struct{ Space, Filter, Collection, Document string }

func (s *Store) SourceGeneration(ctx context.Context) (int64, error) {
	tx, err := s.beginPrivacyRead(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(context.Background())
	return privacy.LockOwnerRead(ctx, tx, privacy.OwnerKnowledge)
}

func (s *Store) Sources(ctx context.Context, q knowledge.SourceQuery) (knowledge.SourcePage, error) {
	result := knowledge.SourcePage{Items: []knowledge.SourceItem{}}
	filter := ""
	if knowledge.HasCollection(ctx) {
		filter = knowledge.CollectionID(ctx)
	}
	cursor := sourceCursor{Space: space.Scope(ctx), Filter: filter, Collection: "00000000-0000-0000-0000-000000000000", Document: "00000000-0000-0000-0000-000000000000"}
	if q.Cursor != "" {
		raw, err := base64.RawURLEncoding.DecodeString(q.Cursor)
		if err != nil || json.Unmarshal(raw, &cursor) != nil || cursor.Space != space.Scope(ctx) || cursor.Filter != filter || !space.ValidID(cursor.Collection) || !space.ValidID(cursor.Document) {
			return result, &knowledge.Error{Code: knowledge.CodeInvalidRequest}
		}
	}
	tx, err := s.beginPrivacyRead(ctx)
	if err != nil {
		return result, err
	}
	defer tx.Rollback(context.Background())
	if filter != "" {
		if err := checkCollection(ctx, tx, filter); err != nil {
			return result, err
		}
	}
	rows, err := tx.Query(ctx, `SELECT c.id,c.name,r.id,sd.document_id,sd.document_revision_id,sd.canonical_path,m.metadata,CASE WHEN m.metadata IS NULL THEN p.pdf_metadata END
 FROM knowledge_collection_links l
 JOIN knowledge_collections c ON c.id=l.collection_id
 JOIN knowledge_revisions r ON r.id=c.head_revision_id AND r.redacted_at IS NULL
 JOIN knowledge_snapshot_documents sd ON sd.knowledge_revision_id=r.id
 LEFT JOIN knowledge_source_metadata m ON m.document_revision_id=sd.document_revision_id
 JOIN knowledge_document_payloads p ON p.document_revision_id=sd.document_revision_id
 WHERE l.space_id=$1 AND ($2='' OR c.id::text=$2) AND (c.id,sd.document_id)>($3::uuid,$4::uuid)
 ORDER BY c.id,sd.document_id LIMIT $5`, space.Scope(ctx), filter, cursor.Collection, cursor.Document, q.Limit+1)
	if err != nil {
		return result, err
	}
	defer rows.Close()
	for rows.Next() {
		var item knowledge.SourceItem
		var raw, pdfRaw []byte
		if err := rows.Scan(&item.CollectionID, &item.CollectionName, &item.RevisionID, &item.DocumentID, &item.DocumentRevisionID, &item.Path, &raw, &pdfRaw); err != nil {
			return result, err
		}
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &item.Metadata); err != nil {
				return result, err
			}
		}
		if item.Metadata == nil && len(pdfRaw) > 0 {
			var pdf knowledge.PDFMetadata
			if err := json.Unmarshal(pdfRaw, &pdf); err != nil {
				return result, err
			}
			item.Metadata = knowledge.PDFSourceMetadata(&pdf)
		}
		result.Items = append(result.Items, item)
	}
	if err := rows.Err(); err != nil {
		return result, err
	}
	if len(result.Items) > q.Limit {
		result.Items = result.Items[:q.Limit]
		last := result.Items[len(result.Items)-1]
		cursor.Collection, cursor.Document = last.CollectionID, last.DocumentID
		raw, _ := json.Marshal(cursor)
		result.NextCursor = base64.RawURLEncoding.EncodeToString(raw)
	}
	return result, nil
}

func (s *Store) Source(ctx context.Context, revision, document string) (knowledge.SourceDetail, error) {
	tx, err := s.beginPrivacyRead(ctx)
	if err != nil {
		return knowledge.SourceDetail{}, err
	}
	defer tx.Rollback(context.Background())
	if err := checkRevision(ctx, tx, revision); err != nil {
		return knowledge.SourceDetail{}, err
	}
	item := knowledge.SourceItem{CollectionID: knowledge.CollectionID(ctx), RevisionID: revision, DocumentRevisionID: document}
	doc := knowledge.DocumentRevision{ID: document}
	var metadata, pdf []byte
	var redacted bool
	err = tx.QueryRow(ctx, `SELECT c.name,sd.document_id,sd.canonical_path,r.redacted_at IS NOT NULL,p.canonical_markdown,p.pdf_metadata,m.metadata
 FROM knowledge_snapshot_documents sd JOIN knowledge_revisions r ON r.id=sd.knowledge_revision_id
 JOIN knowledge_collections c ON c.id=r.collection_id
 JOIN knowledge_document_payloads p ON p.document_revision_id=sd.document_revision_id
 LEFT JOIN knowledge_source_metadata m ON m.document_revision_id=sd.document_revision_id
 WHERE sd.knowledge_revision_id=$1 AND sd.document_revision_id=$2`, revision, document).Scan(&item.CollectionName, &item.DocumentID, &item.Path, &redacted, &doc.CanonicalMarkdown, &pdf, &metadata)
	if errors.Is(err, pgx.ErrNoRows) {
		return knowledge.SourceDetail{}, scopeMissing()
	}
	if err != nil {
		return knowledge.SourceDetail{}, err
	}
	if redacted {
		return knowledge.SourceDetail{}, &knowledge.Error{Code: knowledge.CodeContentRedacted}
	}
	if len(metadata) > 0 {
		if err := json.Unmarshal(metadata, &item.Metadata); err != nil {
			return knowledge.SourceDetail{}, err
		}
	}
	if len(pdf) > 0 {
		if err := json.Unmarshal(pdf, &doc.PDF); err != nil {
			return knowledge.SourceDetail{}, err
		}
		if item.Metadata == nil {
			item.Metadata = knowledge.PDFSourceMetadata(doc.PDF)
		}
	}
	doc.Nodes, err = loadNodes(ctx, tx, document)
	if err != nil {
		return knowledge.SourceDetail{}, err
	}
	return knowledge.SourceDetail{SourceItem: item, Fragments: knowledge.SourceFragments(doc), PDF: doc.PDF}, nil
}
