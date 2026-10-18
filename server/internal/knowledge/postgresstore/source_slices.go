package postgresstore

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"unicode/utf8"

	"github.com/edu-agent/edu-agent/server/internal/knowledge"
	space "github.com/edu-agent/edu-agent/server/internal/learningspace"
	"github.com/edu-agent/edu-agent/server/internal/privacy"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func invalidSlice() error { return &knowledge.Error{Code: knowledge.CodeInvalidRequest} }

// Metadata and a server-computed canonical digest only: no collection export,
// document body or unselected node payload crosses this validation boundary.
func (s *Store) sourceIdentityTx(ctx context.Context, tx pgx.Tx, id knowledge.SourceIdentity) (knowledge.SourceItem, string, error) {
	item := knowledge.SourceItem{CollectionID: id.CollectionID, RevisionID: id.RevisionID, DocumentID: id.DocumentID, DocumentRevisionID: id.DocumentRevisionID}
	var hash string
	var metadata []byte
	err := tx.QueryRow(ctx, `SELECT c.name,d.canonical_path,m.metadata,encode(sha256(convert_to(p.canonical_markdown,'UTF8')),'hex')
 FROM knowledge_collection_links l JOIN knowledge_collections c ON c.id=l.collection_id
 JOIN knowledge_revisions r ON r.collection_id=c.id JOIN knowledge_snapshot_documents d ON d.knowledge_revision_id=r.id
 JOIN knowledge_document_payloads p ON p.document_revision_id=d.document_revision_id
 LEFT JOIN knowledge_source_metadata m ON m.document_revision_id=d.document_revision_id
 WHERE l.space_id=$1 AND c.id=$2 AND r.id=$3 AND d.document_id=$4 AND d.document_revision_id=$5 AND r.redacted_at IS NULL FOR SHARE OF l`, space.Scope(ctx), id.CollectionID, id.RevisionID, id.DocumentID, id.DocumentRevisionID).Scan(&item.CollectionName, &item.Path, &metadata, &hash)
	if errors.Is(err, pgx.ErrNoRows) {
		return item, "", scopeMissing()
	}
	if err != nil {
		return item, "", err
	}
	if len(metadata) > 0 {
		if err = json.Unmarshal(metadata, &item.Metadata); err != nil {
			return item, "", err
		}
	}
	return item, hash, nil
}
func (s *Store) ValidateSourceManifestTx(ctx context.Context, tx pgx.Tx, m knowledge.SourceManifest) error {
	if m.Schema != 1 || m.SpaceID != space.Scope(ctx) || len(m.Entries) > 100 || m.SHA256 != m.Hash() {
		return invalidSlice()
	}
	g, err := privacy.LockOwnerRead(ctx, tx, privacy.OwnerKnowledge)
	if err != nil {
		return err
	}
	if g != m.Generation {
		return &knowledge.Error{Code: knowledge.CodeContentRedacted}
	}
	for _, e := range m.Entries {
		_, hash, err := s.sourceIdentityTx(ctx, tx, e.SourceIdentity)
		if err != nil {
			return err
		}
		if hash != e.SHA256 {
			return &knowledge.Error{Code: knowledge.CodeContentRedacted}
		}
	}
	return nil
}

type sourceDirectoryCursor struct {
	Manifest string
	Entry    int
	Start    int
	Node     string
}

func (s *Store) ReadSourceDirectoryTx(ctx context.Context, tx pgx.Tx, m knowledge.SourceManifest, cursor string, limit int) (knowledge.SourceDirectoryPage, error) {
	page := knowledge.SourceDirectoryPage{Items: []knowledge.SourceDirectoryEntry{}}
	if limit < 1 || limit > 64 || len(cursor) > 1024 {
		return page, invalidSlice()
	}
	c := sourceDirectoryCursor{Manifest: m.SHA256, Node: uuid.Nil.String(), Start: -1}
	if cursor != "" {
		raw, e := base64.RawURLEncoding.DecodeString(cursor)
		if e != nil || json.Unmarshal(raw, &c) != nil || c.Manifest != m.SHA256 || c.Entry < 0 || c.Entry >= len(m.Entries) || c.Start < 0 || uuid.Validate(c.Node) != nil {
			return page, invalidSlice()
		}
	}
	if err := s.ValidateSourceManifestTx(ctx, tx, m); err != nil {
		return page, err
	}
	last := c
	for ; c.Entry < len(m.Entries); c.Entry++ {
		entry := m.Entries[c.Entry]
		rows, err := tx.Query(ctx, `SELECT n.node_id,n.id,left(n.title,160),n.local_body_start,n.local_body_end,n.local_body_start_line,n.local_body_end_line
 FROM knowledge_node_revisions n JOIN knowledge_document_payloads p ON p.document_revision_id=n.document_revision_id
 WHERE n.document_revision_id=$1 AND n.local_body_end>n.local_body_start AND (n.local_body_start,n.id)>($2,$3::uuid)
 AND (p.pdf_metadata IS NULL OR EXISTS(SELECT 1 FROM jsonb_array_elements(p.pdf_metadata->'ranges') r WHERE (r->'range'->>'start')::int=n.local_body_start AND (r->'range'->>'end')::int=n.local_body_end))
 ORDER BY n.local_body_start,n.id LIMIT $4`, entry.DocumentRevisionID, c.Start, c.Node, limit-len(page.Items)+1)
		if err != nil {
			return page, err
		}
		for rows.Next() {
			var n knowledge.SourceDirectoryEntry
			n.SourceIdentity = entry.SourceIdentity
			if err = rows.Scan(&n.NodeID, &n.NodeRevisionID, &n.Title, &n.Range.Start, &n.Range.End, &n.Range.StartLine, &n.Range.EndLine); err != nil {
				rows.Close()
				return page, err
			}
			if len(page.Items) == limit {
				raw, _ := json.Marshal(last)
				page.NextCursor = base64.RawURLEncoding.EncodeToString(raw)
				rows.Close()
				return page, nil
			}
			page.Items = append(page.Items, n)
			c.Start = n.Range.Start
			c.Node = n.NodeRevisionID
			last = c
		}
		rows.Close()
		if err = rows.Err(); err != nil {
			return page, err
		}
		c.Start = -1
		c.Node = uuid.Nil.String()
	}
	return page, nil
}

func (s *Store) ReadSourceSliceTx(ctx context.Context, tx pgx.Tx, m knowledge.SourceManifest, q knowledge.SourceSliceRequest) (knowledge.SourceDetail, error) {
	detail := knowledge.SourceDetail{}
	if uuid.Validate(q.NodeID) != nil || uuid.Validate(q.NodeRevisionID) != nil || q.Start < 0 || q.End <= q.Start || q.End-q.Start > 4096 {
		return detail, invalidSlice()
	}
	if err := s.ValidateSourceManifestTx(ctx, tx, m); err != nil {
		return detail, err
	}
	allowed := false
	for _, e := range m.Entries {
		if e.SourceIdentity == q.SourceIdentity {
			allowed = true
			break
		}
	}
	if !allowed {
		return detail, scopeMissing()
	}
	item, _, err := s.sourceIdentityTx(ctx, tx, q.SourceIdentity)
	if err != nil {
		return detail, err
	}
	detail.SourceItem = item
	f := knowledge.SourceFragment{NodeID: q.NodeID, NodeRevisionID: q.NodeRevisionID}
	var original knowledge.SourceRange
	var raw []byte
	var pdf []byte
	var line int
	// Count preceding newline bytes inside PostgreSQL; return only the requested
	// byte interval (at most 4096), never the full canonical document.
	err = tx.QueryRow(ctx, `SELECT left(n.title,160),n.local_body_start,n.local_body_end,n.local_body_start_line,n.local_body_end_line,
 substring(convert_to(p.canonical_markdown,'UTF8') from $3+1 for $4-$3),
 1+(SELECT count(*) FROM regexp_matches(encode(substring(convert_to(p.canonical_markdown,'UTF8') from 1 for $3),'escape'),E'\n','g')),p.pdf_metadata
 FROM knowledge_node_revisions n JOIN knowledge_document_payloads p ON p.document_revision_id=n.document_revision_id
 WHERE n.document_revision_id=$1 AND n.id=$2 AND n.node_id=$5 AND $3>=n.local_body_start AND $4<=n.local_body_end`, q.DocumentRevisionID, q.NodeRevisionID, q.Start, q.End, q.NodeID).Scan(&f.Title, &original.Start, &original.End, &original.StartLine, &original.EndLine, &raw, &line, &pdf)
	if errors.Is(err, pgx.ErrNoRows) {
		return detail, invalidSlice()
	}
	if err != nil {
		return detail, err
	}
	start, end := q.Start, q.End
	for len(raw) > 0 && !utf8.RuneStart(raw[0]) {
		raw = raw[1:]
		start++
	}
	for len(raw) > 0 && !utf8.Valid(raw) {
		raw = raw[:len(raw)-1]
		end--
	}
	if len(raw) == 0 {
		return detail, invalidSlice()
	}
	f.Text = string(raw)
	f.Range = knowledge.SourceRange{Start: start, End: end, StartLine: line, EndLine: line + strings.Count(strings.TrimSuffix(f.Text, "\n"), "\n")}
	f.Truncated = start > original.Start || end < original.End
	if len(pdf) > 0 {
		var p knowledge.PDFMetadata
		if err = json.Unmarshal(pdf, &p); err != nil {
			return detail, err
		}
		for _, r := range p.Ranges {
			if r.Range.Start == original.Start && r.Range.End == original.End {
				f.Page = r.Number
				break
			}
		}
		if f.Page == 0 {
			return detail, invalidSlice()
		}
		if detail.Metadata == nil {
			detail.Metadata = knowledge.PDFSourceMetadata(&p)
		}
	}
	f.Proof = &knowledge.SourceSliceProof{Schema: 2, OriginalRange: original, Range: f.Range, SHA256: knowledge.SourceTextHash(f.Text)}
	detail.Fragments = []knowledge.SourceFragment{f}
	return detail, nil
}
