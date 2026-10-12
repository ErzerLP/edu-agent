package knowledge

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"strings"
	"unicode/utf8"

	space "github.com/edu-agent/edu-agent/server/internal/learningspace"
	"github.com/edu-agent/edu-agent/server/internal/research"
)

// SourceMetadata is server-derived and immutable at the document revision boundary.
// Missing metadata means unknown; Markdown prefixes are never provenance.
type SourceMetadata struct {
	Kind        string            `json:"kind"`
	Locator     string            `json:"locator,omitempty"`
	FinalURL    string            `json:"final_url,omitempty"`
	MediaType   string            `json:"media_type,omitempty"`
	Parser      string            `json:"parser"`
	Coverage    string            `json:"coverage"`
	Fingerprint string            `json:"fingerprint"`
	Status      string            `json:"status"`
	DerivedFrom *SourceDerivation `json:"derived_from,omitempty"`
}

type SourceDerivation struct {
	ArtifactID string `json:"artifact_id"`
	Version    int64  `json:"version"`
}

type SourceItem struct {
	CollectionID       string          `json:"collection_id"`
	CollectionName     string          `json:"collection_name"`
	RevisionID         string          `json:"revision_id"`
	DocumentID         string          `json:"document_id"`
	DocumentRevisionID string          `json:"document_revision_id"`
	Path               string          `json:"path"`
	Metadata           *SourceMetadata `json:"metadata"`
}

type SourceQuery struct {
	Cursor string
	Limit  int
}
type SourcePage struct {
	Items      []SourceItem `json:"items"`
	NextCursor string       `json:"next_cursor,omitempty"`
}
type SourceFragment struct {
	NodeID         string      `json:"node_id"`
	NodeRevisionID string      `json:"node_revision_id"`
	Title          string      `json:"title"`
	Range          SourceRange `json:"range"`
	Text           string      `json:"text"`
	Truncated      bool        `json:"truncated"`
	Page           int         `json:"page,omitempty"`
}
type SourceDetail struct {
	SourceItem
	Fragments []SourceFragment `json:"fragments"`
	PDF       *PDFMetadata     `json:"pdf,omitempty"`
}

type sourceCatalogStore interface {
	Sources(context.Context, SourceQuery) (SourcePage, error)
	Source(context.Context, string, string) (SourceDetail, error)
	SourceGeneration(context.Context) (int64, error)
}

func (s *Service) Sources(ctx context.Context, q SourceQuery) (SourcePage, error) {
	if q.Limit == 0 {
		q.Limit = 30
	}
	if q.Limit < 1 || q.Limit > 100 || len(q.Cursor) > 1024 {
		return SourcePage{}, &Error{Code: CodeInvalidRequest}
	}
	store, ok := s.store.(sourceCatalogStore)
	if !ok {
		return SourcePage{}, &Error{Code: CodeNotFound}
	}
	return store.Sources(ctx, q)
}
func (s *Service) Source(ctx context.Context, revision, document string) (SourceDetail, error) {
	if !validUUID(revision) || !validUUID(document) || !HasCollection(ctx) {
		return SourceDetail{}, &Error{Code: CodeInvalidRequest}
	}
	store, ok := s.store.(sourceCatalogStore)
	if !ok {
		return SourceDetail{}, &Error{Code: CodeNotFound}
	}
	return store.Source(ctx, revision, document)
}
func (s *Service) PrepareFetchedSource(ctx context.Context, source research.Source) (string, string, error) {
	store, ok := s.store.(sourceCatalogStore)
	if !ok {
		return "", "", &Error{Code: CodeNotFound}
	}
	generation, err := store.SourceGeneration(ctx)
	if err != nil {
		return "", "", err
	}
	markdown, receipt := FetchedSourceReceipt(ctx, generation, source)
	return markdown, receipt, nil
}

type fetchedReceipt struct {
	Space      string         `json:"space"`
	Generation int64          `json:"generation"`
	BodyHash   string         `json:"body_hash"`
	Metadata   SourceMetadata `json:"metadata"`
}

func sourceSignature(raw []byte) string {
	mac := hmac.New(sha256.New, identityReviewKey)
	mac.Write([]byte("knowledge-source-v1\n"))
	mac.Write(raw)
	return hex.EncodeToString(mac.Sum(nil))
}
func FetchedSourceReceipt(ctx context.Context, generation int64, source research.Source) (string, string) {
	if !source.StorageAllowed || source.Text == "" || (source.Status != "parsed" && source.Status != "partial") || source.PDF != nil {
		return "", ""
	}
	m := SourceMetadata{Kind: "url", Locator: source.Locator, FinalURL: source.FinalURL, MediaType: source.Kind, Parser: source.Parser, Coverage: source.Coverage, Fingerprint: source.Fingerprint, Status: source.Status}
	// The body is generated here so client formatting cannot escape receipt binding.
	provenance, _ := json.Marshal(m)
	markdown := "# 网页原文\n\n    " + string(provenance) + "\n\n    " + strings.ReplaceAll(source.Text, "\n", "\n    ") + "\n"
	raw, _ := json.Marshal(fetchedReceipt{space.Scope(ctx), generation, sha256Hex([]byte(markdown)), m})
	return markdown, base64.RawURLEncoding.EncodeToString(raw) + "." + sourceSignature(raw)
}
func verifySourceReceipt(ctx context.Context, generation int64, markdown, receipt string) (*SourceMetadata, error) {
	parts := strings.Split(receipt, ".")
	if len(parts) != 2 || len(receipt) > 32768 {
		return nil, &Error{Code: CodeInvalidRequest}
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[0])
	var r fetchedReceipt
	if err != nil || !hmac.Equal([]byte(sourceSignature(raw)), []byte(parts[1])) || json.Unmarshal(raw, &r) != nil || r.Space != space.Scope(ctx) || r.Generation != generation || r.BodyHash != sha256Hex([]byte(markdown)) {
		return nil, &Error{Code: CodeInvalidRequest}
	}
	return &r.Metadata, nil
}
func PDFSourceMetadata(pdf *PDFMetadata) *SourceMetadata {
	status := "parsed"
	if pdf.Report.Coverage != "complete_text" {
		status = "partial"
	}
	return &SourceMetadata{Kind: "pdf", Locator: pdf.Locator, FinalURL: pdf.Locator, MediaType: "application/pdf", Parser: pdf.Report.Parser, Coverage: pdf.Report.Coverage, Fingerprint: pdf.Report.Fingerprint, Status: status}
}

// Fragments use canonical UTF-8 byte positions, exactly as existing citations do.
func SourceFragments(document DocumentRevision) []SourceFragment {
	result := []SourceFragment{}
	for _, node := range document.Nodes {
		r := node.LocalBodyRange
		page := 0
		if document.PDF != nil {
			for _, p := range document.PDF.Ranges {
				if p.Range.Start == r.Start && p.Range.End == r.End {
					page = p.Number
					break
				}
			}
			// Metadata and pages without extracted text are not original fragments.
			if page == 0 {
				continue
			}
		}
		if r.End <= r.Start {
			continue
		}
		end := r.End
		if end-r.Start > 4096 {
			end = r.Start + 4096
			for end > r.Start && !utf8.RuneStart(document.CanonicalMarkdown[end]) {
				end--
			}
		}
		if end < r.End {
			r.EndLine = r.StartLine + strings.Count(strings.TrimSuffix(document.CanonicalMarkdown[r.Start:end], "\n"), "\n")
		}
		r.End = end
		f := SourceFragment{NodeID: node.NodeID, NodeRevisionID: node.ID, Title: node.Title, Range: r, Text: document.CanonicalMarkdown[r.Start:end], Truncated: end < node.LocalBodyRange.End, Page: page}
		result = append(result, f)
	}
	return result
}
