package llmselector

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/edu-agent/edu-agent/server/internal/integrations/llm"
	"github.com/edu-agent/edu-agent/server/internal/knowledge"
)

type retrievalFixtureStore struct {
	revision knowledge.KnowledgeRevision
}

func (s retrievalFixtureStore) Head(context.Context) (*knowledge.KnowledgeRevision, error) {
	revision := s.revision
	return &revision, nil
}

func (s retrievalFixtureStore) Revision(_ context.Context, id string) (knowledge.KnowledgeRevision, error) {
	if id != s.revision.ID {
		return knowledge.KnowledgeRevision{}, &knowledge.Error{Code: knowledge.CodeNotFound}
	}
	return s.revision, nil
}

func (retrievalFixtureStore) DocumentIdentityExists(context.Context, string) (bool, error) {
	return false, nil
}

func (retrievalFixtureStore) NodeIdentityOwner(context.Context, string) (string, bool, error) {
	return "", false, nil
}

func (retrievalFixtureStore) ReadyNodeArtifacts(context.Context, string) ([]knowledge.NodeArtifact, error) {
	return []knowledge.NodeArtifact{}, nil
}

func (retrievalFixtureStore) LookupImportOperation(context.Context, string) (knowledge.ImportOperationRecord, bool, error) {
	return knowledge.ImportOperationRecord{}, false, nil
}

func (retrievalFixtureStore) CommitImport(context.Context, knowledge.PreparedCommit) (knowledge.ImportResult, error) {
	panic("retrieval fixture does not support imports")
}

type strictChat struct {
	mode string
}

func (f strictChat) Chat(_ context.Context, request llm.ChatRequest) (llm.ChatResult, error) {
	if f.mode == "timeout" {
		return llm.ChatResult{}, &llm.Error{Category: llm.ErrorTimeout}
	}
	if f.mode == "malformed" {
		return llm.ChatResult{JSON: json.RawMessage(`{"knowledge_revision_id":`)}, nil
	}
	var input struct {
		KnowledgeRevisionID string                `json:"knowledge_revision_id"`
		CandidateSetHash    string                `json:"candidate_set_hash"`
		Candidates          []knowledge.Candidate `json:"candidates"`
	}
	if len(request.Messages) < 2 || json.Unmarshal([]byte(request.Messages[1].Content), &input) != nil || len(input.Candidates) == 0 {
		return llm.ChatResult{}, &llm.Error{Category: llm.ErrorInvalidRequest}
	}
	response := knowledge.SelectorResponse{KnowledgeRevisionID: input.KnowledgeRevisionID, CandidateSetHash: input.CandidateSetHash}
	switch f.mode {
	case "unknown":
		response.Decisions = []knowledge.Decision{{NodeRevisionID: "70000000-0000-4000-8000-000000000001", Action: "select"}}
	case "cross-revision":
		response.KnowledgeRevisionID = "70000000-0000-4000-8000-000000000002"
		response.Decisions = []knowledge.Decision{{NodeRevisionID: input.Candidates[0].NodeRevisionID, Action: "select"}}
	case "over-budget":
		for i := 0; i < 4; i++ {
			response.Decisions = append(response.Decisions, knowledge.Decision{NodeRevisionID: input.Candidates[0].NodeRevisionID, Action: "select"})
		}
	default:
		for _, candidate := range input.Candidates {
			action := "select"
			if candidate.HasChildren {
				action = "select_expand"
			}
			response.Decisions = append(response.Decisions, knowledge.Decision{NodeRevisionID: candidate.NodeRevisionID, Action: action})
		}
	}
	encoded, _ := json.Marshal(response)
	return llm.ChatResult{JSON: encoded}, nil
}

func TestServiceRetrieveFakeLLMFallbackMatchesLexicalBaseline(t *testing.T) {
	const revisionID = "10000000-0000-4000-8000-000000000001"
	revision := retrievalFixtureRevision(revisionID)
	baselineService, err := knowledge.NewService(retrievalFixtureStore{revision: revision}, knowledge.NewCanonicalizer(), knowledge.ServiceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	baseline, err := baselineService.Retrieve(t.Context(), knowledge.RetrievalCommand{Query: "needle"})
	if err != nil {
		t.Fatal(err)
	}
	reasons := map[string]string{
		"timeout": "selector_timeout", "malformed": "selector_schema_error", "unknown": "selector_unknown_candidate",
		"cross-revision": "selector_cross_revision", "over-budget": "selector_over_budget",
	}
	for _, mode := range []string{"timeout", "malformed", "unknown", "cross-revision", "over-budget"} {
		t.Run(mode, func(t *testing.T) {
			selector := New(strictChat{mode: mode})
			service, err := knowledge.NewService(retrievalFixtureStore{revision: revision}, knowledge.NewCanonicalizer(), knowledge.ServiceOptions{Selector: selector})
			if err != nil {
				t.Fatal(err)
			}
			actual, err := service.Retrieve(t.Context(), knowledge.RetrievalCommand{Query: "needle"})
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(traceFingerprint(baseline), traceFingerprint(actual)) {
				t.Fatalf("fallback changed candidate hashes/actions: baseline=%+v actual=%+v", traceFingerprint(baseline), traceFingerprint(actual))
			}
			if !reflect.DeepEqual(hitFingerprint(baseline), hitFingerprint(actual)) {
				t.Fatalf("fallback changed hit IDs/ranges/slice hashes: baseline=%+v actual=%+v", hitFingerprint(baseline), hitFingerprint(actual))
			}
			if !actual.Degraded || len(actual.Trace) == 0 {
				t.Fatalf("fake model failure was not marked degraded: %+v", actual)
			}
			for _, trace := range actual.Trace {
				if trace.ReasonCode != reasons[mode] || !trace.Degraded {
					t.Fatalf("mode %s trace reason/degraded = %+v", mode, trace)
				}
			}
			if mode == "over-budget" {
				if !actual.Truncated {
					t.Fatal("over-budget fake response was not marked truncated")
				}
			} else if actual.Truncated != baseline.Truncated {
				t.Fatalf("mode %s changed truncation: baseline=%v actual=%v", mode, baseline.Truncated, actual.Truncated)
			}
		})
	}
}

type traceFingerprintValue struct {
	Hash      string
	Decisions []knowledge.Decision
}

func traceFingerprint(result knowledge.RetrievalResult) []traceFingerprintValue {
	fingerprint := make([]traceFingerprintValue, 0, len(result.Trace))
	for _, trace := range result.Trace {
		fingerprint = append(fingerprint, traceFingerprintValue{Hash: trace.CandidateSetHash, Decisions: trace.Decisions})
	}
	return fingerprint
}

type hitFingerprintValue struct {
	DocumentID, DocumentRevisionID, NodeID, NodeRevisionID string
	HeadingRange, LocalBodyRange, SectionRange             knowledge.SourceRange
	SliceSHA256                                            string
}

func hitFingerprint(result knowledge.RetrievalResult) []hitFingerprintValue {
	fingerprint := make([]hitFingerprintValue, 0, len(result.Hits))
	for _, hit := range result.Hits {
		fingerprint = append(fingerprint, hitFingerprintValue{
			DocumentID: hit.DocumentID, DocumentRevisionID: hit.DocumentRevisionID, NodeID: hit.NodeID, NodeRevisionID: hit.NodeRevisionID,
			HeadingRange: hit.HeadingRange, LocalBodyRange: hit.LocalBodyRange, SectionRange: hit.SectionRange, SliceSHA256: hit.SliceSHA256,
		})
	}
	return fingerprint
}

func retrievalFixtureRevision(id string) knowledge.KnowledgeRevision {
	canonical := "# Root\n\n## Match\nneedle body\n\n### Deep\nneedle deeper\n\n## Other\nneedle other\n"
	rootID := "30000000-0000-4000-8000-000000000001"
	matchID := "30000000-0000-4000-8000-000000000002"
	deepID := "30000000-0000-4000-8000-000000000003"
	otherID := "30000000-0000-4000-8000-000000000004"
	all := knowledge.SourceRange{Start: 0, End: len(canonical), StartLine: 1, EndLine: 8}
	matchParent := rootID
	deepParent := matchID
	return knowledge.KnowledgeRevision{
		ID: id,
		Documents: []knowledge.SnapshotDocument{{Path: "fixture.md", Revision: knowledge.DocumentRevision{
			ID: "20000000-0000-4000-8000-000000000001", DocumentID: "40000000-0000-4000-8000-000000000001", CanonicalMarkdown: canonical,
			Nodes: []knowledge.NodeRevision{
				{ID: rootID, NodeID: "50000000-0000-4000-8000-000000000001", HeadingLevel: 0, Children: []string{matchID, otherID}},
				{ID: matchID, NodeID: "50000000-0000-4000-8000-000000000002", ParentNodeRevisionID: &matchParent, HeadingLevel: 2, Title: "Match", Children: []string{deepID}, LocalBodyRange: all, SectionRange: all},
				{ID: deepID, NodeID: "50000000-0000-4000-8000-000000000003", ParentNodeRevisionID: &deepParent, HeadingLevel: 3, Title: "Deep", LocalBodyRange: all, SectionRange: all},
				{ID: otherID, NodeID: "50000000-0000-4000-8000-000000000004", ParentNodeRevisionID: &matchParent, HeadingLevel: 2, Title: "Other", LocalBodyRange: all, SectionRange: all},
			},
		}}},
	}
}
