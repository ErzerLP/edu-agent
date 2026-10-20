package research

import (
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/edu-agent/edu-agent/server/internal/pdfsource"
)

// WebDocument is an immutable public snapshot, never a knowledge SourceIdentity.
// Its owner, permissions and lifetime are supplied by the learning host.
type WebDocument struct {
	Resources      []WebResource     `json:"resources,omitempty"`
	ID             string            `json:"document_id"`
	ResourceID     string            `json:"resource_id"`
	RevisionID     string            `json:"revision_id"`
	RequestURL     string            `json:"request_url"`
	URL            string            `json:"url"`
	Title          string            `json:"title"`
	FetchedAt      time.Time         `json:"fetched_at"`
	MediaType      string            `json:"media_type"`
	Parser         string            `json:"parser"`
	Coverage       string            `json:"coverage"`
	Fingerprint    string            `json:"fingerprint"`
	SHA256         string            `json:"document_sha256"`
	StorageAllowed bool              `json:"storage_allowed"`
	Fragments      []Fragment        `json:"fragments"`
	PDF            *pdfsource.Report `json:"pdf,omitempty"`
	Text           string            `json:"text"`
	NextCursor     string            `json:"next_cursor,omitempty"`
}

type WebResource struct {
	ID          string `json:"resource_id"`
	Title       string `json:"title"`
	URL         string `json:"url"`
	Summary     string `json:"summary"`
	SummaryOnly bool   `json:"summary_only"`
}

type WebCitation struct {
	DocumentID     string `json:"document_id"`
	RevisionID     string `json:"revision_id"`
	FragmentID     string `json:"fragment_id"`
	DocumentSHA256 string `json:"document_sha256"`
	SliceSHA256    string `json:"slice_sha256"`
	Quote          string `json:"quote"`
	Page           int    `json:"page,omitempty"`
}

func TextHash(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

// NewWebDocument validates the fetcher result at the persistence boundary.
func NewWebDocument(id string, source Source) (WebDocument, error) {
	if !source.StorageAllowed || source.Status != "parsed" && source.Status != "partial" || source.FetchedAt == nil || source.Text == "" || len(source.Text) > MaxText || !utf8.ValidString(source.Text) || len(source.Fragments) == 0 {
		return WebDocument{}, ErrPolicy
	}
	if _, err := ValidateURL(source.FinalURL); err != nil {
		return WebDocument{}, err
	}
	seen := map[string]bool{}
	for _, f := range source.Fragments {
		if f.ID == "" || seen[f.ID] || f.Start < 0 || f.End <= f.Start || f.End > len(source.Text) || !utf8.ValidString(source.Text[f.Start:f.End]) || source.Text[f.Start:f.End] != f.Text {
			return WebDocument{}, ErrPolicy
		}
		seen[f.ID] = true
		if source.PDF != nil {
			found := false
			for _, p := range source.PDF.Pages {
				if p.Number == f.Page && f.Start >= p.Start && f.End <= p.End && p.Text != "" {
					found = true
					break
				}
			}
			if !found {
				return WebDocument{}, ErrPolicy
			}
		} else if f.Page != 0 {
			return WebDocument{}, ErrPolicy
		}
	}
	title := source.Title
	if title == "" {
		title = source.FinalURL
	}
	return WebDocument{ID: id, ResourceID: source.ID, RevisionID: source.RevisionID, RequestURL: source.Locator, URL: source.FinalURL, Title: title, FetchedAt: *source.FetchedAt, MediaType: source.Kind, Parser: source.Parser, Coverage: source.Coverage, Fingerprint: source.Fingerprint, SHA256: TextHash(source.Text), StorageAllowed: true, Fragments: source.Fragments, PDF: source.PDF, Text: source.Text}, nil
}

// Cite accepts only fragments actually supplied to this model invocation.
func (d WebDocument) Cite(fragmentID, quote string, supplied []string) (WebCitation, error) {
	if !slices.Contains(supplied, fragmentID) || quote == "" || !utf8.ValidString(quote) || d.SHA256 != TextHash(d.Text) {
		return WebCitation{}, ErrPolicy
	}
	for _, f := range d.Fragments {
		if f.ID == fragmentID && f.Start >= 0 && f.End <= len(d.Text) && f.End > f.Start && d.Text[f.Start:f.End] == f.Text && strings.Contains(f.Text, quote) {
			return WebCitation{DocumentID: d.ID, RevisionID: d.RevisionID, FragmentID: f.ID, DocumentSHA256: d.SHA256, SliceSHA256: TextHash(f.Text), Quote: quote, Page: f.Page}, nil
		}
	}
	return WebCitation{}, ErrPolicy
}
