package api_test

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/edu-agent/edu-agent/server/internal/knowledge"
	"github.com/getkin/kin-openapi/openapi3"
	"go.yaml.in/yaml/v3"
)

func TestOpenAPIParsesAndDeclaresKnowledgeRoutes(t *testing.T) {
	data, err := os.ReadFile("openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := yaml.Unmarshal(data, &document); err != nil {
		t.Fatal(err)
	}
	paths, ok := document["paths"].(map[string]any)
	if !ok {
		t.Fatalf("OpenAPI paths are missing: %#v", document["paths"])
	}
	for route, expected := range map[string]struct {
		scope string
		codes []string
	}{
		"/v1/knowledge/revisions/head":                {"knowledge:read", []string{"200", "401", "403", "404", "429", "500"}},
		"/v1/knowledge/imports":                       {"knowledge:write", []string{"200", "201", "400", "401", "403", "409", "413", "422", "429", "500"}},
		"/v1/knowledge/revisions/{revisionID}/tree":   {"knowledge:read", []string{"200", "400", "401", "403", "404", "429", "500"}},
		"/v1/knowledge/revisions/{revisionID}/export": {"knowledge:read", []string{"200", "400", "401", "403", "404", "429", "500"}},
		"/v1/knowledge/retrievals":                    {"knowledge:read", []string{"200", "400", "401", "403", "404", "413", "429", "500"}},
	} {
		pathItem := paths[route].(map[string]any)
		var operation map[string]any
		for _, method := range []string{"get", "post"} {
			if candidate, exists := pathItem[method].(map[string]any); exists {
				operation = candidate
				break
			}
		}
		if operation == nil || operation["x-required-scope"] != expected.scope {
			t.Fatalf("route %s scope = %#v, want %s", route, operation["x-required-scope"], expected.scope)
		}
		responses, ok := operation["responses"].(map[string]any)
		if !ok {
			t.Fatalf("route %s responses are missing", route)
		}
		for _, code := range expected.codes {
			if _, exists := responses[code]; !exists {
				t.Errorf("route %s is missing response %s", route, code)
			}
		}
	}
}

func TestKnowledgeReviewAndRetrievalSchemasValidateDomainPayloads(t *testing.T) {
	document, err := openapi3.NewLoader().LoadFromFile("openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if err := document.Validate(context.Background()); err != nil {
		t.Fatal(err)
	}
	decodeDomain := func(value any) map[string]any {
		t.Helper()
		encoded, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		var decoded map[string]any
		if err := json.Unmarshal(encoded, &decoded); err != nil {
			t.Fatal(err)
		}
		return decoded
	}
	const (
		revisionID = "10000000-0000-4000-8000-000000000001"
		documentID = "20000000-0000-4000-8000-000000000001"
		nodeID     = "30000000-0000-4000-8000-000000000001"
		nodeRevID  = "40000000-0000-4000-8000-000000000001"
	)
	review := knowledge.IdentityReview{
		BasisHash: strings.Repeat("a", 64), OperationID: revisionID, Receipt: strings.Repeat("b", 64),
		Documents: []knowledge.DocumentIdentityReview{{
			Path: "notes/topic.md", Locator: strings.Repeat("c", 64), ReasonCode: "document_match_ambiguous",
			Candidates: []knowledge.IdentityCandidate{{StableID: documentID, RevisionID: revisionID, ReasonCode: "same_path", Evidence: map[string]any{"path": "notes/topic.md"}}},
		}},
		Nodes: []knowledge.NodeIdentityReview{{
			Path: "notes/topic.md", Locator: strings.Repeat("d", 64), Preorder: 0, ReasonCode: "node_match_ambiguous",
			Candidates: []knowledge.IdentityCandidate{{StableID: nodeID, RevisionID: nodeRevID, ReasonCode: "node_similarity", Score: 500000, Evidence: map[string]any{"semantic_similarity": 500000}}},
		}},
	}
	conflict := map[string]any{
		"error":           map[string]any{"code": "identity_review_required", "message": "Knowledge import could not be committed", "request_id": "request-1"},
		"identity_review": decodeDomain(review),
	}
	conflictSchema := document.Paths.Find("/v1/knowledge/imports").Post.Responses.Value("409").Value.Content.Get("application/json").Schema.Value
	if err := conflictSchema.VisitJSON(conflict, openapi3.EnableJSONSchema2020()); err != nil {
		t.Fatalf("real identity review response failed schema validation: %v", err)
	}

	sourceRange := knowledge.SourceRange{Start: 0, End: 7, StartLine: 1, EndLine: 1}
	candidate := knowledge.Candidate{Ordinal: 0, NodeRevisionID: nodeRevID, Score: 1000000, Title: "Topic", TitleSHA256: strings.Repeat("e", 64), HasChildren: false, LocalBodyScore: 1000000}
	retrieval := knowledge.RetrievalResult{
		KnowledgeRevisionID: revisionID, RetrieverVersion: knowledge.RetrieverVersion, SelectorVersion: knowledge.SelectorVersion,
		QueryContextVersion: knowledge.QueryContextVersion, SummarySnapshot: []string{}, DocumentShortlist: []string{"notes/topic.md"},
		Trace: []knowledge.RetrievalTrace{{
			Index: 0, Depth: 0, ParentNodeRevisionID: "50000000-0000-4000-8000-000000000001", Candidates: []knowledge.Candidate{candidate},
			Decisions: []knowledge.Decision{{NodeRevisionID: nodeRevID, Action: "select"}}, CandidateSetHash: strings.Repeat("f", 64), ReasonCode: "selector_not_configured", Degraded: true,
		}},
		Hits: []knowledge.RetrievalHit{{
			DocumentID: documentID, DocumentRevisionID: revisionID, NodeID: nodeID, NodeRevisionID: nodeRevID, Path: "notes/topic.md",
			HeadingRange: sourceRange, LocalBodyRange: sourceRange, SectionRange: sourceRange, CanonicalSlice: "# Topic", SliceSHA256: strings.Repeat("a", 64), Provenance: "canonical_markdown",
		}},
		Degraded: true,
	}
	retrievalSchema := document.Paths.Find("/v1/knowledge/retrievals").Post.Responses.Value("200").Value.Content.Get("application/json").Schema.Value
	if err := retrievalSchema.VisitJSON(decodeDomain(retrieval), openapi3.EnableJSONSchema2020()); err != nil {
		t.Fatalf("real retrieval response failed schema validation: %v", err)
	}
}

func TestKnowledgeConflictResponseSchemaValidatesRealPayload(t *testing.T) {
	document, err := openapi3.NewLoader().LoadFromFile("openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if err := document.Validate(context.Background()); err != nil {
		t.Fatal(err)
	}
	response := document.Paths.Find("/v1/knowledge/imports").Post.Responses.Value("409")
	if response == nil || response.Value == nil {
		t.Fatal("knowledge import 409 response is missing")
	}
	media := response.Value.Content.Get("application/json")
	if media == nil || media.Schema == nil || media.Schema.Value == nil {
		t.Fatal("knowledge import 409 response schema is missing")
	}
	payload := map[string]any{}
	if err := json.Unmarshal([]byte(`{
		"error":{"code":"revision_conflict","message":"Knowledge operation failed","request_id":"request-1"},
		"current_revision_id":"10000000-0000-4000-8000-000000000001"
	}`), &payload); err != nil {
		t.Fatal(err)
	}
	if err := media.Schema.Value.VisitJSON(payload, openapi3.EnableJSONSchema2020()); err != nil {
		t.Fatalf("real conflict response failed schema validation: %v", err)
	}
	payload["current_revision_id"] = nil
	if err := media.Schema.Value.VisitJSON(payload, openapi3.EnableJSONSchema2020()); err != nil {
		t.Fatalf("empty-head conflict response failed schema validation: %v", err)
	}
	payload["unexpected"] = true
	if err := media.Schema.Value.VisitJSON(payload, openapi3.EnableJSONSchema2020()); err == nil {
		t.Fatal("conflict schema accepted an undeclared top-level response field")
	}
}
