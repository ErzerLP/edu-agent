package mentorrun

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
)

// Losing the specific rule here makes both generation and worker logs useless.
func TestCitationReasonPreservesWebAndSourceRules(t *testing.T) {
	for _, reason := range []string{
		"citation_format", "citation_number_out_of_range", "citation_not_referenced",
		"web_citation_unknown_document", "web_citation_revision_mismatch",
		"web_citation_unsupplied_fragment", "web_citation_unknown_fragment",
		"web_quote_mismatch", "web_quote_invalid", "web_document_hash_mismatch",
		"web_fragment_range_mismatch", "web_slice_hash_mismatch", "web_page_mismatch",
		"source_citation_out_of_range", "source_fragment_out_of_range",
		"source_citation_duplicate", "source_proof_schema_mismatch",
		"source_document_hash_mismatch", "source_fragment_mismatch", "mixed_origin_rule",
	} {
		t.Run(reason, func(t *testing.T) {
			err := &citationFailure{reason: reason}
			if !errors.Is(err, errCitation) || citationReason(err) != reason {
				t.Fatalf("reason=%q, want=%q", citationReason(err), reason)
			}
		})
	}
}

func TestCitationDiagnosticsDoNotLogUntrustedReason(t *testing.T) {
	const secret = "private-quote-and-provider-secret"
	var log bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&log, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })
	logRunFailure(context.Background(), row{}, &citationFailure{reason: secret})
	if strings.Contains(log.String(), secret) || !strings.Contains(log.String(), `"rejection_reason":"invalid_source_citation"`) {
		t.Fatalf("unsafe or missing rejection classification: %s", log.String())
	}
}
