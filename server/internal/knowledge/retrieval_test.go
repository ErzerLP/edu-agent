package knowledge

import (
	"context"
	"reflect"
	"strings"
	"testing"
)

type staticSelector struct {
	response SelectorResponse
	err      error
}

func (s staticSelector) Select(context.Context, SelectorRequest) (SelectorResponse, error) {
	return s.response, s.err
}

func TestSelectorValidationFallsBackAtomically(t *testing.T) {
	candidates := []Candidate{
		{Ordinal: 0, NodeRevisionID: "10000000-0000-4000-8000-000000000001", Score: 10, Title: "One", HasChildren: false},
		{Ordinal: 1, NodeRevisionID: "10000000-0000-4000-8000-000000000002", Score: 5, Title: "Two", HasChildren: true},
	}
	for i := range candidates {
		candidates[i].TitleSHA256 = sha256Hex([]byte(candidates[i].Title))
	}
	request := SelectorRequest{
		KnowledgeRevisionID:  "20000000-0000-4000-8000-000000000001",
		ParentNodeRevisionID: "30000000-0000-4000-8000-000000000001",
		Candidates:           candidates,
	}
	request.CandidateSetHash = CandidateSetHash(request.KnowledgeRevisionID, request.ParentNodeRevisionID, candidates)
	fallback := lexicalDecisions(candidates)
	allNodes := map[string]NodeRevision{
		candidates[0].NodeRevisionID: {ID: candidates[0].NodeRevisionID},
		candidates[1].NodeRevisionID: {ID: candidates[1].NodeRevisionID},
	}
	tests := []struct {
		name      string
		response  SelectorResponse
		err       error
		reason    string
		truncated bool
	}{
		{name: "timeout", err: &SelectorFailure{Reason: "selector_timeout"}, reason: "selector_timeout"},
		{name: "stale hash", response: SelectorResponse{KnowledgeRevisionID: request.KnowledgeRevisionID, CandidateSetHash: "stale"}, reason: "selector_stale_response"},
		{name: "cross revision", response: SelectorResponse{KnowledgeRevisionID: "20000000-0000-4000-8000-000000000099", CandidateSetHash: request.CandidateSetHash}, reason: "selector_cross_revision"},
		{name: "unknown", response: SelectorResponse{KnowledgeRevisionID: request.KnowledgeRevisionID, CandidateSetHash: request.CandidateSetHash, Decisions: []Decision{{NodeRevisionID: "40000000-0000-4000-8000-000000000001", Action: "select"}}}, reason: "selector_unknown_candidate"},
		{name: "over budget", response: SelectorResponse{KnowledgeRevisionID: request.KnowledgeRevisionID, CandidateSetHash: request.CandidateSetHash, Decisions: []Decision{{NodeRevisionID: candidates[0].NodeRevisionID, Action: "select"}, {NodeRevisionID: candidates[1].NodeRevisionID, Action: "expand"}, {NodeRevisionID: candidates[0].NodeRevisionID, Action: "select"}, {NodeRevisionID: candidates[1].NodeRevisionID, Action: "expand"}}}, reason: "selector_over_budget", truncated: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			service := &Service{selector: staticSelector{response: test.response, err: test.err}}
			decisions, degraded, reason, truncated := service.selectLayer(context.Background(), request, allNodes)
			if !degraded || reason != test.reason || truncated != test.truncated || !reflect.DeepEqual(decisions, fallback) {
				t.Fatalf("fallback mismatch: decisions=%+v degraded=%v reason=%s truncated=%v", decisions, degraded, reason, truncated)
			}
		})
	}
}

func TestRetrievalScoreFieldsAreRuneSafeAt2048Bytes(t *testing.T) {
	query := retrievalTokens("needle")
	prefix := strings.Repeat("界", 682)
	if len(prefix) != 2046 {
		t.Fatalf("fixture prefix bytes = %d", len(prefix))
	}
	if got := fieldScore(query, prefix+"needle"); got != 0 {
		t.Fatalf("field score included token after 2048-byte boundary: %d", got)
	}
	inside := strings.Repeat("界", 680) + " needle"
	if got := fieldScore(query, inside); got != 1000000 {
		t.Fatalf("field score lost token inside 2048-byte boundary: %d", got)
	}
	longNeedle := prefix + " needle"
	canonical := longNeedle
	childID := "10000000-0000-4000-8000-000000000002"
	nodeID := "10000000-0000-4000-8000-000000000001"
	document := DocumentRevision{ID: "20000000-0000-4000-8000-000000000001", CanonicalMarkdown: canonical, Nodes: []NodeRevision{
		{ID: "30000000-0000-4000-8000-000000000001", Children: []string{nodeID}},
		{ID: nodeID, Title: longNeedle, AncestorTitles: []string{longNeedle}, Children: []string{childID}, LocalBodyRange: SourceRange{Start: 0, End: len(canonical)}},
		{ID: childID, Title: longNeedle},
	}}
	candidates := scoreNodes(document, []NodeRevision{document.Nodes[1]}, query, nil)
	if len(candidates) != 1 || candidates[0].Score != 0 || candidates[0].LocalBodyScore != 0 {
		t.Fatalf("node score used unbounded fields: %+v", candidates)
	}
	documents := scoreDocuments([]SnapshotDocument{{Path: longNeedle, Revision: document}}, query, NewCanonicalizer())
	if len(documents) != 1 || documents[0].score != 0 {
		t.Fatalf("document score used unbounded fields: %+v", documents)
	}
}

func TestCandidateSetHashUsesFrozenCanonicalForm(t *testing.T) {
	candidates := []Candidate{{Ordinal: 0, NodeRevisionID: "10000000-0000-4000-8000-000000000001", Score: 42, TitleSHA256: stringsOf('a', 64)}}
	first := CandidateSetHash("20000000-0000-4000-8000-000000000001", "30000000-0000-4000-8000-000000000001", candidates)
	candidates[0].SummaryArtifactID = "40000000-0000-4000-8000-000000000001"
	second := CandidateSetHash("20000000-0000-4000-8000-000000000001", "30000000-0000-4000-8000-000000000001", candidates)
	if first == second || len(first) != 64 || len(second) != 64 {
		t.Fatalf("candidate set hash did not bind summary artifact: %s %s", first, second)
	}
}

func stringsOf(value byte, count int) string {
	result := make([]byte, count)
	for i := range result {
		result[i] = value
	}
	return string(result)
}
