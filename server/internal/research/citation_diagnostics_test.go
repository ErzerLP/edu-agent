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
