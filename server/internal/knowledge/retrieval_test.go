package knowledge

import (
	"context"
	"reflect"
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
