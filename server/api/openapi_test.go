package api_test

import (
	"context"
	"encoding/json"
	"os"
	"testing"

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
