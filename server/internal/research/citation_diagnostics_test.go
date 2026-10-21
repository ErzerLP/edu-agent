package research

import (
	"errors"
	"testing"
)

func TestCitationValidationReportsFixedReason(t *testing.T) {
	source := Source{ID: "source", RevisionID: "revision", Text: "actual text", Fragments: []Fragment{{ID: "fragment", Start: 0, End: 11, Text: "actual text"}}}
	for _, test := range []struct {
		name     string
		citation Citation
		mutate   func(*Source)
		want     string
	}{
		{"source", Citation{SourceID: "private-source", RevisionID: "revision", FragmentID: "fragment", Quote: "actual"}, nil, "citation_source_missing"},
		{"revision", Citation{SourceID: "source", RevisionID: "private-revision", FragmentID: "fragment", Quote: "actual"}, nil, "citation_revision_mismatch"},
		{"fragment", Citation{SourceID: "source", RevisionID: "revision", FragmentID: "private-fragment", Quote: "actual"}, nil, "citation_fragment_missing"},
		{"quote", Citation{SourceID: "source", RevisionID: "revision", FragmentID: "fragment", Quote: "private-body"}, nil, "citation_quote_mismatch"},
		{"range", Citation{SourceID: "source", RevisionID: "revision", FragmentID: "fragment", Quote: "actual"}, func(s *Source) { s.Fragments[0].End = 12 }, "citation_range_mismatch"},
	} {
		t.Run(test.name, func(t *testing.T) {
			s := source
			s.Fragments = append([]Fragment(nil), source.Fragments...)
			if test.mutate != nil {
				test.mutate(&s)
			}
			err := (Synthesis{Points: []Point{{Text: "point", Citations: []Citation{test.citation}}}}).Validate([]Source{s})
			if !errors.Is(err, ErrPolicy) || ValidationReason(err) != test.want {
				t.Fatalf("validation=%v, reason=%q, want=%q", err, ValidationReason(err), test.want)
			}
		})
	}
}

func TestWebCitationValidationReportsFixedReason(t *testing.T) {
	for _, test := range []struct {
		name     string
		fragment string
		quote    string
		supplied []string
		mutate   func(*WebDocument)
		want     string
	}{
		{"unread", "fragment", "actual", nil, nil, "web_citation_unsupplied_fragment"},
		{"empty_quote", "fragment", "", []string{"fragment"}, nil, "web_quote_invalid"},
		{"invalid_utf8", "fragment", "\xff", []string{"fragment"}, nil, "web_quote_invalid"},
		{"hash", "fragment", "actual", []string{"fragment"}, func(d *WebDocument) { d.SHA256 = "forged" }, "web_document_hash_mismatch"},
		{"unknown_fragment", "missing", "actual", []string{"missing"}, nil, "web_citation_unknown_fragment"},
		{"range", "fragment", "actual", []string{"fragment"}, func(d *WebDocument) { d.Fragments[0].End = 12 }, "web_fragment_range_mismatch"},
		{"quote", "fragment", "invented", []string{"fragment"}, nil, "web_quote_mismatch"},
	} {
		t.Run(test.name, func(t *testing.T) {
			d := WebDocument{ID: "document", Text: "actual text", SHA256: TextHash("actual text"), Fragments: []Fragment{{ID: "fragment", Start: 0, End: 11, Text: "actual text"}}}
			if test.mutate != nil {
				test.mutate(&d)
			}
			_, err := d.Cite(test.fragment, test.quote, test.supplied)
			if !errors.Is(err, ErrPolicy) || ValidationReason(err) != test.want {
				t.Fatalf("reason=%q, want=%q", ValidationReason(err), test.want)
			}
		})
	}
}
